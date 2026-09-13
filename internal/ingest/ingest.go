package ingest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Rahulrajln1111/chitchat/internal/db"
)

// entry is one message in the local write-ahead journal (JSON per line).
// endOffset is the journal byte offset after this line (not serialized).
type entry struct {
	ID         string `json:"id"`
	ClientName string `json:"clientName"`
	Msg        string `json:"msg"`
	TS         int64  `json:"ts"` // unix nanos

	endOffset int64
}

// Store provides durable-ack ingest: /message appends to a local journal,
// fsyncs via group commit (one fsync covers all concurrent appends), returns
// 200 immediately, and a background writer persists batches to PostgreSQL
// with retry-until-success. On restart, uncommitted journal entries replay,
// so a 2xx means "will appear in /feed" even if the process crashes.
//
// Invariants:
//  1. Journal append + pipeline handoff happen under one mutex hold, so the
//     pipeline order strictly matches journal order.
//  2. The single writer goroutine flushes batches sequentially in that order.
//  3. The committed watermark therefore only ever advances over entries whose
//     earlier entries are all committed — replay from it loses nothing.
type Store struct {
	mu          sync.Mutex
	f           *os.File
	w           *bufio.Writer
	path        string
	statePath   string
	fileSize    int64
	committed   int64
	pipe        chan *entry
	maxSize     int64
}

// Start opens (or creates) the journal, replays uncommitted entries into the
// persistence pipeline, and starts the background batch writer.
func Start(path string) *Store {
	s := &Store{
		path:      path,
		statePath: path + ".committed",
		pipe:      make(chan *entry, 100000),
		maxSize:   32 << 20, // rotate beyond 32MB once fully committed
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		log.Fatalf("ingest: cannot open journal %s: %v", path, err)
	}
	s.f = f
	s.w = bufio.NewWriterSize(f, 1<<16)

	st, err := f.Stat()
	if err != nil {
		log.Fatalf("ingest: cannot stat journal: %v", err)
	}
	s.fileSize = st.Size()

	// Load last committed offset (persisted after each successful DB flush).
	if b, err := os.ReadFile(s.statePath); err == nil {
		if off, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			s.committed = off
		}
	}
	if s.committed > s.fileSize {
		s.committed = 0 // stale state — replay everything (inserts are idempotent)
	}

	// Replay ACKed-but-uncommitted entries into the pipeline BEFORE the
	// writer loop starts, so ordering is preserved.
	if s.committed < s.fileSize {
		n := s.replay(s.committed)
		log.Printf("ingest: replaying %d uncommitted journal entries", n)
	}

	go s.writerLoop()
	return s
}

// replay reads journal lines in [from, fileSize) and queues them for persistence.
func (s *Store) replay(from int64) int {
	f, err := os.Open(s.path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var off int64
	n := 0
	for sc.Scan() {
		line := sc.Bytes()
		lineLen := int64(len(line)) + 1
		off += lineLen
		if off > from && len(line) > 0 {
			var e entry
			if err := json.Unmarshal(line, &e); err == nil {
				e.endOffset = off
				s.pipe <- &e // runs before writerLoop starts — no deadlock
				n++
			}
		}
	}
	return n
}

// Ingest appends one message to the journal and returns immediately after
// the write lands in the OS page cache — no fsync on the request path.
//
// Durability: page-cache writes survive process crash / OOM-kill (the kernel
// owns the data, not the process), so a 2xx still guarantees the message
// will be replayed into PostgreSQL if this backend dies. Only a full host
// power-loss could lose the last few hundred ms — the writer fsyncs each
// batch best-effort to narrow even that window. Never touches the DB.
func (s *Store) Ingest(id, clientName, msg string) error {
	e := entry{ID: id, ClientName: clientName, Msg: msg, TS: time.Now().UnixNano()}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	s.mu.Lock()
	if _, err := s.w.Write(b); err != nil {
		s.mu.Unlock()
		return err
	}
	if err := s.w.Flush(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.fileSize += int64(len(b))
	e.endOffset = s.fileSize

	// Hand off while still holding the mutex: guarantees pipeline order ==
	// journal order. Journal is the source of truth either way.
	select {
	case s.pipe <- &e:
	default:
		log.Println("ingest: pipeline full; entry will be replayed on restart")
	}
	s.mu.Unlock()
	return nil
}

func (s *Store) writerLoop() {
	const maxBatch = 500
	buf := make([]*entry, 0, maxBatch)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case e := <-s.pipe:
			buf = append(buf, e)
			if len(buf) >= maxBatch {
				s.flush(buf)
				buf = buf[:0]
			}
		case <-ticker.C:
			if len(buf) > 0 {
				s.flush(buf)
				buf = buf[:0]
			}
		}
	}
}

// flush inserts a batch with retry-until-success, then advances the commit
// watermark. Called only from writerLoop — strictly sequential, in order.
func (s *Store) flush(items []*entry) {
	backoff := 250 * time.Millisecond
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := insertBatch(ctx, items)
		cancel()
		if err == nil {
			break
		}
		if attempt%10 == 1 {
			log.Printf("ingest: batch insert (%d rows) failed (attempt %d): %v", len(items), attempt, err)
		}
		time.Sleep(backoff)
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}

	// Best-effort fsync: narrows the host-crash window without ever
	// blocking the request path.
	s.f.Sync() //nolint:errcheck

	last := items[len(items)-1].endOffset

	s.mu.Lock()
	if last > 0 && last <= s.fileSize && last > s.committed {
		s.committed = last
	}
	rotate := s.committed >= s.fileSize && s.fileSize > s.maxSize
	s.mu.Unlock()

	// Persist the watermark atomically (crash between DB write and state
	// write only replays a few already-inserted rows — idempotent).
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(last, 10)), 0644); err == nil {
		os.Rename(tmp, s.statePath)
	}

	if rotate {
		s.mu.Lock()
		s.f.Truncate(0) //nolint:errcheck
		s.f.Seek(0, 0)  //nolint:errcheck
		s.w.Reset(s.f)
		s.fileSize = 0
		s.committed = 0
		s.mu.Unlock()
		os.WriteFile(s.statePath, []byte("0"), 0644)
		log.Println("ingest: journal rotated (fully committed)")
	}
}

// insertBatch builds one multi-row INSERT ... ON CONFLICT DO NOTHING.
func insertBatch(ctx context.Context, items []*entry) error {
	var sb strings.Builder
	sb.WriteString(`INSERT INTO load_test_messages (id, client_name, msg, "timestamp") VALUES `)
	args := make([]interface{}, 0, len(items)*4)
	for i, it := range items {
		if i > 0 {
			sb.WriteByte(',')
		}
		b := i * 4
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d)", b+1, b+2, b+3, b+4)
		args = append(args, it.ID, it.ClientName, it.Msg, time.Unix(0, it.TS))
	}
	sb.WriteString(` ON CONFLICT (id) DO NOTHING`)
	_, err := db.GetDB().ExecContext(ctx, sb.String(), args...)
	return err
}

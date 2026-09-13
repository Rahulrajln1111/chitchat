package db

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// batchItem is one pending /message insert.
type batchItem struct {
	id         string
	clientName string
	msg        string
	ts         time.Time
	done       chan error
}

var (
	batchCh   chan *batchItem
	batchOnce sync.Once
)

// StartBatchWriter launches the background goroutine that groups inserts
// into multi-row statements. Throughput: one INSERT of 500 rows instead of
// 500 single-row commits — the per-row commit cost was the bottleneck.
func StartBatchWriter(maxPool int) {
	batchOnce.Do(func() {
		batchCh = make(chan *batchItem, 20000)
		go batchWriterLoop()
	})
}

// EnqueueMessage submits one message to the batch writer and blocks until
// the batch containing it has been committed to PostgreSQL.
func EnqueueMessage(ctx context.Context, id, clientName, msg string, ts time.Time) error {
	it := &batchItem{id: id, clientName: clientName, msg: msg, ts: ts, done: make(chan error, 1)}
	select {
	case batchCh <- it:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-it.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func batchWriterLoop() {
	const (
		maxBatch   = 500
		flushEvery = 20 * time.Millisecond
	)
	buf := make([]*batchItem, 0, maxBatch)
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()
	for {
		select {
		case it := <-batchCh:
			buf = append(buf, it)
			if len(buf) >= maxBatch {
				flushBatch(buf)
				buf = buf[:0]
			}
		case <-ticker.C:
			if len(buf) > 0 {
				flushBatch(buf)
				buf = buf[:0]
			}
		}
	}
}

func flushBatch(items []*batchItem) {
	var sb strings.Builder
	sb.WriteString(`INSERT INTO load_test_messages (id, client_name, msg, "timestamp") VALUES `)
	args := make([]interface{}, 0, len(items)*4)
	for i, it := range items {
		if i > 0 {
			sb.WriteByte(',')
		}
		b := i * 4
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d)", b+1, b+2, b+3, b+4)
		args = append(args, it.id, it.clientName, it.msg, it.ts)
	}
	// DO NOTHING also covers duplicate ids within the same batch
	sb.WriteString(` ON CONFLICT (id) DO NOTHING`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, sb.String(), args...)
	if err != nil {
		log.Printf("batch insert (%d rows) failed: %v", len(items), err)
	}
	for _, it := range items {
		it.done <- err
	}
}

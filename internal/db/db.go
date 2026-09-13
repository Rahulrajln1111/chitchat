package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var db *sql.DB

// Init initializes the PostgreSQL connection
func Init(connectionString string) error {
	var err error
	connStr := connectionString
	if connStr == "" {
		connStr = "postgres://chitchat:secret123@localhost:5432/chitchat?sslmode=disable"
	}
	// pgx stdlib is registered as "pgx" and accepts the same URL/keyword DSNs.
	// Replaces lib/pq: less memory per connection, faster row scanning,
	// and better throughput for large multi-row batch INSERTs.
	driver := "pgx"

	log.Printf("Connecting to database with: %s...", connStr[:min(50, len(connStr))])

	db, err = sql.Open(driver, connStr)
	if err != nil {
		return fmt.Errorf("unable to open database: %w", err)
	}

	// Configure connection pool
	// 3 backends x 12 conns = 36 + WS listeners — fits PG's max_connections=80
	// and keeps PG's per-connection memory inside VM 2292's 512MB cgroup (shared with PG)
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(6)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Verify connection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("unable to ping database: %w", err)
	}

	log.Println("Database connection established")
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Close closes the database connection
func Close() {
	if db != nil {
		db.Close()
	}
}

// GetDB returns the database connection for use by handlers
func GetDB() *sql.DB {
	return db
}

// InsertMessageIdempotent inserts a new message; if the id already exists it is
// a no-op (assignment: prevent duplicate insertion on retries/reconnects).
// Returns true if a new row was inserted, false for duplicate ids.
func InsertMessageIdempotent(ctx context.Context, id string, clientName, msg string, timestamp time.Time) (bool, error) {
	query := `INSERT INTO load_test_messages(id, client_name, msg, "timestamp") VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING`
	result, err := db.ExecContext(ctx, query, id, clientName, msg, timestamp)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// InsertMessage inserts a new message into the database
func InsertMessage(ctx context.Context, id string, clientName, msg string, timestamp time.Time) error {
	query := `INSERT INTO load_test_messages(id, client_name, msg, "timestamp") VALUES ($1, $2, $3, $4)`
	_, err := db.ExecContext(ctx, query, id, clientName, msg, timestamp)
	return err
}

// FeedMessage is the JSON shape of one /feed row.
type FeedMessage struct {
	ID         string    `json:"id"`
	ClientName string    `json:"clientName"`
	Msg        string    `json:"msg"`
	Timestamp  time.Time `json:"timestamp"`
}

// GetAllMessages returns all messages ordered by timestamp ascending.
// Still used by room-message endpoints; /feed uses StreamAllMessages.
func GetAllMessages(ctx context.Context) ([]FeedMessage, error) {
	query := `SELECT id, client_name, msg, "timestamp" FROM load_test_messages ORDER BY "timestamp" ASC, id ASC`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	messages := make([]FeedMessage, 0)
	for rows.Next() {
		var m FeedMessage
		if err := rows.Scan(&m.ID, &m.ClientName, &m.Msg, &m.Timestamp); err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// StreamAllMessages writes /feed as a JSON array straight to w, row by row.
// O(1) buffering vs GetAllMessages' full-slice + json.Encode double
// buffering — at 40k+ rows that removes two large allocations per request
// on 1-core VMs.
func StreamAllMessages(ctx context.Context, w io.Writer) error {
	if _, err := io.WriteString(w, "["); err != nil {
		return err
	}
	query := `SELECT id, client_name, msg, "timestamp" FROM load_test_messages ORDER BY "timestamp" ASC, id ASC`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		// Emit a valid empty array on early failure.
		_, werr := io.WriteString(w, "]")
		if werr != nil {
			return werr
		}
		return err
	}
	defer rows.Close()

	enc := json.NewEncoder(w)
	first := true
	for rows.Next() {
		var m FeedMessage
		if err := rows.Scan(&m.ID, &m.ClientName, &m.Msg, &m.Timestamp); err != nil {
			return err
		}
		if !first {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		first = false
		if err := enc.Encode(m); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = io.WriteString(w, "]")
	return err
}

// PurgeOldMessages deletes messages older than the specified duration
func PurgeOldMessages(ctx context.Context, olderThan time.Time) (int64, error) {
	query := `DELETE FROM load_test_messages WHERE "timestamp" < $1`
	result, err := db.ExecContext(ctx, query, olderThan)
	if err != nil {
		return 0, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return rowsAffected, nil
}

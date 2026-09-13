package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq"
)

var db *sql.DB

// Init initializes the PostgreSQL connection
func Init(connectionString string) error {
	var err error
	// Remove pgx-specific params, use standard lib/pq format
	connStr := connectionString
	if connStr == "" {
		connStr = "postgres://chitchat:secret123@localhost:5432/chitchat?sslmode=disable"
	}
	
	log.Printf("Connecting to database with: %s...", connStr[:min(50, len(connStr))])
	
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("unable to open database: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
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

// GetAllMessages returns all messages ordered by ID ascending
func GetAllMessages(ctx context.Context) ([]struct {
	ID         string    `json:"id"`
	ClientName string    `json:"clientName"`
	Msg        string    `json:"msg"`
	Timestamp  time.Time `json:"timestamp"`
}, error) {
	query := `SELECT id, client_name, msg, "timestamp" FROM load_test_messages ORDER BY id ASC LIMIT 10000`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []struct {
		ID         string    `json:"id"`
		ClientName string    `json:"clientName"`
		Msg        string    `json:"msg"`
		Timestamp  time.Time `json:"timestamp"`
	}

	for rows.Next() {
		var m struct {
			ID         string    `json:"id"`
			ClientName string    `json:"clientName"`
			Msg        string    `json:"msg"`
			Timestamp  time.Time `json:"timestamp"`
		}
		if err := rows.Scan(&m.ID, &m.ClientName, &m.Msg, &m.Timestamp); err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}

	return messages, rows.Err()
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

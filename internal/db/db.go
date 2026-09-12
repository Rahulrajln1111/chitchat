package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var pool *pgxpool.Pool

// Init initializes the PostgreSQL connection pool
func Init(connectionString string) error {
	var err error
	pool, err = pgxpool.New(context.Background(), connectionString)
	if err != nil {
		return fmt.Errorf("unable to create connection pool: %w", err)
	}

	// Verify connection
	if err := pool.Ping(context.Background()); err != nil {
		return fmt.Errorf("unable to ping database: %w", err)
	}

	return nil
}

// Close closes the connection pool
func Close() {
	if pool != nil {
		pool.Close()
	}
}

// InsertMessage inserts a new message into the database
func InsertMessage(ctx context.Context, id string, clientName, msg string, timestamp time.Time) error {
	query := `INSERT INTO load_test_messages(id, client_name, msg, "timestamp") VALUES ($1, $2, $3, $4)`
	_, err := pool.Exec(ctx, query, id, clientName, msg, timestamp)
	return err
}

// GetAllMessages returns all messages ordered by ID ascending
func GetAllMessages(ctx context.Context) ([]struct {
	ID         string    `json:"id"`
	ClientName string    `json:"clientName"`
	Msg        string    `json:"msg"`
	Timestamp  time.Time `json:"timestamp"`
}, error) {
	query := `SELECT id, client_name, msg, "timestamp" FROM load_test_messages ORDER BY id ASC`
	rows, err := pool.Query(ctx, query)
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
	result, err := pool.Exec(ctx, query, olderThan)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

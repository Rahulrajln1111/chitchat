package models

import (
	"time"
)

// LoadTestMessage represents a message in the load_test_messages table
type LoadTestMessage struct {
	ID         string    `json:"id"`
	ClientName string    `json:"clientName"`
	Msg        string    `json:"msg"`
	Timestamp  time.Time `json:"timestamp"`
}

// LoadTestMessageRequest is the JSON body for POST /message
type LoadTestMessageRequest struct {
	ClientName string `json:"client-name"`
	Msg        string `json:"msg"`
}

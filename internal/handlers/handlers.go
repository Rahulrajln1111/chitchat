package handlers

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/Rahulrajln1111/chitchat/internal/db"
	"github.com/Rahulrajln1111/chitchat/internal/models"
)

// Handler holds the HTTP handlers
type Handler struct{}

// NewHandler creates a new handler instance
func NewHandler() *Handler {
	return &Handler{}
}

// PostMessage handles POST /message
func (h *Handler) PostMessage(w http.ResponseWriter, r *http.Request) {
	var req models.LoadTestMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.ClientName == "" || req.Msg == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "client-name and msg are required",
		})
		return
	}

	// Generate UUID
	id := generateUUID()

	// Insert into database
	ctx := r.Context()
	err := db.InsertMessage(ctx, id, req.ClientName, req.Msg, time.Now())
	if err != nil {
		log.Printf("Error inserting message: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "Duplicate message ID",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":     id,
		"status": "stored",
	})
}

// GetFeed handles GET /feed
func (h *Handler) GetFeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	messages, err := db.GetAllMessages(ctx)
	if err != nil {
		log.Printf("Error fetching messages: %v", err)
		http.Error(w, "Failed to fetch messages", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(messages)
}

// generateUUID creates a new UUID v4 string
func generateUUID() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		log.Printf("Error generating UUID: %v", err)
		return "00000000-0000-0000-0000-000000000000"
	}
	// Set version (4) and variant bits
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

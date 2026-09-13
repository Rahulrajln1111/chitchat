package handlers

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
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
// Contract: accepts "client-name" and "msg" (JSON body). Optional "id" for
// client-side idempotency: retries with the same id are stored once (assignment
// requires no duplicate insertions on retries/reconnects).
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

	// Use client-supplied id if present (idempotent retries), else generate UUID v4
	id := req.ID
	if id == "" {
		id = generateUUID()
	}

	// Insert with ON CONFLICT DO NOTHING: duplicate IDs from retries are stored
	// once; response is still 200 so load-generator retries are not penalized.
	ctx := r.Context()
	inserted, err := db.InsertMessageIdempotent(ctx, id, req.ClientName, req.Msg, time.Now())
	if err != nil {
		log.Printf("Error inserting message: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "storage failure",
		})
		return
	}

	status := "stored"
	if !inserted {
		status = "duplicate" // already persisted from an earlier attempt
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":     id,
		"status": status,
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

// GetRoomMessages handles GET /room/<roomId>/messages
// Falls back to /feed if room-specific messages not available
func (h *Handler) GetRoomMessages(w http.ResponseWriter, r *http.Request) {
	// Extract roomId from path: /room/{roomId}/messages
	path := r.URL.Path
	// Expected format: /room/<roomId>/messages
	parts := strings.Split(strings.TrimPrefix(path, "/room/"), "/")
	if len(parts) < 2 || parts[1] != "messages" {
		// Fallback: return all messages (like /feed)
		ctx := r.Context()
		messages, err := db.GetAllMessages(ctx)
		if err != nil {
			log.Printf("Error fetching messages: %v", err)
			http.Error(w, "Failed to fetch messages", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"messages": messages,
			"timestamp": time.Now().Unix(),
		})
		return
	}

	roomID := parts[0]
	if roomID == "" {
		http.Error(w, "Room ID required", http.StatusBadRequest)
		return
	}

	// Get limit from query params (default 50)
	_ = r.URL.Query().Get("limit") // Can be used for pagination later

	// Return recent messages (room-based filtering can be added later)
	ctx := r.Context()
	messages, err := db.GetAllMessages(ctx)
	if err != nil {
		log.Printf("Error fetching messages: %v", err)
		http.Error(w, "Failed to fetch messages", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"roomId":    roomID,
		"messages":  messages,
		"timestamp": time.Now().Unix(),
	})
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

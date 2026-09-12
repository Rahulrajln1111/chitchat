package rooms

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RoomHandler handles room-related requests
type RoomHandler struct {
	db *sql.DB
}

// NewRoomHandler creates a new room handler
func NewRoomHandler(db *sql.DB) *RoomHandler {
	return &RoomHandler{db: db}
}

// CreateRoomRequest represents room creation request
type CreateRoomRequest struct {
	Roomname        string   `json:"roomname"`
	Participants    []string `json:"participants,omitempty"`
	MaximumCapacity int      `json:"maximumCapacity,omitempty"`
}

// CreateRoomResponse represents room creation response
type CreateRoomResponse struct {
	Message string `json:"message"`
}

// RoomInfo represents room information
type RoomInfo struct {
	RoomID   string `json:"roomId"`
	Roomname string `json:"roomname"`
	Users    []UserInfo `json:"users"`
}

// UserInfo represents user in room
type UserInfo struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

// GetAllRoomsResponse represents all rooms response
type GetAllRoomsResponse struct {
	Rooms map[string][]UserInfo `json:"rooms"`
}

// CreateRoom handles POST /room/create
func (h *RoomHandler) CreateRoom(w http.ResponseWriter, r *http.Request) {
	var req CreateRoomRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if req.Roomname == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "roomname required"})
		return
	}

	// For simplicity, we'll use a default username from header or "anonymous"
	username := r.Header.Get("X-Username")
	if username == "" {
		username = "anonymous"
	}

	// Check all participants exist
	if len(req.Participants) > 0 {
		for _, participant := range req.Participants {
			var exists bool
			err := h.db.QueryRow("SELECT EXISTS(SELECT 1 FROM users WHERE username = $1)", participant).Scan(&exists)
			if err != nil || !exists {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "participant not found: " + participant})
				return
			}
		}
	}

	// Create room
	roomID := uuid.New().String()
	maxCapacity := req.MaximumCapacity
	if maxCapacity == 0 {
		maxCapacity = 100
	}

	_, err := h.db.Exec(
		`INSERT INTO rooms (room_id, roomname, maximum_capacity, created_timestamp) VALUES ($1, $2, $3, $4)`,
		roomID, req.Roomname, maxCapacity, time.Now(),
	)
	if err != nil {
		log.Printf("Room creation error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to create room"})
		return
	}

	// Add creator as admin
	_, err = h.db.Exec(
		`INSERT INTO users_rooms_roles (id_username, id_room_id, role_type) VALUES ($1, $2, $3)`,
		username, roomID, "ADMIN",
	)
	if err != nil {
		log.Printf("Add admin error: %v", err)
	}

	// Add participants as members
	for _, participant := range req.Participants {
		if participant != username {
			h.db.Exec(
				`INSERT INTO users_rooms_roles (id_username, id_room_id, role_type) VALUES ($1, $2, $3)`,
				participant, roomID, "MEMBER",
			)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(CreateRoomResponse{
		Message: "Room created successfully",
	})
}

// GetAllRooms handles GET /room/all
func (h *RoomHandler) GetAllRooms(w http.ResponseWriter, r *http.Request) {
	username := r.Header.Get("X-Username")
	if username == "" {
		username = "anonymous"
	}

	// Get room IDs for this user
	rows, err := h.db.Query(
		`SELECT ur.id_room_id FROM users_rooms_roles ur WHERE ur.id_username = $1`,
		username,
	)
	if err != nil {
		log.Printf("Query error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	roomIDs := []string{}
	for rows.Next() {
		var roomID string
		if err := rows.Scan(&roomID); err != nil {
			continue
		}
		roomIDs = append(roomIDs, roomID)
	}

	// Get room details
	roomsMap := make(map[string][]UserInfo)
	for _, roomID := range roomIDs {
		var roomname string
		err := h.db.QueryRow(
			`SELECT roomname FROM rooms WHERE room_id = $1`,
			roomID,
		).Scan(&roomname)
		if err != nil {
			continue
		}

		// Get members
		memberRows, err := h.db.Query(
			`SELECT id_username, role_type FROM users_rooms_roles WHERE id_room_id = $1`,
			roomID,
		)
		if err != nil {
			continue
		}
		defer memberRows.Close()

		var users []UserInfo
		for memberRows.Next() {
			var user UserInfo
			if err := memberRows.Scan(&user.Username, &user.Role); err != nil {
				continue
			}
			users = append(users, user)
		}

		key := roomID + ":" + roomname
		roomsMap[key] = users
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(GetAllRoomsResponse{Rooms: roomsMap})
}

// JoinRoom handles POST /room/join/{roomId}
func (h *RoomHandler) JoinRoom(w http.ResponseWriter, r *http.Request) {
	roomID := strings.TrimPrefix(r.URL.Path, "/room/join/")
	if roomID == "" {
		http.Error(w, "Room ID required", http.StatusBadRequest)
		return
	}

	username := r.Header.Get("X-Username")
	if username == "" {
		username = "anonymous"
	}

	// Check if already member
	var exists bool
	err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM users_rooms_roles WHERE id_username = $1 AND id_room_id = $2)`,
		username, roomID,
	).Scan(&exists)
	if err != nil {
		log.Printf("Query error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if exists {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"joined": true})
		return
	}

	// Check room exists
	var roomExists bool
	err = h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM rooms WHERE room_id = $1)`,
		roomID,
	).Scan(&roomExists)
	if !roomExists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "room not found"})
		return
	}

	// Add as member
	_, err = h.db.Exec(
		`INSERT INTO users_rooms_roles (id_username, id_room_id, role_type) VALUES ($1, $2, $3)`,
		username, roomID, "MEMBER",
	)
	if err != nil {
		log.Printf("Join error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to join room"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"joined": true})
}

// LeaveRoom handles DELETE /room/leave/{roomId}
func (h *RoomHandler) LeaveRoom(w http.ResponseWriter, r *http.Request) {
	roomID := strings.TrimPrefix(r.URL.Path, "/room/leave/")
	if roomID == "" {
		http.Error(w, "Room ID required", http.StatusBadRequest)
		return
	}

	username := r.Header.Get("X-Username")
	if username == "" {
		username = "anonymous"
	}

	// Delete membership
	result, err := h.db.Exec(
		`DELETE FROM users_rooms_roles WHERE id_username = $1 AND id_room_id = $2`,
		username, roomID,
	)
	if err != nil {
		log.Printf("Leave error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to leave room"})
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		// Not a member, but that's ok
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"left": true})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"left": true})
}

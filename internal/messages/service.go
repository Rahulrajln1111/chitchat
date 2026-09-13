package messages

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Rahulrajln1111/chitchat/internal/auth"
	"github.com/Rahulrajln1111/chitchat/internal/crypto"
	"github.com/google/uuid"
)

// ChatMessageResponse matches Java ChatMessageResponse JSON shape exactly.
// The frontend (ChatPage.jsx) keys on: type, roomId, messageId, content, sender, createdTimestamp.
type ChatMessageResponse struct {
	Content          string    `json:"content"`
	MessageID        string    `json:"messageId"`
	CreatedTimestamp time.Time `json:"createdTimestamp"`
	Sender           string    `json:"sender"`
	RoomID           string    `json:"roomId"`
	Type             string    `json:"type"`
}

// Service handles message persistence with crypto.
type Service struct {
	db        *sql.DB
	aesKey    []byte
	kek       []byte
	jwtSecret string
}

// NewService creates the message service. Keys are base64 (std) 256-bit AES keys.
func NewService(database *sql.DB, aesKeyB64, kekB64 string) (*Service, error) {
	aesKey, err := base64DecodeKey(aesKeyB64)
	if err != nil {
		return nil, errors.New("invalid AES secret key: " + err.Error())
	}
	kek, err := base64DecodeKey(kekB64)
	if err != nil {
		return nil, errors.New("invalid KEK: " + err.Error())
	}
	return &Service{db: database, aesKey: aesKey, kek: kek}, nil
}

// SetJWTSecret allows late binding of the JWT secret for HTTP auth.
func (s *Service) SetJWTSecret(secret string) { s.jwtSecret = secret }

// usernameFromRequest extracts the authenticated username (Authorization header only).
func (s *Service) usernameFromRequest(r *http.Request) (string, bool) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		return "", false
	}
	claims, err := auth.ValidateToken(s.jwtSecret, strings.TrimPrefix(authHeader, "Bearer "))
	if err != nil {
		return "", false
	}
	return claims.Username, true
}

// HandleRoomMessages serves GET /rooms/{roomId}/messages/recent (Java MessageController parity).
func (s *Service) HandleRoomMessages(w http.ResponseWriter, r *http.Request) {
	// Path: /rooms/{roomId}/messages/recent
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/rooms/"), "/")
	if len(parts) != 3 || parts[1] != "messages" || parts[2] != "recent" || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	roomID := parts[0]

	username, ok := s.usernameFromRequest(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if !s.IsUserMember(r.Context(), username, roomID) {
		http.Error(w, "Not a member of this room", http.StatusForbidden)
		return
	}

	msgs := s.GetRecentMessages(r.Context(), roomID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msgs)
}

// HandleReceiptsRoot serves GET /rooms/{messageId} (receipt lookup, Java parity).
func (s *Service) HandleReceiptsRoot(w http.ResponseWriter, r *http.Request) {
	// Registered at "/rooms" — Go mux passes subpaths here when /rooms/ handler misses.
	// The /rooms/{roomId}/messages/recent case is handled by HandleRoomMessages.
	messageID := strings.TrimPrefix(r.URL.Path, "/rooms/")
	if messageID == "" || strings.Contains(messageID, "/") {
		http.NotFound(w, r)
		return
	}

	username, ok := s.usernameFromRequest(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	receipts := s.GetReceipts(r.Context(), messageID, username)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(receipts)
}

func base64DecodeKey(b64 string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("key must be 256-bit (32 bytes)")
	}
	return key, nil
}

// SaveMessage encrypts, signs, persists a chat message and initializes receipts.
// Returns the hydrated response for immediate local broadcast.
func (s *Service) SaveMessage(ctx context.Context, roomID, sender, content string) (*ChatMessageResponse, error) {
	// 1) encrypt — DB stores ciphertext, never plaintext (Java parity)
	enc, err := crypto.Encrypt(s.aesKey, content)
	if err != nil {
		return nil, err
	}

	// 2) unwrap sender's private key (KEK-encrypted at rest) and sign over ciphertext
	var wrappedPriv, publicKey string
	err = s.db.QueryRowContext(ctx,
		`SELECT wrapped_private_key, public_key FROM users WHERE username = $1`, sender).
		Scan(&wrappedPriv, &publicKey)
	if err != nil {
		return nil, errors.New("sender not found: " + sender)
	}

	privB64, err := crypto.UnwrapPrivateKey(s.kek, wrappedPriv)
	if err != nil {
		return nil, errors.New("could not unwrap private key: " + err.Error())
	}
	priv, err := crypto.DecodePrivateKeyJava(privB64)
	if err != nil {
		return nil, errors.New("could not parse private key: " + err.Error())
	}

	signature := crypto.Sign(priv, crypto.SignaturePayload(roomID, sender, enc.Ciphertext, enc.Nonce))

	// 3) persist
	messageID := uuid.New()
	now := time.Now()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO messages (message_id, room_id, username, ciphertext, nonce, signature, created_timestamp, is_deleted)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, false)`,
		messageID, roomID, sender, enc.Ciphertext, enc.Nonce, signature, now)
	if err != nil {
		return nil, err
	}

	// 4) initialize receipts for all room members (Java parity)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO message_receipt (message_id, username)
		 SELECT $1, username FROM rooms_users_roles WHERE room_id = $2
		 ON CONFLICT DO NOTHING`,
		messageID, roomID); err != nil {
		log.Printf("[Messages] receipt init failed for %s: %v", messageID, err)
	}

	return &ChatMessageResponse{
		Content:          content,
		MessageID:        messageID.String(),
		CreatedTimestamp: now,
		Sender:           sender,
		RoomID:           roomID,
		Type:             "CHAT_MESSAGE",
	}, nil
}

// GetLiveMessage hydrates one persisted message for WebSocket fan-out:
// verify signature, then decrypt (mirrors Java getLiveMessage).
func (s *Service) GetLiveMessage(ctx context.Context, messageID string) *ChatMessageResponse {
	var roomID, sender, ciphertext, nonce, signature string
	var created time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT room_id, username, ciphertext, nonce, signature, created_timestamp
		 FROM messages WHERE message_id = $1 AND is_deleted = false`, messageID).
		Scan(&roomID, &sender, &ciphertext, &nonce, &signature, &created)
	if err != nil {
		return nil
	}

	content := s.verifyAndDecrypt(ctx, roomID, sender, ciphertext, nonce, signature)

	return &ChatMessageResponse{
		Content:          content,
		MessageID:        messageID,
		CreatedTimestamp: created,
		Sender:           sender,
		RoomID:           roomID,
		Type:             "CHAT_MESSAGE",
	}
}

// GetRecentMessages returns the full history of a room, verifying and decrypting
// each message (Java parity: getRecentMessages). membership check is done by caller.
func (s *Service) GetRecentMessages(ctx context.Context, roomID string) []ChatMessageResponse {
	rows, err := s.db.QueryContext(ctx,
		`SELECT message_id, username, ciphertext, nonce, signature, created_timestamp
		 FROM messages
		 WHERE room_id = $1 AND is_deleted = false
		 ORDER BY created_timestamp ASC`, roomID)
	if err != nil {
		log.Printf("[Messages] history query failed: %v", err)
		return []ChatMessageResponse{}
	}
	defer rows.Close()

	out := []ChatMessageResponse{}
	for rows.Next() {
		var messageID, sender, ciphertext, nonce, signature string
		var created time.Time
		if err := rows.Scan(&messageID, &sender, &ciphertext, &nonce, &signature, &created); err != nil {
			continue
		}
		out = append(out, ChatMessageResponse{
			Content:          s.verifyAndDecrypt(ctx, roomID, sender, ciphertext, nonce, signature),
			MessageID:        messageID,
			CreatedTimestamp: created,
			Sender:           sender,
			RoomID:           roomID,
			Type:             "CHAT_MESSAGE",
		})
	}
	return out
}

// verifyAndDecrypt mirrors Java: signature over ciphertext first, then AES-GCM decrypt.
func (s *Service) verifyAndDecrypt(ctx context.Context, roomID, sender, ciphertext, nonce, signature string) string {
	var publicKey sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT public_key FROM users WHERE username = $1`, sender).Scan(&publicKey)
	if err != nil || !publicKey.Valid || publicKey.String == "" {
		return "[SIGNATURE VERIFICATION FAILED - unknown sender]"
	}

	pub, err := crypto.DecodePublicKeyJava(publicKey.String)
	if err != nil {
		return "[SIGNATURE VERIFICATION FAILED - unknown sender]"
	}
	if !crypto.Verify(pub, crypto.SignaturePayload(roomID, sender, ciphertext, nonce), signature) {
		return "[SIGNATURE VERIFICATION FAILED - forged or modified]"
	}

	content, err := crypto.Decrypt(s.aesKey, ciphertext, nonce)
	if err != nil {
		return "[TAMPERED - AES-GCM authentication failed]"
	}
	return content
}

// Receipt represents one row of message_receipt.
type Receipt struct {
	MessageID        string     `json:"messageId"`
	Username         string     `json:"username"`
	MessageDelivered *time.Time `json:"messageDelivered"`
	MessageRead      *time.Time `json:"messageRead"`
}

// GetReceipts returns all receipts for a message, but only if the requester
// is a member of the message's room (Java parity: getMessageReceipt).
func (s *Service) GetReceipts(ctx context.Context, messageID, username string) []Receipt {
	// room of the message
	var roomID string
	err := s.db.QueryRowContext(ctx, `SELECT room_id FROM messages WHERE message_id = $1`, messageID).Scan(&roomID)
	if err != nil {
		return nil
	}
	// membership
	var member bool
	err = s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM rooms_users_roles WHERE username = $1 AND room_id = $2)`,
		username, roomID).Scan(&member)
	if err != nil || !member {
		return nil
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT message_id, username, message_delivered, message_read
		 FROM message_receipt WHERE message_id = $1`, messageID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := []Receipt{}
	for rows.Next() {
		var r Receipt
		var mid string
		if err := rows.Scan(&mid, &r.Username, &r.MessageDelivered, &r.MessageRead); err != nil {
			continue
		}
		// Java serializes the composite id as {messageId: {username, messageId}} plus timestamps;
		// frontend reads r.messageId.username and r.messageDelivered / r.messageRead.
		r.MessageID = messageID
		out = append(out, r)
	}
	return out
}

// UpdateDelivered marks a message delivered for username (with Java's validation).
func (s *Service) UpdateDelivered(ctx context.Context, messageID, roomID, username string) {
	s.updateReceipt(ctx, messageID, roomID, username, true)
}

// UpdateRead marks a message read for username (with Java's validation).
func (s *Service) UpdateRead(ctx context.Context, messageID, roomID, username string) {
	s.updateReceipt(ctx, messageID, roomID, username, false)
}

func (s *Service) updateReceipt(ctx context.Context, messageID, roomID, username string, delivered bool) {
	// Java checks: message exists, requester is member of that room, and message belongs to the room
	var msgRoom string
	err := s.db.QueryRowContext(ctx, `SELECT room_id FROM messages WHERE message_id = $1`, messageID).Scan(&msgRoom)
	if err != nil || msgRoom != roomID {
		return
	}
	var member bool
	err = s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM rooms_users_roles WHERE username = $1 AND room_id = $2)`,
		username, roomID).Scan(&member)
	if err != nil || !member {
		return
	}

	if delivered {
		s.db.ExecContext(ctx,
			`UPDATE message_receipt SET message_delivered = $1 WHERE message_id = $2 AND username = $3`,
			time.Now(), messageID, username)
	} else {
		s.db.ExecContext(ctx,
			`UPDATE message_receipt SET message_read = $1 WHERE message_id = $2 AND username = $3`,
			time.Now(), messageID, username)
	}
}

// StatusUpdate matches Java MailDeliveryResponse JSON shape.
type StatusUpdate struct {
	MessageDelivered *time.Time `json:"messageDelivered"`
	MessageRead      *time.Time `json:"messageRead"`
	Username         string     `json:"username"`
	MessageID        string     `json:"messageId"`
	Type             string     `json:"type"`
}

// GetStatusUpdate rebuilds a status event from the DB (Java parity in broadcastStatusLocally).
func (s *Service) GetStatusUpdate(ctx context.Context, messageID, roomID, username string) *StatusUpdate {
	var msgRoom string
	err := s.db.QueryRowContext(ctx, `SELECT room_id FROM messages WHERE message_id = $1`, messageID).Scan(&msgRoom)
	if err != nil || msgRoom != roomID {
		return nil
	}
	var member bool
	err = s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM rooms_users_roles WHERE username = $1 AND room_id = $2)`,
		username, roomID).Scan(&member)
	if err != nil || !member {
		return nil
	}

	var delivered, read sql.NullTime
	err = s.db.QueryRowContext(ctx,
		`SELECT message_delivered, message_read FROM message_receipt WHERE message_id = $1 AND username = $2`,
		messageID, username).Scan(&delivered, &read)
	if err != nil {
		return nil
	}

	var d, r *time.Time
	if delivered.Valid {
		t := delivered.Time
		d = &t
	}
	if read.Valid {
		t := read.Time
		r = &t
	}
	return &StatusUpdate{
		MessageDelivered: d,
		MessageRead:      r,
		Username:         username,
		MessageID:        messageID,
		Type:             "MESSAGE_STATUS_UPDATE",
	}
}

// IsUserMember checks room membership (used by WS handlers and history endpoint).
func (s *Service) IsUserMember(ctx context.Context, username, roomID string) bool {
	var member bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM rooms_users_roles WHERE username = $1 AND room_id = $2)`,
		username, roomID).Scan(&member)
	return err == nil && member
}

// marshal is a helper used by hub for envelope serialization.
func marshal(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

var _ = io.Discard // keep io import if unused later
var _ = marshal

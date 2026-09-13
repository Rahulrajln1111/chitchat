package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Rahulrajln1111/chitchat/internal/crypto"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// Claims represents JWT claims
type Claims struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

// AuthHandler handles authentication requests
type AuthHandler struct {
	db        *sql.DB
	jwtSecret string
	kekBytes  []byte
}

// NewAuthHandler creates the auth handler. kekB64 is the base64 key-encryption-key.
func NewAuthHandler(database *sql.DB, jwtSecret, kekB64 string) *AuthHandler {
	kek, err := base64.StdEncoding.DecodeString(kekB64)
	if err != nil || len(kek) != 32 {
		log.Fatalf("Invalid KEK for auth handler")
	}
	return &AuthHandler{
		db:       database,
		jwtSecret: jwtSecret,
		kekBytes: kek,
	}
}

// LoginRequest represents login request body
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginResponse represents login response
type LoginResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// RefreshRequest represents token refresh request
type RefreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

// CreateUserRequest represents user creation request
type CreateUserRequest struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	Tagline       string `json:"tagline,omitempty"`
	ProfilePicture string `json:"profilePicture,omitempty"`
}

// CreateUserResponse represents user creation response
type CreateUserResponse struct {
	Username string `json:"username"`
	Message  string `json:"message"`
}

// Login handles POST /auth/login
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if req.Username == "" || req.Password == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "username and password required"})
		return
	}

	// Find user
	var storedPassword string
	err := h.db.QueryRow("SELECT password FROM users WHERE username = $1", req.Username).Scan(&storedPassword)
	if err == sql.ErrNoRows {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid credentials"})
		return
	} else if err != nil {
		log.Printf("DB error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// Verify password
	if err := bcrypt.CompareHashAndPassword([]byte(storedPassword), []byte(req.Password)); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid credentials"})
		return
	}

	// Generate tokens
	accessToken := generateToken(h.jwtSecret, req.Username, "access", 8*time.Hour) // Java parity: 8h access
	refreshToken := generateToken(h.jwtSecret, req.Username, "refresh", 7*24*time.Hour)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(LoginResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	})
}

// Refresh handles POST /auth/refresh
// The frontend sends the raw refresh token as the request body (Java parity),
// so accept both raw string and JSON-wrapped forms.
func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	refreshToken := strings.TrimSpace(string(body))
	// Try JSON-wrapped too (backward compat)
	if strings.HasPrefix(refreshToken, "{") {
		var req RefreshRequest
		if json.Unmarshal(body, &req) == nil && req.RefreshToken != "" {
			refreshToken = req.RefreshToken
		}
	}
	// Strip stray quotes
	refreshToken = strings.Trim(refreshToken, `"`)

	if refreshToken == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshToken required"})
		return
	}

	// Validate refresh token
	claims, err := ValidateToken(h.jwtSecret, refreshToken)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid refresh token"})
		return
	}

	// Check user exists
	var exists bool
	err = h.db.QueryRow("SELECT EXISTS(SELECT 1 FROM users WHERE username = $1)", claims.Username).Scan(&exists)
	if !exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "user not found"})
		return
	}

	// Generate new access token
	accessToken := generateToken(h.jwtSecret, claims.Username, "access", 8*time.Hour) // Java parity: 8h access

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(LoginResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken, // Keep same refresh token
	})
}

// CreateUser handles POST /user/create
func (h *AuthHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if req.Username == "" || req.Password == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "username and password required"})
		return
	}

	// Check if user exists
	var exists bool
	err := h.db.QueryRow("SELECT EXISTS(SELECT 1 FROM users WHERE username = $1)", req.Username).Scan(&exists)
	if err != nil {
		log.Printf("DB error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "username already taken"})
		return
	}

	// Hash password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("Password hash error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// Generate Ed25519 keypair (Java-compatible SPKI/PKCS8 encoding)
	publicKey, privateKey, err := crypto.GenerateSigningKeyPair()
	if err != nil {
		log.Printf("Key generation error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	privKeyB64, err := crypto.EncodePrivateKeyJava(privateKey)
	if err != nil {
		log.Printf("Private key encode error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	pubKeyB64, err := crypto.EncodePublicKeyJava(publicKey)
	if err != nil {
		log.Printf("Public key encode error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	wrappedPrivateKey, err := crypto.WrapPrivateKey(h.kekBytes, privKeyB64)
	if err != nil {
		log.Printf("Key wrap error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// Insert user
	_, err = h.db.Exec(
		`INSERT INTO users (username, password, tagline, profile_picture, public_key, wrapped_private_key, timestamp) 
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		req.Username, string(hashedPassword), req.Tagline, req.ProfilePicture,
		pubKeyB64, wrappedPrivateKey, time.Now(),
	)
	if err != nil {
		log.Printf("Insert error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to create user"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(CreateUserResponse{
		Username: req.Username,
		Message:  "User created successfully",
	})
}

func generateToken(secret, username, typ string, duration time.Duration) string {
	claims := Claims{
		Username: username,
		Role:     "USER",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(duration)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
			Issuer:    "chitchat",
			Subject:   username,
			ID:        generateUUID(),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		log.Printf("Token generation error: %v", err)
		return ""
	}
	return signed
}

func ValidateToken(secret, tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, jwt.ErrSignatureInvalid
	}

	return claims, nil
}

func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return strings.ReplaceAll(fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), " ", "")
}

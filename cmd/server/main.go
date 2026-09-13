package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Rahulrajln1111/chitchat/internal/auth"
	"github.com/Rahulrajln1111/chitchat/internal/db"
	"github.com/Rahulrajln1111/chitchat/internal/handlers"
	"github.com/Rahulrajln1111/chitchat/internal/rooms"
)

// getEnv gets environment variable or returns default value
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// maskPassword masks the password in a connection string for logging
func maskPassword(url string) string {
	// Simple mask for postgres://user:password@host/database
	if idx := len(url); idx > 0 {
		return fmt.Sprintf("%s***@%s", url[:idx], url[idx:])
	}
	return url
}

func main() {
	// Get database connection string from .env file or environment
	dbURL := getEnv("DATABASE_URL", "")
	if dbURL == "" {
		// Fallback to hardcoded default (matches application.properties)
		dbURL = "postgres://chitchat:17e64df8659a2324ccd8d024101d89a8@127.0.0.1:5432/chitchat?sslmode=disable"
		log.Println("WARNING: DATABASE_URL not set, using default")
	} else {
		log.Printf("Using database: %s", maskPassword(dbURL))
	}

	// Initialize database connection
	if err := db.Init(dbURL); err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	log.Println("Connected to PostgreSQL")

	// Create HTTP handlers
	messageHandler := handlers.NewHandler()
	authHandler := auth.NewAuthHandler(db.GetDB(), os.Getenv("JWT_SECRET"))
	roomHandler := rooms.NewRoomHandler(db.GetDB())

	// Set up routers
	mux := http.NewServeMux()

	// Static files (frontend) - serve from /home/student/chitchat-frontend/dist
	frontendDir := os.Getenv("FRONTEND_DIR")
	if frontendDir == "" {
		frontendDir = "/home/student/chitchat-frontend/dist"
	}
	
	// API routes (must be registered before static to take priority)
	// Load test routes
	mux.HandleFunc("/message", messageHandler.PostMessage)
	mux.HandleFunc("/feed", messageHandler.GetFeed)

	// Auth routes
	mux.HandleFunc("/auth/login", authHandler.Login)
	mux.HandleFunc("/auth/refresh", authHandler.Refresh)
	mux.HandleFunc("/user/create", authHandler.CreateUser)

	// Room routes
	mux.HandleFunc("/room/create", roomHandler.CreateRoom)
	mux.HandleFunc("/room/all", roomHandler.GetAllRooms)
	mux.HandleFunc("/room/join/", roomHandler.JoinRoom)
	mux.HandleFunc("/room/leave/", roomHandler.LeaveRoom)

	// Serve static files for all other routes
	// Use a custom handler that never redirects
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// For root path, serve index.html directly with 200
		if r.URL.Path == "/" {
			// Serve index.html with correct content type
			http.ServeFile(w, r, filepath.Join(frontendDir, "index.html"))
			return
		}
		// For other paths, use the file server
		fileServer := http.FileServer(http.Dir(frontendDir))
		fileServer.ServeHTTP(w, r)
	}))

	// Configure server
	server := &http.Server{
		Addr:         ":3000",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine
	go func() {
		log.Printf("Starting server on port 3000")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	// Start auto-purge goroutine (every 5 minutes)
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			oneHourAgo := time.Now().Add(-1 * time.Hour)
			deleted, err := db.PurgeOldMessages(ctx, oneHourAgo)
			cancel()
			if err != nil {
				log.Printf("Error purging old messages: %v", err)
			} else {
				log.Printf("Purged %d old messages", deleted)
			}
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}

	log.Println("Server stopped")
}

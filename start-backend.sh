#!/bin/bash
# Production launcher for Go backend
# Builds and runs the Go server
# Run from the repo root. Extra args are forwarded.

set -e

cd "$(dirname "$0")"

# Build the Go binary
echo "Building Go server..."
go build -o chitchat-server ./cmd/server

# Run the server
echo "Starting Go server on port 3000..."
exec ./chitchat-server "$@"

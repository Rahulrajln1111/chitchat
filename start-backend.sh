#!/bin/bash
# Production launcher for Go backend
# Sources .env for environment variables, then runs the Go server
# Run from the repo root. Extra args are forwarded.

set -e

cd "$(dirname "$0")"

# Source .env file if it exists (contains DATABASE_URL and other config)
if [ -f .env ]; then
    echo "Loading configuration from .env..."
    set -a
    source .env
    set +a
fi

# Use precompiled binary (build separately with 'go build'):
# go build -o chitchat-server ./cmd/server

# Run the server
echo "Starting Go server on port ${SERVER_PORT:-3000}..."
exec ./chitchat-server "$@"

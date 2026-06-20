#!/bin/bash
set -e

# Development script that starts both frontend and backend with hot reloading
# Press Ctrl+C to stop both servers

echo "Starting DBBat development environment..."

# Kill any process already listening on port 4200
if lsof -i :4200 -t >/dev/null 2>&1; then
    echo "Killing existing process on port 4200..."
    lsof -i :4200 -t | xargs kill 2>/dev/null || true
    sleep 1
fi

# Start PostgreSQL if not running
echo "Ensuring PostgreSQL is running..."
if docker ps --format '{{.Names}}' | grep -q "dbbat-postgres"; then
    echo "PostgreSQL already running, skipping..."
else
    docker compose up -d postgres
fi
sleep 2

# Cleanup function to kill background processes
cleanup() {
    echo ""
    echo "Shutting down..."
    # Kill the frontend dev server if running
    if [ -n "$FRONT_PID" ]; then
        kill $FRONT_PID 2>/dev/null || true
    fi
    # Kill devloop if running
    if [ -n "$DEVLOOP_PID" ]; then
        kill $DEVLOOP_PID 2>/dev/null || true
    fi
    exit 0
}

# Set up trap for cleanup on Ctrl+C
trap cleanup SIGINT SIGTERM

# Start frontend dev server in background
echo "Starting frontend dev server..."
cd "$(dirname "$0")/../front"
bun run dev &
FRONT_PID=$!
cd - > /dev/null

# Wait for frontend to be ready
sleep 3

echo ""
echo "========================================"
echo "  DBBat Development Environment"
echo "========================================"
echo ""
echo "  Frontend (Vite):  http://localhost:5173/app/"
echo "  Backend (API):    http://localhost:4200/api/"
echo "  App (proxied):    http://localhost:4200/app/"
echo ""
echo "  Press Ctrl+C to stop all servers"
echo "========================================"
echo ""

# Start backend with devloop (build-first hot reload)
# shellcheck source=scripts/run-backend-dev.sh
. "$(dirname "$0")/run-backend-dev.sh"
go run ./cmd/devloop &
DEVLOOP_PID=$!

# Wait for either process to exit
wait $DEVLOOP_PID $FRONT_PID

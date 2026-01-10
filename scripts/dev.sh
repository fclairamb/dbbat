#!/bin/bash
set -e

# Development script that starts both frontend and backend with hot reloading
# Press Ctrl+C to stop both servers

echo "Starting DBBat development environment..."

# Start PostgreSQL if not running
echo "Ensuring PostgreSQL is running..."
docker compose up -d postgres
sleep 2

# Cleanup function to kill background processes
cleanup() {
    echo ""
    echo "Shutting down..."
    # Kill the frontend dev server if running
    if [ -n "$FRONT_PID" ]; then
        kill $FRONT_PID 2>/dev/null || true
    fi
    # Kill air if running
    if [ -n "$AIR_PID" ]; then
        kill $AIR_PID 2>/dev/null || true
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
echo "  Backend (API):    http://localhost:8080/api/"
echo "  App (proxied):    http://localhost:8080/app/"
echo ""
echo "  Press Ctrl+C to stop all servers"
echo "========================================"
echo ""

# Start backend with Air (foreground)
air &
AIR_PID=$!

# Wait for either process to exit
wait $AIR_PID $FRONT_PID

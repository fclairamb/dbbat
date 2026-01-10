#!/bin/bash

# Check if backend is running and accessible
echo "Checking if backend API is accessible..."

response=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/api/health 2>/dev/null || echo "000")

if [ "$response" = "000" ]; then
    echo "❌ ERROR: Cannot connect to backend API at http://localhost:8080"
    echo ""
    echo "Please start the backend server first:"
    echo "  RUN_MODE=test ./dbbat serve"
    echo ""
    exit 1
fi

if [ "$response" = "404" ]; then
    echo "⚠️  WARNING: Backend is running but /api/health endpoint returned 404"
    echo "   This might be okay if the health endpoint doesn't exist."
fi

if [ "$response" = "200" ]; then
    echo "✅ Backend API is accessible"
fi

# Check if dev server is running
dev_response=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:5173/app/ 2>/dev/null || echo "000")

if [ "$dev_response" = "000" ]; then
    echo "❌ ERROR: Cannot connect to dev server at http://localhost:5173"
    echo ""
    echo "Please start the dev server first:"
    echo "  bun run dev"
    echo ""
    exit 1
fi

if [ "$dev_response" = "200" ]; then
    echo "✅ Dev server is accessible"
fi

echo ""
echo "All prerequisites met! You can now run the tests."

#!/bin/bash
set -e

echo "Building DBBat frontend..."

cd "$(dirname "$0")/../front"

# Install dependencies
echo "Installing dependencies..."
bun install

# Generate API types
echo "Generating API types..."
bun run generate-client

# Build the frontend
echo "Building frontend..."
bun run build:no-check

# Copy to backend resources
echo "Copying to backend resources..."
rm -rf ../internal/api/resources
mkdir -p ../internal/api/resources
cp -r dist/* ../internal/api/resources/

echo "Frontend build complete!"
echo "Files copied to internal/api/resources/"

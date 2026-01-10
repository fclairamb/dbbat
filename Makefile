.PHONY: dev dev-front dev-back dev-stop build build-front build-all test test-e2e lint clean

# Development environment with hot reloading (frontend + backend)
dev:
	@./scripts/dev.sh

# Start only frontend dev server
dev-front:
	@echo "Starting frontend dev server..."
	@cd front && bun run dev

# Start only backend with Air (requires frontend to be running separately for HMR)
dev-back:
	@echo "Starting backend with Air hot reloading..."
	@docker compose up -d postgres
	@sleep 2
	@air

# Stop development environment
dev-stop:
	@echo "Stopping development environment..."
	@-pkill -f "bun run dev" 2>/dev/null || true
	@-pkill -f "air" 2>/dev/null || true
	@docker compose down

# Build variables for ldflags
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X 'github.com/fclairamb/dbbat/internal/version.Version=$(VERSION)' \
           -X 'github.com/fclairamb/dbbat/internal/version.Commit=$(COMMIT)' \
           -X 'github.com/fclairamb/dbbat/internal/version.BuildTime=$(BUILD_TIME)'

# Build the binary
build:
	go build -ldflags "$(LDFLAGS)" -o ./bin/dbbat ./cmd/dbbat

# Build frontend
build-front:
	@./scripts/build-frontend.sh

# Build everything
build-all: build-front build

# Run Go unit tests
test:
	go test ./...

# Run E2E tests (builds production server, starts it in test mode, runs Playwright tests)
test-e2e:
	@echo "Running E2E tests..."
	@cd front && bun run test:e2e

# Run linter
lint:
	golangci-lint run

# Clean build artifacts
clean:
	rm -rf ./bin ./tmp

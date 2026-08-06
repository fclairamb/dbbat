
# Development environment with hot reloading (frontend + backend)
dev:
	@./scripts/dev.sh

# Build everything
build-app: build-front build-binary

build-image:
	docker build -t dbbat .

demo: build-app
	@echo "Starting demo environment..."
	docker compose up -d postgres
	DBB_RUN_MODE="demo" DBB_DSN="postgres://postgres:postgres@localhost:5001/dbbat?sslmode=disable" ./dbbat

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
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GIT_TIME ?= $(shell TZ=UTC git log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo "unknown")
LDFLAGS := -X 'github.com/fclairamb/dbbat/internal/version.Version=$(VERSION)' \
           -X 'github.com/fclairamb/dbbat/internal/version.Commit=$(COMMIT)' \
           -X 'github.com/fclairamb/dbbat/internal/version.GitTime=$(GIT_TIME)'

# Build the binary
build-binary:
	go build -ldflags "$(LDFLAGS)" -o ./dbbat .

# Build frontend
build-front:
	@./scripts/build-frontend.sh

# Run Go unit tests
test:
	go test ./...

# Run E2E tests (builds production server, starts it in test mode, runs Playwright tests)
test-e2e:
	@echo "Running E2E tests..."
	@cd front && bun run test:e2e

# Regenerate the website's showcase media (screenshots + approval video).
#
# On demand only — never wired into a release. Brings up its own throwaway
# PostgreSQL container and its own demo-mode dbbat on non-default ports, so it
# cannot disturb a running `make dev` stack. See scripts/showcase.sh.
showcase:
	@./scripts/showcase.sh

# Run Oracle integration tests (requires Docker)
test-e2e-oracle:
	go test -tags integration -v -timeout 15m ./internal/proxy/oracle/...

# Protocol integration suites (require Docker).
#
# Every test starts its own upstream container *and* its own PostgreSQL storage
# container, so a suite is dominated by container startup rather than by the
# assertions. On an idle laptop the MongoDB suite runs in ~4min; with other
# containers competing for the Docker daemon it has been measured at 7min and
# once past 12min. `go test`'s default timeout is 10 minutes, so the plain
# `go test -tags integration ./internal/proxy/mongodb/...` panics on a busy
# machine even when nothing is wrong. These targets carry the same -timeout 40m
# the CI workflow uses. A run that exceeds *that* is a real regression.
test-integration-mongodb:
	go test -tags integration -v -timeout 40m -count=1 ./internal/proxy/mongodb/...

test-integration-mysql:
	go test -tags integration -v -timeout 40m -count=1 ./internal/proxy/mysql/...

test-integration-postgresql:
	go test -tags integration -v -timeout 40m -count=1 ./internal/proxy/postgresql/...

# mcr.microsoft.com/mssql/server is published for linux/amd64 only and takes a
# couple of minutes to finish recovery, so on an arm64 laptop this suite runs
# under emulation if it runs at all. CI (ubuntu-24.04) is where it is expected
# to pass.
test-integration-mssql:
	go test -tags integration -v -timeout 40m -count=1 ./internal/proxy/mssql/...

# Run linter
lint:
	golangci-lint run

# Clean build artifacts
clean:
	rm -rf ./bin ./tmp

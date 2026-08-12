
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
#
# `-race` matches what CI runs (.github/workflows/ci.yml). The proxies are
# concurrency-heavy — accept loop, per-connection goroutines, shutdown
# WaitGroup — and races there are probabilistic, so a local gate without the
# detector lets them through to CI (or to production).
test:
	go test -race ./...

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

# Run Oracle integration tests (requires Docker).
#
# Twelve tests, each starting its own Oracle container: 15m was never enough and
# turned a slow machine into a red suite. CI budgets 60m for this job; 40m
# matches the other integration targets below and still fails fast on a real
# regression.
#
# The default image is gvenzl/oracle-free:23-slim (Oracle 23ai Free) on every
# host and in every environment: it has an arm64 build, and 23ai is the version
# the proxy work is validated against. The former default,
# gvenzl/oracle-xe:18.4.0-slim, is published for linux/amd64 only and does not
# boot under emulation on Apple Silicon — it dies in instance startup
# (ORA-27300 / ORA-00442), so the suite gave an arm64 developer no signal.
#
# Override the image with ORACLE_TEST_IMAGE, e.g. to reproduce the pinned 18c
# run on an amd64 machine:
#
#   ORACLE_TEST_IMAGE=gvenzl/oracle-xe:18.4.0-slim make test-e2e-oracle
#
# CI (.github/workflows/integration.yml) runs the suite twice: once on this
# default, and once pinned to the 18c XE image so that coverage is kept.
#
# `-race`, and it costs nothing measurable. This is the only suite that puts
# two proxy goroutines on the same session at once — client reader, upstream
# reader, limit watchdog, approval gate — so it is the only place those pairs
# get exercised at all; the unit tests carry the detector but drive one
# goroutine. Running without it hid two live races on the in-flight query
# (see `trackerMu` in internal/proxy/oracle/session.go).
#
# Measured on this target, same machine, back to back:
#
#   without -race   6m38s wall   (392s test time,  8.7s user CPU)
#   with    -race   6m16s wall   (371s test time, 14.6s user CPU)
#
# The detector really is a ~2x CPU tax — but on ~6 seconds of CPU against a
# suite that spends six minutes booting Oracle containers, so the wall-clock
# difference is inside the run-to-run noise. -timeout 40m is unchanged.
test-e2e-oracle:
	go test -race -tags integration -v -timeout 40m -count=1 ./internal/proxy/oracle/...

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

# The Kubernetes tunnel end to end: one k3s cluster (privileged container), a
# PostgreSQL pod inside it, and dbbat dialing through `pods/portforward`. Unlike
# the protocol suites this one boots a whole control plane, so the first run on
# a cold image cache is dominated by pulling k3s. Override the cluster with
# K3S_TEST_IMAGE=rancher/k3s:vX.Y.Z-k3s1 — it must be 1.31 or newer for the
# websocket transport assertion to hold.
test-integration-kubernetes:
	go test -tags integration -v -timeout 40m -count=1 ./internal/proxy/kubernetes/...

# Run linter
lint:
	golangci-lint run

# Clean build artifacts
clean:
	rm -rf ./bin ./tmp

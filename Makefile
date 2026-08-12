
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
# `-race`, and it costs nothing measurable. The integration suites are the only
# place two proxy goroutines meet on one session — client reader, upstream
# reader, limit watchdog, approval gate — and this is the suite with the most
# of that state and the most client shapes; the unit tests carry the detector
# but drive a single goroutine. Running without it hid two live races on the
# in-flight query (see `trackerMu` in internal/proxy/oracle/session.go).
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
#
# `-race` on the three below, for the reason spelled out on test-e2e-oracle:
# these are the only suites that put two proxy goroutines on one session, and
# the detector is what turns that into signal. Measured on the Oracle target at
# 6m38s without / 6m16s with — the CPU tax is real but invisible next to
# container startup, and the same holds here (PostgreSQL 2m37s with it on).
# Each of the three has been run green under the detector; PostgreSQL needed a
# fix first (see `bookMu` in internal/proxy/postgresql/session.go).
test-integration-mongodb:
	go test -race -tags integration -v -timeout 40m -count=1 ./internal/proxy/mongodb/...

test-integration-mysql:
	go test -race -tags integration -v -timeout 40m -count=1 ./internal/proxy/mysql/...

test-integration-postgresql:
	go test -race -tags integration -v -timeout 40m -count=1 ./internal/proxy/postgresql/...

# mcr.microsoft.com/mssql/server is published for linux/amd64 only, so on an
# arm64 laptop this suite runs under emulation. CI (ubuntu-24.04) is where it is
# expected to pass, but emulation is not a reason to skip it locally: measured
# under Rosetta on an M-series host, the whole suite (175 cases) took 3m39s wall
# with `-race` on, SQL Server itself reaching "ready for client connections" in
# ~6s per container.
#
# `-race`, same reason as the targets above. The TDS proxy runs the two relay
# pumps over one session like the others do, and the detector reported nothing —
# it guards the in-flight statement behind `pendingMu`, `statsMu`, `heldMu` and
# `preparedMu` already, which is what the other proxies had to be taught.
test-integration-mssql:
	go test -race -tags integration -v -timeout 40m -count=1 ./internal/proxy/mssql/...

# The Kubernetes tunnel end to end: one k3s cluster (privileged container), a
# PostgreSQL pod inside it, and dbbat dialing through `pods/portforward`. Unlike
# the protocol suites this one boots a whole control plane, so the first run on
# a cold image cache is dominated by pulling k3s. Override the cluster with
# K3S_TEST_IMAGE=rancher/k3s:vX.Y.Z-k3s1 — it must be 1.31 or newer for the
# websocket transport assertion to hold.
#
# No `-race` here either, for the same reason as mssql: booting a k3s control
# plane is expensive enough that the suite has not been run under the detector,
# and this one exercises the tunnel rather than a proxy session's two relay
# goroutines, so it is the least likely of the six to be hiding one. Same todo.
test-integration-kubernetes:
	go test -tags integration -v -timeout 40m -count=1 ./internal/proxy/kubernetes/...

# Run linter
lint:
	golangci-lint run

# Clean build artifacts
clean:
	rm -rf ./bin ./tmp

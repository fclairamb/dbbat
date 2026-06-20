# devloop — replace Air with build-first hot reload

## Goal

Replace the Air dependency with a custom `cmd/devloop` package that:
1. Watches `.go` files for changes
2. Builds the new binary first (`./tmp/dbbat`)
3. Only then stops the currently running process and starts the new one

This eliminates the multi-second downtime window Air introduces (it kills before building).

Reference implementation: `../solidping/server/cmd/devloop/main.go`

## Why

Air kills the child before building. On a project of this size, builds take 3–8 seconds. With devloop, the server stays up during the build and the dead window is only the graceful-shutdown time (~500 ms).

## Implementation

### New file: `cmd/devloop/main.go`

Adapt `solidping/server/cmd/devloop/main.go` with dbbat-specific constants:

| Constant | solidping value | dbbat value |
|----------|----------------|-------------|
| `binPath` | `./tmp/solidping` | `./tmp/dbbat` |
| `nextBinPath` | `./tmp/solidping.next` | `./tmp/dbbat.next` |
| `binArg` | `serve` | `serve` |
| Excluded dirs | `tmp, vendor, .git, testdata, res, apps, openapi` | `tmp, vendor, .git, testdata, front, website` |

Watch `.go` files only (no `.sql` — migrations require a manual restart, and are infrequent).

### Files to modify

| File | Change |
|------|--------|
| `Makefile` | `dev-back`: replace `air` → `go run ./cmd/devloop`; `dev-stop`: replace `pkill -f "air"` → `pkill -f "dbbat serve"` |
| `scripts/dev.sh` | Replace `air &` + `$AIR_PID` → `go run ./cmd/devloop &` + `$DEVLOOP_PID` |

### Files to delete

| File | Reason |
|------|--------|
| `.air.toml` | No longer needed |

Note: `scripts/run-backend-dev.sh` is referenced in `.air.toml` as `full_bin` but does not exist on disk — nothing to delete there.

### Dependency

`fsnotify` is already present in `go.mod` as an indirect dep (v1.9.0). It needs to be promoted to a direct dependency by importing it in `cmd/devloop/main.go` — `go mod tidy` will update `go.mod` accordingly.

## Verification

1. `make dev-back` — starts devloop; edit any `.go` file; confirm server stays up during build and reloads after
2. `make dev` — full stack (frontend + devloop); Ctrl+C cleanly shuts down both
3. `make dev-stop` — kills devloop and frontend processes cleanly
4. Build failure: introduce a syntax error in a `.go` file; confirm server keeps running the old binary and devloop logs the build error without crashing

**Status**: Todo | **Created**: 2026-05-16

## Implementation Plan

1. **`cmd/devloop/main.go`** — port `solidping/server/cmd/devloop/main.go` verbatim except:
   - `binPath = "./tmp/dbbat"`, `nextBinPath = "./tmp/dbbat.next"`, `binArg = "serve"`.
   - Excluded dirs: `tmp, vendor, .git, testdata, front, website`.
   - Build the package at module root (`.`) and watch `.go` files only (`_test.go` skipped), exactly as the reference does.
   - Drop ldflags `-s -w`? Keep them: faster relinks, no debug-info downside for dev.
   - Adjust the "watching server/ for .go changes" log line to dbbat (watch repo root).

2. **`scripts/run-backend-dev.sh`** — repurpose from an `exec ./tmp/dbbat serve` wrapper into an env-only file that is *sourced* (exports `DBB_DSN`, `DBB_KEY`, `DBB_RUN_MODE`, `DBB_REDIRECTS`). devloop launches the child directly with the inherited environment, so the dev env vars must be present in devloop's environment. Both `dev-back` and `dev.sh` source it before running devloop. (The spec assumed this file didn't exist; it does and carries the dev env, so it is preserved/repurposed rather than deleted.)

3. **`Makefile`** — `dev-back`: source `run-backend-dev.sh` then `go run ./cmd/devloop` instead of `air`; `dev-stop`: replace `pkill -f "air"` → `pkill -f "dbbat serve"`.

4. **`scripts/dev.sh`** — source `run-backend-dev.sh`, replace `air &` + `$AIR_PID` with `go run ./cmd/devloop &` + `$DEVLOOP_PID`.

5. **`.air.toml`** — delete.

6. **`go.mod`** — `go mod tidy` promotes `github.com/fsnotify/fsnotify` from indirect to direct once it is imported by `cmd/devloop/main.go`.

7. **QA** — `make build-app lint test`; `go vet ./cmd/devloop`; `go build -o /tmp/devloop-check ./cmd/devloop`.

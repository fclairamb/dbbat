#!/usr/bin/env bash
#
# Regenerate the website's showcase media (screenshots + the approval video).
#
# Strictly on demand: `make showcase`, or the workflow_dispatch-only
# .github/workflows/showcase.yml. Nothing here runs on a release.
#
# ISOLATION — read this before changing anything below.
#
# Demo mode DROPS EVERY TABLE on startup (see storeOpts.DropTablesFirst in
# main.go). This script therefore brings up its OWN throwaway PostgreSQL
# container, on its OWN port, with no volume, and starts its OWN dbbat process
# on ports that collide with neither the documented defaults (4200/5433/5001)
# nor the e2e suite's (8080/5433/5001).
#
# It must never call `docker compose` — a developer's shared stack, and the
# database behind it, is not ours to stop. Cleanup only ever touches the
# container this script created, by name.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

# --- knobs (mirrored in front/showcase/config.ts) ---------------------------
SHOWCASE_API_PORT="${SHOWCASE_API_PORT:-8099}"
SHOWCASE_PROXY_PORT="${SHOWCASE_PROXY_PORT:-5499}"
SHOWCASE_PG_PORT="${SHOWCASE_PG_PORT:-5099}"
SHOWCASE_OUT="${SHOWCASE_OUT:-${REPO_ROOT}/website/static/img/showcase}"
SHOWCASE_WORK="${SHOWCASE_WORK:-${REPO_ROOT}/front/showcase/.artifacts}"
SHOWCASE_PG_IMAGE="${SHOWCASE_PG_IMAGE:-postgres:15}"
SHOWCASE_CONTAINER="${SHOWCASE_CONTAINER:-dbbat-showcase-postgres}"
SHOWCASE_PROJECT="${SHOWCASE_PROJECT:-}"        # "screenshots" | "video" | ""
SHOWCASE_SKIP_BUILD="${SHOWCASE_SKIP_BUILD:-0}"
SHOWCASE_SKIP_TRANSCODE="${SHOWCASE_SKIP_TRANSCODE:-0}"
SHOWCASE_KEEP="${SHOWCASE_KEEP:-0}"             # leave the stack up afterwards
SHOWCASE_BINARY="${SHOWCASE_BINARY:-${REPO_ROOT}/dbbat}"

# A throwaway key: this instance holds nothing but demo data.
SHOWCASE_KEY="${SHOWCASE_KEY:-MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=}"

# Pin the browser clock for the whole run so "3 minutes ago" labels do not
# churn between regenerations.
SHOWCASE_FIXED_TIME="${SHOWCASE_FIXED_TIME:-$(date -u +%Y-%m-%dT%H:%M:00Z)}"

export SHOWCASE_API_PORT SHOWCASE_PROXY_PORT SHOWCASE_PG_PORT \
       SHOWCASE_OUT SHOWCASE_WORK SHOWCASE_FIXED_TIME

VIDEO_DIR="${SHOWCASE_WORK}/video"
DBBAT_PID=""
STARTED_CONTAINER=0

log() { printf '\033[1;36m[showcase]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[showcase]\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31m[showcase]\033[0m %s\n' "$*" >&2; exit 1; }

cleanup() {
  local status=$?
  if [ "${SHOWCASE_KEEP}" = "1" ]; then
    warn "SHOWCASE_KEEP=1 — leaving dbbat (pid ${DBBAT_PID:-none}) and container ${SHOWCASE_CONTAINER} running"
    return
  fi
  if [ -n "${DBBAT_PID}" ] && kill -0 "${DBBAT_PID}" 2>/dev/null; then
    log "stopping the showcase dbbat instance (pid ${DBBAT_PID})"
    kill "${DBBAT_PID}" 2>/dev/null || true
    wait "${DBBAT_PID}" 2>/dev/null || true
  fi
  if [ "${STARTED_CONTAINER}" = "1" ]; then
    log "removing the throwaway upstream container ${SHOWCASE_CONTAINER}"
    docker rm -f "${SHOWCASE_CONTAINER}" >/dev/null 2>&1 || true
  fi
  exit "${status}"
}
trap cleanup EXIT INT TERM

require() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not on PATH"; }

port_busy() {
  # `nc -z` is not everywhere; lsof is on macOS and the GitHub runners.
  if command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
  else
    ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
    return $((1 - $?))
  fi
}

# --- preflight --------------------------------------------------------------
require docker
require bun
require git

for port in "${SHOWCASE_API_PORT}" "${SHOWCASE_PROXY_PORT}" "${SHOWCASE_PG_PORT}"; do
  if port_busy "${port}"; then
    die "port ${port} is already in use — set SHOWCASE_API_PORT / SHOWCASE_PROXY_PORT / SHOWCASE_PG_PORT to free ones"
  fi
done

if [ "${SHOWCASE_SKIP_TRANSCODE}" != "1" ] && ! command -v ffmpeg >/dev/null 2>&1; then
  warn "ffmpeg not found — the video will be left as raw WebM (set SHOWCASE_SKIP_TRANSCODE=1 to silence this)"
  SHOWCASE_SKIP_TRANSCODE=1
fi

mkdir -p "${SHOWCASE_OUT}" "${SHOWCASE_WORK}" "${VIDEO_DIR}"
rm -f "${VIDEO_DIR}"/*.webm 2>/dev/null || true

# --- build ------------------------------------------------------------------
if [ "${SHOWCASE_SKIP_BUILD}" = "1" ]; then
  log "SHOWCASE_SKIP_BUILD=1 — using the existing ${SHOWCASE_BINARY}"
  [ -x "${SHOWCASE_BINARY}" ] || die "${SHOWCASE_BINARY} is missing; drop SHOWCASE_SKIP_BUILD"
else
  log "building the app (frontend + binary)"
  make build-app
fi

# --- throwaway upstream -----------------------------------------------------
if docker ps -a --format '{{.Names}}' | grep -qx "${SHOWCASE_CONTAINER}"; then
  log "reusing the existing container ${SHOWCASE_CONTAINER}"
  docker start "${SHOWCASE_CONTAINER}" >/dev/null
else
  log "starting the throwaway upstream (${SHOWCASE_PG_IMAGE}) on :${SHOWCASE_PG_PORT}"
  docker run -d --rm \
    --name "${SHOWCASE_CONTAINER}" \
    -e POSTGRES_USER=postgres \
    -e POSTGRES_PASSWORD=postgres \
    -p "${SHOWCASE_PG_PORT}:5432" \
    -v "${REPO_ROOT}/docker/postgres/init.sql:/docker-entrypoint-initdb.d/init.sql:ro" \
    "${SHOWCASE_PG_IMAGE}" >/dev/null
  STARTED_CONTAINER=1
fi

log "waiting for the upstream to accept connections"
for _ in $(seq 1 60); do
  if docker exec "${SHOWCASE_CONTAINER}" pg_isready -U postgres >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker exec "${SHOWCASE_CONTAINER}" pg_isready -U postgres >/dev/null 2>&1 \
  || die "the upstream container never became ready"
# pg_isready goes green before init.sql has finished; the demo database is the
# thing we actually need.
for _ in $(seq 1 60); do
  if docker exec "${SHOWCASE_CONTAINER}" psql -U postgres -lqt 2>/dev/null | cut -d'|' -f1 | grep -qw demo; then
    break
  fi
  sleep 1
done

# --- demo-mode dbbat --------------------------------------------------------
log "starting dbbat in demo mode (api :${SHOWCASE_API_PORT}, pg proxy :${SHOWCASE_PROXY_PORT})"
DBB_RUN_MODE=demo \
DBB_DSN="postgres://postgres:postgres@localhost:${SHOWCASE_PG_PORT}/dbbat?sslmode=disable" \
DBB_KEY="${SHOWCASE_KEY}" \
DBB_LISTEN_API=":${SHOWCASE_API_PORT}" \
DBB_LISTEN_PG=":${SHOWCASE_PROXY_PORT}" \
DBB_LISTEN_ORA="" \
DBB_LISTEN_MYSQL="" \
DBB_LISTEN_MONGO="" \
DBB_APPROVAL_ENABLED=true \
DBB_APPROVAL_SLACK_DELAY=0 \
DBB_RATE_LIMIT_ENABLED=false \
DBB_LOG_LEVEL="${SHOWCASE_LOG_LEVEL:-warn}" \
  "${SHOWCASE_BINARY}" serve >"${SHOWCASE_WORK}/dbbat.log" 2>&1 &
DBBAT_PID=$!

log "waiting for the API"
for _ in $(seq 1 60); do
  if curl -fsS "http://localhost:${SHOWCASE_API_PORT}/api/v1/health" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "${DBBAT_PID}" 2>/dev/null; then
    tail -30 "${SHOWCASE_WORK}/dbbat.log" >&2 || true
    die "dbbat exited during startup"
  fi
  sleep 1
done
curl -fsS "http://localhost:${SHOWCASE_API_PORT}/api/v1/health" >/dev/null \
  || { tail -30 "${SHOWCASE_WORK}/dbbat.log" >&2 || true; die "the API never became ready"; }

# --- capture ----------------------------------------------------------------
log "running the showcase Playwright project"
PLAYWRIGHT_ARGS=(test --config=showcase/playwright.config.ts)
if [ -n "${SHOWCASE_PROJECT}" ]; then
  PLAYWRIGHT_ARGS+=(--project="${SHOWCASE_PROJECT}")
fi
(cd front && bunx playwright "${PLAYWRIGHT_ARGS[@]}")

# --- transcode --------------------------------------------------------------
transcode() {
  local src="$1" base="$2"
  local fps="${SHOWCASE_FPS:-22}"

  if ffmpeg -hide_banner -encoders 2>/dev/null | grep -q libsvtav1; then
    log "encoding ${base} as AV1 (SVT-AV1)"
    ffmpeg -y -loglevel error -i "${src}" \
      -c:v libsvtav1 -preset 6 -crf 42 -g 120 -r "${fps}" \
      -pix_fmt yuv420p -movflags +faststart -an \
      "${SHOWCASE_OUT}/${base}-av1.mp4"
  else
    warn "this ffmpeg has no libsvtav1 encoder — skipping the AV1 rendition"
  fi

  # Safari only hardware-decodes AV1 on very recent silicon, so the <video>
  # element needs an H.264 <source> too.
  log "encoding ${base} as H.264"
  ffmpeg -y -loglevel error -i "${src}" \
    -c:v libx264 -crf 26 -preset slow -r "${fps}" \
    -pix_fmt yuv420p -movflags +faststart -an \
    "${SHOWCASE_OUT}/${base}-h264.mp4"
}

shopt -s nullglob
videos=("${VIDEO_DIR}"/*.webm)
shopt -u nullglob

if [ "${#videos[@]}" -eq 0 ]; then
  log "no video recorded (screenshots-only run?)"
elif [ "${SHOWCASE_SKIP_TRANSCODE}" = "1" ]; then
  warn "transcode skipped — copying the raw WebM instead"
  for src in "${videos[@]}"; do
    cp "${src}" "${SHOWCASE_OUT}/$(basename "${src}")"
  done
else
  for src in "${videos[@]}"; do
    base="$(basename "${src}" .webm)"
    transcode "${src}" "${base}"
  done
fi

# --- manifest ---------------------------------------------------------------
log "writing the version manifest"
bun "${REPO_ROOT}/scripts/showcase-manifest.mjs" "${SHOWCASE_OUT}"

log "done — assets in ${SHOWCASE_OUT}"
ls -la "${SHOWCASE_OUT}"

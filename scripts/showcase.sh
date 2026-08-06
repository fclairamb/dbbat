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
SHOWCASE_PROJECT="${SHOWCASE_PROJECT:-}"        # "screenshots" | "poster" | "video" | ""
SHOWCASE_SKIP_BUILD="${SHOWCASE_SKIP_BUILD:-0}"
SHOWCASE_SKIP_TRANSCODE="${SHOWCASE_SKIP_TRANSCODE:-0}"
SHOWCASE_SKIP_WEBP="${SHOWCASE_SKIP_WEBP:-0}"   # leave the PNGs unaccompanied
SHOWCASE_WEBP_QUALITY="${SHOWCASE_WEBP_QUALITY:-80}"
SHOWCASE_WEBP_MAX_WIDTH="${SHOWCASE_WEBP_MAX_WIDTH:-1280}"
SHOWCASE_KEEP="${SHOWCASE_KEEP:-0}"             # leave the stack up afterwards
SHOWCASE_BINARY="${SHOWCASE_BINARY:-${REPO_ROOT}/dbbat}"

# A throwaway key: this instance holds nothing but demo data.
SHOWCASE_KEY="${SHOWCASE_KEY:-MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=}"

# SHOWCASE_FIXED_TIME is not defaulted here: the pin is a constant derived from
# SHOWCASE_EPOCH in front/showcase/config.ts, and global-setup reads it once the
# scenario's own rows have been dated onto that same epoch. Set it only to force
# a specific instant.
export SHOWCASE_API_PORT SHOWCASE_PROXY_PORT SHOWCASE_PG_PORT \
       SHOWCASE_OUT SHOWCASE_WORK

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

# --- still renditions -------------------------------------------------------
#
# Every PNG in the output directory gets a WebP sibling. The homepage serves
# those (a <picture> for the grid, poster= for the clip); the PNGs stay exactly
# where they are, because the docs and any external embed link to them and a
# reader who clicks a screenshot wants the full-size original.
#
# Anything wider than SHOWCASE_WEBP_MAX_WIDTH is downscaled to it. The stills
# are captured at 2560x1600 (deviceScaleFactor 2) and render at ~350 CSS px in
# the homepage grid, so 1280 is still generously retina there. Never upscales:
# the 1280x800 video poster is re-encoded at its native size.

webp_encoder() {
  if [ "${SHOWCASE_SKIP_WEBP}" = "1" ]; then
    return
  fi
  # Ubuntu's ffmpeg is built with libwebp, Homebrew's is not; cwebp is the same
  # library's own CLI and is what the `webp` package installs everywhere.
  if command -v ffmpeg >/dev/null 2>&1 \
    && ffmpeg -hide_banner -encoders 2>/dev/null | grep -q ' libwebp'; then
    echo ffmpeg
  elif command -v cwebp >/dev/null 2>&1; then
    echo cwebp
  fi
}

# Width of an image, or 0 when we cannot tell (ffprobe ships with ffmpeg).
image_width() {
  local width
  width="$(ffprobe -v error -select_streams v:0 -show_entries stream=width \
    -of csv=p=0 "$1" 2>/dev/null || true)"
  case "${width}" in
    '' | *[!0-9]*) echo 0 ;;
    *) echo "${width}" ;;
  esac
}

webp_rendition() {
  local encoder="$1" src="$2"
  local dst="${src%.png}.webp"
  local width target
  width="$(image_width "${src}")"
  target=0
  if [ "${width}" -gt "${SHOWCASE_WEBP_MAX_WIDTH}" ]; then
    target="${SHOWCASE_WEBP_MAX_WIDTH}"
  fi

  if [ "${encoder}" = "ffmpeg" ]; then
    local scale=()
    if [ "${target}" -gt 0 ]; then
      scale=(-vf "scale=${target}:-2:flags=lanczos")
    fi
    ffmpeg -y -loglevel error -i "${src}" "${scale[@]}" \
      -c:v libwebp -quality "${SHOWCASE_WEBP_QUALITY}" -preset picture \
      -compression_level 6 "${dst}"
  else
    local resize=()
    if [ "${target}" -gt 0 ]; then
      resize=(-resize "${target}" 0)
    fi
    cwebp -quiet -q "${SHOWCASE_WEBP_QUALITY}" -m 6 -sharp_yuv \
      "${resize[@]}" "${src}" -o "${dst}"
  fi
}

WEBP_ENCODER="$(webp_encoder)"
shopt -s nullglob
stills=("${SHOWCASE_OUT}"/*.png)
shopt -u nullglob

if [ "${SHOWCASE_SKIP_WEBP}" = "1" ]; then
  log "SHOWCASE_SKIP_WEBP=1 — not writing the WebP renditions"
elif [ "${#stills[@]}" -eq 0 ]; then
  log "no PNG to re-encode"
elif [ -z "${WEBP_ENCODER}" ]; then
  warn "no WebP encoder found (ffmpeg without libwebp, and no cwebp) — the website will fall back to the PNGs"
else
  log "writing WebP renditions with ${WEBP_ENCODER} (q${SHOWCASE_WEBP_QUALITY}, max ${SHOWCASE_WEBP_MAX_WIDTH}px wide)"
  for src in "${stills[@]}"; do
    webp_rendition "${WEBP_ENCODER}" "${src}"
  done
fi

# --- manifest ---------------------------------------------------------------
log "writing the version manifest"
bun "${REPO_ROOT}/scripts/showcase-manifest.mjs" "${SHOWCASE_OUT}"

log "done — assets in ${SHOWCASE_OUT}"
ls -la "${SHOWCASE_OUT}"

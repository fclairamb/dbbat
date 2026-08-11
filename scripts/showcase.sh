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
SHOWCASE_PG_TIMEOUT="${SHOWCASE_PG_TIMEOUT:-60}"  # seconds to wait for the upstream (pg_isready + demo db)
# One or more Playwright projects, comma-separated; empty runs them all.
# "screenshots" | "poster" | "video" | "mcp-poster" | "mcp-video"
SHOWCASE_PROJECT="${SHOWCASE_PROJECT:-}"
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

# Docker's state string ("running", "exited", "created", ...), or "gone" when
# the container cannot be inspected at all (removed, never created, etc).
container_state() {
  docker inspect -f '{{.State.Status}}' "${SHOWCASE_CONTAINER}" 2>/dev/null || echo "gone"
}

dump_container_diagnostics() {
  local exit_code
  exit_code="$(docker inspect -f '{{.State.ExitCode}}' "${SHOWCASE_CONTAINER}" 2>/dev/null || echo 'unknown')"
  warn "container ${SHOWCASE_CONTAINER} state: $(container_state), exit code: ${exit_code}"
  warn "container ${SHOWCASE_CONTAINER} logs (last 50 lines):"
  docker logs --tail 50 "${SHOWCASE_CONTAINER}" 2>&1 || true
}

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
  # No --rm: a crashed container must survive long enough for the diagnostics
  # below to inspect it. The named cleanup trap still removes it on exit.
  docker run -d \
    --name "${SHOWCASE_CONTAINER}" \
    -e POSTGRES_USER=postgres \
    -e POSTGRES_PASSWORD=postgres \
    -p "${SHOWCASE_PG_PORT}:5432" \
    -v "${REPO_ROOT}/docker/postgres/init.sql:/docker-entrypoint-initdb.d/init.sql:ro" \
    "${SHOWCASE_PG_IMAGE}" >/dev/null
  STARTED_CONTAINER=1
fi

log "waiting for the upstream to accept connections (timeout ${SHOWCASE_PG_TIMEOUT}s)"
pg_ready=0
container_died=0
for _ in $(seq 1 "${SHOWCASE_PG_TIMEOUT}"); do
  if docker exec "${SHOWCASE_CONTAINER}" pg_isready -U postgres >/dev/null 2>&1; then
    pg_ready=1
    break
  fi
  state="$(container_state)"
  if [ "${state}" != "running" ]; then
    container_died=1
    break
  fi
  sleep 1
done
if [ "${pg_ready}" != "1" ]; then
  dump_container_diagnostics
  if [ "${container_died}" = "1" ]; then
    die "the upstream container died before becoming ready (state: $(container_state))"
  else
    die "the upstream container never became ready (timed out after ${SHOWCASE_PG_TIMEOUT}s, still running)"
  fi
fi

# pg_isready goes green before init.sql has finished; the demo database is the
# thing we actually need.
log "waiting for the demo database to be created (timeout ${SHOWCASE_PG_TIMEOUT}s)"
demo_ready=0
container_died=0
for _ in $(seq 1 "${SHOWCASE_PG_TIMEOUT}"); do
  if docker exec "${SHOWCASE_CONTAINER}" psql -U postgres -lqt 2>/dev/null | cut -d'|' -f1 | grep -qw demo; then
    demo_ready=1
    break
  fi
  state="$(container_state)"
  if [ "${state}" != "running" ]; then
    container_died=1
    break
  fi
  sleep 1
done
if [ "${demo_ready}" != "1" ]; then
  dump_container_diagnostics
  warn "databases currently on the upstream:"
  docker exec "${SHOWCASE_CONTAINER}" psql -U postgres -lqt 2>&1 || true
  if [ "${container_died}" = "1" ]; then
    die "the upstream container died before the demo database was created (state: $(container_state))"
  else
    die "the demo database was never created (timed out after ${SHOWCASE_PG_TIMEOUT}s — check docker/postgres/init.sql)"
  fi
fi

# --- demo-mode dbbat --------------------------------------------------------
#
# LISTENERS — the showcase dials exactly two of them: the PostgreSQL proxy and
# the API. Every other listener dbbat knows about must be off. A throwaway
# instance has no business binding :1522/:3307/:27018/:1434, and on a
# developer's laptop those are already held by their own `make dev` stack — the
# SQL Server listener, added to dbbat after this script was written, is exactly
# how that broke: `dbbat exited during startup`, bind error buried in the log.
#
# So there is no denylist to forget to update. The *used* set is named once
# below; the candidate set is discovered from internal/config at run time; and
# everything in the candidate set that is not used is exported empty. A sixth
# protocol is disabled the day it lands.

SHOWCASE_LISTENERS_USED=(DBB_LISTEN_PG DBB_LISTEN_API)

# Every DBB_LISTEN_* dbbat knows about, read off the config package. The
# `koanf:"listen_x"` struct tags are the source of truth — envTransform() maps
# DBB_<TAG> onto them — and any literal DBB_LISTEN_* spelled out in there is
# unioned in, so neither spelling alone can be missed.
discover_listener_vars() {
  {
    grep -rhoE 'koanf:"listen_[a-z0-9_]+"' internal/config/ 2>/dev/null \
      | sed -E 's/^koanf:"(.*)"$/\1/' \
      | tr '[:lower:]' '[:upper:]' \
      | sed 's/^/DBB_/' || true
    grep -rhoE 'DBB_LISTEN_[A-Z0-9_]+' internal/config/ 2>/dev/null || true
  } | sort -u
}

SHOWCASE_LISTENER_VARS=()
while IFS= read -r listener_var; do
  if [ -n "${listener_var}" ]; then
    SHOWCASE_LISTENER_VARS+=("${listener_var}")
  fi
done < <(discover_listener_vars)

# Fail loudly rather than degrade silently: an empty (or incomplete) candidate
# list would disable nothing and reintroduce the very bug this replaces.
if [ "${#SHOWCASE_LISTENER_VARS[@]}" -eq 0 ]; then
  die "no DBB_LISTEN_* listeners found in internal/config — the naming convention changed; fix discover_listener_vars() in scripts/showcase.sh before this run binds ports it never needed"
fi
for used in "${SHOWCASE_LISTENERS_USED[@]}"; do
  if ! printf '%s\n' "${SHOWCASE_LISTENER_VARS[@]}" | grep -qx "${used}"; then
    die "${used} is not among the listeners discovered in internal/config (found: ${SHOWCASE_LISTENER_VARS[*]}) — the DBB_LISTEN_* naming convention changed; fix discover_listener_vars() in scripts/showcase.sh"
  fi
done

SHOWCASE_LISTENERS_OFF=()
for listener_var in "${SHOWCASE_LISTENER_VARS[@]}"; do
  keep=0
  for used in "${SHOWCASE_LISTENERS_USED[@]}"; do
    if [ "${listener_var}" = "${used}" ]; then
      keep=1
      break
    fi
  done
  if [ "${keep}" = "0" ]; then
    SHOWCASE_LISTENERS_OFF+=("${listener_var}")
  fi
done

DBBAT_ENV=(
  DBB_RUN_MODE=demo
  "DBB_DSN=postgres://postgres:postgres@localhost:${SHOWCASE_PG_PORT}/dbbat?sslmode=disable"
  "DBB_KEY=${SHOWCASE_KEY}"
  "DBB_LISTEN_API=:${SHOWCASE_API_PORT}"
  "DBB_LISTEN_PG=:${SHOWCASE_PROXY_PORT}"
  DBB_APPROVAL_ENABLED=true
  DBB_APPROVAL_SLACK_DELAY=0
  DBB_RATE_LIMIT_ENABLED=false
  "DBB_LOG_LEVEL=${SHOWCASE_LOG_LEVEL:-warn}"
)
for listener_var in "${SHOWCASE_LISTENERS_OFF[@]}"; do
  DBBAT_ENV+=("${listener_var}=")
done

# Which listener variable asked for a given port, if any. Used to turn a bare
# bind error into something actionable.
listener_var_for_port() {
  local port="$1" entry name value
  for entry in "${DBBAT_ENV[@]}"; do
    name="${entry%%=*}"
    value="${entry#*=}"
    case "${name}" in
      DBB_LISTEN_*) ;;
      *) continue ;;
    esac
    if [ -n "${value}" ] && [ "${value##*:}" = "${port}" ]; then
      printf '%s' "${name}"
      return 0
    fi
  done
  return 1
}

# A bind collision is the one startup failure with a specific, actionable
# cause, so surface exactly that line instead of 30 lines of log. Everything
# else falls back to the tail.
dbbat_startup_diagnostics() {
  local logfile="${SHOWCASE_WORK}/dbbat.log" bind_line="" port="" var=""
  if [ -f "${logfile}" ]; then
    bind_line="$(grep -m1 'bind: address already in use' "${logfile}" || true)"
  fi
  if [ -z "${bind_line}" ]; then
    if [ -f "${logfile}" ]; then
      tail -30 "${logfile}" >&2 || true
    else
      warn "no log at ${logfile} — dbbat never got far enough to write one"
    fi
    return
  fi
  warn "${bind_line}"
  port="$(printf '%s\n' "${bind_line}" | sed -nE 's/.*:([0-9]+): bind: address already in use.*/\1/p' | head -1)"
  if [ -n "${port}" ]; then
    var="$(listener_var_for_port "${port}" || true)"
    if [ -n "${var}" ]; then
      warn "port ${port} is the one ${var} asked for — free it, or point the matching SHOWCASE_*_PORT elsewhere"
    else
      warn "port ${port} is not one this run asked for — a listener escaped the allowlist above (disabled: ${SHOWCASE_LISTENERS_OFF[*]:-none})"
    fi
  fi
  warn "full log: ${logfile}"
}

log "starting dbbat in demo mode (api :${SHOWCASE_API_PORT}, pg proxy :${SHOWCASE_PROXY_PORT})"
log "listeners off: ${SHOWCASE_LISTENERS_OFF[*]:-none}"
env "${DBBAT_ENV[@]}" "${SHOWCASE_BINARY}" serve >"${SHOWCASE_WORK}/dbbat.log" 2>&1 &
DBBAT_PID=$!

log "waiting for the API"
for _ in $(seq 1 60); do
  if curl -fsS "http://localhost:${SHOWCASE_API_PORT}/api/v1/health" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "${DBBAT_PID}" 2>/dev/null; then
    dbbat_startup_diagnostics
    die "dbbat exited during startup"
  fi
  sleep 1
done
curl -fsS "http://localhost:${SHOWCASE_API_PORT}/api/v1/health" >/dev/null \
  || { dbbat_startup_diagnostics; die "the API never became ready"; }

# --- capture ----------------------------------------------------------------
log "running the showcase Playwright project"
PLAYWRIGHT_ARGS=(test --config=showcase/playwright.config.ts)
if [ -n "${SHOWCASE_PROJECT}" ]; then
  # Comma-separated, because a clip and its poster are two projects and asking
  # for one without the other is almost never what you meant.
  IFS=',' read -r -a showcase_projects <<< "${SHOWCASE_PROJECT}"
  for project in "${showcase_projects[@]}"; do
    [ -n "${project}" ] && PLAYWRIGHT_ARGS+=(--project="${project}")
  done
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

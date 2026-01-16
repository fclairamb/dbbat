# =============================================================================
# DBBat Multi-Stage Build
# =============================================================================
# This Dockerfile documents and executes the complete build process:
#   1. Frontend: React app built with Bun
#   2. Backend: Go binary with embedded frontend
#   3. Runtime: Minimal distroless image
#
# Usage:
#   docker build -t dbbat .
#   docker run -p 5432:5432 -p 8080:8080 dbbat
# =============================================================================

# -----------------------------------------------------------------------------
# Stage 1: Frontend Build
# -----------------------------------------------------------------------------
# Builds the React frontend using Bun. The output (dist/) will be embedded
# into the Go binary in the next stage.
FROM oven/bun:1.3.6 AS frontend

WORKDIR /app/front

# Install dependencies first (better layer caching)
COPY front/package.json front/bun.lock ./
RUN bun install --frozen-lockfile

# Copy OpenAPI spec for type generation
COPY internal/api/openapi.yml ../internal/api/

# Copy frontend source
COPY front/ ./

# Generate TypeScript types from OpenAPI spec and build
# Note: Using build:no-check to skip TypeScript type checking (matches scripts/build-frontend.sh)
# Type checking should be done separately in CI via `bun run lint` in the frontend job
RUN bun run generate-client && bun run build:no-check

# -----------------------------------------------------------------------------
# Stage 2: Backend Build
# -----------------------------------------------------------------------------
# Builds the Go binary with the frontend embedded via go:embed.
FROM golang:1.25.6-trixie AS backend

WORKDIR /app

# Install dependencies first (better layer caching)
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Copy frontend build output to the embed location
COPY --from=frontend /app/front/dist ./internal/api/resources/

# Build arguments for version info (can be overridden at build time)
ARG VERSION=dev
ARG COMMIT=unknown
ARG GIT_TIME=unknown

# Build the application with version info
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w \
    -X 'github.com/fclairamb/dbbat/internal/version.Version=${VERSION}' \
    -X 'github.com/fclairamb/dbbat/internal/version.Commit=${COMMIT}' \
    -X 'github.com/fclairamb/dbbat/internal/version.GitTime=${GIT_TIME}'" \
    -o dbbat .

# -----------------------------------------------------------------------------
# Stage 3: Runtime
# -----------------------------------------------------------------------------
# Minimal runtime image using distroless for security.
# - No shell, no package manager, no unnecessary tools
# - Runs as non-root user
FROM gcr.io/distroless/base-debian13:nonroot

WORKDIR /app

# Copy binary from builder
COPY --from=backend /app/dbbat .

# Expose ports:
#   5432 - PostgreSQL proxy
#   8080 - REST API and web UI
EXPOSE 5432 8080

# Run the binary
CMD ["./dbbat"]

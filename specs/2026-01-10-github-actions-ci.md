# GitHub Actions CI/CD Pipeline

## Overview

This specification defines the GitHub Actions CI/CD pipeline for DBBat, covering:
- Frontend build with caching
- Backend build with lint and tests
- Playwright E2E tests
- Website deployment to GitHub Pages
- GoReleaser for multi-platform binaries and container images

## Current State Analysis

### Existing Infrastructure
- **Frontend**: React app (Bun) in `front/`, builds to `internal/api/resources/`
- **Backend**: Go 1.25.5 with ldflags versioning (`internal/version/version.go`)
- **Website**: Docusaurus in `website/`, has existing `website/.github/workflows/deploy.yml`
- **Docker**: Multi-stage Dockerfile with distroless base
- **Tests**: Go unit tests (with testcontainers), Playwright E2E tests
- **Version info**: `Version`, `Commit`, `BuildTime` set via ldflags

### Key Observations
1. Frontend build generates API types from OpenAPI spec (`generate-client`)
2. E2E tests require: PostgreSQL, built binary, test mode (`DBB_RUN_MODE=test`)
3. Website deployment already exists but is isolated in `website/.github/`
4. No existing GoReleaser configuration

## Architecture Decision: Workflow Structure

### Option A: Single Workflow (Recommended)
One `ci.yml` with multiple jobs and smart dependencies.

**Pros:**
- Single status check for PRs
- Easy to visualize entire pipeline
- Shared workflow context and variables

**Cons:**
- Larger file to maintain

### Option B: Multiple Workflows
Separate files: `ci.yml`, `release.yml`, `deploy-website.yml`

**Pros:**
- Separation of concerns
- Independent triggers
- Smaller files

**Cons:**
- More files to coordinate
- Harder to share state between workflows

### Recommendation
Use **Option A** for CI/testing with a separate `release.yml` for GoReleaser (triggered on tags only). Move the website deployment into the main workflow.

## Workflow Design

### Triggers

```yaml
on:
  push:
    branches: [main]
    tags: ["v*"]
  pull_request:
    branches: [main]
```

### Jobs Overview

```
┌──────────────────────────────────────────────────────────────────┐
│                         CI Pipeline                               │
├──────────────────────────────────────────────────────────────────┤
│                                                                   │
│  ┌─────────────┐   ┌─────────────┐   ┌──────────────────────┐    │
│  │  lint       │   │  frontend   │   │  test (go unit)      │    │
│  │  (parallel) │   │  (parallel) │   │  (parallel)          │    │
│  └─────────────┘   └──────┬──────┘   └──────────────────────┘    │
│                           │                                       │
│                           ▼                                       │
│                    ┌─────────────┐                                │
│                    │   build     │←── needs: frontend             │
│                    │  (backend)  │                                │
│                    └──────┬──────┘                                │
│                           │                                       │
│                           ▼                                       │
│                    ┌─────────────┐                                │
│                    │  e2e-tests  │←── needs: build                │
│                    │ (playwright)│                                │
│                    └─────────────┘                                │
│                                                                   │
│  ┌───────────────────────────────────────────────────────────┐   │
│  │  website-deploy (only on main, when website/** changed)   │   │
│  └───────────────────────────────────────────────────────────┘   │
│                                                                   │
└──────────────────────────────────────────────────────────────────┘

                    Release Pipeline (tags only)
┌──────────────────────────────────────────────────────────────────┐
│  ┌─────────────┐                                                 │
│  │  release    │←── goreleaser: binaries + containers            │
│  │  (on v*)    │                                                 │
│  └─────────────┘                                                 │
└──────────────────────────────────────────────────────────────────┘
```

## Job Specifications

### 1. Lint Job

```yaml
lint:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version: "1.25"
    - uses: golangci/golangci-lint-action@v6
      with:
        version: latest
```

### 2. Frontend Job

**Caching Strategy:**
- Cache `front/node_modules` keyed by `bun.lockb` hash
- Cache `~/.bun/install/cache` for global Bun cache

```yaml
frontend:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: oven-sh/setup-bun@v2

    - name: Cache frontend dependencies
      uses: actions/cache@v4
      with:
        path: |
          front/node_modules
          ~/.bun/install/cache
        key: frontend-${{ hashFiles('front/bun.lockb') }}
        restore-keys: frontend-

    - name: Install dependencies
      run: bun install
      working-directory: front

    - name: Generate API types
      run: bun run generate-client
      working-directory: front

    - name: Lint frontend
      run: bun run lint
      working-directory: front

    - name: Build frontend
      run: bun run build
      working-directory: front

    - name: Upload frontend artifacts
      uses: actions/upload-artifact@v4
      with:
        name: frontend-dist
        path: front/dist/
        retention-days: 1
```

### 3. Test Job (Go Unit Tests)

```yaml
test:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version: "1.25"

    - name: Run tests
      run: go test -race -coverprofile=coverage.out ./...

    - name: Upload coverage
      uses: codecov/codecov-action@v4
      with:
        files: coverage.out
      continue-on-error: true  # Don't fail if codecov is not configured
```

**Note:** testcontainers-go spins up PostgreSQL containers automatically, so no external service needed.

### 4. Build Job

```yaml
build:
  runs-on: ubuntu-latest
  needs: [frontend]
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version: "1.25"

    - name: Download frontend artifacts
      uses: actions/download-artifact@v4
      with:
        name: frontend-dist
        path: internal/api/resources/

    - name: Build binary
      run: |
        VERSION=${GITHUB_REF_NAME:-dev}
        COMMIT=${GITHUB_SHA::7}
        BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)
        go build -ldflags "-X 'github.com/fclairamb/dbbat/internal/version.Version=$VERSION' \
                          -X 'github.com/fclairamb/dbbat/internal/version.Commit=$COMMIT' \
                          -X 'github.com/fclairamb/dbbat/internal/version.BuildTime=$BUILD_TIME'" \
          -o ./bin/dbbat ./cmd/dbbat

    - name: Upload binary
      uses: actions/upload-artifact@v4
      with:
        name: dbbat-binary
        path: bin/dbbat
        retention-days: 1
```

### 5. E2E Tests Job

```yaml
e2e-tests:
  runs-on: ubuntu-latest
  needs: [build]
  services:
    postgres:
      image: postgres:15
      env:
        POSTGRES_USER: postgres
        POSTGRES_PASSWORD: postgres
      ports:
        - 5002:5432
      volumes:
        - ${{ github.workspace }}/docker/postgres/init.sql:/docker-entrypoint-initdb.d/init.sql
      options: >-
        --health-cmd "pg_isready -U postgres"
        --health-interval 10s
        --health-timeout 5s
        --health-retries 5
  steps:
    - uses: actions/checkout@v4
    - uses: oven-sh/setup-bun@v2

    - name: Download binary
      uses: actions/download-artifact@v4
      with:
        name: dbbat-binary
        path: bin/

    - name: Make binary executable
      run: chmod +x bin/dbbat

    - name: Install Playwright
      run: |
        bun install
        bunx playwright install --with-deps chromium
      working-directory: front

    - name: Start DBBat server
      run: |
        ./bin/dbbat serve &
        sleep 5  # Wait for server to start
      env:
        DBB_RUN_MODE: test
        DBB_DSN: postgres://postgres:postgres@localhost:5002/dbbat?sslmode=disable
        DBB_KEY: MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=

    - name: Run Playwright tests
      run: bun run test:e2e:chromium
      working-directory: front

    - name: Upload test results
      uses: actions/upload-artifact@v4
      if: always()
      with:
        name: playwright-report
        path: front/playwright-report/
        retention-days: 7

    - name: Upload test screenshots
      uses: actions/upload-artifact@v4
      if: failure()
      with:
        name: playwright-screenshots
        path: front/test-results/
        retention-days: 7
```

**Challenge:** GitHub Actions service containers don't support mounting workspace files. We'll need an alternative approach.

### 5b. E2E Tests Job (Alternative - Docker Compose)

Since service containers can't mount workspace files, use docker-compose:

```yaml
e2e-tests:
  runs-on: ubuntu-latest
  needs: [build]
  steps:
    - uses: actions/checkout@v4
    - uses: oven-sh/setup-bun@v2

    - name: Download binary
      uses: actions/download-artifact@v4
      with:
        name: dbbat-binary
        path: bin/

    - name: Make binary executable
      run: chmod +x bin/dbbat

    - name: Start PostgreSQL
      run: docker compose up -d postgres

    - name: Wait for PostgreSQL
      run: |
        for i in {1..30}; do
          if docker compose exec -T postgres pg_isready -U postgres; then
            echo "PostgreSQL is ready"
            break
          fi
          echo "Waiting for PostgreSQL... ($i/30)"
          sleep 2
        done

    - name: Install Playwright
      run: |
        bun install
        bunx playwright install --with-deps chromium
      working-directory: front

    - name: Start DBBat server
      run: |
        ./bin/dbbat serve &
        sleep 5
      env:
        DBB_RUN_MODE: test
        DBB_DSN: postgres://postgres:postgres@localhost:5002/dbbat?sslmode=disable
        DBB_KEY: MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=

    - name: Run Playwright tests
      run: bun run test:e2e:chromium
      working-directory: front

    - name: Upload test artifacts
      uses: actions/upload-artifact@v4
      if: always()
      with:
        name: playwright-results
        path: |
          front/playwright-report/
          front/test-results/
        retention-days: 7

    - name: Stop services
      if: always()
      run: docker compose down
```

### 6. Website Deployment Job

```yaml
website-deploy:
  runs-on: ubuntu-latest
  if: github.ref == 'refs/heads/main' && github.event_name == 'push'
  # Only run if website files changed
  steps:
    - uses: actions/checkout@v4
      with:
        fetch-depth: 2

    - name: Check for website changes
      id: changes
      run: |
        if git diff --name-only HEAD~1 HEAD | grep -q '^website/'; then
          echo "changed=true" >> $GITHUB_OUTPUT
        else
          echo "changed=false" >> $GITHUB_OUTPUT
        fi

    - uses: oven-sh/setup-bun@v2
      if: steps.changes.outputs.changed == 'true'

    - name: Install dependencies
      if: steps.changes.outputs.changed == 'true'
      run: bun install
      working-directory: website

    - name: Build website
      if: steps.changes.outputs.changed == 'true'
      run: bun run build
      working-directory: website

    - name: Deploy to GitHub Pages
      if: steps.changes.outputs.changed == 'true'
      uses: JamesIves/github-pages-deploy-action@v4
      with:
        folder: website/build
        branch: gh-pages
        clean: true
```

**Alternative:** Use path filters at the workflow level with `paths:` trigger.

## GoReleaser Configuration

### `.goreleaser.yml`

```yaml
version: 2

project_name: dbbat

before:
  hooks:
    # Build frontend before Go build
    - ./scripts/build-frontend.sh

builds:
  - id: dbbat
    main: ./cmd/dbbat
    binary: dbbat
    goos:
      - linux
      - darwin
      - windows
    goarch:
      - amd64
      - arm64
    # Windows only on amd64
    ignore:
      - goos: windows
        goarch: arm64
    ldflags:
      - -s -w
      - -X 'github.com/fclairamb/dbbat/internal/version.Version={{ .Version }}'
      - -X 'github.com/fclairamb/dbbat/internal/version.Commit={{ .ShortCommit }}'
      - -X 'github.com/fclairamb/dbbat/internal/version.BuildTime={{ .Date }}'
    env:
      - CGO_ENABLED=0

archives:
  - id: default
    formats:
      - tar.gz
    format_overrides:
      - goos: windows
        formats:
          - zip
    name_template: >-
      {{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}
    files:
      - LICENSE*
      - README*

checksum:
  name_template: 'checksums.txt'
  algorithm: sha256

changelog:
  sort: asc
  filters:
    exclude:
      - '^docs:'
      - '^test:'
      - '^chore:'
      - Merge pull request
      - Merge branch

dockers:
  # AMD64
  - id: amd64
    goos: linux
    goarch: amd64
    image_templates:
      - "ghcr.io/fclairamb/dbbat:{{ .Version }}-amd64"
      - "ghcr.io/fclairamb/dbbat:latest-amd64"
    dockerfile: Dockerfile.goreleaser
    use: buildx
    build_flag_templates:
      - "--platform=linux/amd64"
      - "--label=org.opencontainers.image.title={{ .ProjectName }}"
      - "--label=org.opencontainers.image.version={{ .Version }}"
      - "--label=org.opencontainers.image.source={{ .GitURL }}"
      - "--label=org.opencontainers.image.created={{ .Date }}"
      - "--label=org.opencontainers.image.revision={{ .FullCommit }}"

  # ARM64
  - id: arm64
    goos: linux
    goarch: arm64
    image_templates:
      - "ghcr.io/fclairamb/dbbat:{{ .Version }}-arm64"
      - "ghcr.io/fclairamb/dbbat:latest-arm64"
    dockerfile: Dockerfile.goreleaser
    use: buildx
    build_flag_templates:
      - "--platform=linux/arm64"
      - "--label=org.opencontainers.image.title={{ .ProjectName }}"
      - "--label=org.opencontainers.image.version={{ .Version }}"
      - "--label=org.opencontainers.image.source={{ .GitURL }}"
      - "--label=org.opencontainers.image.created={{ .Date }}"
      - "--label=org.opencontainers.image.revision={{ .FullCommit }}"

docker_manifests:
  - name_template: "ghcr.io/fclairamb/dbbat:{{ .Version }}"
    image_templates:
      - "ghcr.io/fclairamb/dbbat:{{ .Version }}-amd64"
      - "ghcr.io/fclairamb/dbbat:{{ .Version }}-arm64"
  - name_template: "ghcr.io/fclairamb/dbbat:latest"
    image_templates:
      - "ghcr.io/fclairamb/dbbat:latest-amd64"
      - "ghcr.io/fclairamb/dbbat:latest-arm64"

release:
  github:
    owner: fclairamb
    name: dbbat
  draft: false
  prerelease: auto
  name_template: "v{{ .Version }}"
```

### `Dockerfile.goreleaser`

A simpler Dockerfile for GoReleaser (binary already built):

```dockerfile
FROM gcr.io/distroless/base-debian13:nonroot

WORKDIR /app

COPY dbbat .

EXPOSE 5432 8080

CMD ["./dbbat"]
```

### Release Workflow (`.github/workflows/release.yml`)

```yaml
name: Release

on:
  push:
    tags:
      - 'v*'

permissions:
  contents: write
  packages: write

jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - uses: actions/setup-go@v5
        with:
          go-version: "1.25"

      - uses: oven-sh/setup-bun@v2

      - name: Login to GitHub Container Registry
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Set up QEMU
        uses: docker/setup-qemu-action@v3

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3

      - name: Run GoReleaser
        uses: goreleaser/goreleaser-action@v6
        with:
          version: latest
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

## Suggestions and Proposals

### 1. Test Matrix for Playwright

Consider running E2E tests on multiple browsers in parallel:

```yaml
e2e-tests:
  runs-on: ubuntu-latest
  needs: [build]
  strategy:
    fail-fast: false
    matrix:
      browser: [chromium, firefox, webkit]
  steps:
    # ...
    - name: Run Playwright tests
      run: bun run test:e2e:${{ matrix.browser }}
```

**Trade-off:** More coverage vs. longer CI time and more resource usage. Recommendation: Start with Chromium only, add others later if needed.

### 2. Caching Improvements

**Go Module Cache:**
```yaml
- uses: actions/setup-go@v5
  with:
    go-version: "1.25"
    cache: true  # Enabled by default, caches ~/go/pkg/mod
```

**Playwright Browser Cache:**
```yaml
- name: Cache Playwright browsers
  uses: actions/cache@v4
  with:
    path: ~/.cache/ms-playwright
    key: playwright-${{ hashFiles('front/bun.lockb') }}
```

### 3. PR Preview for Website

Consider deploying preview builds for PRs:

```yaml
- name: Deploy PR Preview
  if: github.event_name == 'pull_request'
  uses: JamesIves/github-pages-deploy-action@v4
  with:
    folder: website/build
    branch: gh-pages
    target-folder: pr-${{ github.event.pull_request.number }}
```

### 4. Security Scanning

Add security scanning with `govulncheck`:

```yaml
security:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version: "1.25"
    - name: Run govulncheck
      run: go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

### 5. Dependency Updates with Dependabot

Create `.github/dependabot.yml`:

```yaml
version: 2
updates:
  - package-ecosystem: gomod
    directory: /
    schedule:
      interval: weekly

  - package-ecosystem: npm
    directory: /front
    schedule:
      interval: weekly

  - package-ecosystem: npm
    directory: /website
    schedule:
      interval: weekly

  - package-ecosystem: github-actions
    directory: /
    schedule:
      interval: weekly
```

### 6. Concurrency Control

Prevent redundant runs when pushing multiple commits:

```yaml
concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true
```

### 7. Branch Protection

After CI is set up, configure branch protection rules for `main`:
- Require status checks to pass (lint, test, e2e-tests)
- Require PR reviews
- Require linear history (optional)

## Files to Create

1. `.github/workflows/ci.yml` - Main CI workflow
2. `.github/workflows/release.yml` - GoReleaser workflow
3. `.goreleaser.yml` - GoReleaser configuration
4. `Dockerfile.goreleaser` - Simplified Dockerfile for releases
5. `.github/dependabot.yml` - Dependency updates (optional)

## Files to Remove

1. `website/.github/workflows/deploy.yml` - Moved to main workflow

## Open Questions

1. **Test coverage requirements**: Should we enforce minimum coverage percentages?
2. **Docker Hub**: Should we also push to Docker Hub, or GHCR only?
3. **Release branches**: Should releases come from `main` only, or also `release/*` branches?
4. **Nightly builds**: Would automatic nightly builds from `main` be useful?
5. **SBOM generation**: Should GoReleaser generate Software Bill of Materials?

## Implementation Order

1. Create `.github/workflows/ci.yml` with lint, test, frontend, build jobs
2. Add E2E tests job
3. Migrate website deployment to main workflow
4. Create GoReleaser configuration
5. Create release workflow
6. Test with a test tag (e.g., `v0.0.1-test`)
7. Remove `website/.github/workflows/deploy.yml`
8. (Optional) Add Dependabot, security scanning, etc.

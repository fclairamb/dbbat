#!/bin/bash
# Dev environment variables for the backend. Sourced by the dev loop
# (scripts/dev.sh and the Makefile dev-back target) before launching
# cmd/devloop, which starts ./tmp/dbbat serve with this environment.
# DBB_REDIRECTS proxies /app/* to the Vite dev server for hot module replacement.
export DBB_DSN="postgres://postgres:postgres@localhost:5001/dbbat?sslmode=disable"
export DBB_KEY="MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE="
export DBB_RUN_MODE="test"
export DBB_REDIRECTS="/app:localhost:5173/app"

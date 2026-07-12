#!/bin/bash
export DBB_DSN="postgres://postgres:postgres@localhost:5001/dbbat?sslmode=disable"
export DBB_KEY="MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE="
export DBB_RUN_MODE="test"
export DBB_REDIRECTS="/app:localhost:5173/app"
exec ./tmp/dbbat serve

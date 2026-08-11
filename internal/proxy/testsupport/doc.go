// Package testsupport holds fixtures shared by the protocol proxies'
// integration suites (PostgreSQL, MySQL, MongoDB, Oracle).
//
// Everything of substance in this package is behind the `integration` build
// tag, so a normal `go test ./...` neither compiles nor links it. This file
// carries no tag purely so the package still has a Go file — and therefore
// still builds — in a plain `go build ./...`.
package testsupport

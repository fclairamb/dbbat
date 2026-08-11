// Package testsupport holds fixtures shared by the protocol proxies' test
// suites (PostgreSQL, MySQL, MongoDB, Oracle, SQL Server).
//
// Nothing here carries the `integration` build tag. The SQL Server suite mints
// its grants from the same fixture but runs against an in-process fake, so it
// is untagged and would not otherwise be able to import a tagged helper. That
// costs nothing in the shipped binary: this package is only ever imported by
// _test.go files, so it is absent from `go list -deps .` and never links into
// dbbat itself.
package testsupport

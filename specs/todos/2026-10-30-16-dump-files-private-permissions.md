---
model: opus
effort: low
---

# Session captures are created world-readable

## Problem

Found by the security scan of 2026-10-30 (finding L5).

Severity: Low. Triggered by any local user of the host running dbbat, outside containers.

`.pcapng` captures hold plaintext statements, bind parameters and result rows. They are created with `os.Create` (`internal/dump/writer.go:86`), so mode 0666 minus the umask (usually 0644), in a directory created 0755 (`internal/proxy/postgresql/server.go:185`, `internal/proxy/oracle/server.go:139`). Any local account can read every captured session.

## Proposal

1. Create capture files with `os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)` in `internal/dump/writer.go`.
2. Create the dump directory with 0700 everywhere it is created (grep every `MkdirAll(s.dumpConfig.Dir` and the shared helper if any).

## Tests

- `internal/dump/writer_test.go`: a new capture file has mode 0600. This fails before the fix.
- Positive control: writing, closing and reading back a capture still works, and the anonymise/decode CLI can open it.

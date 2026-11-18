# Uniform retention settings

## Goal

One config section, one naming rule and one set of rules for every retention
window, in the config file and in the environment. The old names are removed,
not aliased.

## Why

Three windows exist today. Each one was added on its own.

| What | Environment | Config key | Unset means | A typo (`30d`) does |
|---|---|---|---|---|
| Statements and their captured rows | `DBB_QUERY_STORAGE_RETENTION` | `query_storage.retention` | never | WARN, nothing deleted |
| Closed connections | `DBB_CONNECTION_RETENTION` | `connection.retention` | inherit the statement window | WARN, nothing deleted |
| Local capture files | `DBB_DUMP_RETENTION` | `dump.retention` | `24h` | deletes after `24h`, no log line |

- Three name shapes: `<TABLE>_STORAGE_RETENTION`, `<TABLE>_RETENTION`,
  `DUMP_RETENTION`. The second needs its own entry in `envExactKeys`, because
  no prefix rule matches it.
- Three sections of the config file.
- Three meanings of "unset".
- Two behaviours on a mistake. The dump one deletes: `runDumpCleanup` falls
  back to 24 hours when the value does not parse, and says nothing.
- The dump window is parsed in five places. Each proxy starts its own hourly
  sweep of the same directory.

The archive design
(`specs/ideas/2026-11-17-01-archive-statements-to-object-storage.md`) adds an
archive window. It should land in a clean scheme, not as a fourth shape.

## Design

### Names

`DBB_RETENTION_<OBJECT>_<ACTION>_AFTER`, and the same words as a dotted key.

| What | Environment | Config key | Default |
|---|---|---|---|
| Statements and their captured rows | `DBB_RETENTION_QUERIES_DELETE_AFTER` | `retention.queries.delete_after` | never |
| Closed connections | `DBB_RETENTION_CONNECTIONS_DELETE_AFTER` | `retention.connections.delete_after` | never |
| Local capture files | `DBB_RETENTION_DUMPS_DELETE_AFTER` | `retention.dumps.delete_after` | `24h` |

```yaml
retention:
  queries:
    delete_after: 720h
  connections:
    delete_after: 8760h
  dumps:
    delete_after: 24h
```

- The object is a plural noun the operator already knows: `queries` (the
  table, the API path, the `query_storage` section), `connections`, `dumps`
  (`DBB_DUMP_DIR`).
- The action is `delete`. The archive design adds `archive`:
  `DBB_RETENTION_QUERIES_ARCHIVE_AFTER`, `retention.queries.archive_after`.
- The same dotted key is the name of the store parameter when the Settings
  page gets one (archive design, phase 1). One name in the environment, the
  file, the store and the API.

Settings that say what is kept or where, not for how long, do not move:
`DBB_QUERY_STORAGE_STORE_RESULTS`, `DBB_QUERY_STORAGE_MAX_RESULT_ROWS`,
`DBB_QUERY_STORAGE_MAX_RESULT_BYTES`, `DBB_DUMP_DIR`, `DBB_DUMP_MAX_SIZE`,
`DBB_DUMP_UPLOAD_URL`.

### Values

- A Go duration, like every other duration in dbbat (`90s`, `720h`).
- `0` means never. Unset means the default.
- The default is never. dbbat deletes nothing it was not told to delete.
- One exception: dumps default to `24h`. A capture is a debugging file with
  the full payload of a session, it is opt-in (`DBB_DUMP_DIR`), and a spool
  that never empties fills the disk.
- No inheritance. Today an unset connection window takes the statement window.
  After this change it means never: statements can expire, the ledger stays.
  The ledger is small and it is the evidence.

### One rule for a mistake

`config.RetentionWindows()` resolves the three windows. When anything is
wrong, every window is zero, nothing is deleted (dumps included), and one
startup WARN names the variable and the value. It is never a startup failure:
a proxy that keeps everything is better than a proxy that does not start.

Wrong means:

- a value that is not a Go duration, or is negative;
- a `DBB_RETENTION_*` variable or a `retention.*` key that is not in the table
  above (`DBB_RETENTION_QUERY_DELETE_AFTER`). Today a mistyped name is
  silently unset;
- a connection window shorter than the statement window (deleting a
  connection cascades to its statements);
- a connection window set while statements are kept forever.

The last two rules exist today. The first two are new for dumps.

### Code

- `internal/config/config.go`: a `RetentionConfig` struct under
  `Config.Retention`. `QueryStorageConfig.Retention`, `DumpConfig.Retention`
  and the whole `ConnectionConfig` (it has no other field) go away.
- `envTransform`: one rule, `retention_<object>_<setting>` to
  `retention.<object>.<setting>`, with the object taken from a known list.
  The `connection_retention` entry of `envExactKeys` goes away.
- `RetentionWindows` gains `Dumps`. Unknown keys are found in
  `operatorLayer`, which holds what the operator set without the defaults.
- `retention.go` runs the dump sweep (`dump.CleanupOldFiles`) next to the
  other two. The five `runDumpCleanup` loops, their constants and their
  goroutine names go away.
- `store.Options.QueryRetention` is fed from `RetentionWindows().Queries`, as
  today.
- Every mention of the old names: `CLAUDE.md`, `README.md`,
  `website/docs/configuration/index.md`,
  `website/docs/features/query-logging.md`,
  `website/docs/features/session-dumps.md`, `website/docs/compliance.md`,
  `internal/api/openapi.yml`, the comments in `internal/api/audit_verify.go`
  and `internal/api/observability.go`.

### Tests

- `connection_retention_test.go`, `query_storage_retention_test.go` and the
  retention part of `dump_env_test.go` become one table test in
  `internal/config/retention_test.go`.
- Each row of the names table: the variable sets the key, and the key alone
  sets the window.
- Each kind of mistake: all three windows are zero and the WARN names the
  culprit.
- Nothing set: statements and connections are never deleted, dumps at 24h.
- A test greps the docs and the OpenAPI file for the three removed names.

## Breaking change

PR title: `feat(config)!: uniform retention settings`.

A deployment that still sets an old name gets no error. The variable is
ignored:

- `DBB_QUERY_STORAGE_RETENTION` and `DBB_CONNECTION_RETENTION`: history is
  kept forever. Nothing is lost.
- `DBB_DUMP_RETENTION` longer than `24h`: capture files are deleted sooner
  than configured. This is the one case that loses data.

The changelog lists the three renames and says so.

## Out of scope

- The store parameter layer and the Settings page (archive design, phase 1).
- Windows per server and per server group (same).
- Uploaded captures. Their lifetime is the bucket's lifecycle rule.
- `audit_log`, never deleted. OAuth states and stale instance rows have a
  fixed lifetime and no setting.

## Open questions

1. **Warn when a removed name is set?** Ten lines: at startup, if one of the
   three old variables is present, WARN with the new name. It goes against
   "ignore the past", and it covers the one case above that loses data.
   Recommended: yes, for the dump variable at least.
2. **`queries` or `statements`?** The docs say "statements", the table, the
   API and the config say "queries". Recommended: `queries`.
3. **Dumps: `24h` or never by default?** Never would make the rule have no
   exception. Recommended: keep `24h`.

# SQLcl Client Validation

## Goal

Validate that **Oracle SQLcl** (the modern command-line client from Oracle, JDBC thin–based) can:

1. Connect through the dbbat Oracle proxy using terminated auth (username + API key as Oracle password).
2. Successfully run `SELECT` queries against the upstream database via the proxy.
3. Have its queries and result rows captured in dbbat's audit log.

SQLcl is the natural successor to SQL*Plus and is widely used by DBAs and developers. Its JDBC thin driver is the same one shipped with ojdbc11, so it exercises the same parsing paths as DBeaver and other JDBC-thin clients — but standalone, scriptable, and easy to drive from CI.

## Why this matters

SQLcl support is a proxy of "JDBC thin works end-to-end". Today the client compatibility table in `docs/oracle.md` lists JDBC thin as **"SQL works, row capture partial"**. We want SQLcl listed as a first-class supported client, with both SQL extraction and row capture validated.

The `internal/proxy/oracle/ttc_auth.go` file already has special cases for SQLcl (chunked username encoding in AUTH_TERMINAL phase). Those code paths need to be exercised against a real SQLcl client end-to-end.

## Acceptance criteria

- [ ] SQLcl connects to the dbbat Oracle proxy with `username/api_key@host:port/service_name`.
- [ ] `SELECT 1 FROM DUAL` returns the expected row.
- [ ] `SELECT * FROM <real table>` returns rows correctly (data integrity, column types, NULLs).
- [ ] dbbat's `/api/v1/connections` and `/api/v1/queries` endpoints show the session and the SQL text.
- [ ] Result rows are captured (where applicable) and visible via the query detail endpoint.
- [ ] The compatibility table in `docs/oracle.md` is updated with SQLcl status.

## Test environment

- **Windows host**: `15.237.251.23` (Oracle Instant Client already installed). SQLcl install candidate.
- **Upstream DB**: `abynonprod` (Stonal non-prod Oracle, see `db-provision.priv.md`).
- **Proxy**: dbbat dev instance reachable from the Windows host.

## Notes

- SQLcl requires a JRE (Java 11+). Check whether one is already on the Windows host before installing.
- Use `connect username/api_key@host:port/service_name` — SQLcl's EZConnect syntax mirrors SQL*Plus.
- For row-capture debugging, set `DBB_LOG_LEVEL=debug` and watch `logs/`.

## Status (2026-04-25)

**Currently failing** — tested against `test-relay-v5` deployed on aws/master cluster.

### Test setup

- Windows host `15.237.251.23` with **JDK 21.0.11** at `C:\jdk\jdk-21.0.11` and **SQLcl 26.1.0** at `C:\sqlcl`.
- Connection string used: `admin/<API_KEY>@//k8s-tooling-dbbatora-…elb.eu-west-3.amazonaws.com:1522/abyla_abynonprod`.
- SQL: `SELECT 1 FROM DUAL`.

### What works

- Pre-auth TNS relay completes — Connect / Resend / Accept / Set Protocol / Set Data Types all succeed.
- AUTH Phase 1 received and parsed (username `ADMIN`, terminal info).
- O5LOGON challenge sent to client (96-byte sesskey + 20-byte vfrdata).

### What fails

Client returns **ORA-17401: Protocol violation. [ 0, ]** because dbbat closes the connection right after AUTH Phase 2.

dbbat's parse error: `AUTH Phase 2: missing AUTH_PASSWORD`.

Looking at the captured Phase 2 payload, the JDBC thin 23.7 driver shipped with SQLcl 26.1 sends:
- `AUTH_SESSKEY` — present, 96-byte hex value (the encrypted client session key).
- `AUTH_PASSWORD` — **key present, value is empty** (`01 0d 0d 'AUTH_PASSWORD' 00 00` on the wire).

This is a different wire shape from python-oracledb / go-ora / older JDBC drivers, all of which send a non-empty AUTH_PASSWORD.

### Root cause hypothesis

JDBC thin 23.7 appears to use a session-key-only verification mode (or expects the server to derive the password verifier from a different combined key). Our O5LOGON server implementation (`internal/proxy/oracle/ttc_auth.go`) requires `AUTH_PASSWORD` to be non-empty in order to recover the cleartext API key and authenticate the client — so SQLcl never gets past auth.

This is **separate** from the sqlplus 23c blocker (NS negotiation). Two distinct gaps for the two flagship Oracle CLI clients.

### Possible directions

1. **Implement OPRD / session-key-only verification path** — when AUTH_PASSWORD is empty, derive expected client-side proof from the session key and compare. Needs reverse-engineering against go-ora / Oracle docs. Probably the right long-term fix.
2. **MitM password re-encryption** — full upstream-passthrough of AUTH Phase 1+2 with dbbat injecting the real Oracle password in place of the API key. Heavier but covers both SQLcl and sqlplus, since dbbat would no longer rely on AUTH_PASSWORD being present client-side.
3. **Document SQLcl as not yet supported** alongside sqlplus 23c, recommend python-oracledb / go-ora / DBeaver in the meantime.

### Logs / artifacts

- Pod log line: `WARN msg="client authentication failed" error="failed to parse AUTH Phase 2: AUTH Phase 2: missing AUTH_PASSWORD"`.
- Full Phase 2 payload hex captured in dbbat pod logs (`kubectl --context aws/master -n tooling logs dbbat-...`).
- Test scripts on Windows host: `C:\Users\Florent\run_sqlcl.bat`, `sqlplus_test.sql`.

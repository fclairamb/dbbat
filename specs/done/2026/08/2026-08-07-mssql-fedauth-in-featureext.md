# SQL Server: reject federated auth requested through FEATUREEXT

## Goal

Refuse a LOGIN7 that asks for federated authentication (Azure AD / FEDAUTH)
through its `FEATUREEXT` block, with the same clear TDS error the proxy already
gives an integrated-security login — instead of relaying the request upstream
and letting the exchange fail somewhere less legible.

## Why

`Login7.Validate()` (in `internal/proxy/mssql/login7.go`) catches the two
*flag*-driven unsupported auth modes: `optionFlags2IntSecurity` (NTLM/Kerberos)
and a non-empty `SSPI` blob. It does not look inside the `FEATUREEXT` block,
which is where a client requests **FEDAUTH** (feature id `0x02`) — the Azure AD
path.

Stage 2 of the SQL Server proxy relays `FEATUREEXT` upstream verbatim, on
purpose: dbbat forwards the upstream's login response to the client untouched,
so the features negotiated upstream have to be the ones the client asked for
(see `buildUpstreamLogin` in `internal/proxy/mssql/upstream.go` and the
"upstream leg" section of `docs/mssql.md`). That is right for UTF-8 support,
session recovery and column encryption. It is wrong for FEDAUTH: dbbat cannot
mint or validate a federated token, so the client and the upstream would start
an exchange the proxy has no part in.

In practice this is hard to trigger — the proxy answers `FEDAUTHREQUIRED = 0` in
PRELOGIN, and every mainstream driver falls back to a SQL login on that answer
— which is why it was left as a follow-up rather than fixed in stage 2. It is
still a fail-open gap in an auth path, and dbbat's documented v1 position is
"SQL authentication only".

## Implementation

- Add a minimal FEATUREEXT walker next to `decodeFeatureExt` in
  `internal/proxy/mssql/login7.go`. The block is a run of
  `{feature id (1 byte), length (DWORD), data}` entries terminated by feature id
  `0xFF` — the same shape `scanFeatureExtAck` already walks in
  `internal/proxy/mssql/tokens.go`, so that function is the model.
- Expose `Login7.FederatedAuthRequested() bool`, true when the block carries
  feature id `0x02` (FEDAUTH).
- Extend `Login7.Validate()` to refuse it with an `ErrLogin7Unsupported` wrapping
  a message telling the user to connect with a SQL login, matching the wording
  of the integrated-security refusal.
- A malformed block should fail closed (refuse), not be skipped: a walker that
  gives up silently is exactly how a feature request would slip past.
- Tests in `internal/proxy/mssql/login7_test.go`: a login carrying FEDAUTH is
  refused; a login carrying an unrelated feature (UTF-8 support, `0x0A`) is
  accepted and its block still survives the re-serialize; a truncated block is
  refused.
- Update the "What is deliberately unsupported in v1" list in `docs/mssql.md` to
  say FEDAUTH is refused in LOGIN7 as well as declined in PRELOGIN.

No GitHub issue exists for this yet — one should be filed.

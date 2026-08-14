# Oracle: the AUTH-phase refusal's summary tail is guessed, so go-ora loses the message text

## Goal

A client refused at AUTH Phase 2 should read dbbat's actual sentence — "no
active grant for this database; request access via dbbat" — whatever driver it
is. go-ora reads only the code today: `ORA-1045 error occur at position: 0`.

## Why

`authRefusalOERShape` (`internal/proxy/oracle/session.go`) has to decide how many
trailing fields the summary object carries *before* anything has been learned from
the upstream, because a client refused at AUTH never gets that far. It guesses off
customHash: a modern client (the 12c/18453 challenge) gets the two "fields added
in Oracle Database 20c", a legacy one gets none.

Measured on 2026-08-14 against Oracle 23ai Free
(`TestIntegration_AuthRefusalAcrossClients`), that guess is wrong for exactly one
client: **go-ora negotiates customHash but parses like a legacy client** — its
summary reader takes the message CLR straight after the wide RetCode pair — so it
reads dbbat's first trailing field as an empty message and renders the code
itself. python-oracledb thin and sqlplus both get the full text.

The two readings are mutually exclusive at the byte level, so the tail was given
to the client that cannot parse the frame at all without it (python-oracledb
reported `DPY-5002` before this fix). That trade is only forced because the count
is *guessed*. It does not have to be: the upstream's own AUTH Phase 1 response
carries a summary shaped for this very client's forwarded capabilities, and
`learnOERTail` already reads it (`upstream_auth_client.go`, in
`readUpstreamAuthMessages`). An OCI session has it before it is challenged —
`beginUpstreamAuth` runs at step 4b — and a thin session does not, only because
dbbat defers upstream auth for thin clients to step 6.

## Implementation

- On the refusal path only, learn before writing: if `!s.oer.tailLearned` and
  `s.upstreamConn != nil` and `s.upstreamAuthResp == nil`, call
  `beginUpstreamAuth()` and ignore its error, then re-read the shape. The session
  is being torn down either way, so the extra round trip costs nothing a
  successful login pays, and `learnOERTail` does the rest.
- Keep the customHash guess as the fallback for when that fails (bad stored
  credentials, an unreachable upstream, `s.database` not yet resolved).
- Weigh one thing before doing it: this makes an *unauthenticated* client able to
  provoke an upstream AUTH Phase 1 for the schema user. dbbat already opens the
  upstream socket and relays Set Protocol before any client auth, and already
  runs this exact phase for every OCI client before authenticating them, so it is
  an increment rather than a new exposure — but it is the reason this was not done
  in the first pass.
- The measurement is already in place: the go-ora subtest of
  `TestIntegration_AuthRefusalAcrossClients` asserts the code and documents the
  missing text. Tighten it to the full text once the tail is learned, and update
  the table in `docs/oracle.md`, "The refusal that happens before the session:
  AUTH Phase 2".

package oracle

// Upstream Oracle authentication runs on the relay-phase socket using stored
// database credentials. The socket has already negotiated TNS Connect / Accept
// / Set Protocol / Set Data Types with the real upstream — keeping that exact
// socket through AUTH ensures the TTC capability levels stay aligned with the
// client's view, so caps-rich drivers (SQLcl JDBC thin) can send OALL8 messages
// that upstream parses correctly.
//
// The exchange is split across the client O5LOGON challenge: Phase 1 runs first
// (runUpstreamAuthPhase1) so its end-of-call marker seeds the client-facing
// challenge, then Phase 2 runs after the client authenticates
// (runUpstreamAuthPhase2). Both live in upstream_auth_client.go.

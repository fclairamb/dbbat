package store

// AuditEventAuthFailureCeiling is written when one username reaches the API's
// per-username login failure ceiling (failures summed over every source
// address within an hour), which blocks password login for that username from
// everywhere until the window ends. The per-source backoff is the everyday
// throttle and leaves no audit trace; this backstop against distributed
// guessing does, because unlike the backoff it affects the account's owner.
// UserID is set when the username names an existing account.
const AuditEventAuthFailureCeiling = "auth.failure_ceiling"

package store

// AuditEventAuthFailureCeiling is written when one username reaches the API's
// per-username login failure ceiling (failures summed over every source
// address within an hour). Until the window ends, or until a correct password
// lifts it, every wrong password for that username is answered 429 from
// every source. The per-source backoff is the everyday throttle and leaves no
// audit trace; this backstop against distributed guessing does, because it
// spans every source.
// UserID is set when the username names an existing account.
const AuditEventAuthFailureCeiling = "auth.failure_ceiling"

// AuditEventAuthSuccessDuringCeiling is written when a correct password is
// presented for a username past its login failure ceiling: the login goes
// through (the ceiling only refuses wrong passwords) and lifts the ceiling,
// and this event keeps the trace of the guessing burst it ended. UserID is set
// when the username names an existing account.
const AuditEventAuthSuccessDuringCeiling = "auth.success_during_failure_ceiling"

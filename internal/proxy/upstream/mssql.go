package upstream

import "errors"

// MSSQLConfig is everything a SQL Server (TDS) login needs from a server row.
type MSSQLConfig struct {
	// Host and Port name the target. The transport itself comes from the
	// injected DialFunc; the host is also the TLS server name under the
	// verify-* modes.
	Host string
	Port int
	// Username, Password and Database are the stored upstream credentials.
	Username string
	Password string
	Database string
	// AppName is the LOGIN7 AppName dbbat presents upstream, so a DBA reading
	// sys.dm_exec_sessions.program_name can tell who is connected.
	AppName string
	// SSLMode is the row's ssl_mode; interpreted by PlanFor.
	SSLMode string
}

// PRELOGIN ENCRYPTION values (MS-TDS 2.2.6.5), duplicated here from the
// protocol package so the ssl_mode → wire-value mapping can live next to the
// policy it belongs to rather than inside the connector.
const (
	// MSSQLEncryptOff — "I can encrypt but would rather not." The trap: TDS
	// still runs a TLS handshake in this mode and encrypts the login packet
	// only. dbbat never offers it upstream, because "encrypted" and
	// "not encrypted" are the only two outcomes an ssl_mode can express.
	MSSQLEncryptOff byte = 0x00
	// MSSQLEncryptOn — encrypt the whole session.
	MSSQLEncryptOn byte = 0x01
	// MSSQLEncryptNotSup — no TLS at all.
	MSSQLEncryptNotSup byte = 0x02
	// MSSQLEncryptReq — sent by a server that requires encryption.
	MSSQLEncryptReq byte = 0x03
)

// SQL Server upstream failures worth telling apart. They are declared here, not
// in the protocol package, because MSSQLRetryable — the decision of whether the
// ssl_mode plan may move on to its next attempt — matches on them.
var (
	// ErrMSSQLNoAttempt is returned when the attempt chain ended without
	// producing either a connection or an error. It cannot happen with a plan
	// from PlanFor (which always has at least one attempt) and exists so the
	// exhausted-loop path is not a silent nil.
	ErrMSSQLNoAttempt = errors.New("mssql: ssl_mode produced no connection attempt")
	// ErrMSSQLEncryptionMismatch — the server's PRELOGIN answer is incompatible
	// with the encryption this attempt offered.
	ErrMSSQLEncryptionMismatch = errors.New("mssql: upstream refused the offered transport encryption")
	// ErrMSSQLTLSHandshake — the encapsulated TLS handshake with the upstream
	// failed.
	ErrMSSQLTLSHandshake = errors.New("mssql: upstream TLS handshake failed")
	// ErrMSSQLLoginRejected — the upstream refused the stored credentials. It
	// deliberately ends the attempt chain: retrying a rejected password over a
	// different transport would be a downgrade triggered by the wrong signal.
	ErrMSSQLLoginRejected = errors.New("mssql: upstream rejected the login")
)

// MSSQLPlan resolves the ssl_mode plan for a SQL Server target. It is PlanFor
// and nothing else — the policy has exactly one implementation.
func MSSQLPlan(cfg MSSQLConfig) Plan {
	return PlanFor(cfg.SSLMode, cfg.Host)
}

// MSSQLEncryptionOption maps one attempt onto the PRELOGIN ENCRYPTION byte to
// offer for it.
//
// Only two of the four values are ever sent: ENCRYPT_ON for an encrypted
// attempt, ENCRYPT_NOT_SUP for a plaintext one. ENCRYPT_OFF is deliberately
// unused — it means "encrypt the login packet, then revert", which is neither
// of the two outcomes an ssl_mode distinguishes and would make upstream_tls a
// lie in one direction or the other.
func MSSQLEncryptionOption(a Attempt) byte {
	if a.Encrypted() {
		return MSSQLEncryptOn
	}

	return MSSQLEncryptNotSup
}

// MSSQLAnswerAcceptable reports whether a server's ENCRYPTION answer honors
// what the attempt asked for.
//
// It is strict on purpose. A plaintext attempt that gets anything but
// ENCRYPT_NOT_SUP means the server wants to encrypt something, and a session
// recorded as upstream_tls=false must not have any TLS in it; an encrypted
// attempt that gets ENCRYPT_OFF or ENCRYPT_NOT_SUP means the server would drop
// back to cleartext after the login packet, which `require` and the verify-*
// modes forbid. Under the opportunistic modes the mismatch is retryable, so
// being strict costs a redial rather than a connection.
func MSSQLAnswerAcceptable(a Attempt, serverValue byte) bool {
	if a.Encrypted() {
		return serverValue == MSSQLEncryptOn || serverValue == MSSQLEncryptReq
	}

	return serverValue == MSSQLEncryptNotSup
}

// MSSQLRetryable reports whether a failed attempt should be followed by the
// next one in the plan.
//
// Only a *transport* problem justifies changing the encryption of the retry:
// an encryption mismatch (the server insists on the opposite of what we
// offered) or a failed TLS handshake. A rejected login, an unreachable host or
// a malformed response ends the chain with that error, exactly as the MySQL and
// PostgreSQL fallback chains do.
func MSSQLRetryable(err error) bool {
	return errors.Is(err, ErrMSSQLEncryptionMismatch) || errors.Is(err, ErrMSSQLTLSHandshake)
}

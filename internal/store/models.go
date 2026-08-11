package store

import (
	"encoding/json"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// Role constants for user authorization
const (
	RoleAdmin     = "admin"
	RoleViewer    = "viewer"
	RoleConnector = "connector"
)

// Control constants for grant restrictions
const (
	ControlReadOnly  = "read_only"
	ControlBlockCopy = "block_copy"
	ControlBlockDDL  = "block_ddl"
)

// ValidControls lists all valid control values
var ValidControls = []string{
	ControlReadOnly,
	ControlBlockCopy,
	ControlBlockDDL,
}

// User represents a DBBat user
type User struct {
	bun.BaseModel `bun:"table:users,alias:u"`

	UID               uuid.UUID   `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	Username          string      `bun:"username,notnull,unique" json:"username"`
	PasswordHash      string      `bun:"password_hash,notnull" json:"-"`
	Roles             StringArray `bun:"roles" json:"roles"`
	RateLimitExempt   bool        `bun:"rate_limit_exempt,notnull,default:false" json:"rate_limit_exempt"`
	PasswordChangedAt *time.Time  `bun:"password_changed_at" json:"-"`
	CreatedAt         time.Time   `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt         time.Time   `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
	DeletedAt         *time.Time  `bun:"deleted_at,soft_delete" json:"-"`
	// ProtocolData holds protocol-specific per-user material (Oracle O5LOGON
	// user salts, etc.) in a single generic jsonb column — mirroring
	// APIKey.ProtocolData — rather than protocol-specific user columns.
	// nil until first needed (populated lazily at API key creation).
	ProtocolData *UserProtocolData `bun:"protocol_data,type:jsonb,nullzero" json:"-"`
}

// UserProtocolData is the per-protocol material attached to a user, stored as
// a single jsonb column so protocol-specific fields don't proliferate as table
// columns. Absent protocols are omitted.
type UserProtocolData struct {
	Oracle  *OracleUserData `json:"oracle,omitempty"`
	MongoDB *MongoUserData  `json:"mongodb,omitempty"`
}

// MongoUserData holds the per-user MongoDB SCRAM verifier material, letting a
// client authenticate to the proxy with the driver-default SCRAM-SHA-256 (which
// keeps the cleartext password off the wire) instead of being forced onto
// authMechanism=PLAIN. Populated lazily whenever the user's password is set
// after this feature shipped; absent otherwise (PLAIN stays the fallback).
type MongoUserData struct {
	SCRAMSHA256 *MongoSCRAMCredentials `json:"scram_sha256,omitempty"`
}

// MongoSCRAMCredentials are the SCRAM-SHA-256 stored credentials derived from
// the user's password (RFC 5802 / RFC 7677). Salt and Iterations are public
// challenge material; StoredKey and ServerKey are password-equivalent secrets
// and are encrypted at rest with the dbbat master key (AAD-bound to the user
// UID), mirroring the encrypted Oracle O5LOGON verifiers.
type MongoSCRAMCredentials struct {
	Salt       []byte `json:"salt,omitempty"`
	Iterations int    `json:"iterations,omitempty"`
	StoredKey  []byte `json:"stored_key,omitempty"`
	ServerKey  []byte `json:"server_key,omitempty"`
}

// OracleUserData holds the per-USER O5LOGON salts. Every API key created for
// the user derives its O5LOGON verifiers from these shared salts (instead of
// per-key random salts), so the Oracle proxy can commit to one salt in the
// AUTH challenge while keeping ALL of the user's keys as login candidates.
// Salts are public challenge material (sent to any connecting client), so they
// are stored unencrypted, like the per-key salts.
type OracleUserData struct {
	O5LogonUserSalt6949  []byte `json:"o5logon_user_salt_6949,omitempty"`
	O5LogonUserSalt18453 []byte `json:"o5logon_user_salt_18453,omitempty"`
}

// OracleData returns the user's Oracle protocol material, or nil if absent.
func (u *User) OracleData() *OracleUserData {
	if u.ProtocolData == nil {
		return nil
	}

	return u.ProtocolData.Oracle
}

// MongoData returns the user's MongoDB protocol material, or nil if absent.
func (u *User) MongoData() *MongoUserData {
	if u.ProtocolData == nil {
		return nil
	}

	return u.ProtocolData.MongoDB
}

// MongoSCRAMCredentials returns the user's stored MongoDB SCRAM-SHA-256
// credentials, or nil when the user has no stored verifier (so the MongoDB
// proxy falls back to PLAIN for them).
func (u *User) MongoSCRAMCredentials() *MongoSCRAMCredentials {
	data := u.MongoData()
	if data == nil {
		return nil
	}

	return data.SCRAMSHA256
}

// HasChangedPassword returns true if the user has changed their initial password
func (u *User) HasChangedPassword() bool {
	return u.PasswordChangedAt != nil
}

// HasRole checks if the user has a specific role
func (u *User) HasRole(role string) bool {
	for _, r := range u.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// IsAdmin returns true if the user has the admin role
func (u *User) IsAdmin() bool {
	return u.HasRole(RoleAdmin)
}

// IsViewer returns true if the user has the viewer role
func (u *User) IsViewer() bool {
	return u.HasRole(RoleViewer)
}

// IsConnector returns true if the user has the connector role
func (u *User) IsConnector() bool {
	return u.HasRole(RoleConnector)
}

// UserUpdate represents fields that can be updated
type UserUpdate struct {
	PasswordHash *string
	Roles        StringArray
}

// Protocol constants for database connections
const (
	ProtocolPostgreSQL = "postgresql"
	ProtocolOracle     = "oracle"
	ProtocolMySQL      = "mysql"
	ProtocolMariaDB    = "mariadb"
	ProtocolMongoDB    = "mongodb"
	// ProtocolMSSQL is Microsoft SQL Server (TDS). The protocol column is a
	// plain TEXT column with no CHECK constraint, so adding a value needs no
	// migration.
	ProtocolMSSQL = "mssql"
	// ProtocolSSH marks a row that is an SSH bastion (a dial path), not a
	// grantable/connectable database target.
	ProtocolSSH = "ssh"
	// ProtocolKubernetes marks a row that is a Kubernetes cluster (a dial path
	// via `pods/portforward`), not a grantable/connectable database target.
	// Like ProtocolSSH it is a tunnel discriminator, not a wire protocol.
	ProtocolKubernetes = "kubernetes"
)

// IsTunnelProtocol reports whether a protocol names a *dial path* rather than a
// database target. Tunnel rows are never grantable, never listable, and never
// appear in target listings; they exist only to be pointed at by a via_uid.
func IsTunnelProtocol(protocol string) bool {
	return protocol == ProtocolSSH || protocol == ProtocolKubernetes
}

// IsMySQLFamily reports whether the given protocol speaks the MySQL wire
// protocol. The MySQL proxy serves both — they share the same listener,
// auth plugins, and wire-protocol handling. The distinction matters mostly
// for upstream connection setup (server version banner, auth plugin
// negotiation) and for UI labeling.
func IsMySQLFamily(protocol string) bool {
	return protocol == ProtocolMySQL || protocol == ProtocolMariaDB
}

// Server represents a target dbbat knows how to reach: a database target
// (protocol postgresql|oracle|mysql|mariadb|mongodb|mssql) or an SSH bastion
// (protocol ssh). Both share the same storage shape — host, port, username,
// encrypted secret — with the protocol column as discriminator. ViaUID, when
// set, points at an SSH server row: "dial this server through that bastion".
type Server struct {
	bun.BaseModel `bun:"table:servers,alias:d"`

	UID         uuid.UUID `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	Name        string    `bun:"name,notnull,unique" json:"name"`
	Description string    `bun:"description" json:"description"`
	Host        string    `bun:"host,notnull" json:"host"`
	Port        int       `bun:"port,notnull" json:"port"`
	// DatabaseName is the target database name; nullable/empty for SSH bastions.
	DatabaseName      string `bun:"database_name" json:"database_name"`
	Username          string `bun:"username,notnull" json:"username"`
	Password          string `bun:"-" json:"-"`                          // Decrypted, not stored
	PasswordEncrypted []byte `bun:"password_encrypted,notnull" json:"-"` // Encrypted form
	// SSLMode is meaningful for database targets only; nullable for SSH bastions.
	SSLMode           string  `bun:"ssl_mode" json:"ssl_mode"`
	Protocol          string  `bun:"protocol,notnull,default:'postgresql'" json:"protocol"`
	OracleServiceName *string `bun:"oracle_service_name" json:"oracle_service_name,omitempty"`
	// ViaUID references an SSH server row to tunnel through; nil = direct dial.
	ViaUID *uuid.UUID `bun:"via_uid,type:uuid" json:"via_uid,omitempty"`
	// ProtocolData holds protocol-specific per-server settings (MongoDB upstream
	// authSource, SSH key material, etc.) in a single generic jsonb column —
	// mirroring User.ProtocolData — rather than a dedicated column per setting.
	ProtocolData *ServerProtocolData `bun:"protocol_data,type:jsonb,nullzero" json:"-"`
	Listable     bool                `bun:"listable,notnull" json:"listable"`
	CreatedBy    *uuid.UUID          `bun:"created_by,type:uuid" json:"created_by"`
	CreatedAt    time.Time           `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt    time.Time           `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
	DeletedAt    *time.Time          `bun:"deleted_at,soft_delete" json:"-"`
}

// ServerProtocolData is per-protocol material attached to a server, stored
// as a single jsonb column so protocol-specific settings don't proliferate as
// table columns — mirrors UserProtocolData. Absent protocols are omitted.
type ServerProtocolData struct {
	MongoDB    *MongoDatabaseData    `json:"mongodb,omitempty"`
	SSH        *SSHServerData        `json:"ssh,omitempty"`
	Kubernetes *KubernetesServerData `json:"kubernetes,omitempty"`
}

// MongoDatabaseData holds MongoDB-specific per-database settings.
type MongoDatabaseData struct {
	// AuthSource is the upstream SCRAM authSource; empty defers to
	// MongoAuthSourceOrDefault's "admin" default.
	AuthSource string `json:"auth_source,omitempty"`
}

// SSHServerData holds the material for an SSH bastion row. The private key and
// passphrase are password-equivalent secrets, encrypted at rest with the dbbat
// master key (AAD-bound to the server UID), mirroring the encrypted database
// password. KnownHostKey is the bastion's public host key, learned on first
// connect (TOFU) and verified on every subsequent connect; it is public
// challenge material, stored in clear and surfaced read-only in the API/UI.
type SSHServerData struct {
	PrivateKeyEncrypted []byte `json:"private_key_encrypted,omitempty"`
	PassphraseEncrypted []byte `json:"passphrase_encrypted,omitempty"`
	KnownHostKey        string `json:"known_host_key,omitempty"`
	// PrivateKey / Passphrase are the decrypted, in-memory-only forms (never
	// serialized to jsonb — omitted via the encrypted round-trip helpers).
	PrivateKey string `json:"-"`
	Passphrase string `json:"-"`
}

// KubernetesServerData holds the material for a Kubernetes cluster row. The
// ServiceAccount bearer token is *not* here: it reuses the row's
// password_encrypted column, so it travels the same AAD-bound encryption path
// as a database password or an SSH private key.
//
// CACert is the API server's PEM CA bundle — public challenge material, stored
// in clear and surfaced read-only in the API/UI, exactly like SSH's
// KnownHostKey. Namespace scopes every lookup and every port-forward: it is the
// namespace the Role/RoleBinding in docs/kubernetes.md grants access to.
type KubernetesServerData struct {
	CACert string `json:"ca_cert,omitempty"`
	// LearnedCACert is the bundle dbbat pinned itself, on the first connect of
	// a row that supplied none (TOFU) — deliberately a *separate* field from
	// CACert so "I pasted this" and "we learned this" never blur into each
	// other, and the UI can say which of the two is in force.
	//
	// It is only ever consulted when CACert is empty: an operator-supplied
	// bundle always wins, and pasting one is what retires a stale learned pin.
	LearnedCACert string `json:"learned_ca_cert,omitempty"`
	Namespace     string `json:"namespace,omitempty"`
	// InsecureSkipTLSVerify disables API server certificate verification. It
	// exists for the throwaway-cluster case (a kind cluster with a rotating CA)
	// and is deliberately not the default: with it set, anything that can
	// intercept the API server connection can read the ServiceAccount token.
	InsecureSkipTLSVerify bool `json:"insecure_skip_tls_verify,omitempty"`
}

// MongoData returns the server's MongoDB protocol material, or nil if absent.
func (db *Server) MongoData() *MongoDatabaseData {
	if db.ProtocolData == nil {
		return nil
	}

	return db.ProtocolData.MongoDB
}

// SSHData returns the server's SSH protocol material, or nil if absent.
func (db *Server) SSHData() *SSHServerData {
	if db.ProtocolData == nil {
		return nil
	}

	return db.ProtocolData.SSH
}

// KubernetesData returns the server's Kubernetes material, or nil if absent.
func (db *Server) KubernetesData() *KubernetesServerData {
	if db.ProtocolData == nil {
		return nil
	}

	return db.ProtocolData.Kubernetes
}

// IsSSH reports whether this server row is an SSH bastion rather than a
// database target.
func (db *Server) IsSSH() bool {
	return db.Protocol == ProtocolSSH
}

// IsKubernetes reports whether this server row is a Kubernetes cluster tunnel
// rather than a database target.
func (db *Server) IsKubernetes() bool {
	return db.Protocol == ProtocolKubernetes
}

// IsTunnel reports whether this row is a dial path (SSH bastion or Kubernetes
// cluster) rather than a grantable database target.
func (db *Server) IsTunnel() bool {
	return IsTunnelProtocol(db.Protocol)
}

// KubernetesNamespaceOrDefault returns the namespace every lookup and
// port-forward for this cluster row is scoped to, defaulting to "default" when
// unset — the same convention kubectl applies to a context with no namespace.
func (db *Server) KubernetesNamespaceOrDefault() string {
	if kd := db.KubernetesData(); kd != nil && kd.Namespace != "" {
		return kd.Namespace
	}

	return "default"
}

// ServerUpdate represents fields that can be updated
type ServerUpdate struct {
	Description       *string
	Host              *string
	Port              *int
	DatabaseName      *string
	Username          *string
	Password          *string // Plaintext password to encrypt
	SSLMode           *string
	Protocol          *string
	OracleServiceName *string
	MongoAuthSource   *string
	Listable          *bool
	ViaUID            *uuid.UUID // Set to tunnel through an SSH server
	ClearViaUID       bool       // When true, clears via_uid (direct dial)
	// SSH secrets (plaintext, to encrypt). Set on SSH server rows.
	SSHPrivateKey *string
	SSHPassphrase *string
	// Kubernetes cluster material. Public (the CA bundle and the namespace),
	// so unlike the ServiceAccount token — which travels as Password — these
	// are stored in clear in protocol_data.kubernetes.
	K8sCACert                *string
	K8sNamespace             *string
	K8sInsecureSkipTLSVerify *bool
	// K8sClearLearnedCACert, when true, forgets the TOFU-learned bundle so the
	// next connect learns afresh. It is the exit for a cluster whose CA
	// rotated — the case that made TOFU worth having at all.
	K8sClearLearnedCACert bool
}

// Connection represents a connection through the proxy
type Connection struct {
	bun.BaseModel `bun:"table:connections,alias:c"`

	UID              uuid.UUID  `bun:"uid,pk,type:uuid" json:"uid"` // UUIDv7 set in Go
	UserID           uuid.UUID  `bun:"user_id,notnull,type:uuid" json:"user_id"`
	DatabaseID       uuid.UUID  `bun:"database_id,notnull,type:uuid" json:"database_id"`
	SourceIP         string     `bun:"source_ip,notnull,type:inet" json:"source_ip"`
	ConnectedAt      time.Time  `bun:"connected_at,notnull,default:current_timestamp" json:"connected_at"`
	LastActivityAt   time.Time  `bun:"last_activity_at,notnull,default:current_timestamp" json:"last_activity_at"`
	DisconnectedAt   *time.Time `bun:"disconnected_at" json:"disconnected_at"`
	Queries          int64      `bun:"queries,notnull,default:0" json:"queries"`
	BytesTransferred int64      `bun:"bytes_transferred,notnull,default:0" json:"bytes_transferred"`

	// UpstreamTLS reports whether the proxy→upstream leg of this session was
	// encrypted. The server row's ssl_mode states a policy, not an outcome:
	// the opportunistic modes ("prefer", and the empty default) fall back to
	// plaintext when the target refuses TLS, so only the session knows which
	// way it went. Recording it is what makes that fallback auditable rather
	// than silent.
	UpstreamTLS bool `bun:"upstream_tls,notnull,default:false" json:"upstream_tls"`

	// InstanceID is the dbbat process that opened this connection. It scopes
	// the startup reconcile (Store.CloseOrphanedConnections) so a replica can
	// never close another replica's live sessions. Internal bookkeeping, not
	// part of the API surface — hence json:"-".
	InstanceID string `bun:"instance_id,notnull,default:''" json:"-"`

	// RunID is the *run* that opened this connection: a UUID minted in memory
	// at startup, so it is unique per live process even when several replicas
	// share an instance id. It is what lets the reconcile ask whether a live
	// run still owns a row, rather than trusting that an id means a process.
	//
	// A nil pointer means the row was written before run tracking existed —
	// deliberately distinct from a run whose id is empty. See noLiveOwner for
	// how those rows are judged.
	RunID *string `bun:"run_id" json:"-"`

	// DumpKey is the blob-storage object key of this session's capture, once
	// it has been uploaded (DBB_DUMP_UPLOAD_URL). Empty means the capture — if
	// there is one — is still in the local spool, which is also the permanent
	// state when uploads are disabled.
	//
	// It is stored rather than derived because the API addresses captures by
	// connection UID alone and cannot otherwise tell which instance wrote the
	// object; deriving it would turn every download into a bucket LIST.
	// Internal bookkeeping, not API surface — hence json:"-": the key exposes
	// the bucket layout and callers already have the download endpoint.
	DumpKey string `bun:"dump_key,notnull,default:''" json:"-"`

	// GrantUID is the access grant this session authenticated under — the
	// auth-time GetActiveGrant (or equivalent per-protocol) pick, stamped at
	// CreateConnection and never updated afterwards. A connection is bound to
	// exactly one grant for its whole life: the LimitGuard watchdog already
	// terminates the session when that grant expires or is revoked
	// (internal/proxy/shared/limits.go), so there is never a live row that
	// should be re-pointed at a different grant.
	//
	// nil covers two cases that are indistinguishable from here: the row
	// predates this column, or the owning grant has since been deleted.
	// Either way, consumers (mayApproveQuery, populateGrantCounters) fall back
	// to their pre-stamp heuristics.
	GrantUID *uuid.UUID `bun:"grant_uid,type:uuid" json:"grant_uid"`

	// QueryChainMAC is the head of this connection's query chain, and
	// QueryChainLen is the position that head sits at. Without them, deleting
	// the *last* queries of a connection would leave a chain that still
	// verified; with them the stored head no longer matches what the surviving
	// rows compute.
	//
	// Three writers, all sealing with the key: CloseConnection at a clean
	// teardown, the reconcile for a session whose process died, and
	// RefreshOpenChainStamps on the reclaim timer for a session that is still
	// open. The last is why the stamp is not necessarily *final*: a live
	// session's stamp is a prefix of its chain, re-sealed one sweep at a time,
	// and checkStampedHead judges an open session accordingly.
	//
	// nil/0 on a connection that logged nothing, or one no sweep has reached
	// yet. Internal integrity state, not API surface.
	QueryChainMAC []byte `bun:"query_chain_mac" json:"-"`

	// QueryChainLen is the head's chain_seq — which, chain_seq being dense from
	// 1, is the number of statements the session logged. Deliberately not a
	// count of *surviving* statements: DBB_QUERY_STORAGE_RETENTION reaps the
	// oldest statements of a long-lived session, and a stamp that moved with
	// them would report a break on every truncated session.
	QueryChainLen int64 `bun:"query_chain_len,notnull,default:0" json:"-"`

	// QueryChainStampVersion says which format QueryChainMAC is in:
	// queryChainStampLegacy (a verbatim copy of the head MAC, written by
	// 0.23.x, forgeable without the key) or queryChainStampKeyed. Every writer
	// produces the keyed format now; pre-upgrade rows keep the legacy one
	// forever, since no migration can re-seal them without the chain key.
	QueryChainStampVersion int16 `bun:"query_chain_stamp_version,notnull,default:0" json:"-"`
}

// Instance is one *run* of one dbbat process sharing this store. The row is
// upserted at startup, refreshed by a heartbeat, and deleted on a clean
// shutdown, so its presence and freshness answer "is this run still alive?" —
// which is what lets the startup reconcile reclaim the connections of a run
// that is provably gone. See Store.CloseOrphanedConnections.
//
// The key is the pair, not the instance id: two live replicas that share an
// instance id must both be able to prove they are alive, or the second one to
// register would silently make the first look dead. A row whose run id is ”
// predates run tracking (seeded by a migration, or written by an older build).
type Instance struct {
	bun.BaseModel `bun:"table:instances,alias:i"`

	InstanceID string    `bun:"instance_id,pk" json:"instance_id"`
	RunID      string    `bun:"run_id,pk,notnull,default:''" json:"run_id"`
	StartedAt  time.Time `bun:"started_at,notnull,default:current_timestamp" json:"started_at"`
	LastSeenAt time.Time `bun:"last_seen_at,notnull,default:current_timestamp" json:"last_seen_at"`
}

// ConnectionFilter represents filters for listing connections
type ConnectionFilter struct {
	UserID     *uuid.UUID
	DatabaseID *uuid.UUID
	BeforeUID  *uuid.UUID // Cursor: return connections with UID < this value
	Limit      int
	Offset     int
}

// QueryParameters stores parameter values for prepared statements
type QueryParameters struct {
	Values      []string `json:"values"`                 // Decoded string representation
	Raw         []string `json:"raw,omitempty"`          // Base64-encoded raw bytes
	FormatCodes []int16  `json:"format_codes,omitempty"` // 0=text, 1=binary
	TypeOIDs    []uint32 `json:"type_oids,omitempty"`    // PostgreSQL type OIDs
}

// Query represents a query execution record
type Query struct {
	bun.BaseModel `bun:"table:queries,alias:q"`

	UID           uuid.UUID        `bun:"uid,pk,type:uuid" json:"uid"` // UUIDv7 set in Go
	ConnectionID  uuid.UUID        `bun:"connection_id,notnull,type:uuid" json:"connection_id"`
	SQLText       string           `bun:"sql_text,notnull" json:"sql_text"`
	Parameters    *QueryParameters `bun:"parameters,type:jsonb" json:"parameters,omitempty"`
	ExecutedAt    time.Time        `bun:"executed_at,notnull,default:current_timestamp" json:"executed_at"`
	DurationMs    *float64         `bun:"duration_ms,type:numeric(10,3)" json:"duration_ms"`
	RowsAffected  *int64           `bun:"rows_affected" json:"rows_affected"`
	Error         *string          `bun:"error" json:"error"`
	CopyFormat    *string          `bun:"copy_format" json:"copy_format,omitempty"`       // 'text', 'csv', 'binary', or nil for non-COPY
	CopyDirection *string          `bun:"copy_direction" json:"copy_direction,omitempty"` // 'in', 'out', or nil for non-COPY

	// ResultsTruncated is true when result capture stopped on a storage limit
	// (max_result_rows / max_result_bytes). The rows that were captured before
	// the limit are still stored, so this is what tells a short — or empty —
	// row set apart from a query that genuinely returned that much.
	ResultsTruncated bool `bun:"results_truncated,notnull,default:false" json:"results_truncated"`

	// ResultsDropped is true when dbbat lost rows it meant to keep: the
	// batched row writer's queue was full (the store fell behind the proxy) or
	// a batch insert failed. It is deliberately distinct from
	// ResultsTruncated — truncation is an expected, configured prefix, a drop
	// is dbbat failing to keep up — and the two are never conflated.
	ResultsDropped bool `bun:"results_dropped,notnull,default:false" json:"results_dropped"`

	// Approval hold fields. ApprovalStatus is nil for the overwhelming
	// majority of queries (no pattern matched); when set it is one of
	// ApprovalPending / ApprovalApproved / ApprovalDenied / ApprovalAbandoned.
	// There is deliberately no "timeout" state — see docs/approvals.md.
	ApprovalStatus   *string    `bun:"approval_status" json:"approval_status,omitempty"`
	ApprovalPattern  *string    `bun:"approval_pattern" json:"approval_pattern,omitempty"`
	ResolvedBy       *uuid.UUID `bun:"resolved_by,type:uuid" json:"resolved_by,omitempty"`
	ResolvedAt       *time.Time `bun:"resolved_at" json:"resolved_at,omitempty"`
	ResolutionReason *string    `bun:"resolution_reason" json:"resolution_reason,omitempty"`

	// Tamper-evidence: this statement's position in its *connection's* chain,
	// and the HMAC sealing its immutable identity (uid, connection, position,
	// SQL text, parameters, executed_at) plus PrevMAC. The outcome columns
	// above are written after the insert and are deliberately not covered —
	// see queryChainPayload. Internal integrity state, not API surface.
	ChainSeq *int64 `bun:"chain_seq" json:"-"`
	PrevMAC  []byte `bun:"prev_mac" json:"-"`
	MAC      []byte `bun:"mac" json:"-"`

	// RowChainMAC seals the final head of this query's *result row* chain when
	// the capture finishes, and RowChainLen is how many captured rows that head
	// covers. They are to query_rows what QueryChainMAC / QueryChainLen are to
	// a connection's statements: the only thing that catches rows deleted from
	// the *end* of a capture.
	//
	// It is a keyed MAC over (query, length, head MAC) — deliberately *not* a
	// copy of the head MAC, which is readable from query_rows and could
	// therefore be rewritten to match a truncated capture without the chain
	// key. See rowChainStampMAC.
	//
	// RowChainLen is a count, not the head's row_number — a capture with gaps
	// (rows the writer dropped, rows that failed to encode) has fewer stored
	// rows than its last row_number.
	//
	// nil/0 for a query that captured nothing, for captures written before the
	// row chain migration, and for a capture whose process died before the
	// flush barrier. Internal integrity state, not API surface.
	RowChainMAC []byte `bun:"row_chain_mac" json:"-"`
	RowChainLen int64  `bun:"row_chain_len,notnull,default:0" json:"-"`

	// Joined fields populated only by ListQueries (via a JOIN on connections);
	// not stored on the queries table itself.
	UserID     *uuid.UUID `bun:"user_id,scanonly" json:"user_id,omitempty"`
	DatabaseID *uuid.UUID `bun:"database_id,scanonly" json:"database_id,omitempty"`
}

// QueryRowModel represents a single row from query results or COPY data
type QueryRowModel struct {
	bun.BaseModel `bun:"table:query_rows,alias:qr"`

	UID          uuid.UUID       `bun:"uid,pk,type:uuid" json:"uid"` // UUIDv7 set in Go
	QueryID      uuid.UUID       `bun:"query_id,notnull,type:uuid" json:"query_id"`
	RowNumber    int             `bun:"row_number,notnull" json:"row_number"`
	RowData      json.RawMessage `bun:"row_data,notnull,type:jsonb" json:"row_data"`
	RowSizeBytes int64           `bun:"row_size_bytes,notnull" json:"row_size_bytes"`

	// Tamper-evidence: the HMAC sealing this captured row (its query, its
	// row_number, its uid, its data and its size) plus the previous stored
	// row's MAC. The chain is per query and its position is row_number, which
	// is an ordering and not a dense sequence — see rowChainPayload. Internal
	// integrity state, not API surface.
	PrevMAC []byte `bun:"prev_mac" json:"-"`
	MAC     []byte `bun:"mac" json:"-"`
}

// QueryRow is an alias for API compatibility (without bun.BaseModel for simpler usage)
type QueryRow struct {
	RowNumber    int             `json:"row_number"`
	RowData      json.RawMessage `json:"row_data"`
	RowSizeBytes int64           `json:"row_size_bytes"`
}

// PendingQueryRow is a captured row on its way to storage. Unlike QueryRow it
// carries its own parent query id, so a single INSERT can cover rows belonging
// to different queries — which is what lets one process-wide writer batch
// across concurrent sessions instead of one batch per query.
type PendingQueryRow struct {
	QueryID      uuid.UUID
	RowNumber    int
	RowData      json.RawMessage
	RowSizeBytes int64
}

// QueryWithRows combines a query with its result rows
type QueryWithRows struct {
	Query
	Rows []QueryRow `json:"rows"`
}

// QueryFilter represents filters for listing queries
type QueryFilter struct {
	ConnectionID *uuid.UUID
	UserID       *uuid.UUID
	DatabaseID   *uuid.UUID
	StartTime    *time.Time
	EndTime      *time.Time
	BeforeUID    *uuid.UUID // Cursor: return queries with UID < this value (for stable pagination)
	Limit        int
	Offset       int
}

// AccessGrant is one *instance* of a GrantDefinition: a named user's
// time-boxed access to one database, issued from a definition that describes
// what that access may do.
//
// It deliberately carries no behavioral shape of its own. Controls, quotas,
// approval patterns and approver groups all live on GrantDefinitionID's row
// and are read through the accessors below. The grant row holds only
// instance-lifecycle data — who, where, the window, revocation, usage — plus
// Priority, which ranks this instance against the user's other instances and
// is not policy.
type AccessGrant struct {
	bun.BaseModel `bun:"table:access_grants,alias:ag"`

	UID    uuid.UUID `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	UserID uuid.UUID `bun:"user_id,notnull,type:uuid" json:"user_id"`
	// DatabaseID is the grant's anchor: the database it was issued for. It is
	// what an unbound grant covers, and what a group-bound one falls back to
	// if its group is deleted outright.
	DatabaseID uuid.UUID `bun:"database_id,notnull,type:uuid" json:"database_id"`
	// ServerGroupUID, when set, binds this grant to a server group: the grant
	// then covers every server the group contains **right now**, and only
	// those — the binding replaces the single-database scope rather than
	// adding to it, so a server removed from the group stops being covered
	// even when it is the anchor. "Adding widens, removing narrows", with no
	// exceptions to remember.
	//
	// Membership is live and never snapshotted, so an edit takes effect the
	// instant it is saved — the one deliberate exception to "a live grant's
	// behavior never changes under it" (see ServerGroup). nil = anchor
	// database only, which is what every grant issued from an unscoped
	// definition gets.
	//
	// Quotas and Priority follow the grant, not the database: one
	// max_query_counts and one max_bytes_transferred budget are consumed
	// across the whole group, and Priority ranks group-bound grants against
	// each other on the databases where their groups overlap.
	ServerGroupUID *uuid.UUID `bun:"server_group_uid,type:uuid" json:"server_group_uid,omitempty"`
	// GrantDefinitionID pins the exact definition *version* this grant was
	// issued from. Definitions are immutably versioned (an edit archives the
	// row and inserts a successor), so this reference can never make a live
	// grant's behavior change underneath it.
	GrantDefinitionID uuid.UUID  `bun:"grant_definition_id,notnull,type:uuid" json:"grant_definition_id"`
	GrantedBy         uuid.UUID  `bun:"granted_by,notnull,type:uuid" json:"granted_by"`
	StartsAt          time.Time  `bun:"starts_at,notnull" json:"starts_at"`
	ExpiresAt         time.Time  `bun:"expires_at,notnull" json:"expires_at"`
	RevokedAt         *time.Time `bun:"revoked_at" json:"revoked_at"`
	RevokedBy         *uuid.UUID `bun:"revoked_by,type:uuid" json:"revoked_by"`
	CreatedAt         time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`

	// Priority ranks this grant against the other active grants the same user
	// holds on the same database: the highest priority wins at auth time (ties
	// broken by latest expiry, then newest). Auto-calculated from the
	// definition's controls by AutoPriority unless an admin set it explicitly,
	// which is why it is a plain int16 rather than a pointer — by the time a
	// grant row exists the value is always resolved.
	Priority int16 `bun:"priority,notnull,default:0" json:"priority"`

	// Definition is the grant's shape, attached by the store on every read
	// path (see Store.attachDefinitions). It is not a bun relation: the
	// attachment is one batched query, which keeps the auth path free of ORM
	// join semantics.
	Definition *GrantDefinition `bun:"-" json:"definition,omitempty"`

	// Computed fields (not stored in DB)
	QueryCount       int64 `bun:"-" json:"query_count"`
	BytesTransferred int64 `bun:"-" json:"bytes_transferred"`
}

// Controls returns the controls this grant enforces, read from its definition.
//
// A grant with no definition attached is treated as carrying *every* control:
// the shape is unknown, so the answer that cannot widen access is "everything
// is restricted". Combined with MaxQueryCounts/MaxBytesTransferred returning a
// zero quota in the same situation, a shapeless grant authorizes nothing at
// all. That is a backstop, not a mode: GetActiveGrant refuses to hand out a
// grant whose definition it could not attach.
func (g *AccessGrant) Controls() []string {
	if g == nil || g.Definition == nil {
		return append([]string(nil), ValidControls...)
	}

	return g.Definition.Controls
}

// HasControl checks if the grant has a specific control enabled
func (g *AccessGrant) HasControl(control string) bool {
	for _, c := range g.Controls() {
		if c == control {
			return true
		}
	}
	return false
}

// MaxQueryCounts is the grant's query quota, read from its definition. nil
// means unlimited; a shapeless grant reports a zero quota (nothing allowed)
// rather than nil, so it fails closed. See Controls.
func (g *AccessGrant) MaxQueryCounts() *int64 {
	if g == nil || g.Definition == nil {
		return newZeroQuota()
	}

	return g.Definition.MaxQueryCounts
}

// MaxBytesTransferred is the grant's transfer quota, read from its definition.
// Same nil/zero semantics as MaxQueryCounts.
func (g *AccessGrant) MaxBytesTransferred() *int64 {
	if g == nil || g.Definition == nil {
		return newZeroQuota()
	}

	return g.Definition.MaxBytesTransferred
}

// ApprovalPatterns are the RE2 patterns that suspend a matching statement
// until an approver resolves it, read from the grant's definition.
func (g *AccessGrant) ApprovalPatterns() []string {
	if g == nil || g.Definition == nil {
		return nil
	}

	return g.Definition.ApprovalPatterns
}

// ApproverUserGroupUIDs lists the groups whose members may resolve holds on this
// grant, in addition to admins, read from the grant's definition. Empty =
// admins only, which is also what a shapeless grant reports: the narrowest
// possible approver set.
func (g *AccessGrant) ApproverUserGroupUIDs() []uuid.UUID {
	if g == nil || g.Definition == nil {
		return nil
	}

	return g.Definition.ApproverUserGroupUIDs
}

// newZeroQuota backs the fail-closed quota accessors: an exhausted quota,
// freshly allocated so no caller can write through the pointer and poison a
// shared value.
func newZeroQuota() *int64 {
	q := int64(0)

	return &q
}

// IsReadOnly returns true if the grant has read_only control
func (g *AccessGrant) IsReadOnly() bool {
	return g.HasControl(ControlReadOnly)
}

// ShouldBlockCopy returns true if COPY commands should be blocked
func (g *AccessGrant) ShouldBlockCopy() bool {
	return g.HasControl(ControlBlockCopy)
}

// ShouldBlockDDL returns true if DDL commands should be blocked
func (g *AccessGrant) ShouldBlockDDL() bool {
	return g.HasControl(ControlBlockDDL)
}

// Grant is an alias for backward compatibility
type Grant = AccessGrant

// GrantDefinition is the *shape* of a grant: name, duration, controls,
// optional quotas, approval gating. It is the single source of truth for what
// a grant may do — every AccessGrant is an instance of one, whether it came
// from a user's grant request or from an admin assigning it directly. Nothing
// creates a grant without a definition.
//
// Definitions are immutably versioned. Editing one archives the current row
// (ArchivedAt) and inserts a successor sharing its LineageUID, so grants keep
// pointing at the exact row they were issued from and an edit never
// retroactively changes live access. Two lifecycle states must not be
// confused:
//
//   - **Archived** (ArchivedAt != nil): superseded by a newer version. Still
//     authorizes the grants pinned to it — it was replaced, not withdrawn.
//   - **Deactivated** (IsActive == false): explicitly withdrawn by an
//     operator, across the whole lineage. Fails closed at auth time: grants
//     pinned to any version of it stop authorizing new connections.
type GrantDefinition struct {
	bun.BaseModel `bun:"table:grant_definitions,alias:gd"`

	UID uuid.UUID `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	// LineageUID is stable across every version of a definition (the root
	// version's own uid). It is what deactivation, hard deletion and
	// "which versions are this definition?" act on. The slug cannot serve
	// that purpose: it is unique only among live rows, so a new definition
	// may legally reuse a slug whose archived rows belong to another lineage.
	LineageUID uuid.UUID `bun:"lineage_uid,notnull,type:uuid" json:"lineage_uid"`
	// ArchivedAt marks a superseded version. NULL = this is the live version
	// of its lineage — exactly one row per lineage has it NULL, enforced by a
	// partial unique index on the slug.
	ArchivedAt *time.Time `bun:"archived_at" json:"archived_at,omitempty"`
	Name       string     `bun:"name,notnull" json:"name"`
	// Slug is a stable, human-typeable, machine-friendly identifier — the
	// thing a CLI invocation, an agent prompt, or a runbook references
	// instead of copying a UUID out of the UI. Mandatory and unique at the
	// database level; the API never auto-generates it (the frontend does,
	// from the name, until the operator edits it manually).
	Slug                string      `bun:"slug,notnull" json:"slug"`
	Description         string      `bun:"description,notnull,default:''" json:"description"`
	DurationSeconds     int64       `bun:"duration_seconds,notnull" json:"duration_seconds"`
	Controls            StringArray `bun:"controls,notnull,default:'{}'" json:"controls"`
	MaxQueryCounts      *int64      `bun:"max_query_counts" json:"max_query_counts"`
	MaxBytesTransferred *int64      `bun:"max_bytes_transferred" json:"max_bytes_transferred"`
	// Priority, when non-nil, is copied verbatim onto every grant
	// materialized from this definition, pinning it above or below the tier
	// its controls would otherwise earn. nil — the default — means "compute
	// it from the controls at materialization time" (see AutoPriority).
	Priority *int16 `bun:"priority" json:"priority"`
	// AutoApprove, when set, makes grant requests against this definition
	// bypass the pending/admin-approval step: the request is approved and
	// the grant materialized instantly at request time.
	AutoApprove bool `bun:"auto_approve,notnull,default:false" json:"auto_approve"`
	// UserGroupUIDs restricts which users may request this definition: a user
	// must belong to at least one of the listed user groups. Empty = every
	// user (the pre-scoping behavior, which every existing definition keeps).
	//
	// Stored as an array on the definition rather than a join table on
	// purpose: an empty scope means "everyone", so a cascade-on-delete join
	// table would fail *open* when a group is deleted. A dangling uid here
	// matches nobody, so the definition fails closed until an admin fixes it.
	//
	// Named `user_group_uids`, not `group_uids`: server groups exist too, and
	// a bare "group" is ambiguous. The old JSON name is still accepted on
	// input for one release; responses only ever emit the new one.
	UserGroupUIDs []uuid.UUID `bun:"user_group_uids,array,notnull,default:'{}'" json:"user_group_uids"`
	// ServerGroupUIDs restricts which databases this definition can be
	// requested against, by naming server groups rather than enumerating
	// servers: a database is in scope when it currently belongs to at least
	// one of the listed groups. Empty = every database — the same semantics
	// the old per-database list had, so every pre-existing definition keeps
	// behaving as before.
	//
	// Membership is resolved **live**, exactly like the user-group scope
	// above: adding a server to a scoped group makes the definition
	// requestable against it immediately, with no edit and therefore no new
	// version. That is the point of groups, and the reason fleet growth is no
	// longer O(definitions) of toil.
	//
	// Stored as an array on the definition rather than a join table for the
	// same reason UserGroupUIDs is: an empty scope means "every database", so
	// a cascade-on-delete join table would fail *open* when a group is
	// deleted. A dangling uid here matches no database — fail closed.
	ServerGroupUIDs []uuid.UUID `bun:"server_group_uids,array,notnull,default:'{}'" json:"server_group_uids"`
	// ApprovalPatterns are SQL patterns (RE2) that suspend a matching
	// statement until an admin or an approver-group member approves it.
	// Empty = no approval gating. Validated at save time so a bad pattern is
	// a 400 rather than a runtime surprise on the proxy hot path.
	ApprovalPatterns StringArray `bun:"approval_patterns,notnull,default:'{}'" json:"approval_patterns"`
	// SampleQueries are representative SQL statements an author saves
	// alongside the patterns to validate them against — a test bench for
	// pattern authoring, not a first-class matcher: the RE2 patterns above
	// remain what the proxy actually evaluates. Because it is just another
	// column on this row, it versions for free with every edit (see the
	// type doc): a definition's saved samples always describe the patterns
	// of that exact version. A sample that stops matching after an edit does
	// not block the save — see POST /grant-definitions/validate-patterns,
	// which reports match/no-match without failing the request.
	SampleQueries StringArray `bun:"sample_queries,notnull,default:'{}'" json:"sample_queries"`
	// ApproverUserGroupUIDs lists the user groups whose members may resolve
	// holds on grants built from this definition, *in addition to* admins.
	// Empty = admins only.
	ApproverUserGroupUIDs []uuid.UUID `bun:"approver_user_group_uids,array,notnull,default:'{}'" json:"approver_user_group_uids"`
	IsActive              bool        `bun:"is_active,notnull,default:true" json:"is_active"`
	CreatedBy             uuid.UUID   `bun:"created_by,notnull,type:uuid" json:"created_by"`
	CreatedAt             time.Time   `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`

	// ActiveGrantCount is how many non-revoked, non-expired grants this
	// definition's *lineage* is currently authorizing. Computed on listing
	// (one grouped query, never per row) so the UI can tell an operator what
	// deactivating it would cut off before they confirm.
	ActiveGrantCount int64 `bun:"-" json:"active_grant_count"`

	// ScopedDatabaseUIDs is ServerGroupUIDs resolved to the concrete
	// databases currently in scope — a read-only convenience so a non-admin
	// requester can narrow their database picker without needing the
	// admin-gated /server-groups endpoint (membership is access-relevant,
	// so that gate stays). It leaks no group names or membership shape,
	// only "these databases are in scope," which a requester could already
	// discover by trying. It is never the access-control gate: that stays
	// enforceRequestScope in internal/api/grant_requests.go, which
	// re-resolves scope itself and does not read this field.
	//
	// The two "in scope" states an empty ServerGroupUIDs and a scoped-but-
	// currently-empty group set would otherwise both stringify as `[]` are
	// told apart with a pointer:
	//   - nil (omitted from the JSON response): ServerGroupUIDs is empty,
	//     meaning every database is in scope.
	//   - non-nil, possibly empty ([]): ServerGroupUIDs is non-empty; this
	//     is the resolved union of every scoped group's current members. An
	//     empty slice means the definition is scoped but currently covers
	//     zero databases (e.g. every referenced group is empty).
	//
	// Only handleListGrantDefinitions and handleGetGrantDefinition populate
	// this (one batched membership query per response, never one per
	// definition); every other read or write path leaves it nil.
	ScopedDatabaseUIDs *[]uuid.UUID `bun:"-" json:"scoped_database_uids,omitempty"`
}

// IsLive reports whether this is the current version of its lineage, as
// opposed to a version archived by a later edit.
func (d *GrantDefinition) IsLive() bool {
	return d != nil && d.ArchivedAt == nil
}

// AppliesToUserGroups reports whether a user belonging to the given groups is
// within this definition's group scope. An empty scope applies to everyone.
func (d *GrantDefinition) AppliesToUserGroups(userGroupUIDs []uuid.UUID) bool {
	if len(d.UserGroupUIDs) == 0 {
		return true
	}

	for _, scoped := range d.UserGroupUIDs {
		for _, owned := range userGroupUIDs {
			if scoped == owned {
				return true
			}
		}
	}

	return false
}

// AppliesToServerGroups reports whether a database belonging to the given
// server groups is within this definition's scope. An empty scope applies to
// every database.
//
// The caller passes the target database's *current* group membership (see
// Store.ListServerGroupUIDsForServer) rather than the database uid, which is
// what makes the scope follow group membership live.
func (d *GrantDefinition) AppliesToServerGroups(serverGroupUIDs []uuid.UUID) bool {
	if len(d.ServerGroupUIDs) == 0 {
		return true
	}

	for _, scoped := range d.ServerGroupUIDs {
		for _, owned := range serverGroupUIDs {
			if scoped == owned {
				return true
			}
		}
	}

	return false
}

// MatchingServerGroup returns the definition's server group that the given
// database currently belongs to — the group a grant materialized from this
// definition binds to. The first of the definition's groups that contains the
// database wins, so the choice is stable for a given definition version.
//
// nil means "no group binds": either the definition is unscoped (it applies to
// every database, which is not the same thing as covering a named set) or the
// database is not in any of its groups. Either way the resulting grant covers
// its anchor database only.
func (d *GrantDefinition) MatchingServerGroup(serverGroupUIDs []uuid.UUID) *uuid.UUID {
	for _, scoped := range d.ServerGroupUIDs {
		for _, owned := range serverGroupUIDs {
			if scoped == owned {
				match := scoped

				return &match
			}
		}
	}

	return nil
}

// AppliesTo reports whether this definition can be requested by a user in the
// given groups against the given database. Both scopes must pass; an empty
// scope on either axis is unrestricted, which is what keeps every
// pre-existing (unscoped) definition behaving exactly as before.
func (d *GrantDefinition) AppliesTo(userGroupUIDs, serverGroupUIDs []uuid.UUID) bool {
	return d.AppliesToUserGroups(userGroupUIDs) && d.AppliesToServerGroups(serverGroupUIDs)
}

// GrantDefinitionFilter narrows ListGrantDefinitions queries.
type GrantDefinitionFilter struct {
	ActiveOnly bool
}

// UserGroup is an organizational grouping of users (data-analysts, SRE, …),
// deliberately kept apart from User.Roles, which are functional
// (admin/viewer/connector). Groups exist to scope grant definitions.
type UserGroup struct {
	bun.BaseModel `bun:"table:user_groups,alias:ug"`

	UID         uuid.UUID  `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	Name        string     `bun:"name,notnull" json:"name"`
	Description string     `bun:"description,notnull,default:''" json:"description"`
	CreatedBy   *uuid.UUID `bun:"created_by,type:uuid" json:"created_by,omitempty"`
	CreatedAt   time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// UserGroupMember is the group ↔ user join row. Membership *is* a join table
// (queried in both directions), and cascading deletes are safe here precisely
// because definition scope does not live in it.
type UserGroupMember struct {
	bun.BaseModel `bun:"table:user_group_members,alias:ugm"`

	GroupUID uuid.UUID `bun:"group_uid,pk,type:uuid" json:"group_uid"`
	UserUID  uuid.UUID `bun:"user_uid,pk,type:uuid" json:"user_uid"`
}

// ServerGroup is a named set of database servers ("the analytics replicas",
// "all staging databases") — the unit rights are scoped on, so a policy names
// a stable set instead of enumerating servers one by one.
//
// It is the server-side mirror of UserGroup, and its membership is
// deliberately **live**: a grant bound to a group covers whatever the group
// contains *right now*. Adding a server to a group therefore immediately
// widens every live grant bound to it — the one place where dbbat's
// "a live grant's behavior never changes under it" rule is knowingly broken,
// because group membership is operational data, exactly as user-group
// membership already is. See docs/grants.md.
type ServerGroup struct {
	bun.BaseModel `bun:"table:server_groups,alias:sg"`

	UID         uuid.UUID  `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	Name        string     `bun:"name,notnull" json:"name"`
	Description string     `bun:"description,notnull,default:''" json:"description"`
	CreatedBy   *uuid.UUID `bun:"created_by,type:uuid" json:"created_by,omitempty"`
	CreatedAt   time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// ServerGroupMember is the group ↔ server join row, the exact counterpart of
// UserGroupMember.
type ServerGroupMember struct {
	bun.BaseModel `bun:"table:server_group_members,alias:sgm"`

	GroupUID  uuid.UUID `bun:"group_uid,pk,type:uuid" json:"group_uid"`
	ServerUID uuid.UUID `bun:"server_uid,pk,type:uuid" json:"server_uid"`
}

// GrantRequestStatus enumerates the lifecycle states a request can be in.
type GrantRequestStatus string

// Lifecycle states for grant requests. Keep these constants matching the
// DB CHECK constraint values exactly.
const (
	GrantRequestPending   GrantRequestStatus = "pending"
	GrantRequestApproved  GrantRequestStatus = "approved"
	GrantRequestDenied    GrantRequestStatus = "denied"
	GrantRequestCancelled GrantRequestStatus = "cancelled" //nolint:misspell // matches DB CHECK constraint
	GrantRequestExpired   GrantRequestStatus = "expired"
)

// GrantRequest is a user-initiated request for a grant of a particular
// shape (definition) on a particular database. Admins approve or deny.
// On approval the system materializes a real AccessGrant from the
// definition + the request's user/database.
type GrantRequest struct {
	bun.BaseModel `bun:"table:grant_requests,alias:gr"`

	UID               uuid.UUID          `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	UserID            uuid.UUID          `bun:"user_id,notnull,type:uuid" json:"user_id"`
	GrantDefinitionID uuid.UUID          `bun:"grant_definition_id,notnull,type:uuid" json:"grant_definition_id"`
	DatabaseID        uuid.UUID          `bun:"database_id,notnull,type:uuid" json:"database_id"`
	Justification     string             `bun:"justification,notnull,default:''" json:"justification"`
	Status            GrantRequestStatus `bun:"status,notnull" json:"status"`
	RequestedAt       time.Time          `bun:"requested_at,notnull,default:current_timestamp" json:"requested_at"`
	DecidedAt         *time.Time         `bun:"decided_at" json:"decided_at,omitempty"`
	DecidedBy         *uuid.UUID         `bun:"decided_by,type:uuid" json:"decided_by,omitempty"`
	DecisionReason    *string            `bun:"decision_reason" json:"decision_reason,omitempty"`
	ResultingGrantID  *uuid.UUID         `bun:"resulting_grant_id,type:uuid" json:"resulting_grant_id,omitempty"`

	// Slack bookkeeping — populated by the notifier (Spec 04). JSON-omitted
	// because the API has no need to expose Slack message coordinates.
	SlackChannel   *string `bun:"slack_channel" json:"-"`
	SlackMessageTS *string `bun:"slack_message_ts" json:"-"`

	// Definition is the **live** version of the lineage GrantDefinitionID
	// points into, attached by the store on every read path (see
	// Store.attachRequestDefinitions).
	//
	// Live, not pinned, and deliberately so: a request is a claim on a policy,
	// not a grant of one. Approving it materializes today's version of that
	// policy (see approveGrantRequestTx), so the version a reader must be
	// shown — name, auto-approve, controls — is today's too. Resolving
	// GrantDefinitionID directly would render the version the request was
	// filed under, which an edit has since superseded and which no listing
	// returns anymore: that is how an edited definition used to leave its
	// requests displaying a bare uid.
	//
	// A grant is the opposite case and keeps the opposite rule: it pins the
	// exact version it was issued from (see AccessGrant.Definition).
	Definition *GrantDefinition `bun:"-" json:"definition,omitempty"`
}

// GrantRequestFilter narrows ListGrantRequests queries.
type GrantRequestFilter struct {
	UserID     *uuid.UUID
	Status     *GrantRequestStatus
	DatabaseID *uuid.UUID
	Limit      int
	Offset     int
}

// GrantFilter represents filters for listing grants
type GrantFilter struct {
	UserID     *uuid.UUID
	DatabaseID *uuid.UUID
	ActiveOnly bool
}

// AuditLog represents an audit log entry
type AuditLog struct {
	bun.BaseModel `bun:"table:audit_log,alias:al"`

	UID         uuid.UUID       `bun:"uid,pk,type:uuid" json:"uid"` // UUIDv7 set in Go
	EventType   string          `bun:"event_type,notnull" json:"event_type"`
	UserID      *uuid.UUID      `bun:"user_id,type:uuid" json:"user_id"`
	PerformedBy *uuid.UUID      `bun:"performed_by,type:uuid" json:"performed_by"`
	Details     json.RawMessage `bun:"details,type:jsonb" json:"details"`
	CreatedAt   time.Time       `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`

	// Tamper-evidence: ChainSeq is this row's position in the audit chain, MAC
	// is the HMAC over its canonical serialization plus PrevMAC. All three are
	// nil/zero on rows written before the chain anchor (unverifiable by
	// construction) and on a store built without an encryption key. They are
	// internal integrity state, not API surface — hence json:"-"; `dbbat audit
	// verify` is how they are consumed. See internal/store/chain.go.
	ChainSeq *int64 `bun:"chain_seq" json:"-"`
	PrevMAC  []byte `bun:"prev_mac" json:"-"`
	MAC      []byte `bun:"mac" json:"-"`
}

// AuditEvent is an alias for backward compatibility
type AuditEvent = AuditLog

// UserRoleSync is the newest directory role-sync audit entry of one user.
//
// It is a projection of `audit_log`, not a table: the audit entry stays the
// only record of what happened, and this carries it verbatim — `Details`
// included, because the directory groups it names are the answer to "why did
// this change?". The joined `Username` saves the caller a second lookup to
// render a row.
type UserRoleSync struct {
	bun.BaseModel `bun:"table:audit_log,alias:al"`

	UID       uuid.UUID       `bun:"uid,type:uuid" json:"uid"`
	EventType string          `bun:"event_type" json:"event_type"`
	UserID    uuid.UUID       `bun:"user_id,type:uuid" json:"user_id"`
	Username  string          `bun:"username,scanonly" json:"username"`
	Details   json.RawMessage `bun:"details,type:jsonb" json:"details"`
	CreatedAt time.Time       `bun:"created_at" json:"created_at"`
}

// AuditFilter represents filters for listing audit events
type AuditFilter struct {
	EventType   *string
	UserID      *uuid.UUID
	PerformedBy *uuid.UUID
	StartTime   *time.Time
	EndTime     *time.Time
	BeforeUID   *uuid.UUID // Cursor: return events with UID < this value
	Limit       int
	Offset      int
}

// ExtractSourceIP extracts the IP address from a net.Addr
func ExtractSourceIP(addr net.Addr) string {
	if tcpAddr, ok := addr.(*net.TCPAddr); ok {
		return tcpAddr.IP.String()
	}
	return addr.String()
}

// API key type constants
const (
	KeyTypeAPI = "api" // Regular API key (dbb_ prefix)
	KeyTypeWeb = "web" // Web session key (web_ prefix)
)

// APIKey represents an API key for authentication
type APIKey struct {
	bun.BaseModel `bun:"table:api_keys,alias:ak"`

	ID           uuid.UUID  `bun:"id,pk,type:uuid,default:gen_random_uuid()" json:"id"`
	UserID       uuid.UUID  `bun:"user_id,notnull,type:uuid" json:"user_id"`
	Name         string     `bun:"name,notnull" json:"name"`
	KeyHash      string     `bun:"key_hash,notnull" json:"-"`
	KeyPrefix    string     `bun:"key_prefix,notnull" json:"key_prefix"`
	KeyType      string     `bun:"key_type,notnull,default:'api'" json:"key_type"`
	ExpiresAt    *time.Time `bun:"expires_at" json:"expires_at"`
	LastUsedAt   *time.Time `bun:"last_used_at" json:"last_used_at"`
	RequestCount int64      `bun:"request_count,notnull,default:0" json:"request_count"`
	CreatedAt    time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	RevokedAt    *time.Time `bun:"revoked_at" json:"revoked_at"`
	RevokedBy    *uuid.UUID `bun:"revoked_by,type:uuid" json:"revoked_by"`
	// ProtocolData holds protocol-specific material (Oracle O5LOGON verifiers,
	// etc.) in a single jsonb column rather than dedicated per-protocol columns.
	// nil when the key has no protocol-specific data.
	ProtocolData *ProtocolData `bun:"protocol_data,type:jsonb,nullzero" json:"-"`
}

// ProtocolData is the per-protocol material attached to an API key, stored as a
// single jsonb column so protocol-specific fields don't proliferate as table
// columns. Absent protocols are omitted.
type ProtocolData struct {
	Oracle *OracleAPIKeyData `json:"oracle,omitempty"`
}

// OracleAPIKeyData is the Oracle O5LOGON verifier material derived from the API
// key for the proxy's terminated authentication. Both verifier types are kept:
// 6949 (legacy SHA-1) and 18453 (12c PBKDF2/HMAC-SHA512). Verifier values are
// encrypted with the dbbat master key (AAD-bound to the key prefix); salts are
// public challenge material. Empty fields are omitted from the jsonb.
type OracleAPIKeyData struct {
	O5LogonSalt6949      []byte `json:"o5logon_salt_6949,omitempty"`
	O5LogonVerifier6949  []byte `json:"o5logon_verifier_6949,omitempty"`
	O5LogonSalt18453     []byte `json:"o5logon_salt_18453,omitempty"`
	O5LogonVerifier18453 []byte `json:"o5logon_verifier_18453,omitempty"`

	// UserSalt records which salt scheme derived the verifiers above:
	// true = the USER's shared salts (users.protocol_data.oracle), so this key
	// is a login candidate alongside the user's other user-salt keys; false /
	// absent = legacy per-key random salts (only usable when it is the single
	// key the challenge was built from). The salts are duplicated here either
	// way so the challenge path never needs a user-row lookup.
	UserSalt bool `json:"user_salt,omitempty"`
}

// OracleData returns the key's Oracle protocol material, or nil if it has none.
func (k *APIKey) OracleData() *OracleAPIKeyData {
	if k.ProtocolData == nil {
		return nil
	}

	return k.ProtocolData.Oracle
}

// IsExpired returns true if the API key has expired
func (k *APIKey) IsExpired() bool {
	if k.ExpiresAt == nil {
		return false
	}
	return time.Now().After(*k.ExpiresAt)
}

// IsRevoked returns true if the API key has been revoked
func (k *APIKey) IsRevoked() bool {
	return k.RevokedAt != nil
}

// IsValid returns true if the API key is not expired and not revoked
func (k *APIKey) IsValid() bool {
	return !k.IsExpired() && !k.IsRevoked()
}

// IsWebSession returns true if this is a web session key
func (k *APIKey) IsWebSession() bool {
	return k.KeyType == KeyTypeWeb
}

// Identity provider constants
const (
	IdentityTypeSlack = "slack"
)

// UserIdentity represents a link between a user and an external identity provider
type UserIdentity struct {
	bun.BaseModel `bun:"table:user_identities,alias:ui"`

	UID         uuid.UUID       `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	UserID      uuid.UUID       `bun:"user_id,notnull,type:uuid" json:"user_id"`
	Provider    string          `bun:"provider,notnull" json:"provider"`
	ProviderID  string          `bun:"provider_id,notnull" json:"provider_id"`
	Email       string          `bun:"email" json:"email,omitempty"`
	DisplayName string          `bun:"display_name" json:"display_name,omitempty"`
	Metadata    json.RawMessage `bun:"metadata,type:jsonb" json:"metadata,omitempty"`
	CreatedAt   time.Time       `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt   time.Time       `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
	DeletedAt   *time.Time      `bun:"deleted_at,soft_delete" json:"-"`
}

// OAuthState represents a temporary OAuth state for CSRF protection
type OAuthState struct {
	bun.BaseModel `bun:"table:oauth_states,alias:os"`

	UID         uuid.UUID       `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	State       string          `bun:"state,notnull,unique" json:"state"`
	Provider    string          `bun:"provider,notnull" json:"provider"`
	RedirectURL string          `bun:"redirect_url" json:"redirect_url,omitempty"`
	Metadata    json.RawMessage `bun:"metadata,type:jsonb" json:"metadata,omitempty"`
	ExpiresAt   time.Time       `bun:"expires_at,notnull" json:"expires_at"`
	CreatedAt   time.Time       `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// APIKeyFilter represents filters for listing API keys
type APIKeyFilter struct {
	UserID     *uuid.UUID
	KeyType    *string // Filter by key type (api, web)
	IncludeAll bool    // Include revoked/expired keys
	Limit      int
	Offset     int
}

// GlobalParameter stores runtime-editable key-value configuration.
type GlobalParameter struct {
	bun.BaseModel `bun:"table:global_parameters,alias:gp"`

	UID       uuid.UUID  `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	GroupKey  string     `bun:"group_key,notnull" json:"group_key"`
	Key       string     `bun:"key,notnull" json:"key"`
	Value     string     `bun:"value,notnull" json:"value"`
	CreatedAt time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time  `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
	DeletedAt *time.Time `bun:"deleted_at,soft_delete" json:"-"`
}

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

	UID               uuid.UUID  `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	Username          string     `bun:"username,notnull,unique" json:"username"`
	PasswordHash      string     `bun:"password_hash,notnull" json:"-"`
	Roles             []string   `bun:"roles,array" json:"roles"`
	RateLimitExempt   bool       `bun:"rate_limit_exempt,notnull,default:false" json:"rate_limit_exempt"`
	PasswordChangedAt *time.Time `bun:"password_changed_at" json:"-"`
	CreatedAt         time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt         time.Time  `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
	DeletedAt         *time.Time `bun:"deleted_at,soft_delete" json:"-"`
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
	Roles        []string
}

// Protocol constants for database connections
const (
	ProtocolPostgreSQL = "postgresql"
	ProtocolOracle     = "oracle"
	ProtocolMySQL      = "mysql"
	ProtocolMariaDB    = "mariadb"
	ProtocolMongoDB    = "mongodb"
)

// IsMySQLFamily reports whether the given protocol speaks the MySQL wire
// protocol. The MySQL proxy serves both — they share the same listener,
// auth plugins, and wire-protocol handling. The distinction matters mostly
// for upstream connection setup (server version banner, auth plugin
// negotiation) and for UI labeling.
func IsMySQLFamily(protocol string) bool {
	return protocol == ProtocolMySQL || protocol == ProtocolMariaDB
}

// Database represents a target database configuration
type Database struct {
	bun.BaseModel `bun:"table:databases,alias:d"`

	UID               uuid.UUID `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	Name              string    `bun:"name,notnull,unique" json:"name"`
	Description       string    `bun:"description" json:"description"`
	Host              string    `bun:"host,notnull" json:"host"`
	Port              int       `bun:"port,notnull" json:"port"`
	DatabaseName      string    `bun:"database_name,notnull" json:"database_name"`
	Username          string    `bun:"username,notnull" json:"username"`
	Password          string    `bun:"-" json:"-"`                          // Decrypted, not stored
	PasswordEncrypted []byte    `bun:"password_encrypted,notnull" json:"-"` // Encrypted form
	SSLMode           string    `bun:"ssl_mode,notnull,default:'prefer'" json:"ssl_mode"`
	Protocol          string    `bun:"protocol,notnull,default:'postgresql'" json:"protocol"`
	OracleServiceName *string   `bun:"oracle_service_name" json:"oracle_service_name,omitempty"`
	// ProtocolData holds protocol-specific per-database settings (MongoDB
	// upstream authSource, etc.) in a single generic jsonb column — mirroring
	// User.ProtocolData — rather than a dedicated column per setting.
	ProtocolData *DatabaseProtocolData `bun:"protocol_data,type:jsonb,nullzero" json:"-"`
	Listable     bool                  `bun:"listable,notnull" json:"listable"`
	CreatedBy    *uuid.UUID            `bun:"created_by,type:uuid" json:"created_by"`
	CreatedAt    time.Time             `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt    time.Time             `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
	DeletedAt    *time.Time            `bun:"deleted_at,soft_delete" json:"-"`
}

// DatabaseProtocolData is per-protocol material attached to a database, stored
// as a single jsonb column so protocol-specific settings don't proliferate as
// table columns — mirrors UserProtocolData. Absent protocols are omitted.
type DatabaseProtocolData struct {
	MongoDB *MongoDatabaseData `json:"mongodb,omitempty"`
}

// MongoDatabaseData holds MongoDB-specific per-database settings.
type MongoDatabaseData struct {
	// AuthSource is the upstream SCRAM authSource; empty defers to
	// MongoAuthSourceOrDefault's "admin" default.
	AuthSource string `json:"auth_source,omitempty"`
}

// MongoData returns the database's MongoDB protocol material, or nil if absent.
func (db *Database) MongoData() *MongoDatabaseData {
	if db.ProtocolData == nil {
		return nil
	}

	return db.ProtocolData.MongoDB
}

// DatabaseUpdate represents fields that can be updated
type DatabaseUpdate struct {
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
}

// QueryRow is an alias for API compatibility (without bun.BaseModel for simpler usage)
type QueryRow struct {
	RowNumber    int             `json:"row_number"`
	RowData      json.RawMessage `json:"row_data"`
	RowSizeBytes int64           `json:"row_size_bytes"`
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

// AccessGrant represents an access grant
type AccessGrant struct {
	bun.BaseModel `bun:"table:access_grants,alias:ag"`

	UID                 uuid.UUID  `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	UserID              uuid.UUID  `bun:"user_id,notnull,type:uuid" json:"user_id"`
	DatabaseID          uuid.UUID  `bun:"database_id,notnull,type:uuid" json:"database_id"`
	Controls            []string   `bun:"controls,array" json:"controls"` // Array of controls: read_only, block_copy, block_ddl
	GrantedBy           uuid.UUID  `bun:"granted_by,notnull,type:uuid" json:"granted_by"`
	StartsAt            time.Time  `bun:"starts_at,notnull" json:"starts_at"`
	ExpiresAt           time.Time  `bun:"expires_at,notnull" json:"expires_at"`
	RevokedAt           *time.Time `bun:"revoked_at" json:"revoked_at"`
	RevokedBy           *uuid.UUID `bun:"revoked_by,type:uuid" json:"revoked_by"`
	MaxQueryCounts      *int64     `bun:"max_query_counts" json:"max_query_counts"`
	MaxBytesTransferred *int64     `bun:"max_bytes_transferred" json:"max_bytes_transferred"`
	CreatedAt           time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`

	// Computed fields (not stored in DB)
	QueryCount       int64 `bun:"-" json:"query_count"`
	BytesTransferred int64 `bun:"-" json:"bytes_transferred"`
}

// HasControl checks if the grant has a specific control enabled
func (g *AccessGrant) HasControl(control string) bool {
	for _, c := range g.Controls {
		if c == control {
			return true
		}
	}
	return false
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

// GrantDefinition is an admin-managed template describing the *shape* of a
// grant: name, duration, controls, optional quotas. Grant requests
// (separately implemented) reference a definition; on approval, a real
// AccessGrant is built from the definition + the request's user/database.
//
// Direct admin grant creation bypasses definitions — they exist to bound
// what users can self-request, not to constrain admins.
type GrantDefinition struct {
	bun.BaseModel `bun:"table:grant_definitions,alias:gd"`

	UID                 uuid.UUID `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	Name                string    `bun:"name,notnull" json:"name"`
	Description         string    `bun:"description,notnull,default:''" json:"description"`
	DurationSeconds     int64     `bun:"duration_seconds,notnull" json:"duration_seconds"`
	Controls            []string  `bun:"controls,array,notnull,default:'{}'" json:"controls"`
	MaxQueryCounts      *int64    `bun:"max_query_counts" json:"max_query_counts"`
	MaxBytesTransferred *int64    `bun:"max_bytes_transferred" json:"max_bytes_transferred"`
	// AutoApprove, when set, makes grant requests against this definition
	// bypass the pending/admin-approval step: the request is approved and
	// the grant materialized instantly at request time.
	AutoApprove bool      `bun:"auto_approve,notnull,default:false" json:"auto_approve"`
	IsActive    bool      `bun:"is_active,notnull,default:true" json:"is_active"`
	CreatedBy   uuid.UUID `bun:"created_by,notnull,type:uuid" json:"created_by"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// GrantDefinitionFilter narrows ListGrantDefinitions queries.
type GrantDefinitionFilter struct {
	ActiveOnly bool
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
}

// AuditEvent is an alias for backward compatibility
type AuditEvent = AuditLog

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

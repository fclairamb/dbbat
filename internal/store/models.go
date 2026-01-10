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

// Database represents a target database configuration
type Database struct {
	bun.BaseModel `bun:"table:databases,alias:d"`

	UID               uuid.UUID  `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	Name              string     `bun:"name,notnull,unique" json:"name"`
	Description       string     `bun:"description" json:"description"`
	Host              string     `bun:"host,notnull" json:"host"`
	Port              int        `bun:"port,notnull,default:5432" json:"port"`
	DatabaseName      string     `bun:"database_name,notnull" json:"database_name"`
	Username          string     `bun:"username,notnull" json:"username"`
	Password          string     `bun:"-" json:"-"`                          // Decrypted, not stored
	PasswordEncrypted []byte     `bun:"password_encrypted,notnull" json:"-"` // Encrypted form
	SSLMode           string     `bun:"ssl_mode,notnull,default:'prefer'" json:"ssl_mode"`
	CreatedBy         *uuid.UUID `bun:"created_by,type:uuid" json:"created_by"`
	CreatedAt         time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt         time.Time  `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}

// DatabaseUpdate represents fields that can be updated
type DatabaseUpdate struct {
	Description  *string
	Host         *string
	Port         *int
	DatabaseName *string
	Username     *string
	Password     *string // Plaintext password to encrypt
	SSLMode      *string
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
	Limit        int
	Offset       int
}

// AccessGrant represents an access grant
type AccessGrant struct {
	bun.BaseModel `bun:"table:access_grants,alias:ag"`

	UID                 uuid.UUID  `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
	UserID              uuid.UUID  `bun:"user_id,notnull,type:uuid" json:"user_id"`
	DatabaseID          uuid.UUID  `bun:"database_id,notnull,type:uuid" json:"database_id"`
	AccessLevel         string     `bun:"access_level,notnull" json:"access_level"` // "read" or "write"
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

// Grant is an alias for backward compatibility
type Grant = AccessGrant

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

// APIKeyFilter represents filters for listing API keys
type APIKeyFilter struct {
	UserID     *uuid.UUID
	KeyType    *string // Filter by key type (api, web)
	IncludeAll bool    // Include revoked/expired keys
	Limit      int
	Offset     int
}

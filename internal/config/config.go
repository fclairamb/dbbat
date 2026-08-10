package config

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// Configuration errors.
var (
	ErrDSNRequired    = errors.New("DBB_DSN environment variable is required")
	ErrKeyRequired    = errors.New("either DBB_KEY or DBB_KEYFILE must be set")
	ErrInvalidKeySize = errors.New("encryption key must be 32 bytes")

	// ErrDumpUploadNeedsDir is returned when an upload target is configured
	// without a local spool. Captures are always written to disk first and
	// uploaded once complete (S3 objects cannot be appended to), so an upload
	// URL with no DBB_DUMP_DIR would silently capture nothing.
	ErrDumpUploadNeedsDir = errors.New("DBB_DUMP_UPLOAD_URL requires DBB_DUMP_DIR")

	// ErrOIDCClientCredentialsRequired is returned when an OIDC issuer is
	// configured without client credentials. Failing at startup beats
	// offering a login button that can only ever end in an error page.
	ErrOIDCClientCredentialsRequired = errors.New(
		"DBB_OIDC_ISSUER requires DBB_OIDC_CLIENT_ID and DBB_OIDC_CLIENT_SECRET")

	// ErrOIDCRoleMappingInvalid is returned when DBB_OIDC_ROLE_MAPPING cannot
	// be parsed. A mapping is an authorization rule: a typo must stop the
	// process, never silently resolve to "no rule" and hand everyone the
	// default role.
	ErrOIDCRoleMappingInvalid = errors.New("DBB_OIDC_ROLE_MAPPING is malformed")

	// ErrAuthDefaultRoleInvalid is returned when DBB_AUTH_DEFAULT_ROLE (or its
	// legacy alias DBB_SLACK_AUTH_DEFAULT_ROLE) names a role that does not
	// exist. Same reasoning as the role mapping: a default role is an
	// authorization decision, and one that fails closed at startup is far
	// better than one that quietly provisions users into a role nothing knows.
	ErrAuthDefaultRoleInvalid = errors.New("DBB_AUTH_DEFAULT_ROLE is not a known role")
)

// DefaultOAuthRole is the role an auto-provisioned OAuth user starts with when
// DBB_AUTH_DEFAULT_ROLE is unset. It mirrors store.RoleConnector, which config
// cannot import (store imports config); TestConfigKnownRolesMatchStore pins the
// two together.
const DefaultOAuthRole = "connector"

// OIDC provider defaults.
const (
	// DefaultOIDCScopes is the scope set requested from the issuer.
	DefaultOIDCScopes = "openid email profile"
	// DefaultOIDCGroupsClaim is the ID-token claim read for directory group
	// membership when DBB_OIDC_GROUPS_CLAIM is unset.
	DefaultOIDCGroupsClaim = "groups"
	// DefaultOIDCDisplayName is the login-button label when the operator
	// does not set one.
	DefaultOIDCDisplayName = "SSO"
)

// RunMode represents the application run mode.
type RunMode string

const (
	// RunModeDefault is the default production mode.
	RunModeDefault RunMode = ""
	// RunModeTest provisions test data on startup.
	RunModeTest RunMode = "test"
	// RunModeDemo provisions demo data on startup with additional protections.
	RunModeDemo RunMode = "demo"
)

// QueryStorageConfig holds configuration for query result storage.
type QueryStorageConfig struct {
	// MaxResultRows is the maximum number of rows to store per query.
	MaxResultRows int `koanf:"max_result_rows"`

	// MaxResultBytes is the maximum total bytes to store per query.
	MaxResultBytes int64 `koanf:"max_result_bytes"`

	// StoreResults enables/disables result storage globally.
	StoreResults bool `koanf:"store_results"`

	// Retention is how long query history (and the captured result rows
	// hanging off it) is kept, as a Go duration (e.g. "720h").
	//
	// Empty or "0" means keep forever, and that is the default: dbbat is an
	// audit tool, so an upgrade must never silently start deleting history.
	// Operators opt in — "720h" (30 days) is a reasonable starting point.
	Retention string `koanf:"retention"`
}

// DefaultQueryStorageRetention keeps query history forever. Retention is
// opt-in: see QueryStorageConfig.Retention.
const DefaultQueryStorageRetention = "0"

// RetentionDuration parses Retention into a duration. A zero or negative
// result means "disabled — keep forever", and no sweep is scheduled.
//
// A malformed value also disables the sweep rather than falling back to some
// built-in period: this sweep permanently deletes audit data, so a typo must
// never be interpreted as "delete more". The caller warns about it (see
// RetentionMisconfigured).
func (c QueryStorageConfig) RetentionDuration() time.Duration {
	if c.Retention == "" {
		return 0
	}

	d, err := time.ParseDuration(c.Retention)
	if err != nil {
		return 0
	}

	if d < 0 {
		return 0
	}

	return d
}

// RetentionMisconfigured reports that Retention was set to something that is
// neither empty nor a usable positive duration — i.e. retention silently ends up
// disabled and the operator probably did not mean that.
func (c QueryStorageConfig) RetentionMisconfigured() bool {
	return c.Retention != "" && c.Retention != "0" && c.RetentionDuration() <= 0
}

// RateLimitConfig holds configuration for API rate limiting.
type RateLimitConfig struct {
	// Enabled enables/disables rate limiting.
	Enabled bool `koanf:"enabled"`

	// RequestsPerMinute is the rate limit for authenticated users.
	RequestsPerMinute int `koanf:"requests_per_minute"`

	// RequestsPerMinuteAnon is the rate limit for unauthenticated requests (by IP).
	RequestsPerMinuteAnon int `koanf:"requests_per_minute_anon"`

	// Burst allows short bursts above the rate limit.
	Burst int `koanf:"burst"`
}

// HashConfig holds password hashing configuration.
type HashConfig struct {
	// Preset is a named configuration preset (default, low, minimal).
	Preset string `koanf:"preset"`

	// MemoryMB is the memory parameter in megabytes (1-1024).
	MemoryMB int `koanf:"memory_mb"`

	// Time is the time/iterations parameter (1-10).
	Time int `koanf:"time"`

	// Threads is the parallelism parameter (1-16).
	Threads int `koanf:"threads"`
}

// AuthCacheConfig holds configuration for authentication caching.
type AuthCacheConfig struct {
	// Enabled enables/disables the authentication cache.
	Enabled bool `koanf:"enabled"`

	// TTLSeconds is the time-to-live for cache entries in seconds.
	TTLSeconds int `koanf:"ttl_seconds"`

	// MaxSize is the maximum number of cache entries.
	MaxSize int `koanf:"max_size"`
}

// RedirectRule represents a path-based redirect for development proxying.
type RedirectRule struct {
	// PathPrefix is the path prefix to match (e.g., "/app").
	PathPrefix string
	// TargetHost is the target host to proxy to (e.g., "localhost:5173").
	TargetHost string
	// TargetPath is the path on the target (e.g., "/").
	TargetPath string
}

// SlackAuthConfig holds Slack OAuth configuration.
//
// Auto-provisioning used to live here (`auto_create_users`, `default_role`)
// even though every OAuth provider read it; it now lives on OAuthUsersConfig.
// The old env/file keys are still accepted as aliases — see
// applyAuthProvisioningAliases.
type SlackAuthConfig struct {
	ClientID     string `koanf:"client_id"`
	ClientSecret string `koanf:"client_secret"`
	TeamID       string `koanf:"team_id"`
}

// Enabled returns true if Slack OAuth is configured with both client ID and secret.
func (c SlackAuthConfig) Enabled() bool {
	return c.ClientID != "" && c.ClientSecret != ""
}

// OIDCAuthConfig holds the generic OpenID Connect login provider — the one
// that lets an organization sign in with Google Workspace, Okta, Microsoft
// Entra, Keycloak or anything else speaking OIDC discovery. Distinct from
// SlackAuthConfig: both can be enabled at once, and the login page shows a
// button per enabled provider.
type OIDCAuthConfig struct {
	// Issuer is the OIDC issuer URL (e.g. "https://accounts.google.com").
	// Setting it is what enables the provider.
	Issuer string `koanf:"issuer"`
	// ClientID and ClientSecret identify this dbbat instance to the issuer.
	ClientID     string `koanf:"client_id"`
	ClientSecret string `koanf:"client_secret"`
	// Scopes is the space- or comma-separated scope list requested at
	// authorization time. "openid" is always added.
	Scopes string `koanf:"scopes"`
	// DisplayName is the login-button label (e.g. "Acme SSO").
	DisplayName string `koanf:"display_name"`
	// EmailDomains is an optional comma-separated allowlist. When set, a
	// login is rejected unless the *verified* email claim's domain is
	// listed — the generic equivalent of Slack's workspace gating.
	EmailDomains string `koanf:"email_domains"`
	// GroupsClaim names the ID-token claim carrying the user's directory
	// group membership. Empty means "groups", which is what Okta, Keycloak
	// and Entra all use by default.
	GroupsClaim string `koanf:"groups_claim"`
	// RoleMapping binds dbbat roles to directory groups, e.g.
	// "admin=db-admins,viewer=analysts". Empty disables the mapping
	// entirely, leaving role assignment manual.
	RoleMapping string `koanf:"role_mapping"`
}

// KnownRoles is the set of role names a role mapping may name. It duplicates
// store.RoleAdmin/RoleViewer/RoleConnector on purpose: store imports config,
// so config cannot import store. TestConfigKnownRolesMatchStore in
// internal/api pins the two lists together.
var KnownRoles = []string{"admin", "viewer", "connector"}

// OAuthUsersConfig holds the auto-provisioning settings that apply to **every**
// OAuth/OIDC login provider — Slack, the generic OIDC issuer, and whatever
// comes next.
//
// They used to live on SlackAuthConfig, which meant an operator who had never
// touched Slack still had to set DBB_SLACK_AUTH_AUTO_CREATE_USERS=false to stop
// their OIDC issuer from minting accounts. The canonical names are now
// DBB_AUTH_AUTO_CREATE_USERS and DBB_AUTH_DEFAULT_ROLE; the DBB_SLACK_AUTH_*
// ones keep working as aliases (applyAuthProvisioningAliases).
type OAuthUsersConfig struct {
	// AutoCreateUsers lets an unknown but verified identity provision itself a
	// local account on first login. Defaults to true.
	AutoCreateUsers bool `koanf:"auto_create_users"`
	// DefaultRole is the role such an account starts with, and the floor a
	// group role mapping can never dig below. Empty means DefaultOAuthRole.
	DefaultRole string `koanf:"default_role"`
}

// Role returns the configured default role, normalized the same way
// ParseRoleMapping normalizes the roles it reads, and falling back to
// DefaultOAuthRole when unset.
func (c OAuthUsersConfig) Role() string {
	if role := strings.ToLower(strings.TrimSpace(c.DefaultRole)); role != "" {
		return role
	}

	return DefaultOAuthRole
}

// Validate refuses a default role that is not a real role. A typo here would
// otherwise provision every auto-created user into a role that grants nothing
// and that no permission check has ever heard of — failing at startup is the
// far cheaper outcome, and it is the same rule ParseRoleMapping applies to the
// role names in a group mapping.
func (c OAuthUsersConfig) Validate() error {
	if role := c.Role(); !slices.Contains(KnownRoles, role) {
		return fmt.Errorf("%w: unknown role %q (known: %s)",
			ErrAuthDefaultRoleInvalid, role, strings.Join(KnownRoles, ", "))
	}

	return nil
}

// Enabled returns true when an issuer is configured. Client id and secret are
// validated at startup once Enabled is true, so a half-configured provider
// fails loudly instead of silently offering a broken login button.
func (c OIDCAuthConfig) Enabled() bool {
	return c.Issuer != ""
}

// ScopeList splits Scopes on whitespace and commas.
func (c OIDCAuthConfig) ScopeList() []string {
	return splitList(c.Scopes)
}

// EmailDomainList splits EmailDomains on whitespace and commas.
func (c OIDCAuthConfig) EmailDomainList() []string {
	return splitList(c.EmailDomains)
}

// GroupsClaimName returns the configured groups claim, defaulting to "groups".
func (c OIDCAuthConfig) GroupsClaimName() string {
	if claim := strings.TrimSpace(c.GroupsClaim); claim != "" {
		return claim
	}

	return DefaultOIDCGroupsClaim
}

// RoleMappingEnabled reports whether a group-to-role mapping is configured.
// Without one, nothing in the login path ever touches a user's roles.
func (c OIDCAuthConfig) RoleMappingEnabled() bool {
	return strings.TrimSpace(c.RoleMapping) != ""
}

// ParseRoleMapping turns "admin=db-admins,viewer=analysts" into
// {"admin": ["db-admins"], "viewer": ["analysts"]}.
//
// Pairs are separated by commas only — never by whitespace — because directory
// groups are routinely named "Domain Admins". The role is the part before the
// first "=", lower-cased and validated against KnownRoles; everything after it
// is the group value, kept verbatim (Entra sends group **object ids**, not
// display names, and matching is exact, case included). Repeating a role
// unions its groups: "admin=db-admins,admin=sre" grants admin to either.
//
// An empty map means "no mapping configured"; an error means the operator
// typed something that cannot be an authorization rule.
func (c OIDCAuthConfig) ParseRoleMapping() (map[string][]string, error) {
	mapping := make(map[string][]string)

	if !c.RoleMappingEnabled() {
		return mapping, nil
	}

	for _, pair := range strings.Split(c.RoleMapping, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		role, group, found := strings.Cut(pair, "=")
		if !found {
			return nil, fmt.Errorf("%w: %q is not a role=group pair", ErrOIDCRoleMappingInvalid, pair)
		}

		role = strings.ToLower(strings.TrimSpace(role))
		group = strings.TrimSpace(group)

		if !slices.Contains(KnownRoles, role) {
			return nil, fmt.Errorf("%w: unknown role %q (known: %s)",
				ErrOIDCRoleMappingInvalid, role, strings.Join(KnownRoles, ", "))
		}

		if group == "" {
			return nil, fmt.Errorf("%w: role %q is mapped to an empty group", ErrOIDCRoleMappingInvalid, role)
		}

		if !slices.Contains(mapping[role], group) {
			mapping[role] = append(mapping[role], group)
		}
	}

	if len(mapping) == 0 {
		return nil, fmt.Errorf("%w: no role=group pair found", ErrOIDCRoleMappingInvalid)
	}

	return mapping, nil
}

// splitList tokenizes a comma- or whitespace-separated env-var value,
// dropping empties so a trailing comma is harmless.
func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})

	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}

	return out
}

// SlackNotifyConfig configures outbound Slack notifications for grant
// request events. Distinct from SlackAuthConfig (login OIDC) so deployments
// can enable one without the other.
type SlackNotifyConfig struct {
	// BotToken is the Slack bot user OAuth token (xoxb-...). Empty
	// disables notifications.
	BotToken string `koanf:"bot_token"`
	// Channel is the Slack channel id or name (e.g. "#dbbat") where
	// notifications are posted. Defaults to "#dbbat".
	Channel string `koanf:"channel"`
	// SigningSecret is the Slack app signing secret used to verify inbound
	// interaction callbacks (Approve/Deny button clicks). Empty disables
	// interactivity: messages carry no buttons and the inbound endpoint is
	// not registered — outbound notifications still work.
	SigningSecret string `koanf:"signing_secret"`
	// AppToken is the Slack app-level token (xapp-...) with the
	// connections:write scope. When set together with a bot token, dbbat
	// opens an outbound Socket Mode connection and receives Approve/Deny
	// interactions over it instead of the inbound HTTP endpoint — for
	// deployments that can't accept inbound Slack traffic. Empty = no Socket
	// Mode.
	AppToken string `koanf:"app_token"`
}

// Enabled returns true when a bot token is set. Channel is enforced at
// startup when Enabled is true; this method only gates whether the
// notifier should run at all.
func (c SlackNotifyConfig) Enabled() bool {
	return c.BotToken != ""
}

// SocketMode returns true when dbbat should open an outbound Slack Socket
// Mode connection: both an app-level token and a bot token are set. An app
// token without a bot token is a misconfiguration caught at startup.
func (c SlackNotifyConfig) SocketMode() bool {
	return c.AppToken != "" && c.BotToken != ""
}

// Interactive returns true when Approve/Deny buttons should be rendered and an
// inbound interaction transport should be served. A bot token (to carry the
// buttons on notification messages) plus at least one inbound transport — a
// signing secret (the HTTP endpoint) or an app-level token (Socket Mode) — is
// required. A signing secret or app token without a bot token is a
// misconfiguration caught at startup.
func (c SlackNotifyConfig) Interactive() bool {
	return c.BotToken != "" && (c.SigningSecret != "" || c.AppToken != "")
}

// ApprovalConfig configures pattern-triggered approval holds — the four-eyes
// control that suspends a matching statement mid-flight until a second human
// approves it.
//
// Enabled defaults to **false**. This feature blocks a live database
// connection on a human being; it ships off and gets turned on deliberately,
// per deployment.
type ApprovalConfig struct {
	// Enabled turns the approval gate on. When false, approval patterns on
	// grants are inert: nothing is ever held.
	Enabled bool `koanf:"enabled"`
	// SlackDelay is how long a hold must remain pending before a Slack
	// notification fires. Zero disables Slack escalation entirely. There is
	// no presence detection: if an admin was watching, they had this long to
	// act, and resolving the hold cancels the pending notification.
	SlackDelay string `koanf:"slack_delay"`
	// SlackSQL includes the (truncated) SQL text in the Slack message.
	// Default on. Slack is a lower trust boundary than the dbbat UI and this
	// feature pipes production query text into it, so it is switchable off.
	SlackSQL bool `koanf:"slack_sql"`
}

// Default approval-hold settings.
const (
	// DefaultApprovalSlackDelay is how long a hold waits before escalating.
	DefaultApprovalSlackDelay = "30s"
	// ApprovalSlackSQLMaxLen bounds the SQL text copied into Slack.
	ApprovalSlackSQLMaxLen = 500
)

// SlackDelayDuration parses SlackDelay, falling back to the default on a
// malformed value (a typo must not silently disable escalation). A zero or
// negative value disables escalation, which is an explicit opt-out.
func (c ApprovalConfig) SlackDelayDuration() time.Duration {
	if c.SlackDelay == "" {
		d, _ := time.ParseDuration(DefaultApprovalSlackDelay)

		return d
	}

	d, err := time.ParseDuration(c.SlackDelay)
	if err != nil {
		fallback, _ := time.ParseDuration(DefaultApprovalSlackDelay)

		return fallback
	}

	if d < 0 {
		return 0
	}

	return d
}

// DumpConfig holds configuration for session packet dumps.
type DumpConfig struct {
	// Dir is the directory for dump files. Empty = disabled.
	Dir string `koanf:"dir"`

	// MaxSize is the maximum dump file size per session in bytes.
	MaxSize int64 `koanf:"max_size"`

	// Retention is the auto-delete duration for old dumps (e.g., "24h").
	//
	// It applies to the local spool only. When UploadURL is set, remote
	// retention is the bucket's own lifecycle policy — dbbat never expires
	// objects it uploaded, and never LISTs the bucket to look for them.
	Retention string `koanf:"retention"`

	// UploadURL is the blob-storage bucket finished captures are uploaded to
	// on session close, e.g. "s3://my-bucket/dbbat-captures". Empty — the
	// default — keeps captures on local disk only, which is the historical
	// behavior.
	//
	// The scheme selects the driver (gocloud.dev/blob): "s3://" for S3 and
	// S3-compatible stores, "file://" for a local directory (useful in tests
	// and for a mounted volume). Any path after the bucket is used as a key
	// prefix. Requires Dir: captures are always spooled locally first and
	// uploaded once complete.
	UploadURL string `koanf:"upload_url"`
}

// Default dump settings.
const (
	DefaultDumpMaxSize   = 10 * 1024 * 1024 // 10MB
	DefaultDumpRetention = "24h"
)

// MySQLConfig holds configuration specific to the MySQL proxy.
type MySQLConfig struct {
	// TLS holds TLS server-termination settings for the proxy. When enabled,
	// the proxy advertises CLIENT_SSL and accepts SSL Request packets from
	// clients, terminating the TLS session at the proxy. Required for clean
	// caching_sha2_password full-auth (cleartext password over TLS).
	TLS TLSConfig `koanf:"tls"`
}

// MongoConfig holds configuration specific to the MongoDB proxy.
type MongoConfig struct {
	// TLS holds TLS server-termination settings for the proxy. Mongo TLS is
	// implicit-from-byte-0 (no STARTTLS dance): when certs are configured (or
	// auto-generated) the proxy terminates TLS, and SASL PLAIN auth is only
	// accepted over TLS. When Disable is true the listener stays plaintext.
	TLS TLSConfig `koanf:"tls"`
}

// MSSQLConfig holds configuration specific to the SQL Server (TDS) proxy.
type MSSQLConfig struct {
	// TLS holds TLS server-termination settings for the proxy. TDS is the odd
	// one out: the handshake is encapsulated inside PRELOGIN-typed TDS packets
	// rather than starting on a clean byte boundary, and a client connecting
	// with Encrypt=no still runs one — TLS then covers the LOGIN7 packet only.
	// When Disable is true the proxy answers ENCRYPT_NOT_SUP and the listener
	// stays plaintext, which also refuses clients that require encryption.
	TLS TLSConfig `koanf:"tls"`

	// TLSMaxVersion caps the client-leg handshake. Empty (the default) means
	// TLS 1.2 — see MSSQLDefaultTLSMaxVersion for why that is not simply "the
	// newest thing crypto/tls supports". "1.3" opts in.
	//
	// It lives here rather than on the shared TLSConfig because it is a TDS
	// encapsulation concern: the other four proxies upgrade on a clean byte
	// boundary and have nothing to decide.
	TLSMaxVersion string `koanf:"tls_max_version"`
}

// SQL Server client-leg TLS version ceilings, as accepted in
// DBB_MSSQL_TLS_MAX_VERSION.
const (
	// MSSQLTLSMaxVersion12 pins the encapsulated handshake to TLS 1.2.
	MSSQLTLSMaxVersion12 = "1.2"
	// MSSQLTLSMaxVersion13 allows TLS 1.3 on the client leg.
	MSSQLTLSMaxVersion13 = "1.3"
	// MSSQLDefaultTLSMaxVersion is what an unset variable means.
	//
	// It is 1.2 on purpose, and not for lack of ambition: under TLS 1.2 both
	// peers end their handshake on a *read*, so the framed→raw switch that TDS
	// encapsulation forces lands on the same byte for both. Under 1.3 the
	// client ends on a *write* and drivers differ on whether that last flight
	// is still encapsulated. dbbat handles both (see internal/proxy/mssql),
	// but only go-mssqldb has been verified end to end, so an existing
	// deployment must not change behavior merely by upgrading.
	MSSQLDefaultTLSMaxVersion = MSSQLTLSMaxVersion12
)

// ErrMSSQLTLSMaxVersionInvalid is returned when DBB_MSSQL_TLS_MAX_VERSION holds
// something other than "1.2" or "1.3". It fails the process at startup rather
// than silently falling back, because a deployment that asked for a TLS floor
// and quietly got a different one is exactly the sort of thing nobody notices.
var ErrMSSQLTLSMaxVersionInvalid = errors.New(
	`invalid DBB_MSSQL_TLS_MAX_VERSION: want "1.2" or "1.3"`)

// ResolveTLSMaxVersion validates TLSMaxVersion and returns the crypto/tls
// constant it names. An empty value resolves to MSSQLDefaultTLSMaxVersion.
func (c MSSQLConfig) ResolveTLSMaxVersion() (uint16, error) {
	switch strings.TrimSpace(c.TLSMaxVersion) {
	case "", MSSQLDefaultTLSMaxVersion:
		return tls.VersionTLS12, nil
	case MSSQLTLSMaxVersion13:
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("%w: got %q", ErrMSSQLTLSMaxVersionInvalid, c.TLSMaxVersion)
	}
}

// PGConfig holds configuration specific to the PostgreSQL proxy.
type PGConfig struct {
	// TLS holds TLS server-termination settings for the proxy. When enabled,
	// the proxy responds 'S' to SSLRequest and terminates TLS at the proxy.
	// Without this, clients with sslmode=prefer silently fall back to
	// plaintext and credentials travel over the wire in the clear.
	TLS TLSConfig `koanf:"tls"`
}

// TLSConfig holds TLS server-side termination settings.
//
// When CertFile and KeyFile are both empty (and Disable is false), the
// proxy auto-generates a self-signed certificate at startup. This is
// suitable for development; production deployments should provide a real
// certificate.
type TLSConfig struct {
	// CertFile is the path to a PEM-encoded server certificate.
	CertFile string `koanf:"cert_file"`

	// KeyFile is the path to a PEM-encoded server private key.
	KeyFile string `koanf:"key_file"`

	// Disable turns off TLS termination entirely. When true, SSL Request
	// packets from clients are refused and connections stay plaintext.
	Disable bool `koanf:"disable"`
}

// Config holds the application configuration.
type Config struct {
	// Proxy listen address.
	ListenPG string `koanf:"listen_pg"`

	// Oracle proxy listen address (empty = disabled).
	ListenOracle string `koanf:"listen_ora"`

	// MySQL proxy listen address (empty = disabled).
	ListenMySQL string `koanf:"listen_mysql"`

	// MongoDB proxy listen address (empty = disabled).
	ListenMongo string `koanf:"listen_mongo"`

	// SQL Server (TDS) proxy listen address (empty = disabled).
	ListenMSSQL string `koanf:"listen_mssql"`

	// REST API listen address.
	ListenAPI string `koanf:"listen_api"`

	// PostgreSQL DSN for DBBat storage.
	DSN string `koanf:"dsn"`

	// Base64-encoded encryption key (alternative to KeyFile).
	Key string `koanf:"key"`

	// Path to file containing encryption key (alternative to Key).
	KeyFile string `koanf:"keyfile"`

	// ConfigFile path (not loaded from config, set via CLI).
	ConfigFile string `koanf:"-"`

	// Encryption key for database credentials (32 bytes).
	// Populated from Key or KeyFile after loading.
	EncryptionKey []byte `koanf:"-"`

	// RunMode controls whether test data is provisioned on startup.
	RunMode RunMode `koanf:"run_mode"`

	// InstanceID identifies this dbbat process among the replicas sharing the
	// same store. It is stamped on every connection row, alongside the run id
	// the store mints at startup, so the reconcile of crash-orphaned
	// connections can tell a dead run's rows from a live one's. Defaults to the
	// hostname, which is the pod name under Kubernetes. Empty is not a valid
	// runtime value — Load fills it in.
	InstanceID string `koanf:"instance_id"`

	// DemoTargetDB specifies the only allowed database target in demo mode.
	// Format: "user:password@host/dbname" (e.g., "demo:demo@localhost/demo")
	// Only applies when RunMode is "demo". If empty, defaults to "demo:demo@localhost/demo".
	DemoTargetDB string `koanf:"demo_target_db"`

	// QueryStorage holds query result storage configuration.
	QueryStorage QueryStorageConfig `koanf:"query_storage"`

	// RateLimit holds rate limiting configuration.
	RateLimit RateLimitConfig `koanf:"rate_limit"`

	// Hash holds password hashing configuration.
	Hash HashConfig `koanf:"hash"`

	// AuthCache holds authentication cache configuration.
	AuthCache AuthCacheConfig `koanf:"auth_cache"`

	// BaseURL is the base URL path for the frontend app (default: "/app").
	BaseURL string `koanf:"base_url"`

	// Redirects contains dev redirect rules parsed from DBB_REDIRECTS env var.
	// Not loaded from config file, parsed from environment only.
	Redirects []RedirectRule `koanf:"-"`

	// LogLevel controls the logging verbosity (debug, info, warn, error).
	// Default is "info".
	LogLevel string `koanf:"log_level"`

	// SlackAuth holds Slack OAuth configuration.
	SlackAuth SlackAuthConfig `koanf:"slack_auth"`

	// Auth holds the auto-provisioning settings shared by every OAuth/OIDC
	// login provider.
	Auth OAuthUsersConfig `koanf:"auth"`

	// OIDCAuth holds the generic OpenID Connect login provider.
	OIDCAuth OIDCAuthConfig `koanf:"oidc"`

	// SlackNotify holds outbound Slack notification configuration for
	// grant request events.
	SlackNotify SlackNotifyConfig `koanf:"slack_notify"`

	// PublicURL is the externally reachable base URL for this dbbat
	// instance. Used to build deep-links in Slack notifications. Required
	// only if SlackNotify is enabled.
	PublicURL string `koanf:"public_url"`

	// Dump holds session packet dump configuration.
	Dump DumpConfig `koanf:"dump"`

	// MySQL holds MySQL proxy specific configuration.
	MySQL MySQLConfig `koanf:"mysql"`

	// Mongo holds MongoDB proxy specific configuration.
	Mongo MongoConfig `koanf:"mongo"`

	// MSSQL holds SQL Server proxy specific configuration.
	MSSQL MSSQLConfig `koanf:"mssql"`

	// PG holds PostgreSQL proxy specific configuration.
	PG PGConfig `koanf:"pg"`

	// Approval holds pattern-triggered approval-hold configuration.
	Approval ApprovalConfig `koanf:"approval"`

	// MCP holds the Model Context Protocol endpoint configuration.
	MCP MCPConfig `koanf:"mcp"`
}

// MCPConfig configures the Model Context Protocol endpoint that lets AI
// agents query databases through dbbat.
//
// Enabled defaults to **true**, deliberately unlike ApprovalConfig. The
// endpoint is API-key gated, and every statement it runs is executed by
// dialing dbbat's own proxy listener as the key's owner — so it grants an
// agent nothing the key holder could not already do with psql, and it adds no
// enforcement path of its own. Turning it off is for deployments that want the
// route surface gone entirely, not a safety default.
type MCPConfig struct {
	// Enabled registers POST/GET/DELETE /api/v1/mcp. When false the routes do
	// not exist at all — a disabled feature should not answer, not even 403.
	Enabled bool `koanf:"enabled"`
}

// Default query storage limits.
const (
	DefaultMaxResultRows  = 100000
	DefaultMaxResultBytes = 100 * 1024 * 1024 // 100MB
)

// Default rate limiting settings.
const (
	DefaultRateLimitEnabled = true
	DefaultRateLimitRPM     = 60
	DefaultRateLimitRPMAnon = 10
	DefaultRateLimitBurst   = 10
)

// Default hash settings (matching current argon2id defaults).
const (
	DefaultHashMemoryMB = 64
	DefaultHashTime     = 1
	DefaultHashThreads  = 4
)

// Default auth cache settings.
const (
	DefaultAuthCacheEnabled    = true
	DefaultAuthCacheTTLSeconds = 300 // 5 minutes
	DefaultAuthCacheMaxSize    = 10000
)

const expectedKeySize = 32

// Default key file constants.
const (
	defaultKeyDirName  = ".dbbat"
	defaultKeyFileName = "key"
	defaultKeyDirPerm  = 0o700
	defaultKeyFilePerm = 0o600
)

// DefaultBaseURL is the default base URL path for the frontend.
const DefaultBaseURL = "/app"

// DefaultLogLevel is the default log level.
const DefaultLogLevel = "info"

// defaultConfig returns a Config with default values.
func defaultConfig() Config {
	return Config{
		ListenPG:     ":5433",
		ListenAPI:    ":4200",
		ListenOracle: ":1522",
		ListenMySQL:  ":3307",
		ListenMongo:  ":27018",
		// 1434/tcp is free: the SQL Server Browser service that owns 1434 is
		// UDP-only, so the +1 convention the other listeners use still works.
		ListenMSSQL: ":1434",
		MSSQL: MSSQLConfig{
			TLSMaxVersion: MSSQLDefaultTLSMaxVersion,
		},
		BaseURL:  DefaultBaseURL,
		LogLevel: DefaultLogLevel,
		QueryStorage: QueryStorageConfig{
			MaxResultRows:  DefaultMaxResultRows,
			MaxResultBytes: DefaultMaxResultBytes,
			StoreResults:   true,
			Retention:      DefaultQueryStorageRetention,
		},
		RateLimit: RateLimitConfig{
			Enabled:               DefaultRateLimitEnabled,
			RequestsPerMinute:     DefaultRateLimitRPM,
			RequestsPerMinuteAnon: DefaultRateLimitRPMAnon,
			Burst:                 DefaultRateLimitBurst,
		},
		Hash: HashConfig{
			MemoryMB: DefaultHashMemoryMB,
			Time:     DefaultHashTime,
			Threads:  DefaultHashThreads,
		},
		AuthCache: AuthCacheConfig{
			Enabled:    DefaultAuthCacheEnabled,
			TTLSeconds: DefaultAuthCacheTTLSeconds,
			MaxSize:    DefaultAuthCacheMaxSize,
		},
		Auth: OAuthUsersConfig{
			AutoCreateUsers: true,
			DefaultRole:     DefaultOAuthRole,
		},
		MCP: MCPConfig{Enabled: true},
		OIDCAuth: OIDCAuthConfig{
			Scopes:      DefaultOIDCScopes,
			DisplayName: DefaultOIDCDisplayName,
			GroupsClaim: DefaultOIDCGroupsClaim,
		},
		SlackNotify: SlackNotifyConfig{
			Channel: "#dbbat",
		},
		Dump: DumpConfig{
			MaxSize:   DefaultDumpMaxSize,
			Retention: DefaultDumpRetention,
		},
		Approval: ApprovalConfig{
			// Off by default: a hold blocks a live database connection on a
			// human. Operators opt in.
			Enabled:    false,
			SlackDelay: DefaultApprovalSlackDelay,
			SlackSQL:   true,
		},
	}
}

// LoadOptions configures how configuration is loaded.
type LoadOptions struct {
	// ConfigFile is the path to a config file (YAML, JSON, or TOML).
	ConfigFile string
}

// koanfDelim is the delimiter used for nested config keys in koanf.
const koanfDelim = "."

// envTransform transforms environment variable names to koanf keys.
// DBB_LISTEN_PG -> listen_pg
// DBB_QUERY_STORAGE_MAX_RESULT_ROWS -> query_storage.max_result_rows
// DBB_RATE_LIMIT_ENABLED -> rate_limit.enabled
// DBB_HASH_MEMORY_MB -> hash.memory_mb
// DBB_AUTH_CACHE_ENABLED -> auth_cache.enabled
func envTransform(k, v string) (string, any) {
	key := strings.ToLower(strings.TrimPrefix(k, "DBB_"))
	// Map known prefixes to nested paths
	// query_storage_* -> query_storage.*
	if strings.HasPrefix(key, "query_storage_") {
		return "query_storage." + strings.TrimPrefix(key, "query_storage_"), v
	}
	// rate_limit_* -> rate_limit.*
	if strings.HasPrefix(key, "rate_limit_") {
		return "rate_limit." + strings.TrimPrefix(key, "rate_limit_"), v
	}
	// hash_* -> hash.*
	if strings.HasPrefix(key, "hash_") {
		return "hash." + strings.TrimPrefix(key, "hash_"), v
	}
	// auth_cache_* -> auth_cache.*
	if strings.HasPrefix(key, "auth_cache_") {
		return "auth_cache." + strings.TrimPrefix(key, "auth_cache_"), v
	}
	// auth_* -> auth.* (DBB_AUTH_AUTO_CREATE_USERS, DBB_AUTH_DEFAULT_ROLE).
	// Must stay *after* the auth_cache_ rule above, which it would otherwise
	// swallow into auth.cache_*.
	if strings.HasPrefix(key, "auth_") {
		return "auth." + strings.TrimPrefix(key, "auth_"), v
	}
	// slack_auth_* -> slack_auth.*
	if strings.HasPrefix(key, "slack_auth_") {
		return "slack_auth." + strings.TrimPrefix(key, "slack_auth_"), v
	}
	// oidc_* -> oidc.*
	if strings.HasPrefix(key, "oidc_") {
		return "oidc." + strings.TrimPrefix(key, "oidc_"), v
	}
	// slack_signing_secret -> slack_notify.signing_secret
	// DBB_SLACK_SIGNING_SECRET is the canonical, documented name; the
	// slack_notify_* prefix rule below keeps the legacy
	// DBB_SLACK_NOTIFY_SIGNING_SECRET working as an accepted alias.
	if key == "slack_signing_secret" {
		return "slack_notify.signing_secret", v
	}
	// slack_notify_* -> slack_notify.*
	if strings.HasPrefix(key, "slack_notify_") {
		return "slack_notify." + strings.TrimPrefix(key, "slack_notify_"), v
	}
	// dump_* -> dump.*
	if strings.HasPrefix(key, "dump_") {
		return "dump." + strings.TrimPrefix(key, "dump_"), v
	}
	// mysql_tls_* -> mysql.tls.*
	if strings.HasPrefix(key, "mysql_tls_") {
		return "mysql.tls." + strings.TrimPrefix(key, "mysql_tls_"), v
	}
	// mongo_tls_* -> mongo.tls.*
	if strings.HasPrefix(key, "mongo_tls_") {
		return "mongo.tls." + strings.TrimPrefix(key, "mongo_tls_"), v
	}
	// mssql_tls_max_version -> mssql.tls_max_version
	//
	// This one is deliberately *not* under mssql.tls.*: the ceiling is a TDS
	// encapsulation setting on MSSQLConfig, not one of the cert/key/disable
	// knobs the five proxies share. It has to be tested before the mssql_tls_
	// prefix rule below, which would otherwise swallow it.
	if key == "mssql_tls_max_version" {
		return "mssql.tls_max_version", v
	}
	// mssql_tls_* -> mssql.tls.*
	if strings.HasPrefix(key, "mssql_tls_") {
		return "mssql.tls." + strings.TrimPrefix(key, "mssql_tls_"), v
	}
	// pg_tls_* -> pg.tls.*
	if strings.HasPrefix(key, "pg_tls_") {
		return "pg.tls." + strings.TrimPrefix(key, "pg_tls_"), v
	}
	// approval_* -> approval.*
	if strings.HasPrefix(key, "approval_") {
		return "approval." + strings.TrimPrefix(key, "approval_"), v
	}
	// mcp_* -> mcp.*
	if strings.HasPrefix(key, "mcp_") {
		return "mcp." + strings.TrimPrefix(key, "mcp_"), v
	}
	return key, v
}

// authProvisioningAliases maps the pre-rename keys the two auto-provisioning
// settings used to live under onto their provider-agnostic home. Both the
// legacy environment variables (DBB_SLACK_AUTH_*, through the slack_auth_
// prefix rule in envTransform) and the legacy config-file keys land on the left
// side, so one table covers both.
var authProvisioningAliases = map[string]string{
	"slack_auth.auto_create_users": "auth.auto_create_users",
	"slack_auth.default_role":      "auth.default_role",
}

// canonicalAuthProvisioningEnv maps the canonical environment variables onto
// the same keys, for the deterministic re-apply below.
var canonicalAuthProvisioningEnv = map[string]string{
	"DBB_AUTH_AUTO_CREATE_USERS": "auth.auto_create_users",
	"DBB_AUTH_DEFAULT_ROLE":      "auth.default_role",
}

// applyAuthProvisioningAliases resolves DBB_AUTH_* against the legacy
// DBB_SLACK_AUTH_* names: explicit canonical wins, then the legacy alias, then
// the default.
//
// Silently ignoring the legacy names would flip auto-provisioning back on for
// every deployment that turned it off, on nothing more than an upgrade.
//
// The canonical variable is re-applied from the environment explicitly, exactly
// like DBB_SLACK_SIGNING_SECRET above: the alias promotion below runs after the
// env provider, so without this a legacy value set alongside a canonical one
// would overwrite it.
func applyAuthProvisioningAliases(k *koanf.Koanf) error {
	for legacy, canonical := range authProvisioningAliases {
		if !k.Exists(legacy) {
			continue
		}

		if err := k.Set(canonical, k.Get(legacy)); err != nil {
			return fmt.Errorf("failed to apply legacy %s: %w", legacy, err)
		}
	}

	for name, canonical := range canonicalAuthProvisioningEnv {
		if v := os.Getenv(name); v != "" {
			if err := k.Set(canonical, v); err != nil {
				return fmt.Errorf("failed to apply %s: %w", name, err)
			}
		}
	}

	return nil
}

// Load reads configuration from environment variables and optional config file.
// Priority order: CLI overrides > Environment variables > Config file > Defaults
func Load(opts LoadOptions, cliOverrides ...func(*Config)) (*Config, error) {
	k := koanf.New(koanfDelim)

	// 1. Load defaults
	if err := k.Load(structs.Provider(defaultConfig(), "koanf"), nil); err != nil {
		return nil, fmt.Errorf("failed to load defaults: %w", err)
	}

	// 2. Determine config file path (CLI option takes precedence over DBB_CONFIG env var)
	configPath := opts.ConfigFile
	if configPath == "" {
		// Load env vars first just to check for DBB_CONFIG
		envK := koanf.New(koanfDelim)
		if err := envK.Load(env.Provider(koanfDelim, env.Opt{Prefix: "DBB_", TransformFunc: envTransform}), nil); err == nil {
			configPath = envK.String("config")
		}
	}

	// 3. Load config file if specified
	if configPath != "" {
		if err := loadConfigFile(k, configPath); err != nil {
			return nil, fmt.Errorf("failed to load config file: %w", err)
		}
	}

	// 4. Load environment variables (DBB_ prefix) - these override config file values
	if err := k.Load(env.Provider(koanfDelim, env.Opt{Prefix: "DBB_", TransformFunc: envTransform}), nil); err != nil {
		return nil, fmt.Errorf("failed to load environment variables: %w", err)
	}

	// Both DBB_SLACK_SIGNING_SECRET (canonical) and the legacy
	// DBB_SLACK_NOTIFY_SIGNING_SECRET map to slack_notify.signing_secret, and
	// the env provider gives no ordering guarantee when both are set. Re-apply
	// the canonical variable explicitly so it deterministically wins.
	if v := os.Getenv("DBB_SLACK_SIGNING_SECRET"); v != "" {
		if err := k.Set("slack_notify.signing_secret", v); err != nil {
			return nil, fmt.Errorf("failed to apply DBB_SLACK_SIGNING_SECRET: %w", err)
		}
	}

	if err := applyAuthProvisioningAliases(k); err != nil {
		return nil, err
	}

	// Unmarshal into Config struct
	cfg := &Config{}
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// 4. Apply CLI overrides (highest priority)
	for _, override := range cliOverrides {
		override(cfg)
	}

	// Validate required fields
	if cfg.DSN == "" {
		return nil, ErrDSNRequired
	}

	if cfg.Dump.UploadURL != "" && cfg.Dump.Dir == "" {
		return nil, ErrDumpUploadNeedsDir
	}

	// Fail here rather than in the proxy: a typo'd TLS ceiling must stop the
	// process, not silently leave the listener on the default.
	if _, err := cfg.MSSQL.ResolveTLSMaxVersion(); err != nil {
		return nil, err
	}

	if cfg.OIDCAuth.Enabled() && (cfg.OIDCAuth.ClientID == "" || cfg.OIDCAuth.ClientSecret == "") {
		return nil, ErrOIDCClientCredentialsRequired
	}

	// The mapping decides who is an admin, so it is parsed here — a typo stops
	// the process instead of quietly degrading to "nobody matches", which at
	// the next login would demote every mapped user.
	if _, err := cfg.OIDCAuth.ParseRoleMapping(); err != nil {
		return nil, err
	}

	// Same reasoning one rung down: the default role is what an unmatched user
	// ends up with, so a typo must not survive startup.
	if err := cfg.Auth.Validate(); err != nil {
		return nil, err
	}

	// Load encryption key from Key or KeyFile
	key, err := loadEncryptionKey(cfg.Key, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load encryption key: %w", err)
	}

	cfg.EncryptionKey = key

	// Parse redirects from DBB_REDIRECTS environment variable
	cfg.Redirects = parseRedirects(os.Getenv("DBB_REDIRECTS"))

	// Normalize base URL
	cfg.BaseURL = normalizeBaseURL(cfg.BaseURL)

	cfg.InstanceID = resolveInstanceID(cfg.InstanceID)

	return cfg, nil
}

// FallbackInstanceID is used when no DBB_INSTANCE_ID is set and the hostname
// cannot be read. It is deliberately a constant rather than a random value: it
// is what attributes a connection row to a recognizable owner, and a value that
// changed on every start would leave a trail of ids nobody can interpret.
//
// It is also the one way replicas can end up sharing an id without anyone
// asking for it — every replica that cannot read its hostname lands here. That
// is no longer dangerous: the reconcile keys on the per-run id the store mints
// at startup, which cannot be shared, so replicas sharing this value still
// cannot close each other's connections. It stays undesirable — several
// processes then answer to one identity in the logs, the UI and the reclaim's
// counts — and reaching it at all takes a broken container; see
// resolveInstanceID.
const FallbackInstanceID = "dbbat"

// resolveInstanceID fills in the instance id when it was not configured.
//
// The hostname is the right default — under Kubernetes it is the pod name, so
// replicas sharing a store do not collide. A pod name that changes on every
// restart is not a problem: Store.CloseOrphanedConnections tracks liveness in
// the instances table, so a replacement pod reclaims its predecessor's orphans
// without recognizing its id.
//
// Setting the same DBB_INSTANCE_ID on several replicas is no longer unsafe —
// every process also mints a run id the reconcile keys on, so a starting
// replica cannot close a live peer's sessions whatever the id says — but it is
// still not worth doing. It buys nothing (nothing has recognized an id since
// liveness tracking landed) and it costs clarity: several processes answer to
// one identity in the logs and in the UI, and the reconcile's "left open by a
// previous run" count then covers the whole id rather than this process.
// A process that finds itself sharing an id says so once; see
// checkSharedInstanceID.
func resolveInstanceID(configured string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}

	if hostname, err := os.Hostname(); err == nil {
		if hostname = strings.TrimSpace(hostname); hostname != "" {
			return hostname
		}
	}

	return FallbackInstanceID
}

// loadConfigFile loads configuration from a file based on its extension.
func loadConfigFile(k *koanf.Koanf, path string) error {
	var parser koanf.Parser

	switch {
	case strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml"):
		parser = yaml.Parser()
	case strings.HasSuffix(path, ".json"):
		parser = json.Parser()
	case strings.HasSuffix(path, ".toml"):
		parser = toml.Parser()
	default:
		// Default to YAML
		parser = yaml.Parser()
	}

	return k.Load(file.Provider(path), parser)
}

// loadEncryptionKey loads the encryption key from base64 string, file, or default location.
func loadEncryptionKey(keyStr, keyFile string) ([]byte, error) {
	// Try base64-encoded key first
	if keyStr != "" {
		key, err := base64.StdEncoding.DecodeString(keyStr)
		if err != nil {
			return nil, fmt.Errorf("failed to decode key: %w", err)
		}

		if len(key) != expectedKeySize {
			return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKeySize, len(key))
		}

		return key, nil
	}

	// Try key file
	if keyFile != "" {
		key, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read key file: %w", err)
		}

		if len(key) != expectedKeySize {
			return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKeySize, len(key))
		}

		return key, nil
	}

	// Fall back to default key file (~/.dbbat/key)
	return loadOrCreateDefaultKey()
}

// DefaultKeyFilePath returns the path to the default key file (~/.dbbat/key).
func DefaultKeyFilePath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	return filepath.Join(homeDir, defaultKeyDirName, defaultKeyFileName), nil
}

// loadOrCreateDefaultKey loads the key from the default location, creating it if necessary.
func loadOrCreateDefaultKey() ([]byte, error) {
	keyPath, err := DefaultKeyFilePath()
	if err != nil {
		return nil, err
	}

	// Try to read existing key file
	content, err := os.ReadFile(keyPath)
	if err == nil {
		// File exists, decode the base64 key
		keyStr := strings.TrimSpace(string(content))
		key, decodeErr := base64.StdEncoding.DecodeString(keyStr)
		if decodeErr != nil {
			return nil, fmt.Errorf("failed to decode key from %s: %w", keyPath, decodeErr)
		}

		if len(key) != expectedKeySize {
			return nil, fmt.Errorf("%w: got %d bytes from %s", ErrInvalidKeySize, len(key), keyPath)
		}

		return key, nil
	}

	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read key file %s: %w", keyPath, err)
	}

	// File doesn't exist, create a new key
	return generateAndSaveDefaultKey(keyPath)
}

// generateAndSaveDefaultKey generates a new encryption key and saves it to the default location.
func generateAndSaveDefaultKey(keyPath string) ([]byte, error) {
	// Create the directory if it doesn't exist
	keyDir := filepath.Dir(keyPath)
	if err := os.MkdirAll(keyDir, defaultKeyDirPerm); err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", keyDir, err)
	}

	// Ensure directory has correct permissions
	if err := os.Chmod(keyDir, defaultKeyDirPerm); err != nil {
		return nil, fmt.Errorf("failed to set permissions on %s: %w", keyDir, err)
	}

	// Generate a new random key
	key := make([]byte, expectedKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("failed to generate encryption key: %w", err)
	}

	// Encode as base64
	keyBase64 := base64.StdEncoding.EncodeToString(key)

	// Write to file
	if err := os.WriteFile(keyPath, []byte(keyBase64+"\n"), defaultKeyFilePerm); err != nil {
		return nil, fmt.Errorf("failed to write key file %s: %w", keyPath, err)
	}

	slog.WarnContext(context.Background(), "generated new encryption key",
		slog.String("path", keyPath),
		slog.String("warning", "losing this key means encrypted credentials cannot be recovered"))

	return key, nil
}

// parseRedirects parses the DBB_REDIRECTS environment variable.
// Format: /path:host:port/targetpath,/path2:host2:port2/targetpath2
// Example: /app:localhost:5173
// If the target path is omitted, it defaults to "/".
func parseRedirects(value string) []RedirectRule {
	if value == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	rules := make([]RedirectRule, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		rule, ok := parseRedirectRule(part)
		if !ok {
			slog.WarnContext(context.Background(), "Invalid redirect rule, skipping", slog.String("rule", part))

			continue
		}

		rules = append(rules, rule)
	}

	if len(rules) > 0 {
		slog.InfoContext(context.Background(), "Loaded redirect rules", slog.Int("count", len(rules)))

		for i := range rules {
			r := &rules[i]
			slog.DebugContext(context.Background(), "Redirect rule",
				slog.String("pathPrefix", r.PathPrefix),
				slog.String("targetHost", r.TargetHost),
				slog.String("targetPath", r.TargetPath))
		}
	}

	return rules
}

// parseRedirectRule parses a single redirect rule.
// Format: /path:host:port/targetpath or /path:host:port
func parseRedirectRule(rule string) (RedirectRule, bool) {
	if !strings.HasPrefix(rule, "/") {
		return RedirectRule{}, false
	}

	colonIdx := strings.Index(rule, ":")
	if colonIdx == -1 {
		return RedirectRule{}, false
	}

	pathPrefix := rule[:colonIdx]
	target := rule[colonIdx+1:]

	if target == "" {
		return RedirectRule{}, false
	}

	var targetHost, targetPath string

	slashIdx := strings.Index(target, "/")

	if slashIdx == -1 {
		// No path in target, e.g., "localhost:5173"
		targetHost = target
		targetPath = "/" // Default to root
	} else {
		// Has path, e.g., "localhost:5173/app"
		targetHost = target[:slashIdx]
		targetPath = target[slashIdx:]
	}

	if targetHost == "" {
		return RedirectRule{}, false
	}

	return RedirectRule{
		PathPrefix: pathPrefix,
		TargetHost: targetHost,
		TargetPath: targetPath,
	}, true
}

// normalizeBaseURL ensures the base URL starts with "/" and doesn't end with "/".
func normalizeBaseURL(baseURL string) string {
	if baseURL == "" || baseURL == "/" {
		return ""
	}

	// Ensure leading slash
	if !strings.HasPrefix(baseURL, "/") {
		baseURL = "/" + baseURL
	}

	// Remove trailing slash
	baseURL = strings.TrimSuffix(baseURL, "/")

	return baseURL
}

// DemoTarget holds the parsed demo target database credentials.
type DemoTarget struct {
	Username string
	Password string
	Host     string
	Server   string
}

// DefaultDemoTargetDB is the default value for DemoTargetDB.
const DefaultDemoTargetDB = "demo:demo@localhost/demo"

// IsDemoMode returns true if running in demo mode.
func (c *Config) IsDemoMode() bool {
	return c.RunMode == RunModeDemo
}

// GetDemoTarget parses and returns the demo target configuration.
// Returns nil if not in demo mode.
func (c *Config) GetDemoTarget() *DemoTarget {
	if !c.IsDemoMode() {
		return nil
	}

	targetDB := c.DemoTargetDB
	if targetDB == "" {
		targetDB = DefaultDemoTargetDB
	}

	return ParseDemoTargetDB(targetDB)
}

// ParseDemoTargetDB parses a demo target string in format "user:pass@host/dbname".
func ParseDemoTargetDB(s string) *DemoTarget {
	// Find @ separator
	atIdx := strings.LastIndex(s, "@")
	if atIdx == -1 {
		return nil
	}

	userPass := s[:atIdx]
	hostDB := s[atIdx+1:]

	// Find : separator in user:pass
	colonIdx := strings.Index(userPass, ":")
	if colonIdx == -1 {
		return nil
	}

	// Find / separator in host/dbname
	slashIdx := strings.Index(hostDB, "/")
	if slashIdx == -1 {
		return nil
	}

	return &DemoTarget{
		Username: userPass[:colonIdx],
		Password: userPass[colonIdx+1:],
		Host:     hostDB[:slashIdx],
		Server:   hostDB[slashIdx+1:],
	}
}

// ValidateDemoTarget checks if the given credentials match the demo target.
// Returns an error message if validation fails, or empty string if valid.
func (c *Config) ValidateDemoTarget(username, password, host, database string) string {
	if !c.IsDemoMode() {
		return ""
	}

	target := c.GetDemoTarget()
	if target == nil {
		return ""
	}

	if username != target.Username || password != target.Password || host != target.Host || database != target.Server {
		return fmt.Sprintf("you can only use %s:%s@%s/%s in demo mode", target.Username, target.Password, target.Host, target.Server)
	}

	return ""
}

// ResolvedHashParams returns the hash parameters after applying presets.
// Individual settings override preset values.
type ResolvedHashParams struct {
	MemoryKB uint32
	Time     uint32
	Threads  uint8
}

// Hash presets.
var hashPresets = map[string]ResolvedHashParams{
	"default": {MemoryKB: 64 * 1024, Time: 1, Threads: 4},
	"low":     {MemoryKB: 16 * 1024, Time: 2, Threads: 2},
	"minimal": {MemoryKB: 4 * 1024, Time: 3, Threads: 1},
}

// GetHashParams returns the resolved hash parameters.
func (c *Config) GetHashParams() ResolvedHashParams {
	// Start with default preset
	params := hashPresets["default"]

	// In test mode, use minimal preset by default for faster test execution
	if c.RunMode == RunModeTest && c.Hash.Preset == "" {
		params = hashPresets["minimal"]
		slog.DebugContext(context.Background(), "using minimal hash preset for test mode")
	}

	// Apply preset if specified (overrides test mode default)
	if c.Hash.Preset != "" {
		if preset, ok := hashPresets[c.Hash.Preset]; ok {
			params = preset
		} else {
			slog.WarnContext(context.Background(), "unknown hash preset, using default", slog.String("preset", c.Hash.Preset))
		}
	}

	// Override with individual settings if specified
	if c.Hash.MemoryMB > 0 {
		params.MemoryKB = uint32(c.Hash.MemoryMB) * 1024
	}
	if c.Hash.Time > 0 {
		params.Time = uint32(c.Hash.Time)
	}
	if c.Hash.Threads > 0 {
		params.Threads = uint8(c.Hash.Threads)
	}

	// Log warning if using weak parameters
	if params.MemoryKB < 16*1024 {
		slog.WarnContext(context.Background(), "using low-security hash parameters",
			slog.Any("memory_kb", params.MemoryKB),
			slog.Int("recommended_min_kb", 16*1024))
	}

	return params
}

// ParseLogLevel parses a log level string and returns the corresponding slog.Level.
// Supported values (case-insensitive): debug, info, warn, warning, error.
// Returns slog.LevelInfo for invalid values.
func ParseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		slog.WarnContext(context.Background(), "invalid log level, using default",
			slog.String("level", level),
			slog.String("default", DefaultLogLevel))
		return slog.LevelInfo
	}
}

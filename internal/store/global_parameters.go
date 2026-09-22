package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/fclairamb/dbbat/internal/config"
)

// ErrParameterNotFound is returned when no matching active parameter exists.
var ErrParameterNotFound = errors.New("parameter not found")

// GetParameter retrieves a single active parameter by group and key.
func (s *Store) GetParameter(ctx context.Context, groupKey, key string) (*GlobalParameter, error) {
	param := new(GlobalParameter)
	err := s.db.NewSelect().
		Model(param).
		Where("group_key = ?", groupKey).
		Where("key = ?", key).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrParameterNotFound
		}
		return nil, fmt.Errorf("failed to get parameter: %w", err)
	}
	return param, nil
}

// GetParameters retrieves all active parameters for a group.
func (s *Store) GetParameters(ctx context.Context, groupKey string) ([]GlobalParameter, error) {
	var params []GlobalParameter
	err := s.db.NewSelect().
		Model(&params).
		Where("group_key = ?", groupKey).
		Order("key ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get parameters: %w", err)
	}
	if params == nil {
		params = []GlobalParameter{}
	}
	return params, nil
}

// GetAllParameters retrieves all active parameters, optionally filtered by group.
func (s *Store) GetAllParameters(ctx context.Context, groupKey string) ([]GlobalParameter, error) {
	var params []GlobalParameter
	q := s.db.NewSelect().
		Model(&params).
		Order("group_key ASC", "key ASC")
	if groupKey != "" {
		q = q.Where("group_key = ?", groupKey)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list parameters: %w", err)
	}
	if params == nil {
		params = []GlobalParameter{}
	}
	return params, nil
}

// SetParameter creates or updates a parameter (upsert on group_key+key).
func (s *Store) SetParameter(ctx context.Context, groupKey, key, value string) error {
	_, err := s.db.NewRaw(
		`INSERT INTO global_parameters (group_key, key, value, updated_at)
		VALUES (?, ?, ?, NOW())
		ON CONFLICT (group_key, key) WHERE deleted_at IS NULL
		DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`,
		groupKey, key, value,
	).Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to set parameter: %w", err)
	}
	return nil
}

// DeleteParameter soft-deletes a parameter.
func (s *Store) DeleteParameter(ctx context.Context, groupKey, key string) error {
	result, err := s.db.NewDelete().
		Model((*GlobalParameter)(nil)).
		Where("group_key = ?", groupKey).
		Where("key = ?", key).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to delete parameter: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrParameterNotFound
	}
	return nil
}

// Public endpoint parameter group and key constants.
const (
	GroupPublic        = "public"
	KeyPublicHost      = "host"
	KeyPublicPGHost    = "pg.host"
	KeyPublicOraHost   = "ora.host"
	KeyPublicMySQLHost = "mysql.host"
	KeyPublicMongoHost = "mongo.host"
	KeyPublicMSSQLHost = "mssql.host"
	KeyPublicPGPort    = "pg.port"
	KeyPublicOraPort   = "ora.port"
	KeyPublicMySQLPort = "mysql.port"
	KeyPublicMongoPort = "mongo.port"
	KeyPublicMSSQLPort = "mssql.port"
	// KeyPublicWebUIURL is the operator-editable Web UI / public base URL
	// (e.g. "https://dbbat.company.com"), reached through an HTTP ingress /
	// reverse proxy. Distinct from Host/PGHost/etc, which advertise the
	// *connection* host reached via direct / TCP load-balancer access.
	KeyPublicWebUIURL = "web_ui_url"
)

// PublicEndpoints holds the operator-configured public advertisement settings.
type PublicEndpoints struct {
	Host      string // default public hostname for all protocols (connection host)
	PGHost    string // optional override; "" = fall back to Host
	OraHost   string
	MySQLHost string
	MongoHost string
	MSSQLHost string
	PGPort    *int // optional override; nil = fall back to local listen port
	OraPort   *int
	MySQLPort *int
	MongoPort *int
	MSSQLPort *int
	// WebUIURL is the operator-configured public base URL for the Web UI /
	// REST API (e.g. "https://dbbat.company.com"), used for Slack deep-links
	// and absolute-URL generation. Independent of Host: the UI is typically
	// reached through an HTTP ingress while Host is reached via TCP
	// load-balancer / direct access.
	WebUIURL string
}

// GetPublicEndpoints reads all public.* parameters and returns the typed struct.
func (s *Store) GetPublicEndpoints(ctx context.Context) (PublicEndpoints, error) {
	params, err := s.GetParameters(ctx, GroupPublic)
	if err != nil {
		return PublicEndpoints{}, err
	}
	var pe PublicEndpoints
	for _, p := range params {
		switch p.Key {
		case KeyPublicHost:
			pe.Host = p.Value
		case KeyPublicPGHost:
			pe.PGHost = p.Value
		case KeyPublicOraHost:
			pe.OraHost = p.Value
		case KeyPublicMySQLHost:
			pe.MySQLHost = p.Value
		case KeyPublicMongoHost:
			pe.MongoHost = p.Value
		case KeyPublicMSSQLHost:
			pe.MSSQLHost = p.Value
		case KeyPublicPGPort:
			if n, err := strconv.Atoi(p.Value); err == nil {
				pe.PGPort = &n
			}
		case KeyPublicOraPort:
			if n, err := strconv.Atoi(p.Value); err == nil {
				pe.OraPort = &n
			}
		case KeyPublicMySQLPort:
			if n, err := strconv.Atoi(p.Value); err == nil {
				pe.MySQLPort = &n
			}
		case KeyPublicMongoPort:
			if n, err := strconv.Atoi(p.Value); err == nil {
				pe.MongoPort = &n
			}
		case KeyPublicMSSQLPort:
			if n, err := strconv.Atoi(p.Value); err == nil {
				pe.MSSQLPort = &n
			}
		case KeyPublicWebUIURL:
			pe.WebUIURL = p.Value
		}
	}
	return pe, nil
}

// SetPublicEndpoints writes only the non-empty/non-nil fields.
func (s *Store) SetPublicEndpoints(ctx context.Context, pe PublicEndpoints) error {
	type kv struct{ key, value string }
	var pairs []kv

	if pe.Host != "" {
		pairs = append(pairs, kv{KeyPublicHost, pe.Host})
	}
	if pe.PGHost != "" {
		pairs = append(pairs, kv{KeyPublicPGHost, pe.PGHost})
	}
	if pe.OraHost != "" {
		pairs = append(pairs, kv{KeyPublicOraHost, pe.OraHost})
	}
	if pe.MySQLHost != "" {
		pairs = append(pairs, kv{KeyPublicMySQLHost, pe.MySQLHost})
	}
	if pe.MongoHost != "" {
		pairs = append(pairs, kv{KeyPublicMongoHost, pe.MongoHost})
	}
	if pe.MSSQLHost != "" {
		pairs = append(pairs, kv{KeyPublicMSSQLHost, pe.MSSQLHost})
	}
	if pe.PGPort != nil {
		pairs = append(pairs, kv{KeyPublicPGPort, strconv.Itoa(*pe.PGPort)})
	}
	if pe.OraPort != nil {
		pairs = append(pairs, kv{KeyPublicOraPort, strconv.Itoa(*pe.OraPort)})
	}
	if pe.MySQLPort != nil {
		pairs = append(pairs, kv{KeyPublicMySQLPort, strconv.Itoa(*pe.MySQLPort)})
	}
	if pe.MongoPort != nil {
		pairs = append(pairs, kv{KeyPublicMongoPort, strconv.Itoa(*pe.MongoPort)})
	}
	if pe.MSSQLPort != nil {
		pairs = append(pairs, kv{KeyPublicMSSQLPort, strconv.Itoa(*pe.MSSQLPort)})
	}
	if pe.WebUIURL != "" {
		pairs = append(pairs, kv{KeyPublicWebUIURL, pe.WebUIURL})
	}

	for _, p := range pairs {
		if err := s.SetParameter(ctx, GroupPublic, p.key, p.value); err != nil {
			return err
		}
	}
	return nil
}

// ResolvedEndpoints holds the fully resolved connection advertisement values.
type ResolvedEndpoints struct {
	PGHost    string
	OraHost   string
	MySQLHost string
	MongoHost string
	MSSQLHost string
	PGPort    int // 0 = protocol disabled
	OraPort   int
	MySQLPort int
	MongoPort int
	MSSQLPort int
	// WebUIURL is the effective Web UI / public base URL: pe.WebUIURL when
	// set, else cfg.PublicURL (the DBB_PUBLIC_URL env var).
	WebUIURL string
}

// ResolvePublicEndpoints applies fallback chains for host and port resolution.
func ResolvePublicEndpoints(pe PublicEndpoints, cfg *config.Config) ResolvedEndpoints {
	resolve := func(protoHost, defaultHost string) string {
		if protoHost != "" {
			return protoHost
		}
		return defaultHost
	}

	resolvePort := func(override *int, listenAddr string) int {
		if override != nil {
			return *override
		}
		if listenAddr == "" {
			return 0
		}
		_, portStr, err := net.SplitHostPort(listenAddr)
		if err != nil {
			return 0
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return 0
		}
		return port
	}

	webUIURL := pe.WebUIURL
	if webUIURL == "" && cfg != nil {
		webUIURL = cfg.PublicURL
	}

	return ResolvedEndpoints{
		PGHost:    resolve(pe.PGHost, pe.Host),
		OraHost:   resolve(pe.OraHost, pe.Host),
		MySQLHost: resolve(pe.MySQLHost, pe.Host),
		MongoHost: resolve(pe.MongoHost, pe.Host),
		MSSQLHost: resolve(pe.MSSQLHost, pe.Host),
		PGPort:    resolvePort(pe.PGPort, cfg.ListenPG),
		OraPort:   resolvePort(pe.OraPort, cfg.ListenOracle),
		MySQLPort: resolvePort(pe.MySQLPort, cfg.ListenMySQL),
		MongoPort: resolvePort(pe.MongoPort, cfg.ListenMongo),
		MSSQLPort: resolvePort(pe.MSSQLPort, cfg.ListenMSSQL),
		WebUIURL:  webUIURL,
	}
}

// ResolveWebUIURL returns the effective Web UI / public base URL: the
// operator-configured public.web_ui_url parameter when set, otherwise
// cfg.PublicURL. Best-effort — a store error falls back to cfg.PublicURL (or
// "" when cfg is nil too) rather than propagating, since callers use this
// for best-effort user-facing text (Slack messages, deep-links) rather than
// anything that should fail a request. Safe to call with a nil cfg.
func (s *Store) ResolveWebUIURL(ctx context.Context, cfg *config.Config) string {
	fallback := ""
	if cfg != nil {
		fallback = cfg.PublicURL
	}

	pe, err := s.GetPublicEndpoints(ctx)
	if err != nil {
		return fallback
	}
	if pe.WebUIURL != "" {
		return pe.WebUIURL
	}
	return fallback
}

// Instance-wide limit parameter group and keys. Same shape as the public.*
// group above, and the same precedence rule: an operator-set parameter wins
// over the deployment's environment variable.
const (
	// GroupLimits holds the instance-wide limits an operator edits from the
	// Settings page, as opposed to the per-grant-definition ones.
	GroupLimits = "limits"

	// KeyLimitsStatementTimeout is a Go duration string ("30s", "5m") bounding
	// how long any single statement may run. Empty or "0" = no limit.
	KeyLimitsStatementTimeout = "statement_timeout"

	// GroupTagging holds the instance-wide statement-tagging settings an
	// operator edits from the Settings page, with the same precedence rule:
	// a set parameter wins over the deployment's environment variable.
	GroupTagging = "tagging"

	// KeyTaggingEnabled carries "true" or "false" — the sqlcommenter-style
	// statement tag on PostgreSQL, MySQL/MariaDB and MongoDB. It does not
	// reach Oracle (KeyTaggingOracle is its own switch) or SQL Server.
	KeyTaggingEnabled = "enabled"

	// KeyTaggingOracle carries "off" or "user" — Oracle's own per-user
	// statement tag. Deliberately a separate key: V$SQL keys on statement
	// text, so this one spends shared-pool cursors and an operator must be
	// able to see that they opted into it specifically.
	KeyTaggingOracle = "oracle"
)

// Limits holds the operator-configured instance-wide limits.
type Limits struct {
	// StatementTimeout is the raw parameter value — a Go duration string, or
	// empty when the operator never set one. Kept as text rather than a
	// time.Duration so "unset" and "explicitly zero" stay distinguishable,
	// which is what the env-var fallback below needs.
	StatementTimeout string
}

// GetLimits reads every limits.* parameter and returns the typed struct.
func (s *Store) GetLimits(ctx context.Context) (Limits, error) {
	params, err := s.GetParameters(ctx, GroupLimits)
	if err != nil {
		return Limits{}, err
	}

	var l Limits

	for _, p := range params {
		if p.Key == KeyLimitsStatementTimeout {
			l.StatementTimeout = p.Value
		}
	}

	return l, nil
}

// SetLimits writes the limits.* parameters. An empty StatementTimeout deletes
// the parameter rather than storing a blank one, so "unset" really does fall
// back to the environment variable instead of pinning "no limit" in the store.
func (s *Store) SetLimits(ctx context.Context, l Limits) error {
	if l.StatementTimeout == "" {
		if err := s.DeleteParameter(ctx, GroupLimits, KeyLimitsStatementTimeout); err != nil &&
			!errors.Is(err, ErrParameterNotFound) {
			return err
		}

		return nil
	}

	return s.SetParameter(ctx, GroupLimits, KeyLimitsStatementTimeout, l.StatementTimeout)
}

// ParseStatementTimeout turns a duration string into the limit it names. An
// empty, malformed or non-positive value means "no limit".
//
// Malformed is deliberately folded into "no limit" rather than into some
// built-in default: this value kills live database sessions, so a typo must
// never be read as "kill sooner". Callers that want to warn about it compare
// against the raw string.
func ParseStatementTimeout(raw string) time.Duration {
	if raw == "" {
		return 0
	}

	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0
	}

	return d
}

// StatementTimeoutMisconfigured reports that raw is neither empty nor "0" nor a
// usable positive duration — i.e. the limit silently ends up disabled and the
// operator probably did not mean that. Same contract as
// QueryStorageConfig.RetentionMisconfigured.
func StatementTimeoutMisconfigured(raw string) bool {
	return raw != "" && raw != "0" && ParseStatementTimeout(raw) <= 0
}

// Tagging holds the operator-configured statement-tagging settings. Raw
// parameter values: empty means the operator never set one, and the
// environment variable decides. That distinction is the whole point of the
// env-var fallback — "explicitly off" and "never chose" must not collapse.
type Tagging struct {
	// Enabled carries "true" / "false" / "". Covers the PostgreSQL,
	// MySQL/MariaDB and MongoDB statement tag (DBB_QUERY_TAGGING).
	Enabled string

	// Oracle carries "off" / "user" / "" — Oracle's own per-user tag
	// (DBB_QUERY_TAGGING_ORACLE), deliberately a separate decision.
	Oracle string
}

// GetTagging reads every tagging.* parameter and returns the typed struct.
func (s *Store) GetTagging(ctx context.Context) (Tagging, error) {
	params, err := s.GetParameters(ctx, GroupTagging)
	if err != nil {
		return Tagging{}, err
	}

	var t Tagging

	for _, p := range params {
		switch p.Key {
		case KeyTaggingEnabled:
			t.Enabled = p.Value
		case KeyTaggingOracle:
			t.Oracle = p.Value
		}
	}

	return t, nil
}

// SetTagging writes the tagging.* parameters. An empty value deletes the
// parameter rather than storing a blank one, so "unset" really does fall back
// to the environment variable instead of pinning a choice in the store — the
// same rule SetLimits applies to the limits.* group.
func (s *Store) SetTagging(ctx context.Context, t Tagging) error {
	for _, p := range []struct {
		key, value string
	}{
		{KeyTaggingEnabled, t.Enabled},
		{KeyTaggingOracle, t.Oracle},
	} {
		if p.value == "" {
			if err := s.DeleteParameter(ctx, GroupTagging, p.key); err != nil &&
				!errors.Is(err, ErrParameterNotFound) {
				return err
			}

			continue
		}

		if err := s.SetParameter(ctx, GroupTagging, p.key, p.value); err != nil {
			return err
		}
	}

	return nil
}

// TaggingOracleMisconfigured reports that raw is neither empty nor a recognised
// Oracle tagging mode. Through the API a bad value is a 400 at write time, so
// reaching this needs a raw-parameters write or a hand-edited store; the
// resolver folds it into "off" with a WARN rather than failing the session.
func TaggingOracleMisconfigured(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", config.QueryTaggingOracleOff, config.QueryTaggingOracleUser:
		return false
	default:
		return true
	}
}

// ResolveQueryTagging applies the fallback chain for the PostgreSQL / MySQL /
// MongoDB statement tag: the operator-set tagging.enabled parameter wins when
// set (either polarity), otherwise the deployment's DBB_QUERY_TAGGING.
func ResolveQueryTagging(t Tagging, cfg *config.Config) bool {
	if t.Enabled != "" {
		return t.Enabled == "true"
	}

	if cfg != nil {
		return cfg.QueryTagging.Enabled
	}

	return false
}

// ResolveOracleTaggingMode applies the fallback chain for Oracle's per-user
// statement tag: the operator-set tagging.oracle parameter wins when set,
// otherwise the deployment's DBB_QUERY_TAGGING_ORACLE. A stored value the
// configuration would reject resolves to off — callers that want to warn
// about it compare against TaggingOracleMisconfigured.
func ResolveOracleTaggingMode(t Tagging, cfg *config.Config) string {
	if t.Oracle != "" {
		if misconfigured := TaggingOracleMisconfigured(t.Oracle); misconfigured {
			return config.QueryTaggingOracleOff
		}

		if on, err := (config.QueryTaggingConfig{Oracle: t.Oracle}).ResolveOracle(); err == nil && on {
			return config.QueryTaggingOracleUser
		}

		return config.QueryTaggingOracleOff
	}

	if cfg != nil {
		if on, err := cfg.QueryTagging.ResolveOracle(); err == nil && on {
			return config.QueryTaggingOracleUser
		}
	}

	return config.QueryTaggingOracleOff
}

// limitsCacheTTL is how long ResolveStatementTimeoutCached reuses the
// parameter group it last read. Short enough that an operator editing the value
// from the Settings page sees it take effect while they are still looking at
// the page; long enough that a login storm is not a query storm.
const limitsCacheTTL = 10 * time.Second

// ResolveStatementTimeoutCached is ResolveStatementTimeout over a short-lived
// memo of the parameter group, for the callers that ask once per connection.
//
// A store error is neither fatal nor fail-closed: it falls back to the
// environment default. Refusing the connection would take the proxy down on a
// transient store blip, and inventing a limit would kill sessions nobody
// configured one for. The fallback is cached too — a store that is down stays
// down for more than one connection.
func (s *Store) ResolveStatementTimeoutCached(ctx context.Context, cfg *config.Config) time.Duration {
	limits, _ := s.resolveParamsCached(ctx, cfg)
	return ResolveStatementTimeout(limits, cfg)
}

// ResolveTaggingCached is ResolveQueryTagging / ResolveOracleTaggingMode over
// the same short-lived memo the statement timeout uses, for the callers that
// ask once per connection. Same error contract: a store error falls back to
// the environment defaults, cached.
func (s *Store) ResolveTaggingCached(ctx context.Context, cfg *config.Config) Tagging {
	if s == nil {
		return Tagging{}
	}

	_, tagging := s.resolveParamsCached(ctx, cfg)

	return tagging
}

// resolveParamsCached reads both parameter groups the per-connection resolvers
// need, over one memo. The groups are read together because they are written
// together (the Settings page saves an operator's whole intent) and because
// half the readers want both. On a nil store, or when the store read fails,
// the zero value falls back to the environment defaults at resolution time.
func (s *Store) resolveParamsCached(ctx context.Context, cfg *config.Config) (Limits, Tagging) {
	if s == nil {
		return Limits{}, Tagging{}
	}

	s.limitsCache.mu.Lock()
	defer s.limitsCache.mu.Unlock()

	if !s.limitsCache.readAt.IsZero() && time.Since(s.limitsCache.readAt) < limitsCacheTTL {
		return s.limitsCache.value, s.limitsCache.tagging
	}

	limits, err := s.GetLimits(ctx)
	if err != nil {
		limits = Limits{}
	}

	tagging, err := s.GetTagging(ctx)
	if err != nil {
		tagging = Tagging{}
	}

	s.limitsCache.value = limits
	s.limitsCache.tagging = tagging
	s.limitsCache.readAt = time.Now()

	return limits, tagging
}

// InvalidateLimits drops the memo so the next resolution re-reads the store.
// Called after this process writes a limits.* or tagging.* parameter; other
// replicas pick the change up within limitsCacheTTL.
func (s *Store) InvalidateLimits() {
	if s == nil {
		return
	}

	s.limitsCache.mu.Lock()
	defer s.limitsCache.mu.Unlock()

	s.limitsCache.readAt = time.Time{}
}

// InvalidateTagging is InvalidateLimits under the name the tagging callers
// read: the two parameter groups share one memo, so one drop serves both.
func (s *Store) InvalidateTagging() {
	s.InvalidateLimits()
}

// ResolveStatementTimeout applies the global fallback chain: the operator-set
// limits.statement_timeout parameter wins when set, otherwise the deployment's
// DBB_STATEMENT_TIMEOUT. Zero means no instance-wide limit, which is the
// default and what every pre-existing deployment gets.
//
// This is only the *global* half. A grant definition may still override it in
// either direction — see AccessGrant.StatementTimeout.
func ResolveStatementTimeout(l Limits, cfg *config.Config) time.Duration {
	if l.StatementTimeout != "" {
		return ParseStatementTimeout(l.StatementTimeout)
	}

	if cfg != nil {
		return ParseStatementTimeout(cfg.StatementTimeout)
	}

	return 0
}

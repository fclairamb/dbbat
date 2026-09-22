package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// handleListParameters lists all active parameters, with optional group_key filter.
func (s *Server) handleListParameters(c *gin.Context) {
	groupKey := c.Query("group_key")
	params, err := s.store.GetAllParameters(c.Request.Context(), groupKey)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list parameters")
		return
	}
	c.JSON(http.StatusOK, params)
}

// handleGetParameter returns a single parameter by group and key.
func (s *Server) handleGetParameter(c *gin.Context) {
	group := c.Param("group")
	key := c.Param("key")
	param, err := s.store.GetParameter(c.Request.Context(), group, key)
	if err != nil {
		if errors.Is(err, store.ErrParameterNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "parameter not found")
			return
		}
		writeInternalError(c, s.logger, err, "failed to get parameter")
		return
	}
	c.JSON(http.StatusOK, param)
}

// setParameterRequest is the body for PUT /parameters/:group/:key.
type setParameterRequest struct {
	Value string `json:"value" binding:"required"`
}

// handleSetParameter creates or updates a parameter.
func (s *Server) handleSetParameter(c *gin.Context) {
	group := c.Param("group")
	key := c.Param("key")

	var req setParameterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())
		return
	}

	if err := s.store.SetParameter(c.Request.Context(), group, key, req.Value); err != nil {
		writeInternalError(c, s.logger, err, "failed to set parameter")
		return
	}

	// Return the updated parameter.
	param, err := s.store.GetParameter(c.Request.Context(), group, key)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to fetch parameter after set")
		return
	}
	c.JSON(http.StatusOK, param)
}

// handleDeleteParameter soft-deletes a parameter.
func (s *Server) handleDeleteParameter(c *gin.Context) {
	group := c.Param("group")
	key := c.Param("key")

	if err := s.store.DeleteParameter(c.Request.Context(), group, key); err != nil {
		if errors.Is(err, store.ErrParameterNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "parameter not found")
			return
		}
		writeInternalError(c, s.logger, err, "failed to delete parameter")
		return
	}
	c.Status(http.StatusNoContent)
}

// instanceListenInfo holds the listen addresses from config.
type instanceListenInfo struct {
	PG    string `json:"pg"`
	Ora   string `json:"ora"`
	MySQL string `json:"mysql"`
	Mongo string `json:"mongo"`
	MSSQL string `json:"mssql"`
	API   string `json:"api"`
}

// instancePublicInfo holds the raw public endpoint settings.
type instancePublicInfo struct {
	Host      string `json:"host"`
	PGHost    string `json:"pg_host"`
	OraHost   string `json:"ora_host"`
	MySQLHost string `json:"mysql_host"`
	MongoHost string `json:"mongo_host"`
	MSSQLHost string `json:"mssql_host"`
	PGPort    *int   `json:"pg_port"`
	OraPort   *int   `json:"ora_port"`
	MySQLPort *int   `json:"mysql_port"`
	MongoPort *int   `json:"mongo_port"`
	MSSQLPort *int   `json:"mssql_port"`
	// WebUIURL is the raw operator-configured Web UI / public base URL
	// override (empty = falling back to DBB_PUBLIC_URL).
	WebUIURL string `json:"web_ui_url"`
}

// instanceResolvedInfo holds the resolved effective connection values.
type instanceResolvedInfo struct {
	PGHost    string `json:"pg_host"`
	PGPort    int    `json:"pg_port"`
	OraHost   string `json:"ora_host"`
	OraPort   int    `json:"ora_port"`
	MySQLHost string `json:"mysql_host"`
	MySQLPort int    `json:"mysql_port"`
	MongoHost string `json:"mongo_host"`
	MongoPort int    `json:"mongo_port"`
	MSSQLHost string `json:"mssql_host"`
	MSSQLPort int    `json:"mssql_port"`
	// WebUIURL is the effective Web UI / public base URL (public.web_ui_url
	// parameter, falling back to DBB_PUBLIC_URL).
	WebUIURL string `json:"web_ui_url"`
}

// instanceLimitsInfo holds the raw operator-configured instance-wide limits.
type instanceLimitsInfo struct {
	// StatementTimeout is the raw limits.statement_timeout parameter — a Go
	// duration string, or empty when the operator never set one and the
	// deployment's DBB_STATEMENT_TIMEOUT is what applies.
	StatementTimeout string `json:"statement_timeout"`
}

// instanceResolvedLimits holds the effective instance-wide limits, after the
// parameter-over-environment fallback.
type instanceResolvedLimits struct {
	// StatementTimeoutSeconds is the effective per-statement limit in seconds,
	// 0 when there is no instance-wide limit. Seconds rather than a duration
	// string because the grant-definition field it is shown next to is in
	// seconds, and the UI compares the two.
	StatementTimeoutSeconds int64 `json:"statement_timeout_seconds"`
	// StatementTimeoutSource says where the effective value came from:
	// "parameter" (the store parameter is set, including to an explicit "0"),
	// "env", or "" when neither is configured. It is what lets the Settings
	// page tell "no limit because an admin said so" from "no limit because
	// nobody set one", and explain why clearing the field does not necessarily
	// disable the limit.
	StatementTimeoutSource string `json:"statement_timeout_source"`
}

// instanceTaggingInfo holds the raw operator-configured statement-tagging
// settings.
type instanceTaggingInfo struct {
	// Enabled is the raw tagging.enabled parameter — "true", "false", or empty
	// when the operator never set one and the deployment's DBB_QUERY_TAGGING
	// is what applies.
	Enabled string `json:"enabled"`
	// Oracle is the raw tagging.oracle parameter — "off", "user", or empty for
	// the DBB_QUERY_TAGGING_ORACLE fallback.
	Oracle string `json:"oracle"`
}

// instanceResolvedTagging holds the effective statement-tagging settings, after
// the parameter-over-environment fallback.
type instanceResolvedTagging struct {
	// Enabled is the effective PostgreSQL / MySQL / MongoDB tagging decision.
	Enabled bool `json:"enabled"`
	// EnabledSource says where it came from: "parameter" (the store parameter
	// is set, either polarity), "env", or "" when neither is configured.
	// During an incident, "tagging is on" and "tagging is on because someone
	// set it in the UI" are different facts — the second tells an operator
	// whether a redeploy will silently revert it.
	EnabledSource string `json:"enabled_source"`
	// Oracle is the effective Oracle per-user tagging mode ("off" or "user").
	Oracle string `json:"oracle"`
	// OracleSource says where the mode came from, with the same values.
	OracleSource string `json:"oracle_source"`
}

// instanceInfoResponse is the full GET /instance response.
type instanceInfoResponse struct {
	Listen instanceListenInfo  `json:"listen"`
	Public *instancePublicInfo `json:"public,omitempty"`
	// Limits is admin-only, like Public: it is an operator setting, not
	// something a connector needs.
	Limits          *instanceLimitsInfo     `json:"limits,omitempty"`
	Tagging         *instanceTaggingInfo    `json:"tagging,omitempty"`
	Resolved        instanceResolvedInfo    `json:"resolved"`
	ResolvedLimits  instanceResolvedLimits  `json:"resolved_limits"`
	ResolvedTagging instanceResolvedTagging `json:"resolved_tagging"`
}

// resolveInstanceLimits turns the stored parameter plus the environment
// default into what actually applies, and says which of the two won.
func resolveInstanceLimits(limits store.Limits, cfg *config.Config) instanceResolvedLimits {
	effective := store.ResolveStatementTimeout(limits, cfg)

	source := ""

	switch {
	case limits.StatementTimeout != "":
		// Set, whatever the value: an explicit "0" *is* a configured choice —
		// it turns the environment default off — and reporting it as "nothing
		// is configured" would make the two indistinguishable in the UI.
		source = "parameter"
	case effective > 0:
		source = "env"
	default:
		// Genuinely nothing: no parameter, and no environment default either.
	}

	return instanceResolvedLimits{
		StatementTimeoutSeconds: int64(effective / time.Second),
		StatementTimeoutSource:  source,
	}
}

// resolveInstanceTagging turns the stored parameters plus the environment
// defaults into what actually applies, and says which of the two won. Same
// shape and the same source vocabulary as resolveInstanceLimits above.
func resolveInstanceTagging(tagging store.Tagging, cfg *config.Config) instanceResolvedTagging {
	enabled := store.ResolveQueryTagging(tagging, cfg)

	enabledSource := ""

	switch {
	case tagging.Enabled != "":
		// Set, whatever the polarity: an explicit "false" *is* a configured
		// choice — it overrides an environment default that turns the feature
		// on — and reporting it as "nothing is configured" would make the two
		// indistinguishable in the UI.
		enabledSource = "parameter"
	case enabled:
		enabledSource = "env"
	}

	oracle := store.ResolveOracleTaggingMode(tagging, cfg)

	oracleSource := ""

	switch {
	case tagging.Oracle != "":
		// Including an unrecognized value: it is what the store holds, and the
		// resolver folded it to off — the Settings page saying "nothing is
		// configured" would hide a value the operator should go fix.
		oracleSource = "parameter"
	case oracle == config.QueryTaggingOracleUser:
		oracleSource = "env"
	}

	return instanceResolvedTagging{
		Enabled:       enabled,
		EnabledSource: enabledSource,
		Oracle:        oracle,
		OracleSource:  oracleSource,
	}
}

// handleGetInstance returns live instance info (listen addrs + public endpoints).
func (s *Server) handleGetInstance(c *gin.Context) {
	ctx := c.Request.Context()
	currentUser := getCurrentUser(c)
	isAdmin := currentUser.IsAdmin()

	pe, err := s.store.GetPublicEndpoints(ctx)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to get public endpoints")
		return
	}

	resolved := store.ResolvePublicEndpoints(pe, s.config)

	limits, err := s.store.GetLimits(ctx)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to get instance limits")
		return
	}

	tagging, err := s.store.GetTagging(ctx)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to get instance tagging")
		return
	}

	listenPG := ""
	listenOra := ""
	listenMySQL := ""
	listenMongo := ""
	listenMSSQL := ""
	listenAPI := ""
	if s.config != nil {
		listenPG = s.config.ListenPG
		listenOra = s.config.ListenOracle
		listenMySQL = s.config.ListenMySQL
		listenMongo = s.config.ListenMongo
		listenMSSQL = s.config.ListenMSSQL
		listenAPI = s.config.ListenAPI
	}

	resp := instanceInfoResponse{
		Listen: instanceListenInfo{
			PG:    listenPG,
			Ora:   listenOra,
			MySQL: listenMySQL,
			Mongo: listenMongo,
			MSSQL: listenMSSQL,
			API:   listenAPI,
		},
		Resolved: instanceResolvedInfo{
			PGHost:    resolved.PGHost,
			PGPort:    resolved.PGPort,
			OraHost:   resolved.OraHost,
			OraPort:   resolved.OraPort,
			MySQLHost: resolved.MySQLHost,
			MySQLPort: resolved.MySQLPort,
			MongoHost: resolved.MongoHost,
			MongoPort: resolved.MongoPort,
			MSSQLHost: resolved.MSSQLHost,
			MSSQLPort: resolved.MSSQLPort,
			WebUIURL:  resolved.WebUIURL,
		},
		ResolvedLimits:  resolveInstanceLimits(limits, s.config),
		ResolvedTagging: resolveInstanceTagging(tagging, s.config),
	}

	if isAdmin {
		resp.Public = &instancePublicInfo{
			Host:      pe.Host,
			PGHost:    pe.PGHost,
			OraHost:   pe.OraHost,
			MySQLHost: pe.MySQLHost,
			MongoHost: pe.MongoHost,
			MSSQLHost: pe.MSSQLHost,
			PGPort:    pe.PGPort,
			OraPort:   pe.OraPort,
			MySQLPort: pe.MySQLPort,
			MongoPort: pe.MongoPort,
			MSSQLPort: pe.MSSQLPort,
			WebUIURL:  pe.WebUIURL,
		}
		resp.Limits = &instanceLimitsInfo{StatementTimeout: limits.StatementTimeout}
		resp.Tagging = &instanceTaggingInfo{Enabled: tagging.Enabled, Oracle: tagging.Oracle}
	}

	c.JSON(http.StatusOK, resp)
}

// updateInstancePublicRequest is the body for PUT /instance/public.
type updateInstancePublicRequest struct {
	Host      string `json:"host"`
	PGHost    string `json:"pg_host"`
	OraHost   string `json:"ora_host"`
	MySQLHost string `json:"mysql_host"`
	MongoHost string `json:"mongo_host"`
	MSSQLHost string `json:"mssql_host"`
	PGPort    *int   `json:"pg_port"`
	OraPort   *int   `json:"ora_port"`
	MySQLPort *int   `json:"mysql_port"`
	MongoPort *int   `json:"mongo_port"`
	MSSQLPort *int   `json:"mssql_port"`
	// WebUIURL sets the Web UI / public base URL override (empty leaves it
	// unset, falling back to DBB_PUBLIC_URL). Distinct from Host: this is
	// where the browser/API is reached (HTTP ingress), not where SQL
	// clients connect (TCP load balancer).
	WebUIURL string `json:"web_ui_url"`
}

// handleUpdateInstancePublic atomically upserts all public.* parameters.
func (s *Server) handleUpdateInstancePublic(c *gin.Context) {
	var req updateInstancePublicRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())
		return
	}

	pe := store.PublicEndpoints{
		Host:      req.Host,
		PGHost:    req.PGHost,
		OraHost:   req.OraHost,
		MySQLHost: req.MySQLHost,
		MongoHost: req.MongoHost,
		MSSQLHost: req.MSSQLHost,
		PGPort:    req.PGPort,
		OraPort:   req.OraPort,
		MySQLPort: req.MySQLPort,
		MongoPort: req.MongoPort,
		MSSQLPort: req.MSSQLPort,
		WebUIURL:  req.WebUIURL,
	}

	if err := s.store.SetPublicEndpoints(c.Request.Context(), pe); err != nil {
		writeInternalError(c, s.logger, err, "failed to update instance public endpoints")
		return
	}

	c.Status(http.StatusNoContent)
}

// updateInstanceLimitsRequest is the body for PUT /instance/limits.
type updateInstanceLimitsRequest struct {
	// StatementTimeout is a Go duration string ("30s", "5m"). An empty value
	// *clears* the parameter, which falls back to DBB_STATEMENT_TIMEOUT rather
	// than disabling the limit — "0" is how an operator disables it outright.
	StatementTimeout string `json:"statement_timeout"`
}

// handleUpdateInstanceLimits writes the limits.* parameters. Admin-only.
func (s *Server) handleUpdateInstanceLimits(c *gin.Context) {
	var req updateInstanceLimitsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())
		return
	}

	// Refused at the edge rather than folded to "no limit" the way the proxy
	// path has to: here there is a human to tell. A typo that silently
	// disabled the limit is exactly the failure this endpoint exists to
	// prevent.
	if store.StatementTimeoutMisconfigured(req.StatementTimeout) {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError,
			"statement_timeout must be a Go duration such as 30s or 5m, \"0\" for no limit, "+
				"or empty to fall back to DBB_STATEMENT_TIMEOUT")
		return
	}

	if err := s.store.SetLimits(c.Request.Context(), store.Limits{StatementTimeout: req.StatementTimeout}); err != nil {
		writeInternalError(c, s.logger, err, "failed to update instance limits")
		return
	}

	// The limits parameter group is memoized on the store for a few seconds
	// (it is read once per connection on five protocols); drop this process's
	// copy so the operator sees the change take effect immediately on the
	// replica they are talking to. Other replicas pick it up on their own TTL.
	s.store.InvalidateLimits()

	c.Status(http.StatusNoContent)
}

// updateInstanceTaggingRequest is the body for PUT /instance/tagging.
type updateInstanceTaggingRequest struct {
	// Enabled turns the PostgreSQL / MySQL / MongoDB statement tag on or off.
	// A boolean, so the store always ends up holding an explicit choice —
	// "false" overrides a DBB_QUERY_TAGGING default of true, and the only way
	// back to the environment default is deleting the raw parameter.
	Enabled bool `json:"enabled"`

	// Oracle is Oracle's own mode: "off" or "user". An empty value clears the
	// parameter, falling back to DBB_QUERY_TAGGING_ORACLE.
	Oracle string `json:"oracle"`
}

// handleUpdateInstanceTagging writes the tagging.* parameters. Admin-only.
func (s *Server) handleUpdateInstanceTagging(c *gin.Context) {
	var req updateInstanceTaggingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())
		return
	}

	// Refused at the edge rather than folded to "off" the way the proxy path
	// has to: here there is a human to tell, and a settings write that would
	// crash every replica on their next restart (the env var's rule) is a
	// worse failure than the one it is modeled on.
	oracle := strings.ToLower(strings.TrimSpace(req.Oracle))
	if oracle != "" && oracle != config.QueryTaggingOracleOff && oracle != config.QueryTaggingOracleUser {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError,
			`oracle must be "off" or "user", or empty to fall back to DBB_QUERY_TAGGING_ORACLE`)
		return
	}

	// Written either way, never blank: "false" is a configured choice that
	// overrides a DBB_QUERY_TAGGING default of true, and storing nothing for it
	// would silently hand the decision back to the environment — the one thing
	// an operator turning the feature off in a hurry must not get.
	enabled := "false"
	if req.Enabled {
		enabled = "true"
	}

	if err := s.store.SetTagging(c.Request.Context(), store.Tagging{
		Enabled: enabled,
		Oracle:  oracle,
	}); err != nil {
		writeInternalError(c, s.logger, err, "failed to update instance tagging")
		return
	}

	// The tagging parameter group shares the limits memo (it is read once per
	// connection on four protocols); drop this process's copy so the operator
	// sees the change take effect immediately. Other replicas pick it up on
	// their own TTL.
	s.store.InvalidateTagging()

	c.Status(http.StatusNoContent)
}

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// CreateDatabaseRequest represents the request to create a database
type CreateDatabaseRequest struct {
	Name              string `json:"name" binding:"required"`
	Description       string `json:"description"`
	Host              string `json:"host" binding:"required"`
	Port              int    `json:"port"`
	DatabaseName      string `json:"database_name"`
	Username          string `json:"username" binding:"required"`
	Password          string `json:"password" binding:"required"`
	SSLMode           string `json:"ssl_mode"`
	Protocol          string `json:"protocol"`
	OracleServiceName string `json:"oracle_service_name"`
	MongoAuthSource   string `json:"mongo_auth_source"`
	Listable          *bool  `json:"listable"`
}

// UpdateDatabaseRequest represents the request to update a database
type UpdateDatabaseRequest struct {
	Description       *string `json:"description"`
	Host              *string `json:"host"`
	Port              *int    `json:"port"`
	DatabaseName      *string `json:"database_name"`
	Username          *string `json:"username"`
	Password          *string `json:"password"`
	SSLMode           *string `json:"ssl_mode"`
	Protocol          *string `json:"protocol"`
	OracleServiceName *string `json:"oracle_service_name"`
	MongoAuthSource   *string `json:"mongo_auth_source"`
	Listable          *bool   `json:"listable"`
}

// DatabaseResponse represents a database with full details (admin only)
type DatabaseResponse struct {
	UID               uuid.UUID  `json:"uid"`
	Name              string     `json:"name"`
	Description       string     `json:"description"`
	Host              string     `json:"host,omitempty"`
	Port              int        `json:"port,omitempty"`
	DatabaseName      string     `json:"database_name,omitempty"`
	Username          string     `json:"username,omitempty"`
	SSLMode           string     `json:"ssl_mode,omitempty"`
	Protocol          string     `json:"protocol,omitempty"`
	OracleServiceName string     `json:"oracle_service_name,omitempty"`
	MongoAuthSource   string     `json:"mongo_auth_source,omitempty"`
	Listable          bool       `json:"listable"`
	CreatedBy         *uuid.UUID `json:"created_by,omitempty"`
}

// DatabaseLimitedResponse represents a database with limited info (non-admin)
type DatabaseLimitedResponse struct {
	UID         uuid.UUID `json:"uid"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
}

// handleCreateDatabase creates a new database configuration
func (s *Server) handleCreateDatabase(c *gin.Context) {
	var req CreateDatabaseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())
		return
	}

	// Check demo mode restrictions
	if s.config != nil {
		if errMsg := s.config.ValidateDemoTarget(req.Username, req.Password, req.Host, req.DatabaseName); errMsg != "" {
			writeError(c, http.StatusForbidden, ErrCodeForbidden, errMsg)
			return
		}
	}

	// Validate and default protocol
	if req.Protocol == "" {
		req.Protocol = store.ProtocolPostgreSQL
	}

	if !isSupportedProtocol(req.Protocol) {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError,
			"protocol must be one of: postgresql, oracle, mysql, mariadb, mongodb")
		return
	}

	// Port is required (the SQL default was dropped in the MySQL phase 1 migration).
	// Surface a protocol-aware suggestion in the error so the user knows the
	// conventional default for their chosen protocol.
	if req.Port == 0 {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError,
			fmt.Sprintf("port is required (suggested default for %s: %d)",
				req.Protocol, defaultPortFor(req.Protocol)))
		return
	}

	// Validate required fields per protocol
	switch req.Protocol {
	case store.ProtocolOracle:
		if req.OracleServiceName == "" && req.DatabaseName == "" {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError,
				"oracle_service_name or database_name is required for Oracle databases")
			return
		}

		if req.OracleServiceName == "" {
			req.OracleServiceName = req.DatabaseName
		}
	default:
		if req.DatabaseName == "" {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError,
				"database_name is required for "+req.Protocol+" databases")
			return
		}

		if req.SSLMode == "" {
			req.SSLMode = "prefer"
		}
	}

	currentUser := getCurrentUser(c)

	var oracleServiceName *string
	if req.OracleServiceName != "" {
		oracleServiceName = &req.OracleServiceName
	}

	var protocolData *store.ServerProtocolData
	if req.MongoAuthSource != "" {
		protocolData = &store.ServerProtocolData{MongoDB: &store.MongoDatabaseData{AuthSource: req.MongoAuthSource}}
	}

	listable := true
	if req.Listable != nil {
		listable = *req.Listable
	}

	db := &store.Server{
		Name:              req.Name,
		Description:       req.Description,
		Host:              req.Host,
		Port:              req.Port,
		DatabaseName:      req.DatabaseName,
		Username:          req.Username,
		Password:          req.Password,
		SSLMode:           req.SSLMode,
		Protocol:          req.Protocol,
		OracleServiceName: oracleServiceName,
		ProtocolData:      protocolData,
		Listable:          listable,
		CreatedBy:         &currentUser.UID,
	}

	result, err := s.store.CreateServer(c.Request.Context(), db, s.encryptionKey)
	if err != nil {
		if errors.Is(err, store.ErrTargetMatchesStorage) {
			writeError(c, http.StatusBadRequest, ErrCodeTargetMatchesSelf, "target database cannot match DBBat storage database")
			return
		}
		writeInternalError(c, s.logger, err, "failed to create database")
		return
	}

	// Log audit event
	details, _ := json.Marshal(map[string]interface{}{
		"name": result.Name,
		"host": result.Host,
		"port": result.Port,
	})
	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "database.created",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, toDatabaseResponse(result))
}

// handleListDatabases lists databases based on user role.
// Admins receive all databases (including non-listable) with full details.
// All other authenticated users receive only listable databases with limited details.
func (s *Server) handleListDatabases(c *gin.Context) {
	currentUser := getCurrentUser(c)

	// Admin sees full details for every database, including non-listable ones.
	if currentUser.IsAdmin() {
		databases, err := s.store.ListServers(c.Request.Context())
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to list databases")
			return
		}
		response := make([]DatabaseResponse, len(databases))
		for i, db := range databases {
			response[i] = toDatabaseResponse(&db)
		}
		successResponse(c, gin.H{"databases": response})
		return
	}

	// Non-admin: only listable databases, limited response (no host/port/creds).
	databases, err := s.store.ListListableServers(c.Request.Context())
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list databases")
		return
	}
	response := make([]DatabaseLimitedResponse, len(databases))
	for i, db := range databases {
		response[i] = toDatabaseLimitedResponse(&db)
	}
	successResponse(c, gin.H{"databases": response})
}

// handleGetDatabase retrieves a specific database based on user role
func (s *Server) handleGetDatabase(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid database UID")
		return
	}

	currentUser := getCurrentUser(c)

	db, err := s.store.GetServerByUID(c.Request.Context(), uid)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "database not found")
		return
	}

	// Admin sees full details
	if currentUser.IsAdmin() {
		successResponse(c, toDatabaseResponse(db))
		return
	}

	// Viewer sees limited info
	if currentUser.IsViewer() {
		successResponse(c, toDatabaseLimitedResponse(db))
		return
	}

	// Connector can only see databases they have grants for
	if currentUser.IsConnector() {
		grant, err := s.store.GetActiveGrant(c.Request.Context(), currentUser.UID, uid)
		if err != nil || grant == nil {
			writeError(c, http.StatusForbidden, ErrCodeForbidden, "no access to this database")
			return
		}
		successResponse(c, toDatabaseLimitedResponse(db))
		return
	}

	writeError(c, http.StatusForbidden, ErrCodeForbidden, "no access to this database")
}

// validateDemoModeUpdate checks if a database update is allowed in demo mode.
// Returns an error message if validation fails, empty string if allowed.
func (s *Server) validateDemoModeUpdate(db *store.Server, req UpdateDatabaseRequest) string {
	if s.config == nil || !s.config.IsDemoMode() {
		return ""
	}

	// No credential changes, no validation needed
	if req.Username == nil && req.Password == nil && req.Host == nil && req.DatabaseName == nil {
		return ""
	}

	target := s.config.GetDemoTarget()
	if target == nil {
		return ""
	}

	// Compute effective values
	username := db.Username
	if req.Username != nil {
		username = *req.Username
	}
	host := db.Host
	if req.Host != nil {
		host = *req.Host
	}
	database := db.DatabaseName
	if req.DatabaseName != nil {
		database = *req.DatabaseName
	}

	errorMsg := fmt.Sprintf("you can only use %s:%s@%s/%s in demo mode", target.Username, target.Password, target.Host, target.Server)

	// If password is being changed, validate full credentials
	if req.Password != nil {
		if errMsg := s.config.ValidateDemoTarget(username, *req.Password, host, database); errMsg != "" {
			return errMsg
		}
		return ""
	}

	// Validate individual fields against demo target
	if req.Username != nil && username != target.Username {
		return errorMsg
	}
	if req.Host != nil && host != target.Host {
		return errorMsg
	}
	if req.DatabaseName != nil && database != target.Server {
		return errorMsg
	}

	return ""
}

// handleUpdateDatabase updates a database
func (s *Server) handleUpdateDatabase(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid database UID")
		return
	}

	var req UpdateDatabaseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())
		return
	}

	// Check demo mode restrictions if credentials are being updated
	if s.config != nil && s.config.IsDemoMode() && (req.Username != nil || req.Password != nil || req.Host != nil || req.DatabaseName != nil) {
		db, err := s.store.GetServerByUID(c.Request.Context(), uid)
		if err != nil {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "database not found")
			return
		}
		if errMsg := s.validateDemoModeUpdate(db, req); errMsg != "" {
			writeError(c, http.StatusForbidden, ErrCodeForbidden, errMsg)
			return
		}
	}

	updates := store.ServerUpdate{
		Description:       req.Description,
		Host:              req.Host,
		Port:              req.Port,
		DatabaseName:      req.DatabaseName,
		Username:          req.Username,
		Password:          req.Password,
		SSLMode:           req.SSLMode,
		Protocol:          req.Protocol,
		OracleServiceName: req.OracleServiceName,
		MongoAuthSource:   req.MongoAuthSource,
		Listable:          req.Listable,
	}

	if err := s.store.UpdateServer(c.Request.Context(), uid, updates, s.encryptionKey); err != nil {
		if errors.Is(err, store.ErrTargetMatchesStorage) {
			writeError(c, http.StatusBadRequest, ErrCodeTargetMatchesSelf, "target database cannot match DBBat storage database")
			return
		}
		if errors.Is(err, store.ErrServerNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "database not found")
			return
		}
		writeInternalError(c, s.logger, err, "failed to update database")
		return
	}

	// Log audit event
	currentUser := getCurrentUser(c)
	details, _ := json.Marshal(map[string]interface{}{
		"database_uid":   uid,
		"updated_fields": req,
	})
	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "database.updated",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{"message": "database updated"})
}

// handleDeleteDatabase deletes a database
func (s *Server) handleDeleteDatabase(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid database UID")
		return
	}

	// Check demo mode restrictions
	if s.config != nil && s.config.IsDemoMode() {
		db, err := s.store.GetServerByUID(c.Request.Context(), uid)
		if err == nil && db.Name == "demo_db" {
			writeError(c, http.StatusForbidden, ErrCodeForbidden, "cannot delete the demo database in demo mode")
			return
		}
	}

	if err := s.store.DeleteServer(c.Request.Context(), uid); err != nil {
		writeInternalError(c, s.logger, err, "failed to delete database")
		return
	}

	// Log audit event
	currentUser := getCurrentUser(c)
	details, _ := json.Marshal(map[string]interface{}{
		"database_uid": uid,
	})
	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "database.deleted",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{"message": "database deleted"})
}

// handleGetDatabaseConnection returns a connection URL template for a database.
//   - Admin: always 200.
//   - Non-admin: 200 if at least one active grant exists; 404 otherwise (avoid leaking existence).
//   - Protocol disabled (port 0): 409.
func (s *Server) handleGetDatabaseConnection(c *gin.Context) {
	uid, err := uuid.Parse(c.Param("uid"))
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid database UID")
		return
	}

	ctx := c.Request.Context()
	currentUser := getCurrentUser(c)

	db, err := s.store.GetServerByUID(ctx, uid)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "database not found")
		return
	}

	// Non-admins: return 404 unless they have an active grant (avoid 403 leaking existence).
	if !currentUser.IsAdmin() {
		grants, err := s.store.ListGrants(ctx, store.GrantFilter{
			UserID:     &currentUser.UID,
			DatabaseID: &uid,
			ActiveOnly: true,
		})
		if err != nil || len(grants) == 0 {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "database not found")
			return
		}
	}

	pe, _ := s.store.GetPublicEndpoints(ctx)
	var endpoints store.ResolvedEndpoints
	if s.config != nil {
		endpoints = store.ResolvePublicEndpoints(pe, s.config)
	}

	info, ok := BuildConnectionURL(db, currentUser, endpoints, "")
	if !ok {
		c.JSON(http.StatusConflict, gin.H{"error": "proxy for this protocol is disabled"})
		return
	}

	c.JSON(http.StatusOK, info)
}

// toDatabaseResponse converts a Server to a DatabaseResponse (admin only, without password)
func toDatabaseResponse(db *store.Server) DatabaseResponse {
	var oracleServiceName string
	if db.OracleServiceName != nil {
		oracleServiceName = *db.OracleServiceName
	}

	var mongoAuthSource string
	if data := db.MongoData(); data != nil {
		mongoAuthSource = data.AuthSource
	}

	return DatabaseResponse{
		UID:               db.UID,
		Name:              db.Name,
		Description:       db.Description,
		Host:              db.Host,
		Port:              db.Port,
		DatabaseName:      db.DatabaseName,
		Username:          db.Username,
		SSLMode:           db.SSLMode,
		Protocol:          db.Protocol,
		OracleServiceName: oracleServiceName,
		MongoAuthSource:   mongoAuthSource,
		Listable:          db.Listable,
		CreatedBy:         db.CreatedBy,
	}
}

// toDatabaseLimitedResponse converts a Server to a limited response (non-admin)
func toDatabaseLimitedResponse(db *store.Server) DatabaseLimitedResponse {
	return DatabaseLimitedResponse{
		UID:         db.UID,
		Name:        db.Name,
		Description: db.Description,
	}
}

// isSupportedProtocol reports whether the given protocol is one the proxy
// can serve. Kept as a helper so the create + update paths share one source
// of truth for the enum.
func isSupportedProtocol(protocol string) bool {
	switch protocol {
	case store.ProtocolPostgreSQL, store.ProtocolOracle, store.ProtocolMySQL, store.ProtocolMariaDB, store.ProtocolMongoDB:
		return true
	default:
		return false
	}
}

// defaultPortFor returns the conventional default TCP port for each protocol.
// Used only to suggest a value in API error messages — the API itself never
// auto-fills the port silently.
func defaultPortFor(protocol string) int {
	switch protocol {
	case store.ProtocolPostgreSQL:
		return 5432
	case store.ProtocolOracle:
		return 1521
	case store.ProtocolMySQL, store.ProtocolMariaDB:
		return 3306
	case store.ProtocolMongoDB:
		// MongoDB's standard target port. The proxy listener defaults to 27018
		// to avoid clashing with a local mongod, but Server.Port is the
		// upstream target's port, which conventionally is 27017.
		return 27017
	default:
		return 0
	}
}

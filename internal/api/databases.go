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
	Name         string `json:"name" binding:"required"`
	Description  string `json:"description"`
	Host         string `json:"host" binding:"required"`
	Port         int    `json:"port"`
	DatabaseName string `json:"database_name" binding:"required"`
	Username     string `json:"username" binding:"required"`
	Password     string `json:"password" binding:"required"`
	SSLMode      string `json:"ssl_mode"`
}

// UpdateDatabaseRequest represents the request to update a database
type UpdateDatabaseRequest struct {
	Description  *string `json:"description"`
	Host         *string `json:"host"`
	Port         *int    `json:"port"`
	DatabaseName *string `json:"database_name"`
	Username     *string `json:"username"`
	Password     *string `json:"password"`
	SSLMode      *string `json:"ssl_mode"`
}

// DatabaseResponse represents a database with full details (admin only)
type DatabaseResponse struct {
	UID          uuid.UUID  `json:"uid"`
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	Host         string     `json:"host,omitempty"`
	Port         int        `json:"port,omitempty"`
	DatabaseName string     `json:"database_name,omitempty"`
	Username     string     `json:"username,omitempty"`
	SSLMode      string     `json:"ssl_mode,omitempty"`
	CreatedBy    *uuid.UUID `json:"created_by,omitempty"`
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
		errorResponse(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	// Check demo mode restrictions
	if s.config != nil {
		if errMsg := s.config.ValidateDemoTarget(req.Username, req.Password, req.Host, req.DatabaseName); errMsg != "" {
			errorResponse(c, http.StatusForbidden, errMsg)
			return
		}
	}

	// Set default port if not provided
	if req.Port == 0 {
		req.Port = 5432
	}

	// Set default SSL mode if not provided
	if req.SSLMode == "" {
		req.SSLMode = "prefer"
	}

	currentUser := getCurrentUser(c)
	db := &store.Database{
		Name:         req.Name,
		Description:  req.Description,
		Host:         req.Host,
		Port:         req.Port,
		DatabaseName: req.DatabaseName,
		Username:     req.Username,
		Password:     req.Password,
		SSLMode:      req.SSLMode,
		CreatedBy:    &currentUser.UID,
	}

	result, err := s.store.CreateDatabase(c.Request.Context(), db, s.encryptionKey)
	if err != nil {
		if errors.Is(err, store.ErrTargetMatchesStorage) {
			errorResponse(c, http.StatusBadRequest, "target database cannot match DBBat storage database")
			return
		}
		s.logger.Error("failed to create database", "error", err)
		errorResponse(c, http.StatusInternalServerError, "failed to create database")
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

// handleListDatabases lists databases based on user role
func (s *Server) handleListDatabases(c *gin.Context) {
	currentUser := getCurrentUser(c)

	databases, err := s.store.ListDatabases(c.Request.Context())
	if err != nil {
		s.logger.Error("failed to list databases", "error", err)
		errorResponse(c, http.StatusInternalServerError, "failed to list databases")
		return
	}

	// Admin sees full details
	if currentUser.IsAdmin() {
		response := make([]DatabaseResponse, len(databases))
		for i, db := range databases {
			response[i] = toDatabaseResponse(&db)
		}
		successResponse(c, gin.H{"databases": response})
		return
	}

	// Viewer sees name and description for all databases
	if currentUser.IsViewer() {
		response := make([]DatabaseLimitedResponse, len(databases))
		for i, db := range databases {
			response[i] = toDatabaseLimitedResponse(&db)
		}
		successResponse(c, gin.H{"databases": response})
		return
	}

	// Connector sees only databases they have grants for
	if currentUser.IsConnector() {
		// Get user's active grants to filter databases
		grants, err := s.store.ListGrants(c.Request.Context(), store.GrantFilter{
			UserID:     &currentUser.UID,
			ActiveOnly: true,
		})
		if err != nil {
			s.logger.Error("failed to list grants", "error", err)
			errorResponse(c, http.StatusInternalServerError, "failed to list databases")
			return
		}

		// Build set of database UIDs user has access to
		accessibleDBs := make(map[uuid.UUID]bool)
		for _, grant := range grants {
			accessibleDBs[grant.DatabaseID] = true
		}

		// Filter databases
		var response []DatabaseLimitedResponse
		for _, db := range databases {
			if accessibleDBs[db.UID] {
				response = append(response, toDatabaseLimitedResponse(&db))
			}
		}
		if response == nil {
			response = []DatabaseLimitedResponse{}
		}
		successResponse(c, gin.H{"databases": response})
		return
	}

	// User with no relevant roles sees nothing
	successResponse(c, gin.H{"databases": []DatabaseLimitedResponse{}})
}

// handleGetDatabase retrieves a specific database based on user role
func (s *Server) handleGetDatabase(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid database UID")
		return
	}

	currentUser := getCurrentUser(c)

	db, err := s.store.GetDatabaseByUID(c.Request.Context(), uid)
	if err != nil {
		s.logger.Error("failed to get database", "error", err)
		errorResponse(c, http.StatusNotFound, "database not found")
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
			errorResponse(c, http.StatusForbidden, "no access to this database")
			return
		}
		successResponse(c, toDatabaseLimitedResponse(db))
		return
	}

	errorResponse(c, http.StatusForbidden, "no access to this database")
}

// handleUpdateDatabase updates a database
func (s *Server) handleUpdateDatabase(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid database UID")
		return
	}

	var req UpdateDatabaseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	// Check demo mode restrictions if credentials are being updated
	if s.config != nil && s.config.IsDemoMode() && (req.Username != nil || req.Password != nil || req.Host != nil || req.DatabaseName != nil) {
		// Get current database to check combined values
		db, err := s.store.GetDatabaseByUID(c.Request.Context(), uid)
		if err != nil {
			errorResponse(c, http.StatusNotFound, "database not found")
			return
		}

		// Use new values if provided, otherwise keep existing
		username := db.Username
		if req.Username != nil {
			username = *req.Username
		}
		// For password, if not being updated, assume it's valid (we can't decrypt to check)
		password := ""
		if req.Password != nil {
			password = *req.Password
		}
		host := db.Host
		if req.Host != nil {
			host = *req.Host
		}
		database := db.DatabaseName
		if req.DatabaseName != nil {
			database = *req.DatabaseName
		}

		// Only validate if password is being changed (we can't check encrypted existing password)
		if req.Password != nil {
			if errMsg := s.config.ValidateDemoTarget(username, password, host, database); errMsg != "" {
				errorResponse(c, http.StatusForbidden, errMsg)
				return
			}
		} else if req.Username != nil || req.Host != nil || req.DatabaseName != nil {
			// If only username, host, or database name is being changed, validate against demo target
			target := s.config.GetDemoTarget()
			if target != nil {
				if req.Username != nil && username != target.Username {
					errorResponse(c, http.StatusForbidden, fmt.Sprintf("you can only use %s:%s@%s/%s in demo mode", target.Username, target.Password, target.Host, target.Database))
					return
				}
				if req.Host != nil && host != target.Host {
					errorResponse(c, http.StatusForbidden, fmt.Sprintf("you can only use %s:%s@%s/%s in demo mode", target.Username, target.Password, target.Host, target.Database))
					return
				}
				if req.DatabaseName != nil && database != target.Database {
					errorResponse(c, http.StatusForbidden, fmt.Sprintf("you can only use %s:%s@%s/%s in demo mode", target.Username, target.Password, target.Host, target.Database))
					return
				}
			}
		}
	}

	updates := store.DatabaseUpdate{
		Description:  req.Description,
		Host:         req.Host,
		Port:         req.Port,
		DatabaseName: req.DatabaseName,
		Username:     req.Username,
		Password:     req.Password,
		SSLMode:      req.SSLMode,
	}

	if err := s.store.UpdateDatabase(c.Request.Context(), uid, updates, s.encryptionKey); err != nil {
		if errors.Is(err, store.ErrTargetMatchesStorage) {
			errorResponse(c, http.StatusBadRequest, "target database cannot match DBBat storage database")
			return
		}
		if errors.Is(err, store.ErrDatabaseNotFound) {
			errorResponse(c, http.StatusNotFound, "database not found")
			return
		}
		s.logger.Error("failed to update database", "error", err)
		errorResponse(c, http.StatusInternalServerError, "failed to update database")
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
		errorResponse(c, http.StatusBadRequest, "invalid database UID")
		return
	}

	if err := s.store.DeleteDatabase(c.Request.Context(), uid); err != nil {
		s.logger.Error("failed to delete database", "error", err)
		errorResponse(c, http.StatusInternalServerError, "failed to delete database")
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

// toDatabaseResponse converts a Database to a DatabaseResponse (admin only, without password)
func toDatabaseResponse(db *store.Database) DatabaseResponse {
	return DatabaseResponse{
		UID:          db.UID,
		Name:         db.Name,
		Description:  db.Description,
		Host:         db.Host,
		Port:         db.Port,
		DatabaseName: db.DatabaseName,
		Username:     db.Username,
		SSLMode:      db.SSLMode,
		CreatedBy:    db.CreatedBy,
	}
}

// toDatabaseLimitedResponse converts a Database to a limited response (non-admin)
func toDatabaseLimitedResponse(db *store.Database) DatabaseLimitedResponse {
	return DatabaseLimitedResponse{
		UID:         db.UID,
		Name:        db.Name,
		Description: db.Description,
	}
}

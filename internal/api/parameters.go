package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

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
	API   string `json:"api"`
}

// instancePublicInfo holds the raw public endpoint settings.
type instancePublicInfo struct {
	Host      string `json:"host"`
	PGHost    string `json:"pg_host"`
	OraHost   string `json:"ora_host"`
	MySQLHost string `json:"mysql_host"`
	PGPort    *int   `json:"pg_port"`
	OraPort   *int   `json:"ora_port"`
	MySQLPort *int   `json:"mysql_port"`
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
	// WebUIURL is the effective Web UI / public base URL (public.web_ui_url
	// parameter, falling back to DBB_PUBLIC_URL).
	WebUIURL string `json:"web_ui_url"`
}

// instanceInfoResponse is the full GET /instance response.
type instanceInfoResponse struct {
	Listen   instanceListenInfo   `json:"listen"`
	Public   *instancePublicInfo  `json:"public,omitempty"`
	Resolved instanceResolvedInfo `json:"resolved"`
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

	listenPG := ""
	listenOra := ""
	listenMySQL := ""
	listenAPI := ""
	if s.config != nil {
		listenPG = s.config.ListenPG
		listenOra = s.config.ListenOracle
		listenMySQL = s.config.ListenMySQL
		listenAPI = s.config.ListenAPI
	}

	resp := instanceInfoResponse{
		Listen: instanceListenInfo{
			PG:    listenPG,
			Ora:   listenOra,
			MySQL: listenMySQL,
			API:   listenAPI,
		},
		Resolved: instanceResolvedInfo{
			PGHost:    resolved.PGHost,
			PGPort:    resolved.PGPort,
			OraHost:   resolved.OraHost,
			OraPort:   resolved.OraPort,
			MySQLHost: resolved.MySQLHost,
			MySQLPort: resolved.MySQLPort,
			WebUIURL:  resolved.WebUIURL,
		},
	}

	if isAdmin {
		resp.Public = &instancePublicInfo{
			Host:      pe.Host,
			PGHost:    pe.PGHost,
			OraHost:   pe.OraHost,
			MySQLHost: pe.MySQLHost,
			PGPort:    pe.PGPort,
			OraPort:   pe.OraPort,
			MySQLPort: pe.MySQLPort,
			WebUIURL:  pe.WebUIURL,
		}
	}

	c.JSON(http.StatusOK, resp)
}

// updateInstancePublicRequest is the body for PUT /instance/public.
type updateInstancePublicRequest struct {
	Host      string `json:"host"`
	PGHost    string `json:"pg_host"`
	OraHost   string `json:"ora_host"`
	MySQLHost string `json:"mysql_host"`
	PGPort    *int   `json:"pg_port"`
	OraPort   *int   `json:"ora_port"`
	MySQLPort *int   `json:"mysql_port"`
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
		PGPort:    req.PGPort,
		OraPort:   req.OraPort,
		MySQLPort: req.MySQLPort,
		WebUIURL:  req.WebUIURL,
	}

	if err := s.store.SetPublicEndpoints(c.Request.Context(), pe); err != nil {
		writeInternalError(c, s.logger, err, "failed to update instance public endpoints")
		return
	}

	c.Status(http.StatusNoContent)
}

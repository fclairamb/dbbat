package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/mcp"
)

// mcpWriteDeadline is how long an MCP response may take to write.
//
// The API server runs a 15s write timeout, which is right for REST and wrong
// here: `await_approval` deliberately holds a request open while a human
// decides. The per-request deadline is extended (never cleared) so a wedged
// MCP request still cannot pin a connection forever.
const mcpWriteDeadline = 5 * time.Minute

// newMCPServer builds the MCP layer for this API server.
//
// The broker is resolved through a closure rather than captured: the process
// wiring installs the shared broker with SetEventPlumbing *after* NewServer,
// and the MCP hold correlator has to see that instance, not the placeholder.
func (s *Server) newMCPServer() *mcp.Server {
	var listeners mcp.LoopbackListeners

	if s.config != nil {
		listeners = mcp.LoopbackListeners{
			PostgreSQL: s.config.ListenPG,
			MySQL:      s.config.ListenMySQL,
			Oracle:     s.config.ListenOracle,
			MongoDB:    s.config.ListenMongo,
			MSSQL:      s.config.ListenMSSQL,
		}
	}

	return mcp.New(mcp.Deps{
		Store:    s.store,
		Logger:   s.logger,
		Broker:   func() *events.Broker { return s.broker },
		Executor: mcp.NewLoopbackExecutor(listeners),
	})
}

// mcpEnabled reports whether the MCP routes should be registered. A nil config
// (unit tests constructing a bare server) counts as enabled, matching the
// shipped default.
func (s *Server) mcpEnabled() bool {
	return s.config == nil || s.config.MCP.Enabled
}

// handleMCP serves the Streamable-HTTP MCP endpoint.
//
// Authentication has already happened in the shared middleware; this handler
// adds one restriction and one adaptation:
//
//   - **API keys only.** The key is not merely the caller's credential here,
//     it is also the password the loopback protocol client authenticates to the
//     proxy with. Basic Auth would work on the wire too, but accepting it would
//     mean this endpoint could be driven with a user's login password, and a
//     browser session token has no business running SQL for an agent.
//   - **A longer write deadline**, so a statement parked on a human approver
//     does not die of the REST write timeout.
func (s *Server) handleMCP(c *gin.Context) {
	if s.mcp == nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "MCP endpoint is disabled")

		return
	}

	if !isAPIKeyAuth(c) {
		writeError(c, http.StatusForbidden, ErrCodeForbidden,
			"the MCP endpoint requires an API key (Authorization: Bearer dbb_...)")

		return
	}

	user := getCurrentUser(c)
	if user == nil {
		writeError(c, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required")

		return
	}

	token := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	if token == "" {
		writeError(c, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required")

		return
	}

	if err := http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(mcpWriteDeadline)); err != nil {
		// httptest recorders and any writer without a deadline hook report
		// ErrNotSupported. Not fatal: it only means the ambient timeout
		// applies, which is exactly the pre-existing behavior.
		s.logger.DebugContext(c.Request.Context(), "MCP write deadline not adjustable", slog.Any("error", err))
	}

	req := c.Request.WithContext(mcp.WithCaller(c.Request.Context(), &mcp.Caller{User: user, APIKey: token}))

	s.mcp.Handler().ServeHTTP(c.Writer, req)
}

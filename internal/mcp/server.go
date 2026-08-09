package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/store"
	"github.com/fclairamb/dbbat/internal/version"
)

// Row cap. The agent may ask for fewer; it may never ask for more.
const (
	// DefaultMaxRows is what `query` returns when the agent says nothing.
	DefaultMaxRows = 200
	// HardMaxRows is the server-side ceiling. An agent asking for a million
	// rows gets HardMaxRows and a truncated flag — a model's idea of a
	// reasonable page size is not a server limit.
	HardMaxRows = 1000
)

// ErrNoGrant means the caller holds no active grant on the named database. It
// deliberately does not distinguish "no such database" from "no grant on it":
// the MCP surface must not be a way to enumerate databases a caller cannot
// reach.
var ErrNoGrant = errors.New("no active grant on that database")

// Caller is the authenticated identity behind one MCP request: the API key's
// owner, and the key itself, which doubles as the database password.
type Caller struct {
	User   *store.User
	APIKey string
}

type callerKey struct{}

// WithCaller attaches the authenticated caller to a request context. The HTTP
// layer does this after the ordinary API-key middleware has run.
func WithCaller(ctx context.Context, c *Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom reads the caller back. Absent means the request did not come
// through the authenticated route, which is a programming error, not a
// recoverable state.
func CallerFrom(ctx context.Context) *Caller {
	c, _ := ctx.Value(callerKey{}).(*Caller)

	return c
}

// GrantStore is the slice of the store this package reads. Narrow on purpose:
// the MCP server reads grant metadata (which the caller can already fetch from
// GET /api/v1/grants) and nothing else. It never reads or writes a query row,
// and it never resolves credentials — the proxy does all of that, over the
// wire, exactly as it does for psql.
type GrantStore interface {
	ListGrants(ctx context.Context, filter store.GrantFilter) ([]store.Grant, error)
	GetServerByUID(ctx context.Context, uid uuid.UUID) (*store.Server, error)
}

// Deps are the collaborators the MCP layer needs.
type Deps struct {
	Store  GrantStore
	Logger *slog.Logger
	// Broker resolves the shared event broker at call time. It is a function
	// because the process wiring installs the shared broker on the API server
	// *after* construction (Server.SetEventPlumbing), and the MCP layer must
	// see that instance rather than the placeholder it was built with.
	Broker func() *events.Broker
	// Executor runs statements through the loopback proxy listeners.
	Executor Executor
	// GraceWindow / AwaitWindow override the defaults; zero means default.
	// Test seams, and the knobs an operator would want first if the shape
	// ever needs tuning.
	GraceWindow time.Duration
	AwaitWindow time.Duration
}

// Server is dbbat's MCP layer.
type Server struct {
	deps        Deps
	schemaCache *mcpsdk.SchemaCache
	execs       *executions
	handler     http.Handler
}

// New builds the MCP layer.
func New(deps Deps) *Server {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}

	if deps.GraceWindow <= 0 {
		deps.GraceWindow = DefaultGraceWindow
	}

	if deps.AwaitWindow <= 0 {
		deps.AwaitWindow = DefaultAwaitWindow
	}

	s := &Server{
		deps:        deps,
		schemaCache: new(mcpsdk.SchemaCache),
		execs:       newExecutions(),
	}

	// Stateless: no Mcp-Session-Id, no server-side session state, one
	// short-lived MCP server per HTTP request bound to that request's
	// authenticated caller. It is the mode the SDK documents for exactly this
	// deployment, and it means a revoked API key stops working on the next
	// call rather than at the end of a session nobody is tracking.
	s.handler = mcpsdk.NewStreamableHTTPHandler(s.serverForRequest, &mcpsdk.StreamableHTTPOptions{
		Stateless: true,
		Logger:    deps.Logger,
	})

	return s
}

// Handler is the Streamable-HTTP MCP endpoint.
func (s *Server) Handler() http.Handler { return s.handler }

// serverForRequest builds the per-request MCP server. Tool closures capture
// the caller, so a handler can never be reached with an identity other than
// the one the middleware authenticated for this very request.
func (s *Server) serverForRequest(r *http.Request) *mcpsdk.Server {
	caller := CallerFrom(r.Context())
	if caller == nil || caller.User == nil {
		// The SDK answers 400. Reaching here means the route was mounted
		// outside the authenticated group.
		return nil
	}

	return s.serverFor(caller)
}

// mcpInstructions is handed to the model at initialize time. It is the only
// place the governance model is explained in-band, so it says the parts an
// agent has to plan around: grants expire, statements can be held, and rows
// are capped.
const mcpInstructions = `This server exposes databases through dbbat, a governed database proxy.

Access rules you must plan around:
- You can only reach databases the API key's owner currently holds a grant on.
  Call list_databases first; it reports each grant's expiry and controls.
- A grant may be read-only, may block DDL, and may carry quotas. Violations
  come back as errors from the database, not as tool errors you can retry.
- A statement matching an approval pattern is suspended mid-flight until a
  human approves it. When that happens, query returns status
  "approval_pending" with an execution_id: call await_approval with it,
  repeatedly if needed, until it returns a final status. Never treat a pending
  hold as a failure and never retry the statement — a retry parks a second
  connection on the same human.
- Every statement you run is logged and attributed to the key's owner.`

// serverFor builds an MCP server whose tools act as one caller.
func (s *Server) serverFor(caller *Caller) *mcpsdk.Server {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:       "dbbat",
		Title:      "DBBat governed database access",
		Version:    version.Version,
		WebsiteURL: "https://dbbat.com",
	}, &mcpsdk.ServerOptions{
		Instructions: mcpInstructions,
		SchemaCache:  s.schemaCache,
		Logger:       s.deps.Logger,
	})

	s.registerTools(srv, caller)

	return srv
}

// accessible is one database the caller currently holds a grant on.
type accessible struct {
	grant  *store.Grant
	server *store.Server
}

// listAccessible returns the databases the caller currently holds an active
// grant on, newest-winning grant first (ListGrants already orders the way the
// proxy picks).
func (s *Server) listAccessible(ctx context.Context, caller *Caller) ([]accessible, error) {
	userUID := caller.User.UID

	grants, err := s.deps.Store.ListGrants(ctx, store.GrantFilter{UserID: &userUID, ActiveOnly: true})
	if err != nil {
		return nil, fmt.Errorf("listing grants: %w", err)
	}

	out := make([]accessible, 0, len(grants))
	seen := make(map[uuid.UUID]struct{}, len(grants))

	for i := range grants {
		g := &grants[i]

		// A user can hold several grants on one database; the proxy admits a
		// session under the first one in this order, so that is the one whose
		// controls the agent should be told about.
		if _, dup := seen[g.DatabaseID]; dup {
			continue
		}

		seen[g.DatabaseID] = struct{}{}

		srv, err := s.deps.Store.GetServerByUID(ctx, g.DatabaseID)
		if err != nil {
			// A grant pointing at a deleted server is not something the agent
			// can act on; skipping it is closer to the truth than failing the
			// whole listing.
			s.deps.Logger.DebugContext(ctx, "skipping grant with unresolvable server",
				slog.String("grant_uid", g.UID.String()), slog.Any("error", err))

			continue
		}

		out = append(out, accessible{grant: g, server: srv})
	}

	return out, nil
}

// resolve finds the caller's grant on a named database.
func (s *Server) resolve(ctx context.Context, caller *Caller, name string) (accessible, error) {
	list, err := s.listAccessible(ctx, caller)
	if err != nil {
		return accessible{}, err
	}

	for _, a := range list {
		if a.server.Name == name {
			return a, nil
		}
	}

	return accessible{}, fmt.Errorf("%w: %q", ErrNoGrant, name)
}

// clampMaxRows applies the server-side row cap.
//
// This is enforced here, on the server, and not merely defaulted: whatever the
// agent asks for, it gets at most HardMaxRows. A model that decides it needs
// 10 million rows would otherwise turn one tool call into an out-of-memory.
func clampMaxRows(requested int) int {
	if requested <= 0 {
		return DefaultMaxRows
	}

	if requested > HardMaxRows {
		return HardMaxRows
	}

	return requested
}

// start launches a statement on a loopback connection and waits out the grace
// window.
//
// The returned execution is always registered: whether it finished inside the
// window or not, the caller reads its outcome from the same place, and a
// backgrounded one can be collected later by await_approval.
func (s *Server) start(ctx context.Context, caller *Caller, a accessible, sqlText string, params []any, maxRows int) *execution {
	s.execs.reap(time.Now())

	// Detached from the HTTP request on purpose: a held statement outlives the
	// request that issued it, and killing it when the request returns would
	// abandon the hold the human is being asked about. ExecutionMaxLifetime is
	// the bound instead.
	execCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ExecutionMaxLifetime)

	e := &execution{
		id:        uuid.NewString(),
		ownerUID:  caller.User.UID,
		database:  a.server.Name,
		sql:       sqlText,
		startedAt: time.Now(),
		cancel:    cancel,
		done:      make(chan struct{}),
	}

	s.execs.add(e)

	// Started before the statement so the hold's pending event cannot be
	// published into a broker nobody is listening to yet.
	go watchHold(execCtx, s.brokerOrNil(), s.execs, e)

	go func() {
		defer cancel()

		result, err := s.deps.Executor.Execute(execCtx, ExecRequest{
			Protocol: a.server.Protocol,
			Database: a.server.Name,
			Username: caller.User.Username,
			APIKey:   caller.APIKey,
			SQL:      sqlText,
			Params:   params,
			MaxRows:  maxRows,
		})

		e.finish(result, err)
		s.execs.markFinished(e)
	}()

	s.wait(ctx, e, s.deps.GraceWindow)

	return e
}

// wait blocks until the execution finishes, the window elapses, or the calling
// request is abandoned. It never returns an error: "not finished" is a state
// the caller renders, not a failure.
func (s *Server) wait(ctx context.Context, e *execution, window time.Duration) {
	timer := time.NewTimer(window)
	defer timer.Stop()

	select {
	case <-e.done:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// finished reports whether the execution has an outcome.
func (e *execution) finished() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

func (s *Server) brokerOrNil() *events.Broker {
	if s.deps.Broker == nil {
		return nil
	}

	return s.deps.Broker()
}

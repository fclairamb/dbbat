package shared

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// UsernameServerSeparator is the character that splits a proxy username into
// the dbbat user and the dbbat server it selects: `alice#demo_datalake_ro`.
//
// The username is the one slot every client, driver and IDE preserves verbatim
// on every connection it opens — unlike the database field, which DataGrip and
// friends rewrite per database as they walk the catalog. Putting the selector
// there is what lets the database field carry the *real* upstream name.
const UsernameServerSeparator = "#"

var (
	// ErrNoDatabaseRequested is returned when the client named neither a
	// database nor a server: there is nothing to resolve.
	ErrNoDatabaseRequested = errors.New("no database requested")

	// ErrTargetNotFound is the deliberately vague answer for "nothing here
	// matches", so a caller cannot enumerate servers they hold no grant on.
	ErrTargetNotFound = errors.New("database not found")

	// ErrDatabaseNotExposed is returned when a `user#server` username selected
	// a server, but the requested database is not the one that server exposes.
	// This is what an IDE's per-database reconnect (`postgres`, `template1`)
	// hits, so the message names both sides.
	ErrDatabaseNotExposed = errors.New("database not exposed by the selected server")

	// ErrTargetAmbiguous is returned when several servers the caller may reach
	// expose the same upstream database name — the `_ro` / `_rw` twin shape.
	ErrTargetAmbiguous = errors.New("database name is ambiguous")
)

// TargetStore is the slice of the store the target resolver needs. Narrow on
// purpose: it keeps the ladder unit-testable against a fake, with no container
// and no schema.
type TargetStore interface {
	// GetServerByName resolves a dbbat server by its unique name.
	GetServerByName(ctx context.Context, name string) (*store.Server, error)
	// ListServersByDatabaseName lists every target whose upstream
	// database_name matches — several rows is the normal case.
	ListServersByDatabaseName(ctx context.Context, databaseName string) ([]store.Server, error)
	// GetActiveGrant is the authoritative "may this user reach this server
	// right now" check, server-group coverage included.
	GetActiveGrant(ctx context.Context, userID, databaseID uuid.UUID) (*store.Grant, error)
}

// TargetRequest is what a protocol front-end knows after reading its startup
// packet: who is connecting, what they typed in the database field, and which
// server their username selected (if any).
type TargetRequest struct {
	// UserID is the authenticated dbbat user. Rung 3 is scoped to the grants
	// this user holds, which is what stops it from widening access.
	UserID uuid.UUID
	// ServerHint is the part after '#' in the username, or "".
	ServerHint string
	// RequestedDB is the client's database field, or "".
	RequestedDB string
	// ProtocolAccepted reports whether a server row's protocol may be reached
	// through this listener. Required — a nil predicate accepts nothing, so a
	// miswired caller fails closed instead of crossing protocols.
	ProtocolAccepted func(protocol string) bool
}

func (r TargetRequest) accepts(protocol string) bool {
	return r.ProtocolAccepted != nil && r.ProtocolAccepted(protocol)
}

// ResolveTarget maps what a client asked for onto exactly one dbbat server.
//
// The ladder, in order — the order is itself a security property:
//
//  1. The requested database is a dbbat server *name*. Exact name match always
//     wins, so an upstream `database_name` can never shadow another server's
//     name. This is the rule every proxy had before the username selector
//     existed, and it keeps every stored connection string working.
//
//  2. The username carried `user#server`. That server is the target; the
//     requested database must be empty or exactly the database it exposes,
//     otherwise the call is refused with a message naming both. Refusing here
//     rather than silently redirecting is deliberate: an IDE that reconnects
//     to `postgres` must be told what went wrong, not handed a different
//     database than the one it asked for. The explanatory message is reserved
//     for a caller who already holds an active grant on that server, so the
//     rung cannot be used to read back the upstream database name of a target
//     the caller may not reach.
//
//  3. Otherwise the requested name is read as an upstream database name, and
//     matched only against servers **the caller currently holds an active
//     grant on**. Exactly one survivor is the answer; several is a refusal
//     naming the candidates (the `_ro` / `_rw` twins), none is "not found".
//     Scoping to grants is what makes this rung incapable of widening access:
//     every server it can return was already reachable by this caller.
//
// The returned server is never an SSH bastion or a Kubernetes cluster — both
// store reads exclude them.
func ResolveTarget(ctx context.Context, st TargetStore, req TargetRequest) (*store.Server, error) {
	requested := strings.TrimSpace(req.RequestedDB)
	hint := strings.TrimSpace(req.ServerHint)

	if requested == "" && hint == "" {
		return nil, ErrNoDatabaseRequested
	}

	// Rung 1: exact server-name match wins.
	if requested != "" {
		if srv, err := st.GetServerByName(ctx, requested); err == nil && req.accepts(srv.Protocol) {
			return srv, nil
		}
	}

	// Rung 2: the username selected a server.
	if hint != "" {
		return resolveFromHint(ctx, st, req, hint, requested)
	}

	// Rung 3: the requested name is an upstream database name.
	return resolveFromGrants(ctx, st, req, requested)
}

// resolveFromHint handles rung 2: the username named a server explicitly.
func resolveFromHint(
	ctx context.Context,
	st TargetStore,
	req TargetRequest,
	hint, requested string,
) (*store.Server, error) {
	srv, err := st.GetServerByName(ctx, hint)
	if err != nil || !req.accepts(srv.Protocol) {
		return nil, fmt.Errorf("%w: no server named %q on this listener", ErrTargetNotFound, hint)
	}

	if requested == "" || requested == srv.DatabaseName {
		return srv, nil
	}

	// The explicit message names the upstream database this server exposes,
	// which is more than "not found" tells. Only say it to someone who already
	// holds an active grant on that server — an authenticated user must not be
	// able to turn a `user#server` probe into a read of the upstream database
	// name of every registered target. Everyone else gets the same answer as an
	// unknown name.
	//
	// req.UserID is a *verified* identity, never a claimed one: every front-end
	// resolves after its credential check (PostgreSQL after the cleartext
	// password exchange, MySQL in OnAuthSuccess, SQL Server after verifying the
	// LOGIN7 credential, MongoDB after SASL). This check therefore narrows what
	// an authenticated caller may learn; it is not what keeps an anonymous one
	// out, and it never was — that is the front-ends' ordering.
	if _, gerr := st.GetActiveGrant(ctx, req.UserID, srv.UID); gerr != nil {
		return nil, fmt.Errorf("%w: no server named %q on this listener", ErrTargetNotFound, hint)
	}

	return nil, fmt.Errorf(
		"%w: server %q exposes database %q, not %q",
		ErrDatabaseNotExposed, srv.Name, srv.DatabaseName, requested,
	)
}

// resolveFromGrants handles rung 3: match the requested name against the
// upstream database_name of the servers this caller may already reach.
func resolveFromGrants(
	ctx context.Context,
	st TargetStore,
	req TargetRequest,
	requested string,
) (*store.Server, error) {
	rows, err := st.ListServersByDatabaseName(ctx, requested)
	if err != nil {
		return nil, ErrTargetNotFound
	}

	var candidates []store.Server

	for i := range rows {
		if !req.accepts(rows[i].Protocol) {
			continue
		}

		// The authoritative coverage check, so a grant bound to a server group
		// counts exactly as the proxies count it — and nothing else does.
		if _, gerr := st.GetActiveGrant(ctx, req.UserID, rows[i].UID); gerr != nil {
			continue
		}

		candidates = append(candidates, rows[i])
	}

	switch len(candidates) {
	case 0:
		return nil, ErrTargetNotFound
	case 1:
		return &candidates[0], nil
	default:
		return nil, fmt.Errorf(
			"%w: database %q is exposed by several servers (%s); connect as \"<user>%s<server>\" to choose one",
			ErrTargetAmbiguous, requested, strings.Join(serverNames(candidates), ", "), UsernameServerSeparator,
		)
	}
}

func serverNames(servers []store.Server) []string {
	names := make([]string, 0, len(servers))
	for i := range servers {
		names = append(names, servers[i].Name)
	}

	return names
}

// ParseUsername splits a proxy username into the dbbat user and the optional
// dbbat server it selects: "alice#demo_datalake_ro" → ("alice",
// "demo_datalake_ro"). Split on the **last** separator so a username may itself
// contain one; with no separator the hint is empty and the name is returned
// unchanged.
//
// Only the bare name is ever looked up, recorded on the connection row, written
// to the audit log, shown in Slack or matched by the approval gate — and the
// upstream credential is the one stored on the server row, so the selector
// never reaches the target database.
func ParseUsername(raw string) (string, string) {
	if idx := strings.LastIndex(raw, UsernameServerSeparator); idx >= 0 {
		return raw[:idx], raw[idx+len(UsernameServerSeparator):]
	}

	return raw, ""
}

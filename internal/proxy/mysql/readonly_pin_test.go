package mysql

import (
	"net"
	"sync"
	"testing"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	gomysqlserver "github.com/go-mysql-org/go-mysql/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// recordingHandler answers every query and remembers it; with fail set it
// answers an error, like a server that does not know the statement.
type recordingHandler struct {
	stubCommandHandler

	mu      sync.Mutex
	queries []string
	fail    bool
}

func (h *recordingHandler) HandleQuery(q string) (*gomysql.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.queries = append(h.queries, q)

	if h.fail {
		return nil, errStubHandlerUnused
	}

	return nil, nil //nolint:nilnil // nil result is an OK packet
}

func pinSession(t *testing.T, h *recordingHandler, controls []string) *Session {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}

		auth := gomysqlserver.NewInMemoryAuthenticationHandler(gomysql.AUTH_NATIVE_PASSWORD)
		if aerr := auth.AddUser("u", "p", gomysql.AUTH_NATIVE_PASSWORD); aerr != nil {
			return
		}

		sc, nerr := gomysqlserver.NewDefaultServer().NewCustomizedConn(c, auth, h)
		if nerr != nil {
			return
		}

		for {
			if sc.HandleCommand() != nil {
				return
			}
		}
	}()

	conn, err := gomysqlclient.Connect(ln.Addr().String(), "u", "p", "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return &Session{
		upstreamConn: conn,
		grant:        &store.Grant{Definition: &store.GrantDefinition{Controls: controls}},
	}
}

func TestPinReadOnly_IssuesSetForReadOnlyGrant(t *testing.T) {
	t.Parallel()

	h := &recordingHandler{}
	s := pinSession(t, h, []string{store.ControlReadOnly})

	require.NoError(t, s.pinReadOnly())
	assert.Equal(t, []string{"SET SESSION TRANSACTION READ ONLY"}, h.queries)
}

func TestPinReadOnly_NoStatementWithoutReadOnly(t *testing.T) {
	t.Parallel()

	h := &recordingHandler{}
	s := pinSession(t, h, []string{store.ControlBlockDDL})

	require.NoError(t, s.pinReadOnly())
	assert.Empty(t, h.queries)
}

func TestPinReadOnly_FailsWhenUpstreamRefuses(t *testing.T) {
	t.Parallel()

	h := &recordingHandler{fail: true}
	s := pinSession(t, h, []string{store.ControlReadOnly})

	require.ErrorIs(t, s.pinReadOnly(), ErrUpstreamReadOnlyMode)
}

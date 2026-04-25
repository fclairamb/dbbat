package mysql

import (
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
)

// handler implements go-mysql-org/go-mysql server.Handler for a single Session.
//
// Phase 1b/1c: UseDB captures the database name during the MySQL handshake so
// the auth handler can look up grants. All command-phase methods return
// ErrCommandNotPermitted because no upstream is connected yet — Phase 1d
// replaces them with real forwarding to the upstream client.Conn, and Phase 2
// adds query interception.
type handler struct {
	session *Session
}

// UseDB is called by go-mysql in two contexts:
//   - During the handshake when the client includes a default database name
//     in HandshakeResponse41 (CLIENT_CONNECT_WITH_DB).
//   - At command time, on COM_INIT_DB.
//
// During handshake (s.authComplete == false) we just stash the requested
// database name; the auth handler validates it in OnAuthSuccess. Command-time
// switches are refused — staying on a single grant-bound database matches
// the PostgreSQL proxy's behavior of disallowing mid-session \c.
func (h *handler) UseDB(dbName string) error {
	if !h.session.authComplete {
		h.session.requestedDB = dbName

		return nil
	}

	if dbName == h.session.database.Name || dbName == h.session.database.DatabaseName {
		return nil
	}

	return ErrSwitchDatabaseDenied
}

func (h *handler) HandleQuery(string) (*gomysql.Result, error) {
	return nil, ErrCommandNotPermitted
}

func (h *handler) HandleFieldList(string, string) ([]*gomysql.Field, error) {
	return nil, ErrCommandNotPermitted
}

func (h *handler) HandleStmtPrepare(string) (int, int, any, error) {
	return 0, 0, nil, ErrCommandNotPermitted
}

func (h *handler) HandleStmtExecute(any, string, []any) (*gomysql.Result, error) {
	return nil, ErrCommandNotPermitted
}

func (h *handler) HandleStmtClose(any) error { return nil }

func (h *handler) HandleOtherCommand(byte, []byte) error {
	return ErrCommandNotPermitted
}

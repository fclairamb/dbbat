package oracle

import (
	"fmt"
	"net"
	"reflect"
	"unsafe"

	goora "github.com/sijms/go-ora/v2"
)

// upstreamAuth performs the full Oracle authentication sequence to the upstream server.
// dbbat acts as an Oracle client, using stored database credentials.
//
// Sequence:
//  1. Send TNS Connect with the database's service name
//  2. Handle Resend loops, receive Accept
//  3. Set Protocol exchange
//  4. Set Data Types exchange
//  5. O5LOGON authentication with stored credentials
//  6. Enter relay mode (handled by caller)
func (s *session) upstreamAuth() error {
	if err := s.upstreamGoOraAuth(); err != nil {
		return fmt.Errorf("upstream auth failed: %w", err)
	}

	s.logger.InfoContext(s.ctx, "upstream Oracle authentication complete")

	return nil
}

// upstreamGoOraAuth uses go-ora as a library to establish an authenticated upstream Oracle
// connection, then extracts the raw net.Conn via reflection for bidirectional relay.
func (s *session) upstreamGoOraAuth() error {
	serviceName := s.database.DatabaseName
	if s.database.OracleServiceName != nil && *s.database.OracleServiceName != "" {
		serviceName = *s.database.OracleServiceName
	}

	// Decrypt password
	if err := s.database.DecryptPassword(s.encryptionKey); err != nil {
		return fmt.Errorf("failed to decrypt database password: %w", err)
	}

	// SERVER+LOCATION=UTC short-circuits go-ora's post-auth `SELECT SYSTIMESTAMP FROM DUAL`
	// in getDBServerTimeZone. Without this, that query has scanned into a time.Time which
	// has unexported fields — go-ora's reflection-based Scan path panics with
	// "reflect.Value.Interface: cannot return value obtained from unexported field or method".
	dsn := fmt.Sprintf("oracle://%s:%s@%s/%s?SERVER+LOCATION=UTC",
		s.database.Username, s.database.Password,
		net.JoinHostPort(s.database.Host, fmt.Sprintf("%d", s.database.Port)), serviceName)

	conn, err := goora.NewConnection(dsn, nil)
	if err != nil {
		return fmt.Errorf("go-ora NewConnection: %w", err)
	}

	// Pre-set NLSData.Language so go-ora skips its post-auth GetNLS PL/SQL block
	// (the same reflective binding path that panics). Any non-empty value works —
	// dbbat only needs the authenticated raw conn for relay, not NLS metadata.
	conn.NLSData.Language = "AMERICAN"

	if err := conn.OpenWithContext(s.ctx); err != nil {
		return fmt.Errorf("go-ora Open: %w", err)
	}

	// Extract the raw net.Conn from go-ora's internal session via reflection
	rawConn := extractGoOraConn(conn)
	if rawConn == nil {
		_ = conn.Close()

		return fmt.Errorf("%w: failed to extract raw connection", ErrAuthFailed)
	}

	s.upstreamConn = rawConn
	// Store the go-ora connection to prevent GC and to close it properly later
	s.goOraConn = conn

	return nil
}

// extractGoOraConn extracts the raw net.Conn from a go-ora Connection via unsafe reflection.
// go-ora's session.conn is unexported, so we use unsafe.Pointer to access it.
func extractGoOraConn(conn *goora.Connection) net.Conn {
	connVal := reflect.ValueOf(conn).Elem()
	sessField := connVal.FieldByName("session")

	if !sessField.IsValid() || sessField.IsNil() {
		return nil
	}

	connField := sessField.Elem().FieldByName("conn")
	if !connField.IsValid() {
		return nil
	}

	// Use unsafe to read the unexported field
	ptr := unsafe.Pointer(connField.UnsafeAddr())
	rawConn := *(*net.Conn)(ptr)

	return rawConn
}

package upstream

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
)

// pgSSLRequestCode is the magic protocol version identifying an SSLRequest
// packet. The PostgreSQL proxy recognizes the same constant on its listener
// side; here it is what dbbat writes upstream.
const pgSSLRequestCode = 80877103

// pgSSLRequest is the 8-byte SSLRequest preamble (length=8, magic
// pgSSLRequestCode) sent before the StartupMessage to probe a Postgres server
// for TLS support.
var pgSSLRequest = func() []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf[0:4], 8)
	binary.BigEndian.PutUint32(buf[4:8], pgSSLRequestCode)

	return buf
}()

// negotiatePostgresSSL probes the upstream Postgres for TLS and either upgrades
// the connection, falls back to plaintext, or fails, per plan. The policy
// itself lives in PlanFor — this function only executes it over PostgreSQL's
// in-band SSLRequest handshake:
//
//   - a plan that never encrypts:   no probe sent, plaintext.
//   - server answers 'S':           upgrade with the plan's tls.Config.
//   - server answers 'N':           continue plaintext if the plan allows it,
//     fail otherwise.
//
// It returns the (possibly upgraded) connection and whether it ended up
// encrypted. On error the original conn is left open; the caller closes it.
func negotiatePostgresSSL(ctx context.Context, conn net.Conn, plan Plan) (net.Conn, bool, error) {
	if !plan.OffersTLS() {
		return conn, false, nil
	}

	if _, err := conn.Write(pgSSLRequest); err != nil {
		return nil, false, fmt.Errorf("send SSLRequest: %w", err)
	}

	resp := make([]byte, 1)
	if _, err := conn.Read(resp); err != nil {
		return nil, false, fmt.Errorf("read SSL response: %w", err)
	}

	switch resp[0] {
	case 'S':
		tlsConn := tls.Client(conn, plan.TLSConfig())
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, false, fmt.Errorf("upstream TLS handshake: %w", err)
		}

		return tlsConn, true, nil
	case 'N':
		if plan.RequiresTLS() {
			return nil, false, fmt.Errorf("%w: ssl_mode=%s", ErrPostgresTLSRequired, plan.Mode)
		}

		return conn, false, nil
	default:
		return nil, false, fmt.Errorf("%w: 0x%02x", ErrPostgresSSLResponse, resp[0])
	}
}

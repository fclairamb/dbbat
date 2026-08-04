package postgresql

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"

	"github.com/fclairamb/dbbat/internal/proxy/upstream"
)

// upstreamSSLRequest is the 8-byte SSLRequest preamble (length=8, magic
// pgSSLRequestCode) sent before the StartupMessage to probe a Postgres server
// for TLS support.
var upstreamSSLRequest = func() []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf[0:4], 8)
	binary.BigEndian.PutUint32(buf[4:8], pgSSLRequestCode)
	return buf
}()

// negotiateUpstreamSSL probes the upstream Postgres for TLS and either
// upgrades the connection, falls back to plaintext, or fails based on
// ssl_mode. The policy itself lives in upstream.PlanFor — this function only
// executes it over PostgreSQL's in-band SSLRequest handshake:
//
//   - a plan that never encrypts:   no probe sent, plaintext.
//   - server answers 'S':           upgrade with the plan's tls.Config.
//   - server answers 'N':           continue plaintext if the plan allows it,
//     fail otherwise.
//
// On error the original conn is left open; the caller is responsible for
// closing it.
func negotiateUpstreamSSL(ctx context.Context, conn net.Conn, host, mode string) (net.Conn, error) {
	plan := upstream.PlanFor(mode, host)

	if !plan.OffersTLS() {
		return conn, nil
	}

	if _, err := conn.Write(upstreamSSLRequest); err != nil {
		return nil, fmt.Errorf("send SSLRequest: %w", err)
	}

	resp := make([]byte, 1)
	if _, err := conn.Read(resp); err != nil {
		return nil, fmt.Errorf("read SSL response: %w", err)
	}

	switch resp[0] {
	case 'S':
		tlsConn := tls.Client(conn, plan.TLSConfig())
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("upstream TLS handshake: %w", err)
		}
		return tlsConn, nil
	case 'N':
		if plan.RequiresTLS() {
			return nil, fmt.Errorf("%w: ssl_mode=%s", ErrUpstreamTLSRequired, mode)
		}
		return conn, nil
	default:
		return nil, fmt.Errorf("%w: 0x%02x", ErrUpstreamSSLResponse, resp[0])
	}
}

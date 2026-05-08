package postgresql

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
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
// ssl_mode. Mirrors libpq sslmode semantics:
//
//   - disable:                  plaintext only, no probe sent.
//   - allow / prefer / "":      probe; on 'S' upgrade with cert verification
//     skipped, on 'N' continue plaintext.
//   - require:                  probe; on 'S' upgrade with cert verification
//     skipped, on 'N' fail.
//   - verify-ca / verify-full:  probe; on 'S' upgrade with full cert + hostname
//     verification, on 'N' fail. (Go stdlib doesn't cleanly express
//     "verify CA but not hostname", so verify-ca is treated as verify-full —
//     stricter than libpq, but safer.)
//
// On error the original conn is left open; the caller is responsible for
// closing it.
func negotiateUpstreamSSL(ctx context.Context, conn net.Conn, host, mode string) (net.Conn, error) {
	if mode == "disable" {
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
		tlsConn := tls.Client(conn, upstreamTLSConfig(host, mode))
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("upstream TLS handshake: %w", err)
		}
		return tlsConn, nil
	case 'N':
		if upstreamTLSRequired(mode) {
			return nil, fmt.Errorf("%w: ssl_mode=%s", ErrUpstreamTLSRequired, mode)
		}
		return conn, nil
	default:
		return nil, fmt.Errorf("%w: 0x%02x", ErrUpstreamSSLResponse, resp[0])
	}
}

// upstreamTLSRequired reports whether the ssl_mode forbids plaintext fallback.
func upstreamTLSRequired(mode string) bool {
	switch mode {
	case "require", "verify-ca", "verify-full":
		return true
	}
	return false
}

// upstreamTLSConfig builds a tls.Config for an upstream connection at the
// given ssl_mode. verify-ca/verify-full both verify the cert chain and the
// hostname (see negotiateUpstreamSSL doc); other modes accept any cert.
func upstreamTLSConfig(host, mode string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch mode {
	case "verify-ca", "verify-full":
		cfg.ServerName = host
	default:
		// require/prefer parity with libpq: encrypt without authenticating the server.
		cfg.InsecureSkipVerify = true
	}
	return cfg
}

package mssql

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/fclairamb/dbbat/internal/config"
)

// ErrTLSConfigInvalid is returned when only one of cert/key files is set.
var ErrTLSConfigInvalid = errors.New("mssql tls: cert_file and key_file must both be set or both empty")

// defaultTLSMaxVersion is the client-leg ceiling when nothing else is asked
// for: TLS 1.2, matching config.MSSQLDefaultTLSMaxVersion.
//
// TDS wraps the handshake in PRELOGIN packets and un-wraps it the moment the
// handshake completes, so both peers have to agree on where that moment is.
// Under TLS 1.2 the last thing each side does is read its peer's Finished,
// which lands the switch on the same byte for both. Under TLS 1.3 the client's
// handshake ends on a *write*, and drivers differ on whether that final flight
// is still encapsulated — a disagreement that classically presents as a hang
// rather than an error. handshakeConn tolerates both (it sniffs the first byte
// of every inbound flight), but only go-mssqldb has been verified end to end,
// so 1.3 stays opt-in via DBB_MSSQL_TLS_MAX_VERSION. See docs/mssql.md.
const defaultTLSMaxVersion = tls.VersionTLS12

// applyMaxVersion sets the negotiated ceiling on a server config, plus the one
// knob that has to move with it.
//
// Session tickets are switched off whenever 1.3 is reachable. In TLS 1.3 the Go
// server emits NewSessionTicket *after* its Finished and before it reads the
// client's — so the tickets ride in the same encapsulated flight, and a client
// whose handshake returns without consuming them is left with unread bytes in
// a PRELOGIN message it will never look at again. That corrupts the stream a
// few reads later, somewhere unrelated. Under TLS 1.2 tickets arrive inside the
// handshake and are always consumed, so the flag only ever costs a resumption
// that TDS — one handshake per connection — barely benefits from.
func applyMaxVersion(cfg *tls.Config, maxVersion uint16) *tls.Config {
	cfg.MaxVersion = maxVersion
	cfg.SessionTicketsDisabled = maxVersion >= tls.VersionTLS13

	return cfg
}

// loadTLSConfig resolves the TLS server config for the SQL Server proxy.
//
// Behavior mirrors the MySQL/PG/Mongo proxies:
//   - cfg.TLS.Disable == true:   returns nil (the listener answers
//     ENCRYPT_NOT_SUP and stays plaintext).
//   - cert_file + key_file set:  load from disk.
//   - both empty (default):      auto-generate a self-signed cert. Suitable
//     for development; production deployments should provide a real cert.
//
// The version ceiling comes from cfg.TLSMaxVersion and is validated here as
// well as in config.Load, so a Server built straight from a struct literal
// cannot end up with an unvalidated value.
func loadTLSConfig(cfg config.MSSQLConfig) (*tls.Config, error) {
	if cfg.TLS.Disable {
		return nil, nil //nolint:nilnil // nil config = TLS disabled, no error
	}

	maxVersion, err := cfg.ResolveTLSMaxVersion()
	if err != nil {
		return nil, fmt.Errorf("mssql tls: %w", err)
	}

	switch {
	case cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "":
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("mssql tls: load cert/key: %w", err)
		}

		return applyMaxVersion(&tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}, maxVersion), nil

	case cfg.TLS.CertFile == "" && cfg.TLS.KeyFile == "":
		selfSigned, err := generateSelfSignedTLS()
		if err != nil {
			return nil, err
		}

		return applyMaxVersion(selfSigned, maxVersion), nil

	default:
		return nil, ErrTLSConfigInvalid
	}
}

// generateSelfSignedTLS produces an in-memory RSA-2048 self-signed certificate
// for the proxy.
func generateSelfSignedTLS() (*tls.Config, error) {
	const rsaBits = 2048

	priv, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, fmt.Errorf("mssql tls: generate rsa key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("mssql tls: serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "dbbat-mssql-proxy"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("mssql tls: create cert: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{derBytes}, PrivateKey: priv}},
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   defaultTLSMaxVersion,
	}, nil
}

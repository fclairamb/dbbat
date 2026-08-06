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

// tlsMaxVersion pins the client-leg handshake to TLS 1.2.
//
// TDS wraps the handshake in PRELOGIN packets and un-wraps it the moment the
// handshake completes, so both peers have to agree on where that moment is.
// Under TLS 1.2 the last thing each side does is read its peer's Finished,
// which lands the switch on the same byte for both. Under TLS 1.3 the client's
// handshake ends on a *write*, and drivers differ on whether that final flight
// is still encapsulated — a disagreement that presents as a hang rather than an
// error. SQL Server clients all speak TLS 1.2, so v1 pins it and says so in
// docs/mssql.md.
const tlsMaxVersion = tls.VersionTLS12

// loadTLSConfig resolves the TLS server config for the SQL Server proxy.
//
// Behavior mirrors the MySQL/PG/Mongo proxies:
//   - cfg.TLS.Disable == true:   returns nil (the listener answers
//     ENCRYPT_NOT_SUP and stays plaintext).
//   - cert_file + key_file set:  load from disk.
//   - both empty (default):      auto-generate a self-signed cert. Suitable
//     for development; production deployments should provide a real cert.
func loadTLSConfig(cfg config.MSSQLConfig) (*tls.Config, error) {
	if cfg.TLS.Disable {
		return nil, nil //nolint:nilnil // nil config = TLS disabled, no error
	}

	switch {
	case cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "":
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("mssql tls: load cert/key: %w", err)
		}

		return &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			MaxVersion:   tlsMaxVersion,
		}, nil

	case cfg.TLS.CertFile == "" && cfg.TLS.KeyFile == "":
		return generateSelfSignedTLS()

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
		MaxVersion:   tlsMaxVersion,
	}, nil
}

package mongodb

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
var ErrTLSConfigInvalid = errors.New("mongodb tls: cert_file and key_file must both be set or both empty")

// loadTLSConfig resolves the TLS server config for the MongoDB proxy.
//
// Behavior mirrors the MySQL/PG proxies:
//   - cfg.TLS.Disable == true:   returns nil (listener stays plaintext).
//   - cert_file + key_file set:  load from disk.
//   - both empty (default):      auto-generate a self-signed cert. Suitable
//     for development; production deployments should provide a real cert.
//
// MongoDB TLS is implicit-from-byte-0 (no STARTTLS): the session peeks the
// first client byte (0x16 = TLS handshake) to support both TLS and plaintext
// on one listener.
func loadTLSConfig(cfg config.MongoConfig) (*tls.Config, error) {
	if cfg.TLS.Disable {
		return nil, nil //nolint:nilnil // nil config = TLS disabled, no error
	}

	switch {
	case cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "":
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("mongodb tls: load cert/key: %w", err)
		}

		return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil

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
		return nil, fmt.Errorf("mongodb tls: generate rsa key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("mongodb tls: serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "dbbat-mongodb-proxy"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("mongodb tls: create cert: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{derBytes}, PrivateKey: priv}},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

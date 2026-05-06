package postgresql

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
var ErrTLSConfigInvalid = errors.New("postgresql tls: cert_file and key_file must both be set or both empty")

// loadTLS resolves the TLS server config used to terminate client TLS at the
// PostgreSQL proxy. PG has no caching_sha2-style RSA fallback, so unlike
// MySQL we don't return an RSA key alongside.
//
// Behavior:
//   - cfg.TLS.Disable == true:    no TLS (caller will reject SSLRequest with 'N').
//   - cert_file + key_file set:   load from disk via tls.LoadX509KeyPair.
//   - both empty (default):       auto-generate a self-signed cert. Suitable
//     for development; production should provide a real certificate.
//   - exactly one set:            return ErrTLSConfigInvalid.
func loadTLS(cfg config.PGConfig) (*tls.Config, error) {
	if cfg.TLS.Disable {
		return nil, nil //nolint:nilnil // nil signals "TLS off" to caller, like MySQL's loadTLSAndRSA
	}

	switch {
	case cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "":
		return loadTLSFromFiles(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	case cfg.TLS.CertFile == "" && cfg.TLS.KeyFile == "":
		return generateSelfSignedTLS()
	default:
		return nil, ErrTLSConfigInvalid
	}
}

func loadTLSFromFiles(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("postgresql tls: load cert/key: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// generateSelfSignedTLS produces an in-memory RSA-2048 self-signed certificate
// for the proxy. CN dbbat-pg-proxy, SAN localhost, 10-year validity — matches
// the MySQL self-signed shape so operator UX is symmetric.
func generateSelfSignedTLS() (*tls.Config, error) {
	const rsaBits = 2048

	priv, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, fmt.Errorf("postgresql tls: generate rsa key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("postgresql tls: serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "dbbat-pg-proxy"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("postgresql tls: create cert: %w", err)
	}

	tlsCert := tls.Certificate{
		Certificate: [][]byte{derBytes},
		PrivateKey:  priv,
	}

	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

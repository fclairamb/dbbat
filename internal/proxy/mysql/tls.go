package mysql

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/fclairamb/dbbat/internal/config"
)

// ErrTLSConfigInvalid is returned when only one of cert/key files is set.
var ErrTLSConfigInvalid = errors.New("mysql tls: cert_file and key_file must both be set or both empty")

// loadTLSAndRSA resolves TLS server config and the RSA keypair used for the
// caching_sha2_password full-auth path on non-TLS connections.
//
// Behavior:
//   - cfg.TLS.Disable == true:   no TLS, no RSA key (caching_sha2 falls back).
//   - cert_file + key_file set:  load from disk; reuse the cert's RSA key for
//     the public-key-retrieval path.
//   - both empty (default):      auto-generate a self-signed cert and a fresh
//     RSA key.  Suitable for development; production should provide a real
//     certificate.
func loadTLSAndRSA(cfg config.MySQLConfig) (*tls.Config, *rsa.PrivateKey, error) {
	if cfg.TLS.Disable {
		return nil, nil, nil
	}

	switch {
	case cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "":
		return loadTLSFromFiles(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	case cfg.TLS.CertFile == "" && cfg.TLS.KeyFile == "":
		return generateSelfSignedTLS()
	default:
		return nil, nil, ErrTLSConfigInvalid
	}
}

func loadTLSFromFiles(certFile, keyFile string) (*tls.Config, *rsa.PrivateKey, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("mysql tls: load cert/key: %w", err)
	}

	rsaKey, _ := cert.PrivateKey.(*rsa.PrivateKey)

	// Validate the key file is RSA — caching_sha2_password's RSA fallback
	// needs an RSA private key. ECDSA/Ed25519 keys give a working TLS server
	// but no public-key-retrieval path; we accept that silently (the proxy
	// just won't support non-TLS caching_sha2 clients).
	if rsaKey == nil {
		// re-parse the key file directly to surface the actual key bytes
		raw, ferr := os.ReadFile(keyFile)
		if ferr == nil {
			rsaKey = parseRSAFromPEM(raw)
		}
	}

	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	return tlsConf, rsaKey, nil
}

func parseRSAFromPEM(data []byte) *rsa.PrivateKey {
	for {
		block, rest := pem.Decode(data)
		if block == nil {
			return nil
		}

		if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
			return k
		}

		if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
			if rk, ok := k.(*rsa.PrivateKey); ok {
				return rk
			}
		}

		data = rest
	}
}

// generateSelfSignedTLS produces an in-memory RSA-2048 self-signed certificate
// for the proxy. The same RSA key is reused for the caching_sha2 public-key
// retrieval path so a single keypair backs both TLS and the RSA fallback.
func generateSelfSignedTLS() (*tls.Config, *rsa.PrivateKey, error) {
	const rsaBits = 2048

	priv, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, nil, fmt.Errorf("mysql tls: generate rsa key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("mysql tls: serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "dbbat-mysql-proxy"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("mysql tls: create cert: %w", err)
	}

	tlsCert := tls.Certificate{
		Certificate: [][]byte{derBytes},
		PrivateKey:  priv,
	}

	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	}

	return tlsConf, priv, nil
}

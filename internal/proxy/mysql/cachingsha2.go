package mysql

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"reflect"
	"strings"
	"unsafe"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	gomysqlserver "github.com/go-mysql-org/go-mysql/server"
)

// driveCachingSha2FullAuth executes the server side of caching_sha2_password
// "full authentication" against a connected client and returns the cleartext
// password the client sent.
//
// We always force the full-auth path: dbbat stores Argon2id hashes, not the
// plaintext-derived hash that would be needed for the fast-auth scramble
// validation. This is safe — fast-auth is purely a performance optimization
// for repeated logins from the same client+server pair.
//
// Two transport paths:
//   - TLS: the client sends the cleartext password over the encrypted
//     channel. We strip the trailing NUL.
//   - non-TLS: the client either already sent the RSA-encrypted password
//     (with --get-server-public-key) or sends 0x02 to request our public key
//     first. We respond with the PEM-encoded RSA pubkey, read the encrypted
//     password, decrypt with RSA-OAEP+SHA1, then XOR with the salt to
//     recover the plaintext.
//
// Returns the plaintext password (without trailing NUL) on success.
func driveCachingSha2FullAuth(c *gomysqlserver.Conn, rsaKey *rsa.PrivateKey) (string, error) {
	if err := writeAuthMoreDataFullAuth(c); err != nil {
		return "", fmt.Errorf("write full-auth packet: %w", err)
	}

	authData, err := c.ReadPacket()
	if err != nil {
		return "", fmt.Errorf("read full-auth response: %w", err)
	}

	if isTLSConn(c) {
		return trimTrailingNUL(string(authData)), nil
	}

	// Non-TLS: client may request the public key first (single 0x02 byte).
	if len(authData) == 1 && authData[0] == 0x02 {
		if rsaKey == nil {
			return "", ErrCachingSha2NeedsRSA
		}

		if err := writeAuthMoreDataPubkey(c, rsaKey); err != nil {
			return "", fmt.Errorf("write pubkey: %w", err)
		}

		authData, err = c.ReadPacket()
		if err != nil {
			return "", fmt.Errorf("read encrypted password: %w", err)
		}
	}

	if rsaKey == nil {
		return "", ErrCachingSha2NeedsRSA
	}

	salt, err := readConnSalt(c)
	if err != nil {
		return "", err
	}

	plain, err := decryptCachingSha2Password(authData, rsaKey, salt)
	if err != nil {
		return "", err
	}

	return plain, nil
}

// packetHeaderSize is the 4-byte length+sequence header WritePacket fills in.
const packetHeaderSize = 4

func writeAuthMoreDataFullAuth(c *gomysqlserver.Conn) error {
	data := make([]byte, packetHeaderSize, packetHeaderSize+2)
	data = append(data, gomysql.MORE_DATE_HEADER, gomysql.CACHE_SHA2_FULL_AUTH)

	return c.WritePacket(data)
}

func writeAuthMoreDataPubkey(c *gomysqlserver.Conn, key *rsa.PrivateKey) error {
	derBytes, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return fmt.Errorf("marshal public key: %w", err)
	}

	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: derBytes})

	data := make([]byte, packetHeaderSize, packetHeaderSize+1+len(pemBytes))
	data = append(data, gomysql.MORE_DATE_HEADER)
	data = append(data, pemBytes...)

	return c.WritePacket(data)
}

func decryptCachingSha2Password(encrypted []byte, key *rsa.PrivateKey, salt []byte) (string, error) {
	decrypted, err := rsa.DecryptOAEP(sha1.New(), rand.Reader, key, encrypted, nil)
	if err != nil {
		return "", fmt.Errorf("rsa-oaep decrypt: %w", err)
	}

	plaintext := xorWithSalt(decrypted, salt)

	return trimTrailingNUL(string(plaintext)), nil
}

// xorWithSalt XORs each byte of data with the salt, cycling the salt.
// The MySQL client encrypts (password XOR salt), so XORing again recovers
// the password.
func xorWithSalt(data, salt []byte) []byte {
	if len(salt) == 0 {
		return data
	}

	out := make([]byte, len(data))
	for i := range data {
		out[i] = data[i] ^ salt[i%len(salt)]
	}

	return out
}

func trimTrailingNUL(s string) string {
	return strings.TrimRight(s, "\x00")
}

// isTLSConn reports whether the underlying transport has been upgraded to
// TLS. The go-mysql library upgrades the embedded net.Conn to *tls.Conn
// when it processes an SSL Request packet during the handshake.
func isTLSConn(c *gomysqlserver.Conn) bool {
	if c == nil || c.Conn == nil {
		return false
	}

	_, ok := c.Conn.Conn.(*tls.Conn)

	return ok
}

// readConnSalt reads the 20-byte challenge salt the library generated when
// it built the initial handshake packet. The library exposes no public
// accessor, so we use unsafe + reflect to read the unexported `salt` field.
//
// This is intentionally fragile — if go-mysql renames or removes the field
// the proxy will fail-fast at handshake time rather than silently mis-decrypt
// passwords. Pinning to a known go-mysql version is the simplest mitigation.
func readConnSalt(c *gomysqlserver.Conn) ([]byte, error) {
	v := reflect.ValueOf(c).Elem()

	field := v.FieldByName("salt")
	if !field.IsValid() {
		return nil, ErrSaltFieldMissing
	}

	if field.Kind() != reflect.Slice || field.Type().Elem().Kind() != reflect.Uint8 {
		return nil, ErrSaltFieldUnexpectedType
	}

	saltSlice := *(*[]byte)(unsafe.Pointer(field.UnsafeAddr()))

	out := make([]byte, len(saltSlice))
	copy(out, saltSlice)

	return out, nil
}

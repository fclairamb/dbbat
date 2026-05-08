package postgresql

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/pbkdf2"
)

// scramMechanism is the SASL mechanism dbbat speaks to upstream Postgres.
//
// SCRAM-SHA-256-PLUS (channel binding) is intentionally NOT supported here:
// the client first message would need to embed a hash of the upstream TLS
// certificate, and we accept any cert in `prefer`/`require` modes — embedding
// the cert hash would convert ssl_mode=require's "encrypt only" guarantee
// into a stronger one without the user opting in. Servers offering only
// SCRAM-SHA-256-PLUS will fail upstream auth with ErrSCRAMNoSupportedMechanism.
const scramMechanism = "SCRAM-SHA-256"

// scramGS2HeaderNoBinding is the GS2 header used for plain SCRAM-SHA-256:
// "n,," means "client doesn't support channel binding". Its base64 encoding
// "biws" is reused as the channel-binding field of the client final message.
const (
	scramGS2HeaderNoBinding = "n,,"
	scramCBindB64           = "biws" // base64("n,,")
)

// scramClient is a SCRAM-SHA-256 client state machine. It produces the two
// outbound payloads (initial response and final message) and validates the
// server's final signature. Two-step usage:
//
//	c, err := newSCRAMClient(password)
//	first := c.firstMessage()                   // send in SASLInitialResponse
//	final, err := c.finalMessage(serverFirst)   // send in SASLResponse
//	err = c.verifyServerFinal(serverFinal)      // validate after AuthenticationSASLFinal
type scramClient struct {
	password    string
	clientNonce string

	// Persisted between messages; needed to compute AuthMessage on the
	// client final message and to verify the server signature.
	clientFirstBare    string
	serverFirstMessage string
	clientFinalNoProof string
	saltedPassword     []byte
}

func newSCRAMClient(password string) (*scramClient, error) {
	nonce := make([]byte, 18) // 24 base64 chars, plenty of entropy.
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("scram nonce: %w", err)
	}
	return &scramClient{
		password:    password,
		clientNonce: base64.StdEncoding.EncodeToString(nonce),
	}, nil
}

// firstMessage returns the bytes for SASLInitialResponse.Data. PostgreSQL
// expects the username field empty here — the upstream already has it from
// the StartupMessage.
func (c *scramClient) firstMessage() []byte {
	c.clientFirstBare = "n=,r=" + c.clientNonce
	return []byte(scramGS2HeaderNoBinding + c.clientFirstBare)
}

// finalMessage parses the server's first message and returns the bytes for
// SASLResponse.Data (the client final message with proof).
func (c *scramClient) finalMessage(serverFirst []byte) ([]byte, error) {
	c.serverFirstMessage = string(serverFirst)

	combinedNonce, salt, iter, err := parseSCRAMServerFirst(c.serverFirstMessage)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(combinedNonce, c.clientNonce) {
		return nil, ErrSCRAMServerNonceMismatch
	}

	c.saltedPassword = pbkdf2.Key([]byte(c.password), salt, iter, sha256.Size, sha256.New)

	clientKey := hmacSHA256(c.saltedPassword, []byte("Client Key"))
	storedKey := sha256Sum(clientKey)

	c.clientFinalNoProof = "c=" + scramCBindB64 + ",r=" + combinedNonce
	authMessage := c.clientFirstBare + "," + c.serverFirstMessage + "," + c.clientFinalNoProof

	clientSignature := hmacSHA256(storedKey, []byte(authMessage))
	clientProof := xorBytes(clientKey, clientSignature)

	return []byte(c.clientFinalNoProof + ",p=" + base64.StdEncoding.EncodeToString(clientProof)), nil
}

// verifyServerFinal checks that the server's final message contains a
// signature derived from the same shared secret. PostgreSQL won't progress
// past AuthenticationSASLFinal unless this passes, but we still verify
// locally — a malicious upstream could otherwise complete auth with garbage.
func (c *scramClient) verifyServerFinal(serverFinal []byte) error {
	v, err := parseSCRAMServerFinal(string(serverFinal))
	if err != nil {
		return err
	}

	serverKey := hmacSHA256(c.saltedPassword, []byte("Server Key"))
	authMessage := c.clientFirstBare + "," + c.serverFirstMessage + "," + c.clientFinalNoProof
	expected := hmacSHA256(serverKey, []byte(authMessage))

	got, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return fmt.Errorf("%w: server signature: %w", ErrSCRAMMalformedMessage, err)
	}
	if !hmac.Equal(got, expected) {
		return ErrSCRAMServerSignature
	}
	return nil
}

// parseSCRAMServerFirst extracts r=, s=, i= from `r=...,s=...,i=...`. Order
// is fixed by RFC 5802 §5.1 ("nonce", "salt", "iteration-count").
func parseSCRAMServerFirst(s string) (string, []byte, int, error) {
	fields := splitSCRAMFields(s)
	r, ok := fields["r"]
	if !ok {
		return "", nil, 0, fmt.Errorf("%w: missing r= in server first", ErrSCRAMMalformedMessage)
	}
	saltB64, ok := fields["s"]
	if !ok {
		return "", nil, 0, fmt.Errorf("%w: missing s= in server first", ErrSCRAMMalformedMessage)
	}
	iStr, ok := fields["i"]
	if !ok {
		return "", nil, 0, fmt.Errorf("%w: missing i= in server first", ErrSCRAMMalformedMessage)
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return "", nil, 0, fmt.Errorf("%w: salt b64: %w", ErrSCRAMMalformedMessage, err)
	}
	iter, err := strconv.Atoi(iStr)
	if err != nil {
		return "", nil, 0, fmt.Errorf("%w: iteration count: %w", ErrSCRAMMalformedMessage, err)
	}
	return r, salt, iter, nil
}

// parseSCRAMServerFinal returns the v= value, or the e= error if the server
// rejected auth (§7 server-final-message). Either form is well-formed; the
// caller treats e= as ErrSCRAMServerSignature's failure mode at a higher level.
func parseSCRAMServerFinal(s string) (string, error) {
	fields := splitSCRAMFields(s)
	if e, ok := fields["e"]; ok {
		return "", fmt.Errorf("%w: server reported %q", ErrSCRAMServerSignature, e)
	}
	v, ok := fields["v"]
	if !ok {
		return "", fmt.Errorf("%w: missing v= in server final", ErrSCRAMMalformedMessage)
	}
	return v, nil
}

// splitSCRAMFields parses a comma-separated `key=value` list. The value half
// may itself contain '=' (base64), so split only on the first.
func splitSCRAMFields(s string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(s, ",") {
		eq := strings.IndexByte(part, '=')
		if eq <= 0 {
			continue
		}
		out[part[:eq]] = part[eq+1:]
	}
	return out
}

func hmacSHA256(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil)
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func xorBytes(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// pickSCRAMMechanism returns scramMechanism if the upstream offered it, or
// the empty string otherwise. The pgproto3 AuthenticationSASL message gives
// us the list as a NUL-terminated array; pgproto3 already strips the NULs
// and gives us a []string.
func pickSCRAMMechanism(offered []string) string {
	for _, m := range offered {
		if m == scramMechanism {
			return m
		}
	}
	return ""
}

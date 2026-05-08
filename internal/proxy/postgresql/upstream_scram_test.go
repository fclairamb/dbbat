package postgresql

import (
	"errors"
	"strings"
	"testing"
)

// RFC 7677 §3 SCRAM-SHA-256 example. Username "user" / password "pencil".
// We exercise the math by injecting the same nonces the RFC uses; the
// expected proof and server signature come straight from the RFC.
const (
	rfcClientNonce  = "rOprNGfwEbeRWgbNEkqO"
	rfcServerFirst  = "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	rfcExpectedFin  = "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	rfcServerFinal  = "v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="
	rfcWrongVerify  = "v=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	rfcServerErrMsg = "e=invalid-proof"
)

// rfcClient constructs a scramClient with the deterministic nonce and the
// RFC's username-bearing client first message ("n=user", not the empty
// username PostgreSQL uses). The math under test is identical either way —
// only the AuthMessage prefix differs.
func rfcClient(t *testing.T) *scramClient {
	t.Helper()
	return &scramClient{
		password:        "pencil",
		clientNonce:     rfcClientNonce,
		clientFirstBare: "n=user,r=" + rfcClientNonce,
	}
}

func TestSCRAMClient_FirstMessageHasEmptyUsername(t *testing.T) {
	t.Parallel()

	c, err := newSCRAMClient("pencil")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got := string(c.firstMessage())
	if !strings.HasPrefix(got, "n,,n=,r=") {
		t.Fatalf("firstMessage: got %q, want prefix %q", got, "n,,n=,r=")
	}
}

func TestSCRAMClient_FinalMessageMatchesRFC(t *testing.T) {
	t.Parallel()

	c := rfcClient(t)
	got, err := c.finalMessage([]byte(rfcServerFirst))
	if err != nil {
		t.Fatalf("finalMessage: %v", err)
	}
	if string(got) != rfcExpectedFin {
		t.Fatalf("finalMessage:\n got  %s\n want %s", got, rfcExpectedFin)
	}
}

func TestSCRAMClient_VerifyServerFinalAcceptsRFCSignature(t *testing.T) {
	t.Parallel()

	c := rfcClient(t)
	if _, err := c.finalMessage([]byte(rfcServerFirst)); err != nil {
		t.Fatalf("final: %v", err)
	}
	if err := c.verifyServerFinal([]byte(rfcServerFinal)); err != nil {
		t.Fatalf("verifyServerFinal: %v", err)
	}
}

func TestSCRAMClient_VerifyServerFinalRejectsWrongSignature(t *testing.T) {
	t.Parallel()

	c := rfcClient(t)
	if _, err := c.finalMessage([]byte(rfcServerFirst)); err != nil {
		t.Fatalf("final: %v", err)
	}
	err := c.verifyServerFinal([]byte(rfcWrongVerify))
	if !errors.Is(err, ErrSCRAMServerSignature) {
		t.Fatalf("expected ErrSCRAMServerSignature, got %v", err)
	}
}

func TestSCRAMClient_VerifyServerFinalSurfacesServerError(t *testing.T) {
	t.Parallel()

	c := rfcClient(t)
	if _, err := c.finalMessage([]byte(rfcServerFirst)); err != nil {
		t.Fatalf("final: %v", err)
	}
	err := c.verifyServerFinal([]byte(rfcServerErrMsg))
	if !errors.Is(err, ErrSCRAMServerSignature) {
		t.Fatalf("expected ErrSCRAMServerSignature for server-reported error, got %v", err)
	}
}

func TestSCRAMClient_RejectsServerNonceMismatch(t *testing.T) {
	t.Parallel()

	c := rfcClient(t)
	bad := "r=DIFFERENTNONCE,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	_, err := c.finalMessage([]byte(bad))
	if !errors.Is(err, ErrSCRAMServerNonceMismatch) {
		t.Fatalf("expected ErrSCRAMServerNonceMismatch, got %v", err)
	}
}

func TestSCRAMClient_RejectsMalformedServerFirst(t *testing.T) {
	t.Parallel()

	c := rfcClient(t)
	_, err := c.finalMessage([]byte("garbage,no,fields"))
	if !errors.Is(err, ErrSCRAMMalformedMessage) {
		t.Fatalf("expected ErrSCRAMMalformedMessage, got %v", err)
	}
}

func TestPickSCRAMMechanism(t *testing.T) {
	t.Parallel()

	got := pickSCRAMMechanism([]string{"SCRAM-SHA-256-PLUS", "SCRAM-SHA-256"})
	if got != "SCRAM-SHA-256" {
		t.Fatalf("pickSCRAMMechanism: got %q, want SCRAM-SHA-256", got)
	}

	got = pickSCRAMMechanism([]string{"SCRAM-SHA-256-PLUS"})
	if got != "" {
		t.Fatalf("pickSCRAMMechanism: PLUS-only should return empty, got %q", got)
	}
}

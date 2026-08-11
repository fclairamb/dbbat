package upstream

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// unrelatedCAPEM mints a throwaway self-signed CA. It has to be generated
// rather than borrowed from a second httptest server: every httptest TLS server
// presents the same built-in certificate, so two of them would "verify" against
// each other and a mismatch test would pass for the wrong reason.
func unrelatedCAPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "not-your-cluster"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestLearnKubernetesCACertCapturesThePresentedChain is the pin the spec asked
// for: the TOFU capture runs against the fake API server and must come back
// with exactly the bundle an operator would otherwise have pasted.
func TestLearnKubernetesCACertCapturesThePresentedChain(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)

	cfg := fake.config()
	cfg.CACert = ""

	learned, err := LearnKubernetesCACert(context.Background(), cfg)
	if err != nil {
		t.Fatalf("LearnKubernetesCACert() error = %v", err)
	}

	if learned != fake.caPEM() {
		t.Errorf("learned bundle = %q, want the API server's own certificate %q", learned, fake.caPEM())
	}
}

// TestLearnKubernetesCACertRefusesAnUnreachableAPIServer keeps the capture from
// silently yielding an empty bundle when nothing answers.
func TestLearnKubernetesCACertRefusesAnUnreachableAPIServer(t *testing.T) {
	t.Parallel()

	_, err := LearnKubernetesCACert(context.Background(), KubernetesConfig{
		Host: "127.0.0.1", Port: 1, Token: "sa-token", Namespace: "data",
	})
	if err == nil {
		t.Fatal("LearnKubernetesCACert() succeeded against an unreachable API server")
	}
}

// TestNewKubernetesTunnelAcceptsALearnedPin covers the third arm of the trust
// switch: a row that pasted nothing but learned something is configured.
func TestNewKubernetesTunnelAcceptsALearnedPin(t *testing.T) {
	t.Parallel()

	tunnel, err := NewKubernetesTunnel(KubernetesConfig{
		Host: "api.example.com", Port: 6443, Token: "t", Namespace: "data",
		LearnedCACert: unrelatedCAPEM(t),
	})
	if err != nil {
		t.Fatalf("NewKubernetesTunnel(learned pin) error = %v", err)
	}

	if !tunnel.learnedPin {
		t.Error("tunnel.learnedPin = false, want true so a trust failure reads as a changed CA")
	}
}

// TestLearnedPinNeverOverridesASuppliedBundle is binding semantic #2: when the
// operator pasted a CA, that is what verification runs against and the learned
// value is not even consulted.
func TestLearnedPinNeverOverridesASuppliedBundle(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)
	fake.allowed = true

	cfg := fake.config() // CACert = the fake's real certificate
	cfg.LearnedCACert = unrelatedCAPEM(t)

	tunnel, err := NewKubernetesTunnel(cfg)
	if err != nil {
		t.Fatalf("NewKubernetesTunnel() error = %v", err)
	}

	if tunnel.learnedPin {
		t.Error("tunnel.learnedPin = true although a bundle was supplied")
	}

	if _, _, err := tunnel.PortForwardAllowed(context.Background()); err != nil {
		t.Fatalf("PortForwardAllowed() error = %v, want the supplied bundle to be the one in force", err)
	}
}

// TestALearnedPinVerifiesLaterConnects closes the loop: what the capture
// returned is what the next connect is verified against.
func TestALearnedPinVerifiesLaterConnects(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)
	fake.allowed = true

	cfg := fake.config()
	cfg.CACert = ""

	learned, err := LearnKubernetesCACert(context.Background(), cfg)
	if err != nil {
		t.Fatalf("LearnKubernetesCACert() error = %v", err)
	}

	cfg.LearnedCACert = learned

	tunnel, err := NewKubernetesTunnel(cfg)
	if err != nil {
		t.Fatalf("NewKubernetesTunnel() error = %v", err)
	}

	allowed, _, err := tunnel.PortForwardAllowed(context.Background())
	if err != nil {
		t.Fatalf("PortForwardAllowed() error = %v, want the learned pin to verify", err)
	}

	if !allowed {
		t.Error("PortForwardAllowed() = false, want true")
	}
}

// TestAChangedCAAgainstALearnedPinIsItsOwnFailure is binding semantic #5: after
// pinning, a certificate the pin does not vouch for must refuse *and* be
// distinguishable from "you pasted the wrong CA".
func TestAChangedCAAgainstALearnedPinIsItsOwnFailure(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)
	fake.allowed = true

	cfg := fake.config()
	cfg.CACert = ""
	cfg.LearnedCACert = unrelatedCAPEM(t)

	tunnel, err := NewKubernetesTunnel(cfg)
	if err != nil {
		t.Fatalf("NewKubernetesTunnel() error = %v", err)
	}

	_, _, err = tunnel.PortForwardAllowed(context.Background())
	if err == nil {
		t.Fatal("PortForwardAllowed() succeeded against a certificate the pin does not vouch for")
	}

	if !errors.Is(err, ErrKubernetesCAPinMismatch) {
		t.Fatalf("error = %v, want ErrKubernetesCAPinMismatch", err)
	}

	if !IsPermanentError(err) {
		t.Error("IsPermanentError(pin mismatch) = false: retrying cannot make a changed CA match")
	}
}

// TestAWrongSuppliedBundleIsNotAPinMismatch is the other half of that
// distinction: an operator who pasted the wrong CA has made a configuration
// mistake, not witnessed a rotation.
func TestAWrongSuppliedBundleIsNotAPinMismatch(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)
	fake.allowed = true

	cfg := fake.config()
	cfg.CACert = unrelatedCAPEM(t)

	tunnel, err := NewKubernetesTunnel(cfg)
	if err != nil {
		t.Fatalf("NewKubernetesTunnel() error = %v", err)
	}

	_, _, err = tunnel.PortForwardAllowed(context.Background())
	if err == nil {
		t.Fatal("PortForwardAllowed() succeeded against a certificate signed by a different CA")
	}

	if errors.Is(err, ErrKubernetesCAPinMismatch) {
		t.Errorf("error = %v, want a plain trust failure, not a pin mismatch", err)
	}
}

func TestPinnableChainPEM(t *testing.T) {
	t.Parallel()

	if _, err := pinnableChainPEM(nil); !errors.Is(err, ErrKubernetesCANotPinnable) {
		t.Fatalf("pinnableChainPEM(nil) error = %v, want ErrKubernetesCANotPinnable", err)
	}

	leaf, ca := leafAndIssuer(t)

	// Leaf first, issuer second — the order a TLS server sends them in. Only
	// the issuer is pinned, so the pin survives the leaf being reissued.
	got, err := pinnableChainPEM([][]byte{leaf.Raw, ca.Raw})
	if err != nil {
		t.Fatalf("pinnableChainPEM() error = %v", err)
	}

	want := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}))
	if got != want {
		t.Errorf("pinnableChainPEM() pinned %q, want only the issuer %q", got, want)
	}

	// A leaf-only chain has nothing issuer-shaped, so the leaf itself is the pin.
	got, err = pinnableChainPEM([][]byte{leaf.Raw})
	if err != nil {
		t.Fatalf("pinnableChainPEM(leaf only) error = %v", err)
	}

	if !strings.Contains(got, "BEGIN CERTIFICATE") {
		t.Errorf("pinnableChainPEM(leaf only) = %q, want the leaf PEM", got)
	}
}

// leafAndIssuer mints a CA and a non-CA leaf signed by it.
func leafAndIssuer(t *testing.T) (*x509.Certificate, *x509.Certificate) {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate ca key: %v", err)
	}

	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cluster-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create ca: %v", err)
	}

	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}

	leafTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "kube-apiserver"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}

	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	return leaf, ca
}

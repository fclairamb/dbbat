package conncheck

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

var errProbeReturned = errors.New("conncheck test: probe returned this")

// panicTestServer is the minimal row runProbe needs for its log line.
func panicTestServer() *store.Server {
	return &store.Server{
		UID:      uuid.New(),
		Protocol: store.ProtocolOracle,
		Host:     "oracle.example.com",
		Port:     1521,
	}
}

func noDial(context.Context) (net.Conn, error) { return nil, errProbeReturned }

// TestRunProbe_RecoversPanic is the regression guard for a connectivity check
// being able to kill the proxy.
//
// The probes drive third-party client libraries against whatever host a server
// row points at, and those libraries are not hardened against a malformed
// handshake — go-ora's newAcceptPacketFromData indexes a TNS Accept packet
// without a length check and panics on a short one, which the scheduled Oracle
// integration run hit against an ordinary test container. The probe runs on its
// own goroutine with no caller to unwind into, so before this guard that panic
// did not fail the check: it took the whole process down, dropping every live
// session on every protocol, because someone pressed "test connection".
func TestRunProbe_RecoversPanic(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		panic any
	}{
		{name: "string panic", panic: "boom"},
		{name: "error panic", panic: errProbeReturned},
		{name: "runtime panic", panic: nil}, // triggers a real index-out-of-range below
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := func(context.Context, *store.Server, dialFunc) error {
				if tc.panic == nil {
					// The shape of the real go-ora crash: an index past the end
					// of a short packet.
					var short []byte

					_ = short[3]

					return nil
				}

				panic(tc.panic)
			}

			err := runProbe(context.Background(), panicTestServer(), noDial, p)

			if err == nil {
				t.Fatal("a panicking probe must return an error, not nil")
			}

			if !errors.Is(err, errProbePanic) {
				t.Fatalf("error = %v, want it to wrap errProbePanic", err)
			}
		})
	}
}

// TestRunProbe_PassesThroughNormalResults makes sure the recover wrapper is
// transparent: a probe that returns (or succeeds) must be unaffected.
func TestRunProbe_PassesThroughNormalResults(t *testing.T) {
	t.Parallel()

	t.Run("error is returned unwrapped", func(t *testing.T) {
		t.Parallel()

		p := func(context.Context, *store.Server, dialFunc) error { return errProbeReturned }

		err := runProbe(context.Background(), panicTestServer(), noDial, p)
		if !errors.Is(err, errProbeReturned) {
			t.Fatalf("error = %v, want errProbeReturned", err)
		}

		if errors.Is(err, errProbePanic) {
			t.Fatal("an ordinary probe error must not be reported as a panic")
		}
	})

	t.Run("success stays success", func(t *testing.T) {
		t.Parallel()

		p := func(context.Context, *store.Server, dialFunc) error { return nil }

		if err := runProbe(context.Background(), panicTestServer(), noDial, p); err != nil {
			t.Fatalf("error = %v, want nil", err)
		}
	})
}

// TestClassifyTargetError_Panic pins the classification. A panic is a dbbat
// bug, so it must not be dressed up as db_auth_failed or db_handshake_failed —
// those send an admin to re-check credentials and ports that are perfectly
// fine.
func TestClassifyTargetError_Panic(t *testing.T) {
	t.Parallel()

	err := runProbe(context.Background(), panicTestServer(), noDial,
		func(context.Context, *store.Server, dialFunc) error { panic("boom") })

	res := classifyTargetError(err)

	if res.Stage != StageTargetAuth {
		t.Errorf("stage = %s, want %s", res.Stage, StageTargetAuth)
	}

	if res.Code != CodeInternal {
		t.Errorf("code = %s, want %s", res.Code, CodeInternal)
	}

	if res.Message == "" {
		t.Error("a panic result must carry a message explaining it is a dbbat bug")
	}
}

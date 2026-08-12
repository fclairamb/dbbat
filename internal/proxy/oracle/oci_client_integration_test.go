//go:build integration

package oracle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// errOCIClientExit is what a non-zero sqlplus exit inside the container comes
// back as. `docker exec` reports a status where exec.Command reports an
// *ExitError, and the callers only ever wrap it into a require.NoError, so all
// it has to carry is the code.
var errOCIClientExit = errors.New("the OCI client exited non-zero")

// requireOCIClientEnv turns "no OCI client here" from a skip into a failure.
//
// The two sqlplus-driven tests below are the *only* end-to-end proof that an
// OCI client can authenticate through dbbat at all, and the only automated
// cover for the unlearned fallback in nextOERFrame. They opened with
// exec.LookPath("sqlplus") and skipped when it was absent — which was every CI
// runner — so both were green by proving nothing. A silent skip is what let
// that gap exist and survive; CI now sets this variable on every Oracle leg
// (see .github/workflows/integration.yml), so losing the client is a red
// suite rather than a quieter one.
const requireOCIClientEnv = "ORACLE_TEST_REQUIRE_OCI_CLIENT"

// hostGatewayAlias is the name the Oracle container resolves back to the
// Docker host. Docker Desktop publishes it already; on Linux (every CI runner)
// it exists only because startOracleContainerWith adds the `host-gateway`
// entry, which is why the container-hosted client needs a fixture started with
// `reachableFromContainers`.
const hostGatewayAlias = "host.docker.internal"

// containerOCIScriptPath is where an sqlplus script is dropped inside the
// Oracle container. /tmp is writable by the `oracle` user the image runs as.
const containerOCIScriptPath = "/tmp/dbbat_oci_probe.sql"

// ociClientEnv pins where sqlplus comes from instead of letting
// plannedOCIClient choose: `path` for an install on PATH, `container` for the
// one bundled in the Oracle image. Anything else (including unset) is auto.
//
// It exists so a developer with an Instant Client installed can still run what
// CI runs — auto would pick their PATH client and never touch the container
// route, which is the only route CI has and therefore the one that has to keep
// working.
const ociClientEnv = "ORACLE_TEST_OCI_CLIENT"

// ociClientKind is where sqlplus comes from.
type ociClientKind int

const (
	// ociClientHostPath is an Instant Client (or any OCI install) on PATH —
	// the flavor the tests used to require, and the flavor `testdata` was
	// captured from (macOS Instant Client 23.3).
	ociClientHostPath ociClientKind = iota

	// ociClientContainer runs sqlplus from the Oracle image the suite already
	// starts, over `docker exec`, pointed back at the proxy through
	// host.docker.internal. gvenzl's images all bundle a client — 23.26 in
	// oracle-free:23-slim, 18.4 in oracle-xe:18.4.0-slim — so this needs no
	// artifact, no download and no cache in CI.
	//
	// It is also a *different* OCI flavor from the Instant Client: the bundled
	// client sends plain key/value length fields where the Instant Client sends
	// 3x UTF-8 buffer sizes (see docs/oracle.md, "OCI wide (4-byte
	// little-endian) TTC encoding"). Nothing exercised it live before.
	ociClientContainer
)

// plannedOCIClient decides where sqlplus comes from, once per process and —
// this is the load-bearing part — *before any container starts*, because the
// container flavor needs the Oracle container created with a route back to the
// host and the proxy bound on more than loopback. Both are container/listener
// creation-time decisions the fixture cannot revisit later.
//
// A client on PATH wins: it is free, it needs no wildcard bind, and it is the
// flavor the byte-level fixtures were captured from.
var plannedOCIClient = sync.OnceValue(func() ociClientKind {
	switch os.Getenv(ociClientEnv) {
	case "path":
		return ociClientHostPath
	case "container":
		return ociClientContainer
	}

	if _, err := exec.LookPath("sqlplus"); err == nil {
		return ociClientHostPath
	}

	return ociClientContainer
})

// ociClient runs an sqlplus script against the proxy and hands back everything
// the client printed, whichever side of the container boundary it ran on.
type ociClient struct {
	// kind is which of the two flavors this is, for the one test that has to
	// tell them apart (see knownBadBundledRefusal).
	kind ociClientKind

	// label names the flavor in test logs, so a failure says which client
	// produced it.
	label string

	// run executes script and returns its combined output. The error is the
	// client's own exit status — a non-zero exit or a failure to run it at all
	// — never an assertion.
	run func(t *testing.T, ctx context.Context, script string) (string, error)
}

// startOracleThroughProxyForOCI is startOracleThroughProxy for the sqlplus
// tests: it opens the fixture up to a container-hosted client when that is
// where sqlplus is going to come from, and leaves it exactly as it was when a
// client is already on PATH.
func startOracleThroughProxyForOCI(t *testing.T, controls []string) *oracleThroughProxy {
	t.Helper()

	return startOracleThroughProxyWith(t, oracleFixtureOptions{
		controls:                controls,
		reachableFromContainers: plannedOCIClient() == ociClientContainer,
	})
}

// requireOCIClient hands back a working sqlplus for env, or ends the test.
//
// "Ends the test" is a skip by default and a failure under
// requireOCIClientEnv. The distinction it is careful to preserve: a missing or
// unreachable *client* is an environment fact (skippable, unless CI declared
// otherwise), while a client that runs and then fails to log in is a finding
// about the proxy and must reach the assertions below as a failure.
func requireOCIClient(t *testing.T, env *oracleThroughProxy) *ociClient {
	t.Helper()

	var (
		client *ociClient
		reason string
	)

	switch plannedOCIClient() {
	case ociClientHostPath:
		client, reason = hostOCIClient(env)
	case ociClientContainer:
		client, reason = containerOCIClient(t, env)
	}

	if client == nil {
		if os.Getenv(requireOCIClientEnv) == "1" {
			t.Fatalf("%s=1 but no OCI client could be used: %s", requireOCIClientEnv, reason)
		}

		t.Skipf("no OCI client available: %s (set %s=1 to make this a failure)", reason, requireOCIClientEnv)
	}

	t.Logf("OCI client: %s", client.label)

	return client
}

// hostOCIClient wraps an sqlplus found on PATH.
func hostOCIClient(env *oracleThroughProxy) (*ociClient, string) {
	sqlplus, err := exec.LookPath("sqlplus")
	if err != nil {
		return nil, fmt.Sprintf("sqlplus is not on PATH: %v", err)
	}

	return &ociClient{
		kind:  ociClientHostPath,
		label: "sqlplus on PATH (" + sqlplus + ")",
		run: func(t *testing.T, ctx context.Context, script string) (string, error) {
			t.Helper()

			path := filepath.Join(t.TempDir(), "oci_probe.sql")
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				return "", err
			}

			cmd := exec.CommandContext(ctx, sqlplus, "-S", env.ociConnectString(env.host), "@"+path)

			out, err := cmd.CombinedOutput()

			return string(out), err
		},
	}, ""
}

// containerOCIClient wraps the sqlplus bundled in the Oracle image the suite
// already started, reached over `docker exec`.
//
// Two things are probed before it is handed back, and each maps to a different
// verdict. That sqlplus exists in the image at all, and that the container can
// actually open a TCP connection back to the proxy — a Docker network that
// does not route to the host is an environment fact, not a protocol bug, and
// letting it through would land as a bewildering TNS error inside an assertion
// about AUTH.
func containerOCIClient(t *testing.T, env *oracleThroughProxy) (*ociClient, string) {
	t.Helper()

	ctx := context.Background()

	if code, out, err := containerExec(ctx, env, []string{"sqlplus", "-v"}); err != nil || code != 0 {
		return nil, fmt.Sprintf("the Oracle image bundles no usable sqlplus (exit %d, err %v): %s", code, err, out)
	}

	probe := fmt.Sprintf("exec 3<>/dev/tcp/%s/%d", hostGatewayAlias, env.port)
	if code, out, err := containerExec(ctx, env, []string{"bash", "-c", probe}); err != nil || code != 0 {
		return nil, fmt.Sprintf(
			"the Oracle container cannot reach the proxy on %s:%d (exit %d, err %v): %s",
			hostGatewayAlias, env.port, code, err, out)
	}

	return &ociClient{
		kind:  ociClientContainer,
		label: "sqlplus bundled in " + oracleTestImage() + " (via docker exec)",
		run: func(t *testing.T, ctx context.Context, script string) (string, error) {
			t.Helper()

			if err := env.oracle.CopyToContainer(ctx, []byte(script), containerOCIScriptPath, 0o644); err != nil {
				return "", err
			}

			code, out, err := containerExec(ctx, env, []string{
				"sqlplus", "-S",
				env.ociConnectString(hostGatewayAlias),
				"@" + containerOCIScriptPath,
			})

			if err == nil && code != 0 {
				err = fmt.Errorf("%w: %d", errOCIClientExit, code)
			}

			return out, err
		},
	}, ""
}

// containerExec runs a command in the Oracle container and demultiplexes its
// output, so stdout and stderr come back the way CombinedOutput would give
// them — sqlplus reports ORA errors on both depending on the error.
func containerExec(ctx context.Context, env *oracleThroughProxy, cmd []string) (int, string, error) {
	code, reader, err := env.oracle.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		return code, "", err
	}

	out, readErr := io.ReadAll(reader)

	return code, string(out), readErr
}

// ociConnectString is the easy-connect string an OCI client uses to reach the
// proxy from host, authenticating as the fixture user with its API key.
func (e *oracleThroughProxy) ociConnectString(host string) string {
	return fmt.Sprintf("%s/%s@//%s:%d/%s", e.username, e.apiKey, host, e.port, e.service)
}

// ociAuthModeNote says which AUTH path the login under test will take, so a log
// from either CI leg is self-describing. Both legs matter and they cover
// different code: unset exercises the wide *rewrite*, =1 the wide *synthetic*
// builders.
func ociAuthModeNote(t *testing.T) {
	t.Helper()

	if forceSyntheticUpstreamAuth {
		t.Logf("%s=1: this login runs on the wide SYNTHETIC AUTH builders", forceSyntheticAuthEnv)

		return
	}

	t.Logf("%s unset: this login runs on the wide AUTH REWRITE path; "+
		"set it to 1 to exercise the synthetic builders", forceSyntheticAuthEnv)
}

// knownBadBundledRefusal is a *measured* defect, not a convenience: under a
// restrictive grant the DB-bundled OCI client's very first call in proxy mode
// is refused and the session then hangs, so the refusal test cannot run on that
// flavor until the bug is fixed. The login test above is unaffected (it holds
// an unrestricted grant, so no gate fires) and does run on it.
//
// Measured on gvenzl/oracle-free:23-slim (client 23.26) against the same image
// as upstream, `read_only` grant, sqlplus's first statement a plain SELECT:
//
//	TTC message func=OFETCH
//	refused a re-execution of an untracked cursor under a restrictive grant cursor_id=27396
//
// A fresh session has no cursors, so 27396 is not a cursor the client could be
// re-executing. The Instant Client 23.3 on PATH passes the same test against
// the same upstream, which is what makes this flavor-specific rather than a
// property of the gate.
//
// Full write-up and repro:
// specs/todos/2026-08-12-12-bundled-oci-client-refused-and-hung-under-a-restrictive-grant.md
const knownBadBundledRefusal = "the bundled OCI client's first call is refused as an untracked cursor " +
	"re-execution under a restrictive grant, and the session then hangs — see " +
	"specs/todos/2026-08-12-12-bundled-oci-client-refused-and-hung-under-a-restrictive-grant.md"

// assertNoOCIAuthMalformation names the two failures a wrong wide AUTH body
// produces, so a regression reads as itself rather than as missing output.
func assertNoOCIAuthMalformation(t *testing.T, output string) {
	t.Helper()

	if strings.Contains(output, "ORA-03120") {
		t.Errorf("the upstream could not parse dbbat's AUTH body:\n%s", output)
	}

	if strings.Contains(output, "ORA-28041") {
		t.Errorf("dbbat mixed the two OCI length conventions:\n%s", output)
	}
}

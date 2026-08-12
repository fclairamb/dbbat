//go:build integration

package oracle

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forceSyntheticAuthEnv turns the upstream AUTH rewrite off for the whole
// integration run, so every test in this package drives the synthetic
// buildClientAuthPhase1 / buildClientAuthPhase2 fallback instead.
const forceSyntheticAuthEnv = "DBBAT_ORACLE_FORCE_SYNTHETIC_AUTH"

// TestMain exists for one reason: the synthetic AUTH builders are otherwise
// unreachable. sendUpstreamAuthPhase1 forwards the client's own packet with the
// username swapped whenever it can, which in practice is always, so the
// fallback ran nowhere — and shipped for months with a preamble one byte short
// of what go-ora v3 puts on the wire (two break markers + ORA-03120 from the
// upstream). A green suite with the rewrite enabled says nothing about it.
//
// Re-run any integration test with the rewrite disabled to exercise it:
//
//	DBBAT_ORACLE_FORCE_SYNTHETIC_AUTH=1 go test -tags integration -v -timeout 20m \
//	  -count=1 -run TestIntegration_MCPExecutesThroughTheProxy ./internal/proxy/oracle/
//
// Both runs must pass. Unset (the default), this is a plain pass-through.
func TestMain(m *testing.M) {
	if os.Getenv(forceSyntheticAuthEnv) == "1" {
		forceSyntheticUpstreamAuth = true
	}

	os.Exit(m.Run())
}

// syntheticWideLoginScript is deliberately minimal: what is under test is
// whether an OCI client can log in *at all* through the synthetic AUTH
// builders, not what it can then compute. The second SELECT is there to show
// the session is a working session and not merely one that survived AUTH.
const syntheticWideLoginScript = `SET PAGESIZE 0
SET FEEDBACK OFF
SELECT 'wide-auth-ok=' || 1 FROM dual;
SELECT 'dbbat_wide_auth_probe=' || 42 FROM dual;
EXIT
`

// syntheticWideProbeSQL is the statement the assertion below looks for in
// `queries`. It is spelled out rather than derived so a change to the script
// cannot quietly stop the log assertion from matching anything.
const syntheticWideProbeSQL = "SELECT 'dbbat_wide_auth_probe=' || 42 FROM dual"

// syntheticWideLoginDeadline bounds the sqlplus run. An OCI client handed a
// body its upstream cannot parse does not fail fast — the upstream stops
// answering and the client waits, which is how the thin-body-to-wide-upstream
// bug presents (two break markers, then ORA-03120, or nothing at all). Without
// a deadline that failure mode is a hung suite rather than a red test.
const syntheticWideLoginDeadline = 3 * time.Minute

// TestIntegration_SqlplusLoginThroughSyntheticAuth closes the gap the
// forced-synthetic CI run left open: that run proves the synthetic builders
// work for *thin* clients only, because every other client in this suite is
// thin. sqlplus is an OCI client, so it negotiates the wide dialect during the
// pre-auth relay and its upstream then parses AUTH at those caps — which is
// exactly the case buildClientAuthPhase1Wide / buildClientAuthPhase2Wide exist
// for, and the case that used to be handed a thin body it could not read.
//
// The test is meaningful in both CI modes and asserts different things in each:
//
//   - DBBAT_ORACLE_FORCE_SYNTHETIC_AUTH=1: the rewrite is off, so this login
//     runs entirely on the wide synthetic builders. This is the new coverage.
//   - unset (the default): the same login runs on the wide *rewrite* path,
//     which keeps the two paths honest about producing a body the same upstream
//     accepts.
//
// Skipped when sqlplus is not installed, which is the case in CI today; the
// byte-level half of the same evidence lives in upstream_auth_client_wide_test.go,
// measured against a real sqlplus capture.
func TestIntegration_SqlplusLoginThroughSyntheticAuth(t *testing.T) {
	sqlplus, err := exec.LookPath("sqlplus")
	if err != nil {
		t.Skipf("sqlplus unavailable: %v", err)
	}

	if forceSyntheticUpstreamAuth {
		t.Logf("%s=1: this login runs on the wide SYNTHETIC AUTH builders", forceSyntheticAuthEnv)
	} else {
		t.Logf("%s unset: this login runs on the wide AUTH REWRITE path; "+
			"set it to 1 to exercise the synthetic builders", forceSyntheticAuthEnv)
	}

	env := startOracleThroughProxy(t, nil)

	script := filepath.Join(t.TempDir(), "wide_login.sql")
	require.NoError(t, os.WriteFile(script, []byte(syntheticWideLoginScript), 0o600))

	runCtx, cancel := context.WithTimeout(context.Background(), syntheticWideLoginDeadline)
	defer cancel()

	cmd := exec.CommandContext(runCtx, sqlplus, "-S",
		fmt.Sprintf("%s/%s@//%s:%d/%s", env.username, env.apiKey, env.host, env.port, env.service),
		"@"+script)

	out, err := cmd.CombinedOutput()
	output := string(out)

	require.NoErrorf(t, err, "sqlplus could not complete a login through the proxy:\n%s", output)

	// ORA-03120 and ORA-28041 are the two failures a wrong wide body produces:
	// the first when the upstream cannot parse the preamble, the second when the
	// 3x-buffer-size and plain-length conventions get mixed. Naming them makes a
	// regression legible instead of just "missing output".
	assert.NotContains(t, output, "ORA-03120",
		"the upstream could not parse dbbat's AUTH body:\n%s", output)
	assert.NotContains(t, output, "ORA-28041",
		"dbbat mixed the two OCI length conventions:\n%s", output)

	assert.Contains(t, output, "wide-auth-ok=1",
		"an OCI client must be able to authenticate through dbbat:\n%s", output)
	assert.Contains(t, output, "dbbat_wide_auth_probe=42",
		"the session must keep working after AUTH:\n%s", output)

	// The assertion that stops this test passing on a thin path by accident: the
	// proxy records what encoding it detected off the client's own Phase 1, and
	// for sqlplus that must be the wide one. Without it a future change that made
	// sqlplus look thin would leave the wide builders uncovered and green.
	widths := env.logs.boolsFor("sending AUTH challenge", "wide_encoding")
	require.NotEmpty(t, widths, "the proxy never logged a negotiated AUTH encoding")
	assert.Contains(t, widths, true,
		"sqlplus must be detected as a wide/OCI client, or this test proves nothing about the wide path")

	assertSyntheticWideQueryLogged(t, env)
}

// assertSyntheticWideQueryLogged checks the OCI session was a first-class dbbat
// session and not just a successful TCP conversation: its statement is in
// `queries`, attributed to the fixture user, and is an ordinary link in that
// connection's HMAC chain. A login that authenticated but logged nothing would
// otherwise pass every assertion above.
func assertSyntheticWideQueryLogged(t *testing.T, env *oracleThroughProxy) {
	t.Helper()

	ctx := context.Background()

	var match *store.Query

	require.Eventually(t, func() bool {
		queries, err := env.store.ListQueries(ctx, store.QueryFilter{Limit: 500})
		if err != nil {
			return false
		}

		for i := range queries {
			if queries[i].SQLText == syntheticWideProbeSQL {
				match = &queries[i]

				return true
			}
		}

		return false
	}, 30*time.Second, 250*time.Millisecond,
		"sqlplus's statement %q was never logged", syntheticWideProbeSQL)

	conn, err := env.store.GetConnectionByUID(ctx, match.ConnectionID)
	require.NoError(t, err)
	assert.Equal(t, env.user.UID, conn.UserID, "the OCI session must join to its dbbat user")

	result, err := env.store.VerifyQueryChain(ctx, match.ConnectionID)
	require.NoError(t, err)
	assert.Nil(t, result.Break, "an OCI session's queries must chain like any other")
	assert.Positive(t, result.Verified, "the chain walk must actually cover rows")
}

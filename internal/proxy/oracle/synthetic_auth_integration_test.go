//go:build integration

package oracle

import (
	"os"
	"testing"
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

package conncheck

import (
	"testing"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestProbeForMSSQL pins that a SQL Server row gets a real login probe rather
// than falling through to "dbbat has no standalone dial path for this
// protocol", which is what the connectivity check reports for an unroutable
// one.
func TestProbeForMSSQL(t *testing.T) {
	t.Parallel()

	if probeFor(store.ProtocolMSSQL) == nil {
		t.Fatal("probeFor(mssql) = nil, want a login probe")
	}
}

// TestIsDBAuthRejectionCoversSQLServer pins that the SQL Server credential
// errors are classified as "the stored credentials are wrong" rather than as a
// generic handshake failure pointing the admin at the network.
func TestIsDBAuthRejectionCoversSQLServer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		msg  string
	}{
		{"18456", "mssql: upstream rejected the login: Login failed for user 'sa'. (error 18456, state 1, class 14)"},
		{"4060", "mssql: upstream rejected the login: Cannot open database requested by the login (error 4060)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if !isDBAuthRejection(textError(tc.msg)) {
				t.Errorf("isDBAuthRejection(%q) = false, want true", tc.msg)
			}
		})
	}
}

// textError is a minimal error carrying exactly the text under test.
type textError string

func (e textError) Error() string { return string(e) }

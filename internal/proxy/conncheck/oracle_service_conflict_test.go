package conncheck

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

var errTestConflictLookup = errors.New("conncheck test: conflict lookup failed")

// conflictResolver is a fakeResolver that also answers the optional
// oracleServiceNameLister half, so a check can be driven with and without a
// service-name conflict in the fleet.
type conflictResolver struct {
	*fakeResolver

	conflict *store.OracleServiceNameConflict
	err      error
	// asked records the service name the checker looked up, proving the check
	// keys off the dedicated column rather than the database name.
	asked string
}

func (c *conflictResolver) GetOracleServiceNameConflict(
	_ context.Context,
	serviceName string,
) (*store.OracleServiceNameConflict, bool, error) {
	c.asked = serviceName

	if c.err != nil {
		return nil, false, c.err
	}

	if c.conflict == nil {
		return nil, false, nil
	}

	return c.conflict, true, nil
}

// twoSpellingConflict is the reported shape: one machine, two spellings, one
// service name.
func twoSpellingConflict(serviceName string) *store.OracleServiceNameConflict {
	return store.OracleServiceNameConflictFor(serviceName, []store.Server{
		{UID: uuid.New(), Name: "abyla_abymutualise02_ro", Host: "oracle-abymutualise02.db.stonal.io", Port: 1521},
		{UID: uuid.New(), Name: "abyla_abymutualise_ro", Host: "abymutualise02.rds.amazonaws.com", Port: 1521},
	})
}

// TestCheck_OracleServiceNameConflict_Warns is the visible half of the dormant
// trap: the row itself is fine — it dials, and the listener's ORA-01017 proves
// the login was attempted — while a client arriving with the *shared* service
// name would be refused ORA-12514 because two rows spell one host two ways. The
// check has to say so, without turning it into a failure of this row.
func TestCheck_OracleServiceNameConflict_Warns(t *testing.T) {
	t.Parallel()

	ln := tnsRefuseListener(t, 1017)
	host, port := splitHostPort(t, ln.Addr().String())

	target := newOracleTarget(host, port)

	resolver := &conflictResolver{
		fakeResolver: newFakeResolver(),
		conflict:     twoSpellingConflict(*target.OracleServiceName),
	}

	checker := New(resolver, testKey())
	checker.timeout = 5 * time.Second

	res := checker.Check(context.Background(), target)

	if len(res.Warnings) != 1 {
		t.Fatalf("Warnings = %+v, want exactly one", res.Warnings)
	}

	if res.Warnings[0].Code != CodeOracleServiceNameConflict {
		t.Errorf("warning code = %s, want %s", res.Warnings[0].Code, CodeOracleServiceNameConflict)
	}

	if res.Warnings[0].Message == "" {
		t.Error("warning message is empty; the admin needs the sentence, not just the code")
	}

	if resolver.asked != *target.OracleServiceName {
		t.Errorf("looked up %q, want the row's oracle_service_name %q", resolver.asked, *target.OracleServiceName)
	}

	// The warning rides alongside the staged outcome; it must not rewrite it.
	if res.Stage != StageTargetAuth || res.Code != CodeDBAuthFailed {
		t.Errorf("stage/code = %s/%s, want the unchanged %s/%s",
			res.Stage, res.Code, StageTargetAuth, CodeDBAuthFailed)
	}
}

// TestCheck_OracleServiceNameConflict_Silent covers every way the check must say
// nothing: no conflict in the fleet, a lookup that failed, a row with no service
// name, a non-Oracle row, and a resolver that cannot answer the optional
// interface at all (every proxy's dial fake).
func TestCheck_OracleServiceNameConflict_Silent(t *testing.T) {
	t.Parallel()

	ln := tnsRefuseListener(t, 1017)
	host, port := splitHostPort(t, ln.Addr().String())

	oracleRow := func() *store.Server { return newOracleTarget(host, port) }

	pgRow := func() *store.Server {
		srv := newOracleTarget(host, port)
		srv.Protocol = store.ProtocolPostgreSQL

		return srv
	}

	noServiceRow := func() *store.Server {
		srv := newOracleTarget(host, port)
		srv.OracleServiceName = nil

		return srv
	}

	for name, tc := range map[string]struct {
		resolver shared.ServerResolver
		srv      *store.Server
	}{
		"the rows claiming the service name agree": {
			&conflictResolver{fakeResolver: newFakeResolver()}, oracleRow(),
		},
		"the lookup failed": {
			&conflictResolver{fakeResolver: newFakeResolver(), err: errTestConflictLookup}, oracleRow(),
		},
		"the row carries no service name": {
			&conflictResolver{fakeResolver: newFakeResolver(), conflict: twoSpellingConflict("ORCLPDB1")}, noServiceRow(),
		},
		"the row is not Oracle": {
			&conflictResolver{fakeResolver: newFakeResolver(), conflict: twoSpellingConflict("ORCLPDB1")}, pgRow(),
		},
		"the resolver cannot answer": {
			newFakeResolver(), oracleRow(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			checker := New(tc.resolver, testKey())
			checker.timeout = 5 * time.Second

			res := checker.Check(context.Background(), tc.srv)

			if len(res.Warnings) != 0 {
				t.Errorf("Warnings = %+v, want none", res.Warnings)
			}
		})
	}
}

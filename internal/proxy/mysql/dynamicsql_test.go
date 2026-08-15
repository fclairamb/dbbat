package mysql

import (
	"errors"
	"testing"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// controlledSession is switchSession with a grant that carries controls, which is
// what the dynamic-SQL policy turns on.
func controlledSession(controls ...string) *Session {
	s := switchSession()
	s.grant = &store.Grant{
		UID:        uuid.New(),
		ExpiresAt:  time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{Controls: controls},
	}

	return s
}

// runOne drives one statement through the intercept path and reports whether the
// upstream was reached and what the client was told.
func runOne(t *testing.T, s *Session, sql string) (bool, error) {
	t.Helper()

	hnd := &handler{session: s}
	reached := false

	_, err := hnd.runIntercepted(sql, nil, func() (*gomysql.Result, error) {
		reached = true

		return &gomysql.Result{}, nil
	})

	return reached, err
}

// TestPreparedPayloadIsClassifiedLikeTheOuterStatement pins the row of the spec's
// table that was measurably allowed: `PREPARE s FROM 'DELETE FROM t'` under a
// grant carrying read_only *and* block_ddl. `PREPARE` matches no write keyword,
// no DDL keyword and no blocked pattern, so the payload was the statement nothing
// classified.
func TestPreparedPayloadIsClassifiedLikeTheOuterStatement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		sql  string
		want error
	}{
		// The direct spelling, which was always refused — the control.
		{"DELETE FROM t", shared.ErrReadOnlyViolation},
		{"PREPARE s FROM 'DELETE FROM t'", shared.ErrReadOnlyViolation},
		{`PREPARE s FROM "DELETE FROM t"`, shared.ErrReadOnlyViolation},
		{"prepare s from 'delete from t'", shared.ErrReadOnlyViolation},
		{"PREPARE s FROM 'UPDATE t SET a = ''x'''", shared.ErrReadOnlyViolation},
		{"PREPARE s FROM/**/'DELETE FROM t'", shared.ErrReadOnlyViolation},
		// MariaDB's one-statement spelling of the same thing.
		{"EXECUTE IMMEDIATE 'DELETE FROM t'", shared.ErrReadOnlyViolation},
		{"EXECUTE IMMEDIATE 'DELETE FROM t WHERE a = ?' USING @a", shared.ErrReadOnlyViolation},
		// The MySQL blocked-pattern list reaches the payload too, so the two
		// enforcement mechanisms agree on what a prepared statement may do.
		{"PREPARE s FROM 'SELECT * FROM t INTO OUTFILE ''/tmp/x'''", shared.ErrMySQLPatternBlocked},
		{"PREPARE s FROM 'LOAD DATA INFILE ''/x'' INTO TABLE t'", shared.ErrMySQLPatternBlocked},
	}

	for _, tc := range tests {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()

			reached, err := runOne(t, controlledSession(store.ControlReadOnly, store.ControlBlockDDL), tc.sql)

			if !errors.Is(err, tc.want) {
				t.Fatalf("runIntercepted(%q) = %v, want %v", tc.sql, err, tc.want)
			}

			if reached {
				t.Fatalf("runIntercepted(%q) reached the upstream", tc.sql)
			}
		})
	}
}

// TestBenignPreparedPayloadStillRuns is the boundary: this must not become a
// blanket refusal of PREPARE. A read is a read whether it is spelled directly or
// prepared, and it stays allowed under a read-only grant.
func TestBenignPreparedPayloadStillRuns(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{
		"PREPARE s FROM 'SELECT 1'",
		`PREPARE s FROM "SELECT * FROM t WHERE a = 1"`,
		"EXECUTE IMMEDIATE 'SELECT 1'",
		"EXECUTE IMMEDIATE 'SELECT * FROM t WHERE a = ?' USING @a",
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()

			reached, err := runOne(t, controlledSession(store.ControlReadOnly, store.ControlBlockDDL), sql)
			if err != nil {
				t.Fatalf("runIntercepted(%q) = %v, want nil", sql, err)
			}

			if !reached {
				t.Fatalf("runIntercepted(%q) never reached the upstream", sql)
			}
		})
	}
}

// opaquePrepares are the forms whose statement text the server builds from values
// dbbat never sees.
var opaquePrepares = []string{
	"PREPARE s FROM @sql",
	"PREPARE s FROM CONCAT('DELETE FROM ', @t)",
	"EXECUTE IMMEDIATE @sql",
}

// TestOpaquePrepareIsRefusedUnderReadOnlyOrBlockDDL: for a grant that says the
// session may not write or may not change schema, "dbbat cannot tell what this
// runs" must not resolve to "allow".
func TestOpaquePrepareIsRefusedUnderReadOnlyOrBlockDDL(t *testing.T) {
	t.Parallel()

	for _, control := range []string{store.ControlReadOnly, store.ControlBlockDDL} {
		for _, sql := range opaquePrepares {
			t.Run(control+" "+sql, func(t *testing.T) {
				t.Parallel()

				reached, err := runOne(t, controlledSession(control), sql)
				if !errors.Is(err, shared.ErrDynamicSQLOpaque) {
					t.Fatalf("runIntercepted(%q) = %v, want ErrDynamicSQLOpaque", sql, err)
				}

				if reached {
					t.Fatalf("runIntercepted(%q) reached the upstream", sql)
				}
			})
		}
	}
}

// TestOpaquePrepareRunsWithoutThoseTwoControls is the boundary the refusal is not
// allowed to cross: `block_copy` alone and a grant with no controls at all are
// unaffected, because dynamic SQL defeats neither.
func TestOpaquePrepareRunsWithoutThoseTwoControls(t *testing.T) {
	t.Parallel()

	for _, controls := range [][]string{nil, {store.ControlBlockCopy}} {
		label := "no controls"
		if len(controls) > 0 {
			label = controls[0]
		}

		for _, sql := range opaquePrepares {
			t.Run(label+" "+sql, func(t *testing.T) {
				t.Parallel()

				reached, err := runOne(t, controlledSession(controls...), sql)
				if err != nil {
					t.Fatalf("runIntercepted(%q) = %v, want nil", sql, err)
				}

				if !reached {
					t.Fatalf("runIntercepted(%q) never reached the upstream", sql)
				}
			})
		}
	}
}

// TestExecuteOfAnUncheckedNameIsRefused covers the half of the pair that carries
// no statement text at all. Checking the PREPARE and letting every EXECUTE
// through would close nothing: the client would only have to build its text with
// `PREPARE s FROM @sql`, which dbbat cannot read, and then run it.
func TestExecuteOfAnUncheckedNameIsRefused(t *testing.T) {
	t.Parallel()

	s := controlledSession(store.ControlReadOnly)

	reached, err := runOne(t, s, "EXECUTE s")
	if !errors.Is(err, shared.ErrDynamicSQLOpaque) {
		t.Fatalf("EXECUTE of an unknown name = %v, want ErrDynamicSQLOpaque", err)
	}

	if reached {
		t.Fatal("EXECUTE of an unknown name reached the upstream")
	}
}

// TestExecuteOfACheckedNameRuns is the other half: a name whose PREPARE dbbat
// read and cleared executes normally, folding case as MySQL does. Without this
// the pair would be unusable under a read-only grant, which is not the decision.
func TestExecuteOfACheckedNameRuns(t *testing.T) {
	t.Parallel()

	s := controlledSession(store.ControlReadOnly)

	if _, err := runOne(t, s, "PREPARE s FROM 'SELECT 1'"); err != nil {
		t.Fatalf("PREPARE of a read = %v, want nil", err)
	}

	for _, sql := range []string{"EXECUTE s", "EXECUTE S", "EXECUTE s USING @a"} {
		reached, err := runOne(t, s, sql)
		if err != nil {
			t.Fatalf("%q after a checked PREPARE = %v, want nil", sql, err)
		}

		if !reached {
			t.Fatalf("%q never reached the upstream", sql)
		}
	}
}

// TestRePreparingANameClearsIt: re-preparing replaces the statement a name stands
// for, so a "checked" flag left over from the previous PREPARE would be the exact
// bypass this bookkeeping exists to close. The second PREPARE is opaque, so it is
// refused outright under read_only — and the name it names must not stay usable.
func TestRePreparingANameClearsIt(t *testing.T) {
	t.Parallel()

	s := controlledSession(store.ControlReadOnly)

	if _, err := runOne(t, s, "PREPARE s FROM 'SELECT 1'"); err != nil {
		t.Fatalf("PREPARE of a read = %v, want nil", err)
	}

	if _, err := runOne(t, s, "PREPARE s FROM @sql"); !errors.Is(err, shared.ErrDynamicSQLOpaque) {
		t.Fatalf("opaque re-PREPARE = %v, want ErrDynamicSQLOpaque", err)
	}

	// Under a grant without those controls the opaque PREPARE is allowed, and the
	// name must come back marked unchecked rather than keeping the old flag.
	relaxed := controlledSession()
	if _, err := runOne(t, relaxed, "PREPARE s FROM 'SELECT 1'"); err != nil {
		t.Fatalf("PREPARE of a read = %v, want nil", err)
	}

	if _, err := runOne(t, relaxed, "PREPARE s FROM @sql"); err != nil {
		t.Fatalf("opaque re-PREPARE under no controls = %v, want nil", err)
	}

	if relaxed.checkedPrepares[preparedNameKey("s")] {
		t.Fatal("an opaque PREPARE left the name marked as checked")
	}
}

// TestExecuteNameIsNotRefusedWithoutThoseTwoControls keeps the EXECUTE half on
// the same boundary as everything else: a `block_copy`-only grant and an
// uncontrolled one never see this refusal.
func TestExecuteNameIsNotRefusedWithoutThoseTwoControls(t *testing.T) {
	t.Parallel()

	for _, controls := range [][]string{nil, {store.ControlBlockCopy}} {
		reached, err := runOne(t, controlledSession(controls...), "EXECUTE s")
		if err != nil {
			t.Fatalf("EXECUTE with controls %v = %v, want nil", controls, err)
		}

		if !reached {
			t.Fatalf("EXECUTE with controls %v never reached the upstream", controls)
		}
	}
}

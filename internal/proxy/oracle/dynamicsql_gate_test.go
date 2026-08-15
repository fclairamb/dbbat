package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestDynamicSQLIsEnforcedOnAStatementPath pins the Oracle row of the escape:
// `BEGIN EXECUTE IMMEDIATE 'DELETE FROM t'; END;` is a write whose first keyword
// is BEGIN, so the prefix-shaped classifiers behind read_only and block_ddl saw
// nothing at all and the statement was measurably allowed.
//
// It runs over a real statement-carrying op rather than over the validator alone,
// because that is where the controls have to bite. All five Oracle paths reach
// shared.ValidateOracleQuery, which is where the unwrapping lives, so covering
// one covers the shape of all of them.
func TestDynamicSQLIsEnforcedOnAStatementPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		controls []string
		sql      string
		wantErr  error
	}{
		{
			name: "a write inside EXECUTE IMMEDIATE is blocked by read_only",
			controls: []string{
				store.ControlReadOnly,
			},
			sql:     "BEGIN EXECUTE IMMEDIATE 'DELETE FROM emp'; END;",
			wantErr: shared.ErrReadOnlyViolation,
		},
		{
			name:     "DDL inside EXECUTE IMMEDIATE is blocked by block_ddl",
			controls: []string{store.ControlBlockDDL},
			sql:      "BEGIN EXECUTE IMMEDIATE 'DROP TABLE emp'; END;",
			wantErr:  shared.ErrDDLBlocked,
		},
		{
			name:     "a literal concatenation is folded before it is classified",
			controls: []string{store.ControlReadOnly},
			sql:      "BEGIN EXECUTE IMMEDIATE 'DELETE ' || 'FROM emp'; END;",
			wantErr:  shared.ErrReadOnlyViolation,
		},
		{
			name:     "DBMS_SQL.PARSE is the same construct",
			controls: []string{store.ControlReadOnly},
			sql:      "BEGIN DBMS_SQL.PARSE(c, 'DELETE FROM emp', DBMS_SQL.NATIVE); END;",
			wantErr:  shared.ErrReadOnlyViolation,
		},
		{
			name:    "an Oracle blocked pattern applies to the payload, with no controls at all",
			sql:     "BEGIN EXECUTE IMMEDIATE q'[ALTER SYSTEM KILL SESSION '123,456']'; END;",
			wantErr: shared.ErrOraclePatternBlocked,
		},
		{
			name:    "an unreadable payload is refused whatever the grant says",
			sql:     "BEGIN EXECUTE IMMEDIATE 'BEGIN EXECUTE IMMEDIATE ''SELECT 1 FROM DUAL''; END;'; END;",
			wantErr: shared.ErrDynamicSQLNotCheckable,
		},
		{
			name:     "a payload dbbat cannot read is refused under read_only",
			controls: []string{store.ControlReadOnly},
			sql:      "BEGIN EXECUTE IMMEDIATE v_stmt; END;",
			wantErr:  shared.ErrDynamicSQLOpaque,
		},
		{
			name:     "and under block_ddl",
			controls: []string{store.ControlBlockDDL},
			sql:      "BEGIN EXECUTE IMMEDIATE v_stmt; END;",
			wantErr:  shared.ErrDynamicSQLOpaque,
		},

		// The boundary. None of these may be refused: this must not become a
		// blanket refusal of PL/SQL that builds SQL at run time.
		{
			name:     "a read inside EXECUTE IMMEDIATE is allowed under read_only",
			controls: []string{store.ControlReadOnly},
			sql:      "BEGIN EXECUTE IMMEDIATE 'SELECT 1 FROM DUAL'; END;",
		},
		{
			name:     "an unreadable payload is allowed under block_copy alone",
			controls: []string{store.ControlBlockCopy},
			sql:      "BEGIN EXECUTE IMMEDIATE v_stmt; END;",
		},
		{
			name: "an unreadable payload is allowed under a grant with no controls",
			sql:  "BEGIN EXECUTE IMMEDIATE v_stmt; END;",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: tc.controls}})

			err := s.handleJDBCExec(buildJDBCExec(tc.sql))

			if tc.wantErr == nil {
				require.NoError(t, err)
				require.NotNil(t, s.tracker.pendingQuery, "an allowed statement is still tracked")

				return
			}

			require.ErrorIs(t, err, tc.wantErr)
			assert.Nil(t, s.tracker.pendingQuery,
				"a refused statement must not be tracked as in flight")
		})
	}
}

package mcp

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The driver failures each protocol actually produces. Static so the linter's
// "no dynamic errors" rule is satisfied, and so the strings sit together where
// they can be compared against statementTimeoutSignatures.
var (
	errPGStatementTimeout = errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")
	errPGUserRequest      = errors.New("ERROR: canceling statement due to user request (SQLSTATE 57014)")
	errMySQLMaxExecution  = errors.New(
		"ERROR 3024 (HY000): Query execution was interrupted, maximum statement execution time exceeded")
	errMongoMaxTimeMS  = errors.New("(MaxTimeMSExpired) operation exceeded time limit")
	errOrdinaryFailure = errors.New(`ERROR: relation "nope" does not exist (SQLSTATE 42P01)`)
	errBareCancel      = errors.New("canceling statement due to user request")
)

func TestClassifyStatementTimeout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		err   error
		limit time.Duration
		want  bool
	}{
		{
			name:  "postgresql server-side cancellation",
			err:   errPGStatementTimeout,
			limit: 30 * time.Second,
			want:  true,
		},
		{
			name:  "postgresql watchdog CancelRequest",
			err:   errPGUserRequest,
			limit: 30 * time.Second,
			want:  true,
		},
		{
			name:  "mysql max_execution_time",
			err:   errMySQLMaxExecution,
			limit: time.Second,
			want:  true,
		},
		{
			name:  "mongodb maxTimeMS",
			err:   errMongoMaxTimeMS,
			limit: time.Second,
			want:  true,
		},
		{
			name:  "an ordinary failure is left alone",
			err:   errOrdinaryFailure,
			limit: 30 * time.Second,
			want:  false,
		},
		{
			// Without a limit configured, a cancellation came from something
			// else entirely and must keep saying what it said.
			name:  "no limit reclassifies nothing",
			err:   errBareCancel,
			limit: 0,
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := classifyStatementTimeout(tc.err, tc.limit)

			if errors.Is(got, ErrStatementTimeout) != tc.want {
				t.Fatalf("classify(%v) → %v, want ErrStatementTimeout=%v", tc.err, got, tc.want)
			}

			if !tc.want {
				if got.Error() != tc.err.Error() {
					t.Fatalf("unclassified error was rewritten: %q", got)
				}

				return
			}

			// The limit is named, and the driver's own words survive.
			if !strings.Contains(got.Error(), tc.limit.String()) {
				t.Fatalf("classified error does not name the limit: %q", got)
			}

			if !errors.Is(got, tc.err) {
				t.Fatalf("classified error lost the original: %q", got)
			}
		})
	}

	if got := classifyStatementTimeout(nil, time.Second); got != nil {
		t.Fatalf("classify(nil) = %v, want nil", got)
	}
}

package mcp

import (
	"errors"
	"strings"
	"testing"
	"time"
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
			err:   errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)"),
			limit: 30 * time.Second,
			want:  true,
		},
		{
			name:  "postgresql watchdog CancelRequest",
			err:   errors.New("ERROR: canceling statement due to user request (SQLSTATE 57014)"),
			limit: 30 * time.Second,
			want:  true,
		},
		{
			name:  "mysql max_execution_time",
			err:   errors.New("ERROR 3024 (HY000): Query execution was interrupted, maximum statement execution time exceeded"),
			limit: time.Second,
			want:  true,
		},
		{
			name:  "mongodb maxTimeMS",
			err:   errors.New("(MaxTimeMSExpired) operation exceeded time limit"),
			limit: time.Second,
			want:  true,
		},
		{
			name:  "an ordinary failure is left alone",
			err:   errors.New(`ERROR: relation "nope" does not exist (SQLSTATE 42P01)`),
			limit: 30 * time.Second,
			want:  false,
		},
		{
			// Without a limit configured, a cancellation came from something
			// else entirely and must keep saying what it said.
			name:  "no limit reclassifies nothing",
			err:   errors.New("canceling statement due to user request"),
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

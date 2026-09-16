package mysql

import (
	"context"
	"testing"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

const querytagConnUID = "0192f3a1-7c4d-7e2a-9b11-3f9a1c7b2e4d"

const querytagPrefix = "/*dbbat='0.28.1',user='florent',conn='3f9a1c7b2e4d',grant='diag-paris'*/ "

// taggedHandler builds a handler over a session running with
// DBB_QUERY_TAGGING on. controls are the grant's, so a test can check that a
// refusal is unchanged by tagging.
func taggedHandler(tagging bool, controls []string) *handler {
	s := &Session{
		logger:       discardLogger(),
		ctx:          context.Background(),
		authComplete: true,
		database:     &store.Server{Name: "prod-entry", DatabaseName: "appdb"},
		grant: &store.Grant{
			UID:        uuid.New(),
			ExpiresAt:  time.Now().Add(time.Hour),
			Definition: &store.GrantDefinition{Controls: controls},
		},
	}

	if tagging {
		s.queryTag = shared.NewQueryTagger(
			"0.28.1", "florent", uuid.MustParse(querytagConnUID), "diag-paris")
	}

	return &handler{session: s}
}

// TestUpstreamText is the one place the text handed to the upstream differs
// from the text dbbat enforces and records, so it gets its own test.
func TestUpstreamText(t *testing.T) {
	t.Parallel()

	assert.Equal(t, querytagPrefix+"SELECT 1",
		taggedHandler(true, nil).upstreamText("SELECT 1"))

	assert.Equal(t, "SELECT 1",
		taggedHandler(false, nil).upstreamText("SELECT 1"),
		"with tagging off not one byte may change")
}

// TestQueryTag_RecordsTheClientText is the central invariant: whatever goes on
// the wire, what runIntercepted carries through to the queries row and the
// audit chain is the statement the client sent.
func TestQueryTag_RecordsTheClientText(t *testing.T) {
	t.Parallel()

	hnd := taggedHandler(true, nil)

	var sawUpstream string

	_, err := hnd.runIntercepted("SELECT 1", nil, func() (*gomysql.Result, error) {
		// Exactly what HandleQuery's closure does.
		sawUpstream = hnd.upstreamText("SELECT 1")

		return &gomysql.Result{}, nil
	})
	require.NoError(t, err)

	assert.Equal(t, querytagPrefix+"SELECT 1", sawUpstream)
}

// TestQueryTag_ControlsAreIdenticalWithTagging — every control matches the
// client's text, before the tag exists. A refused statement never reaches the
// upstream at all, so it is never tagged either.
func TestQueryTag_ControlsAreIdenticalWithTagging(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		controls []string
		sql      string
	}{
		{"read_only write", []string{store.ControlReadOnly}, "DELETE FROM t"},
		{"block_ddl", []string{store.ControlBlockDDL}, "CREATE TABLE t (a int)"},
		{"switch database", nil, "USE otherdb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var offErr, onErr error

			for _, tagging := range []bool{false, true} {
				hnd := taggedHandler(tagging, tt.controls)
				execRan := false

				_, err := hnd.runIntercepted(tt.sql, nil, func() (*gomysql.Result, error) {
					execRan = true

					return &gomysql.Result{}, nil
				})

				assert.False(t, execRan, "a refused statement must never reach the upstream")

				if tagging {
					onErr = err
				} else {
					offErr = err
				}
			}

			require.Error(t, offErr)
			assert.Equal(t, offErr.Error(), onErr.Error(),
				"the refusal must not depend on whether tagging is on")
		})
	}
}

// TestQueryTag_RepeatedExecutionsAreByteIdentical is what keeps the MySQL
// digest aggregating: the same statement on the same session must produce the
// same bytes every time.
func TestQueryTag_RepeatedExecutionsAreByteIdentical(t *testing.T) {
	t.Parallel()

	hnd := taggedHandler(true, nil)

	first := hnd.upstreamText("SELECT count(*) FROM big")
	for range 3 {
		assert.Equal(t, first, hnd.upstreamText("SELECT count(*) FROM big"))
	}
}

package testsupport

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// grantDuration is how long the definitions minted here stay valid. Long
// enough that no suite can outlive its own fixture.
const grantDuration = 24 * time.Hour

// GrantOption tunes the definition a grant is minted from, for the suites that
// care about a field the common case never sets. It is an option rather than
// another parameter so the four suites that only ever pass controls keep
// reading as one line.
type GrantOption func(*store.GrantDefinition)

// WithApprovalPatterns puts approval-hold patterns on the definition — the RE2
// patterns that suspend a matching statement until a second human releases it.
// The SQL Server suites are the ones that exercise holds.
func WithApprovalPatterns(patterns ...string) GrantOption {
	return func(def *store.GrantDefinition) {
		if patterns == nil {
			patterns = []string{}
		}

		def.ApprovalPatterns = patterns
	}
}

// CreateGrantWithControls issues a grant carrying the given controls. Every
// grant is an *instance of a definition* and carries no shape of its own, so
// the definition has to exist first and be named by uid — an inline
// GrantDefinition on the grant is not enough (CreateGrant answers
// ErrGrantDefinitionRequired), which is what used to fail these suites at
// setup.
//
// The definition's name/slug is randomized per call: a suite that seeds twice
// against one store would otherwise trip "grant definition with this slug
// already exists". Callers never have to think about it.
func CreateGrantWithControls(
	ctx context.Context,
	t *testing.T,
	dataStore *store.Store,
	userUID, databaseUID uuid.UUID,
	controls []string,
	opts ...GrantOption,
) (*store.Grant, error) {
	t.Helper()

	if controls == nil {
		controls = []string{}
	}

	name := fmt.Sprintf("itest-%s", uuid.NewString()[:8])

	definition := &store.GrantDefinition{
		Name:             name,
		Slug:             name,
		DurationSeconds:  int64(grantDuration / time.Second),
		Controls:         controls,
		ApprovalPatterns: []string{},
		CreatedBy:        userUID,
	}

	for _, opt := range opts {
		opt(definition)
	}

	def, err := dataStore.CreateGrantDefinition(ctx, definition)
	require.NoError(t, err)

	return dataStore.CreateGrant(ctx, &store.Grant{
		UserID:            userUID,
		DatabaseID:        databaseUID,
		GrantedBy:         userUID,
		GrantDefinitionID: def.UID,
		Definition:        def,
		StartsAt:          time.Now().Add(-time.Hour),
		ExpiresAt:         time.Now().Add(grantDuration),
	})
}

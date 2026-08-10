//go:build integration

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

// CreateGrantWithControls issues a grant carrying the given controls. Every
// grant is an *instance of a definition* and carries no shape of its own, so
// the definition has to exist first and be named by uid — an inline
// GrantDefinition on the grant is not enough (CreateGrant answers
// ErrGrantDefinitionRequired), which is what used to fail these suites at
// setup.
//
// The definition's name/slug is randomised per call: a suite that seeds twice
// against one store would otherwise trip "grant definition with this slug
// already exists". Callers never have to think about it.
func CreateGrantWithControls(
	ctx context.Context,
	t *testing.T,
	dataStore *store.Store,
	userUID, databaseUID uuid.UUID,
	controls []string,
) (*store.Grant, error) {
	t.Helper()

	if controls == nil {
		controls = []string{}
	}

	name := fmt.Sprintf("itest-%s", uuid.NewString()[:8])

	def, err := dataStore.CreateGrantDefinition(ctx, &store.GrantDefinition{
		Name:            name,
		Slug:            name,
		DurationSeconds: int64(grantDuration / time.Second),
		Controls:        controls,
		CreatedBy:       userUID,
	})
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

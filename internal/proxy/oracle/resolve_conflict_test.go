package oracle

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestResolveDatabase_SharedServiceName covers the branch this spec is about:
// resolveDatabase compares candidate upstreams as **text**, so it refuses when
// two rows behind one service name spell their host differently, and relays
// when they agree.
//
// The refusal is correct — the pre-auth relay has no single address to dial —
// and the compare stays textual on purpose. What this pins is that the refusal
// is *readable*: ORA-12514 naming the service name, how many rows claim it and
// how many upstreams they name, with the conflicting row names kept out of a
// pre-auth message and logged instead.
//
// Only dbbat's own PostgreSQL store is involved: the branch is decided by the
// rows, so no Oracle is needed and this runs under `make test`.
func TestResolveDatabase_SharedServiceName(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataStore := newOracleTestStore(t)

	serverKey := make([]byte, 32)
	for i := range serverKey {
		serverKey[i] = byte(0x5A + i)
	}

	// mkRow registers an Oracle row claiming service at host:1521.
	mkRow := func(t *testing.T, name, host, service string) *store.Server {
		t.Helper()

		svc := service
		db, err := dataStore.CreateServer(ctx, &store.Server{
			Name:              name,
			Host:              host,
			Port:              1521,
			DatabaseName:      name,
			OracleServiceName: &svc,
			Username:          "system",
			Password:          "oracle",
			Protocol:          store.ProtocolOracle,
		}, serverKey)
		require.NoError(t, err)

		return db
	}

	// The incident's exact shape: one machine, two spellings, one service name.
	const conflicting = "MUTU02_CONFLICT"

	mkRow(t, "abyla_abymutualise02_ro", "oracle-abymutualise02.db.stonal.io", conflicting)
	mkRow(t, "abyla_abymutualise_ro", "abymutualise02.cusruf0cguz3.eu-west-3.rds.amazonaws.com", conflicting)

	t.Run("two spellings of one host are refused ORA-12514", func(t *testing.T) {
		t.Parallel()

		client, server := newPipeConns(t)

		s := newTestErrorSession(t, server)
		s.store = dataStore

		errCh := make(chan error, 1)
		go func() { errCh <- s.resolveDatabase(buildTestTNSConnect(conflicting)) }()

		pkt, err := readTNSPacket(client)
		require.NoError(t, err)
		assert.Equal(t, TNSPacketTypeRedirect, pkt.Type)

		payload := string(pkt.Payload)
		assert.Contains(t, payload, "ERR=12514")
		assert.Contains(t, payload, conflicting, "the refusal should name the service name the client sent")
		assert.Contains(t, payload, "2 dbbat databases")
		assert.Contains(t, payload, "2 different upstreams")
		assert.Contains(t, payload, "connect using the dbbat database name")

		// Pre-auth: the conflicting row names stay out of the wire message and
		// live in the WARN log and the admin UI instead.
		assert.NotContains(t, payload, "abyla_abymutualise")

		resolveErr := <-errCh
		require.ErrorIs(t, resolveErr, ErrAmbiguousServiceName)
		assert.Contains(t, resolveErr.Error(), "oracle-abymutualise02.db.stonal.io:1521",
			"the returned error should name the upstreams for the operator")
	})

	t.Run("rows agreeing on host:port defer the choice to AUTH", func(t *testing.T) {
		t.Parallel()

		const agreeing = "MUTU02_AGREE"

		mkRow(t, "abyla_agree_a", "oracle-agree.db.stonal.io", agreeing)
		mkRow(t, "abyla_agree_b", "oracle-agree.db.stonal.io", agreeing)

		_, server := newPipeConns(t)

		s := newTestErrorSession(t, server)
		s.store = dataStore

		require.NoError(t, s.resolveDatabase(buildTestTNSConnect(agreeing)))
		assert.Len(t, s.databaseCandidates, 2, "both candidates should be kept for disambiguateDatabase")
		require.NotNil(t, s.database)
		assert.Equal(t, "oracle-agree.db.stonal.io", s.database.Host)
	})

	t.Run("a single claimant resolves without any compare", func(t *testing.T) {
		t.Parallel()

		const lone = "MUTU02_LONE"

		mkRow(t, "abyla_lone", "oracle-lone.db.stonal.io", lone)

		_, server := newPipeConns(t)

		s := newTestErrorSession(t, server)
		s.store = dataStore

		require.NoError(t, s.resolveDatabase(buildTestTNSConnect(lone)))
		assert.Empty(t, s.databaseCandidates)
		require.NotNil(t, s.database)
		assert.Equal(t, "abyla_lone", s.database.Name)
	})
}

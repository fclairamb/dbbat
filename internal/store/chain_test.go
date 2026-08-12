package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// logTestAuditEvents writes n chained audit events and returns them in chain
// order.
func logTestAuditEvents(t *testing.T, ctx context.Context, store *Store, n int) []AuditLog {
	t.Helper()

	for i := range n {
		err := store.LogAuditEvent(ctx, &AuditEvent{
			EventType: "chain_test",
			Details:   json.RawMessage(fmt.Sprintf(`{"i":%d,"b":"x","a":1.0}`, i)),
		})
		require.NoError(t, err, "LogAuditEvent()")
	}

	return readChainedAuditRows(t, ctx, store)
}

func readChainedAuditRows(t *testing.T, ctx context.Context, store *Store) []AuditLog {
	t.Helper()

	var rows []AuditLog

	err := store.db.NewSelect().
		Model(&rows).
		Where("chain_seq >= 1").
		Order("chain_seq ASC").
		Scan(ctx)
	require.NoError(t, err, "reading chained audit rows")

	return rows
}

func TestAuditChainVerifiesClean(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	rows := logTestAuditEvents(t, ctx, store, 5)
	require.Len(t, rows, 5)

	// chain_seq starts at 1 and is contiguous; the anchor is seq 0.
	for i, row := range rows {
		require.NotNil(t, row.ChainSeq)
		require.Equal(t, int64(i+1), *row.ChainSeq)
		require.NotEmpty(t, row.MAC)
		require.NotEmpty(t, row.PrevMAC)
	}

	result, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	require.True(t, result.OK(), "chain should verify: %v", result.Break)
	require.Equal(t, int64(5), result.Verified)
	require.Equal(t, int64(5), result.HeadSeq)
	require.Equal(t, rows[4].MAC, result.HeadMAC)
	require.NotEmpty(t, result.HeadMACHex())

	// The anchor row is the only unchained row on a fresh store: it is the
	// marker saying "everything before here is unverifiable".
	require.Equal(t, int64(0), result.Unchained)
}

func TestAuditChainAnchorExists(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	var anchor AuditLog

	err := store.db.NewSelect().
		Model(&anchor).
		Where("uid = ?", uuid.MustParse(AuditChainAnchorUID)).
		Scan(ctx)
	require.NoError(t, err, "the migration must insert the chain anchor")

	require.Equal(t, AuditChainAnchorEventType, anchor.EventType)
	require.NotNil(t, anchor.ChainSeq)
	require.Equal(t, int64(0), *anchor.ChainSeq)
	require.Nil(t, anchor.MAC, "the anchor is a marker, not a link")
}

// TestAuditChainDetectsModifiedRow is the headline case: an admin with direct
// PostgreSQL access rewrites what an entry says.
func TestAuditChainDetectsModifiedRow(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	rows := logTestAuditEvents(t, ctx, store, 4)

	_, err := store.db.ExecContext(ctx,
		`UPDATE audit_log SET details = '{"i":999}'::jsonb WHERE uid = ?`, rows[2].UID)
	require.NoError(t, err)

	result, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	require.False(t, result.OK(), "a modified entry must break the chain")
	require.Equal(t, rows[2].UID, result.Break.UID)
	require.Equal(t, int64(3), result.Break.ChainSeq)
	require.Contains(t, result.Break.Reason, "modified")
	// The walk stops at the first break: the two clean entries before it.
	require.Equal(t, int64(2), result.Verified)
}

// TestAuditChainDetectsModifiedEventType covers a field other than details, so
// the test above cannot pass by accident on JSON handling alone.
func TestAuditChainDetectsModifiedEventType(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	rows := logTestAuditEvents(t, ctx, store, 3)

	_, err := store.db.ExecContext(ctx,
		`UPDATE audit_log SET event_type = 'innocuous' WHERE uid = ?`, rows[0].UID)
	require.NoError(t, err)

	result, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	require.False(t, result.OK())
	require.Equal(t, int64(1), result.Break.ChainSeq)
}

func TestAuditChainDetectsDeletedRow(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	rows := logTestAuditEvents(t, ctx, store, 5)

	_, err := store.db.ExecContext(ctx, `DELETE FROM audit_log WHERE uid = ?`, rows[2].UID)
	require.NoError(t, err)

	result, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	require.False(t, result.OK(), "a deleted entry must break the chain")
	require.Equal(t, int64(4), result.Break.ChainSeq, "the break is reported at the surviving successor")
	require.Contains(t, result.Break.Reason, "missing or reordered")
}

// TestAuditChainDetectsDeletedFirstRow is the case the key-derived genesis MAC
// exists for: without it, lopping off the start of the chain would look like a
// chain that simply started later.
func TestAuditChainDetectsDeletedFirstRow(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	rows := logTestAuditEvents(t, ctx, store, 3)

	_, err := store.db.ExecContext(ctx, `DELETE FROM audit_log WHERE uid = ?`, rows[0].UID)
	require.NoError(t, err)

	result, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	require.False(t, result.OK())
	require.Equal(t, int64(0), result.Verified)
}

// TestAuditChainDetectsDeletedLastRow: the audit chain is never truncated by
// retention, so a shortened tail is caught by comparing the head against what
// an operator recorded out of band — which is what the head MAC is for.
func TestAuditChainDetectsDeletedLastRow(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	rows := logTestAuditEvents(t, ctx, store, 4)

	before, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	require.True(t, before.OK())

	_, err = store.db.ExecContext(ctx, `DELETE FROM audit_log WHERE uid = ?`, rows[3].UID)
	require.NoError(t, err)

	after, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)

	// The surviving prefix still verifies on its own — that is inherent to a
	// chain — but the head moved, which is exactly what a recorded head
	// detects.
	require.True(t, after.OK())
	require.NotEqual(t, before.HeadMACHex(), after.HeadMACHex())
	require.Equal(t, int64(3), after.HeadSeq)
}

func TestAuditChainDetectsReorderedRows(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	rows := logTestAuditEvents(t, ctx, store, 4)

	// Swap the positions of two entries, keeping everything else intact. The
	// unique index on chain_seq forces the swap through a parking value.
	_, err := store.db.ExecContext(ctx, `UPDATE audit_log SET chain_seq = -1 WHERE uid = ?`, rows[1].UID)
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `UPDATE audit_log SET chain_seq = 2 WHERE uid = ?`, rows[2].UID)
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `UPDATE audit_log SET chain_seq = 3 WHERE uid = ?`, rows[1].UID)
	require.NoError(t, err)

	result, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	require.False(t, result.OK(), "reordered entries must break the chain")
	require.Equal(t, int64(2), result.Break.ChainSeq)
}

// TestAuditChainSurvivesTimestampRoundTrip is the determinism trap the design
// note calls out: PostgreSQL keeps microseconds and Go keeps nanoseconds, so a
// MAC over an untruncated timestamp would never verify again.
func TestAuditChainSurvivesTimestampRoundTrip(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	// Loop so the test does not depend on time.Now() happening to land on a
	// microsecond boundary.
	for range 25 {
		require.NoError(t, store.LogAuditEvent(ctx, &AuditEvent{EventType: "timing"}))
	}

	rows := readChainedAuditRows(t, ctx, store)
	require.NotEmpty(t, rows)

	for _, row := range rows {
		require.Equal(t, row.CreatedAt.UTC().Truncate(time.Microsecond).UnixNano(), row.CreatedAt.UTC().UnixNano(),
			"a stored timestamp must already be microsecond-aligned")
	}

	result, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	require.True(t, result.OK(), "chain should verify after the timestamp round trip: %v", result.Break)
}

// TestCanonicalJSONSurvivesJSONB pins the other determinism trap against a real
// PostgreSQL: jsonb re-renders what it is given, so the canonical form has to
// be a fixed point of that round trip.
func TestCanonicalJSONSurvivesJSONB(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	documents := []string{
		`{"b":1,"a":2}`,
		`{"zzz":1,"a":2,"mm":3}`,
		`{"nested":{"z":1,"a":{"y":2,"b":3}}}`,
		`{"n":1.0}`,
		`{"n":1.50}`,
		`{"n":1e2}`,
		`{"n":1.5e-3}`,
		`{"n":-0}`,
		`{"n":12345678901234567890}`,
		`{"n":0.000001}`,
		`{"s":"a \"quoted\" string"}`,
		`{"s":"tab\there\nnewline"}`,
		`{"s":"unicode: é ☃ 𝄞"}`,
		`{"s":"html <b>&</b> chars"}`,
		`{"s":"slash / backslash \\"}`,
		`{"arr":[1,2.0,"three",null,true,{"k":"v"}]}`,
		`{"empty_obj":{},"empty_arr":[],"null":null}`,
		`{"dup":1,"dup":2}`,
		`[]`,
		`{}`,
		`"just a string"`,
		`42`,
		`true`,
		`null`,
	}

	for i, doc := range documents {
		want, err := canonicalJSON([]byte(doc))
		require.NoErrorf(t, err, "canonicalJSON(%s)", doc)

		uid := newUIDv7()

		_, err = store.db.ExecContext(ctx,
			`INSERT INTO audit_log (uid, event_type, details, created_at) VALUES (?, ?, ?::jsonb, NOW())`,
			uid, fmt.Sprintf("jsonb_roundtrip_%d", i), doc)
		require.NoErrorf(t, err, "inserting %s", doc)

		var stored struct {
			Details json.RawMessage `bun:"details"`
		}

		err = store.db.NewSelect().
			Model((*AuditLog)(nil)).
			Column("details").
			Where("uid = ?", uid).
			Scan(ctx, &stored)
		require.NoError(t, err)

		got, err := canonicalJSON(stored.Details)
		require.NoErrorf(t, err, "canonicalJSON of the stored form of %s", doc)

		require.Equalf(t, string(want), string(got),
			"canonicalJSON is not a fixed point of the jsonb round trip for %s (stored as %s)",
			doc, stored.Details)
	}
}

// TestAuditChainConcurrentAppends is what the advisory lock and the head mutex
// are for: concurrent writers must produce one chain, not a fork.
func TestAuditChainConcurrentAppends(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	const (
		writers = 8
		each    = 6
	)

	var wg sync.WaitGroup

	errs := make(chan error, writers*each)

	for w := range writers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range each {
				err := store.LogAuditEvent(ctx, &AuditEvent{
					EventType: "concurrent",
					Details:   json.RawMessage(fmt.Sprintf(`{"w":%d,"i":%d}`, w, i)),
				})
				if err != nil {
					errs <- err
				}
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err, "concurrent LogAuditEvent()")
	}

	rows := readChainedAuditRows(t, ctx, store)
	require.Len(t, rows, writers*each, "every append must have landed exactly once")

	for i, row := range rows {
		require.Equal(t, int64(i+1), *row.ChainSeq, "chain positions must be contiguous")
	}

	result, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	require.True(t, result.OK(), "concurrent appends must form one valid chain: %v", result.Break)
	require.Equal(t, int64(writers*each), result.Verified)
}

// TestAuditChainRecoversAfterRestart covers the cache being cold: a second
// store against the same database must re-read the head instead of restarting
// the chain at 1.
func TestAuditChainRecoversAfterRestart(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	logTestAuditEvents(t, ctx, store, 3)

	// Same database, fresh in-memory state.
	restarted := &Store{db: store.db, queryChains: newQueryChains(), chainKey: store.chainKey}

	require.NoError(t, restarted.LogAuditEvent(ctx, &AuditEvent{EventType: "after_restart"}))

	rows := readChainedAuditRows(t, ctx, store)
	require.Len(t, rows, 4)
	require.Equal(t, int64(4), *rows[3].ChainSeq)

	result, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	require.True(t, result.OK(), "the chain must survive a restart: %v", result.Break)
}

// TestChainDisabledWithoutKey documents the one configuration where nothing is
// sealed. A serving process never lands here — config always resolves a key —
// but a store built without one must degrade to plain inserts rather than
// pretending.
func TestChainDisabledWithoutKey(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	unkeyed := &Store{db: store.db, queryChains: newQueryChains()}
	require.False(t, unkeyed.ChainEnabled())

	require.NoError(t, unkeyed.LogAuditEvent(ctx, &AuditEvent{EventType: "unsealed"}))

	require.Empty(t, readChainedAuditRows(t, ctx, store), "an unkeyed store must not chain")

	_, err := unkeyed.VerifyAuditChain(ctx)
	require.ErrorIs(t, err, ErrChainKeyUnavailable)

	_, err = unkeyed.VerifyQueryChains(ctx, nil)
	require.ErrorIs(t, err, ErrChainKeyUnavailable)
}

// createChainTestConnection sets up a connection and n statements on it.
func createChainTestConnection(
	t *testing.T, ctx context.Context, store *Store, suffix string, statements int,
) (*Connection, []Query) {
	t.Helper()

	user, database := createTestUserAndDatabase(t, ctx, store, "chain_"+suffix)

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	require.NoError(t, err, "CreateConnection()")

	queries := make([]Query, 0, statements)

	for i := range statements {
		created, err := store.CreateQuery(ctx, &Query{
			ConnectionID: conn.UID,
			SQLText:      fmt.Sprintf("SELECT %d", i),
			Parameters:   &QueryParameters{Values: []string{fmt.Sprintf("v%d", i)}},
		})
		require.NoError(t, err, "CreateQuery()")

		queries = append(queries, *created)
	}

	return conn, queries
}

func TestQueryChainVerifiesClean(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "clean", 4)

	for i, q := range queries {
		require.NotNil(t, q.ChainSeq)
		require.Equal(t, int64(i+1), *q.ChainSeq)
		require.NotEmpty(t, q.MAC)
	}

	one, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, one.Break, "chain should verify")
	require.Equal(t, int64(4), one.Verified)
	require.False(t, one.TruncatedPrefix)

	all, err := store.VerifyQueryChains(ctx, nil)
	require.NoError(t, err)
	require.True(t, all.OK())
	require.Equal(t, int64(1), all.Connections)
	require.Equal(t, int64(4), all.Verified)
}

func TestQueryChainDetectsModifiedStatement(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "modified", 3)

	_, err := store.db.ExecContext(ctx,
		`UPDATE queries SET sql_text = 'SELECT ''innocent''' WHERE uid = ?`, queries[1].UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break)
	require.Equal(t, queries[1].UID, result.Break.UID)
	require.Contains(t, result.Break.Reason, "modified")
}

func TestQueryChainDetectsModifiedParameters(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "params", 2)

	_, err := store.db.ExecContext(ctx,
		`UPDATE queries SET parameters = '{"values":["tampered"]}'::jsonb WHERE uid = ?`, queries[0].UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break)
	require.Equal(t, int64(1), result.Break.ChainSeq)
}

func TestQueryChainDetectsDeletedStatement(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "deleted", 4)

	_, err := store.db.ExecContext(ctx, `DELETE FROM queries WHERE uid = ?`, queries[1].UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break)
	require.Contains(t, result.Break.Reason, "missing or reordered")
}

func TestQueryChainDetectsReorderedStatements(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "reordered", 4)

	_, err := store.db.ExecContext(ctx, `UPDATE queries SET chain_seq = -1 WHERE uid = ?`, queries[1].UID)
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `UPDATE queries SET chain_seq = 2 WHERE uid = ?`, queries[2].UID)
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `UPDATE queries SET chain_seq = 3 WHERE uid = ?`, queries[1].UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "reordered statements must break the chain")
	require.Equal(t, int64(2), result.Break.ChainSeq)
}

// TestQueryChainStampsHeadOnClose covers the half of the design that catches a
// deletion at the *end* of a session.
func TestQueryChainStampsHeadOnClose(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "stamp", 3)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	closed, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.Equal(t, int64(3), closed.QueryChainLen)
	require.Equal(t, queryChainStampKeyed, closed.QueryChainStampVersion,
		"a session closed by this build must be sealed, not stamped in the legacy format")
	require.Equal(t, store.queryChainStampMAC(conn.UID, queryChainStampKeyed, 3, queries[2].MAC),
		closed.QueryChainMAC, "the session head must be sealed on close")
	require.NotEqual(t, queries[2].MAC, closed.QueryChainMAC,
		"the stamp must not be a copy of the head MAC: that value is readable from queries")

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break)
	require.False(t, result.LegacyStamp)
}

func TestQueryChainDetectsTrailingDeletion(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "trailing", 3)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	_, err := store.db.ExecContext(ctx, `DELETE FROM queries WHERE uid = ?`, queries[2].UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "deleting the last statement of a closed session must be detected")
	require.Contains(t, result.Break.Reason, "removed from the end")
}

// TestQueryChainDetectsRestampedTrailingDeletion is the case that actually
// matters, and the one the pre-0.24 stamp lost. An attacker with write access
// to the store does not stop at deleting the last statements — they fix the
// stamp up afterwards, with values they can read straight out of `queries`.
// That worked against a stamp that merely echoed the head MAC. It must not
// work against a keyed one.
func TestQueryChainDetectsRestampedTrailingDeletion(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "restamped", 3)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	// Delete the last statement and re-stamp with everything the attacker can
	// see: the new last statement's MAC and the new length.
	_, err := store.db.ExecContext(ctx, `DELETE FROM queries WHERE uid = ?`, queries[2].UID)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx,
		`UPDATE connections SET query_chain_mac = ?, query_chain_len = ? WHERE uid = ?`,
		queries[1].MAC, 2, conn.UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "a re-stamped truncation must still be detected")
	require.Contains(t, result.Break.Reason, "the stamp was rewritten")
	require.False(t, result.LegacyStamp, "a forged stamp is a break, not a legacy stamp")
}

// TestQueryChainDetectsStampVersionDowngrade is the test the version column
// exists for, and the reason the version is *inside* the MAC.
//
// Relabelling a sealed row as legacy is one UPDATE away, and it must never buy
// the attacker a weaker rule. Since 0.24 the outer answer is a break either
// way, so the *interesting* half of this test is now what happens under
// --allow-legacy-stamps: with the escape hatch on, a relabelled row is checked
// against the raw-head comparison it asked for, and fails it, because a keyed
// stamp is not a copy of the head MAC. Without the version in the MAC the
// relabelling would be free; with it the row can only ever verify as the
// version it was sealed at.
func TestQueryChainDetectsStampVersionDowngrade(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, _ := createChainTestConnection(t, ctx, store, "downgrade", 3)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	_, err := store.db.ExecContext(ctx,
		`UPDATE connections SET query_chain_stamp_version = 0 WHERE uid = ?`, conn.UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "relabelling a sealed stamp as legacy must be detected")
	require.Contains(t, result.Break.Reason, "stamp was not keyed")
	require.False(t, result.LegacyStamp, "a relabelled stamp must not be accepted as a legacy one")

	// The escape hatch is not a way out of this: the relabelled row is held to
	// the unkeyed rule it claims, and a keyed stamp does not satisfy it.
	tolerated, err := store.VerifyQueryChain(ctx, conn.UID, AllowLegacyStamps())
	require.NoError(t, err)
	require.NotNil(t, tolerated.Break,
		"--allow-legacy-stamps must not launder a sealed stamp relabelled as legacy")
	require.Contains(t, tolerated.Break.Reason, "the stamp was rewritten")
	require.False(t, tolerated.LegacyStamp)
}

// TestQueryChainStampMACSealsItsVersion pins the version-in-the-MAC property
// where it actually lives — the stamp's payload — because since 0.24 no
// behavioral test can reach it any more.
//
// A version-0 row is now a break decided before any MAC is computed, so every
// reachable call to queryChainStampMAC passes queryChainStampKeyed. Deleting
// `stamp_version` from the payload would therefore leave the whole suite green
// while removing what docs/audit-chain.md and CLAUDE.md both call the lock on
// relabelling a sealed row as legacy. The behavioral half of that lock is
// covered by TestQueryChainDetectsStampVersionDowngrade, but only through
// format distinctness — a keyed MAC is never equal to a raw head MAC — which
// would still hold with the version gone. This is the assertion that would not.
func TestQueryChainStampMACSealsItsVersion(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)

	connUID := uuid.New()
	head := []byte{0x01, 0x02, 0x03, 0x04}

	keyed := store.queryChainStampMAC(connUID, queryChainStampKeyed, 3, head)

	require.NotEqual(t, keyed, store.queryChainStampMAC(connUID, queryChainStampLegacy, 3, head),
		"the claimed stamp version must be an input to the MAC: without it, relabelling a sealed "+
			"row as version 0 is one UPDATE away from getting the weaker rule applied to it")

	require.NotEqual(t, keyed, store.queryChainStampMAC(connUID, queryChainStampKeyed+1, 3, head),
		"a format this build does not know must not collide with the one it seals")

	// The other three inputs are sealed too — a stamp that moved between
	// sessions, lengths or heads would be liftable from one row to another.
	require.NotEqual(t, keyed, store.queryChainStampMAC(uuid.New(), queryChainStampKeyed, 3, head),
		"the stamp must be bound to its connection")
	require.NotEqual(t, keyed, store.queryChainStampMAC(connUID, queryChainStampKeyed, 4, head),
		"the stamp must be bound to the chain length it claims")
	require.NotEqual(t, keyed,
		store.queryChainStampMAC(connUID, queryChainStampKeyed, 3, []byte{0x01, 0x02, 0x03, 0x05}),
		"the stamp must be bound to the head it seals")
}

// TestQueryChainDowngradeToRawStampIsABreak is the test that inverted when 0.24
// dropped acceptance of the unkeyed stamp, and it is the whole point of doing
// so.
//
// The attacker's move was to replace the *whole* stamp — raw head MAC, matching
// length, version back to 0 — which accepting legacy stamps allowed by
// construction. All it cost them was landing in a counter. Now it is a break,
// so a trailing deletion followed by a downgrade is reported as one rather than
// as a number an operator has to be watching. Under --allow-legacy-stamps the
// old, weaker outcome is still reachable — that is exactly what the flag is,
// and why it goes away in 0.25.
func TestQueryChainDowngradeToRawStampIsABreak(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "downgrade-raw", 3)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	_, err := store.db.ExecContext(ctx, `DELETE FROM queries WHERE uid = ?`, queries[2].UID)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx,
		`UPDATE connections
		    SET query_chain_mac = ?, query_chain_len = ?, query_chain_stamp_version = 0
		  WHERE uid = ?`,
		queries[1].MAC, 2, conn.UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "a downgrade to the unkeyed stamp must be a break, not a count")
	require.Contains(t, result.Break.Reason, "stamp was not keyed")
	require.False(t, result.LegacyStamp, "a break is not a legacy stamp: one meaning, not two")

	all, err := store.VerifyQueryChains(ctx, nil)
	require.NoError(t, err)
	require.False(t, all.OK(), "the sweep must surface the downgrade as a break")
	require.Equal(t, int64(0), all.LegacyStamps,
		"nothing was tolerated, so nothing is counted as a legacy stamp")

	// The escape hatch buys back exactly the pre-0.24 outcome, no more: the
	// forged stamp passes the raw comparison it was built for, and is counted.
	tolerated, err := store.VerifyQueryChains(ctx, nil, AllowLegacyStamps())
	require.NoError(t, err)
	require.True(t, tolerated.OK())
	require.Equal(t, int64(1), tolerated.LegacyStamps,
		"under the flag the sweep must surface how much of the store the walk tolerated")
}

// TestQueryChainLegacyStampIsABreak is the compatibility half, inverted by 0.24:
// a store upgraded from 0.23.x now reports every session it closed before the
// upgrade as a break, because an unkeyed stamp cannot attest to anything.
//
// That is the cliff --allow-legacy-stamps exists for, and the second half here
// is what it buys: the pre-0.24 behavior, unchanged, for the one upgrade that
// has rows nothing can re-seal.
func TestQueryChainLegacyStampIsABreak(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "legacy", 3)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	// Exactly what a 0.23.x CloseConnection left behind.
	_, err := store.db.ExecContext(ctx,
		`UPDATE connections
		    SET query_chain_mac = ?, query_chain_len = ?, query_chain_stamp_version = 0
		  WHERE uid = ?`,
		queries[2].MAC, 3, conn.UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "an unkeyed head stamp must no longer verify")
	require.Contains(t, result.Break.Reason, "stamp was not keyed",
		"the break must say the tail cannot be verified, not accuse anyone of deleting it")
	require.Contains(t, result.Break.Reason, "--allow-legacy-stamps",
		"the break must name the way out for a store mid-upgrade")
	require.False(t, result.LegacyStamp)

	// With the flag, the pre-0.24 behavior: accepted, counted, never verified.
	tolerated, err := store.VerifyQueryChain(ctx, conn.UID, AllowLegacyStamps())
	require.NoError(t, err)
	require.Nil(t, tolerated.Break, "the flag must restore the pre-0.24 outcome: %v", tolerated.Break)
	require.True(t, tolerated.LegacyStamp)

	// And under the flag the legacy stamp still catches the naive attacker,
	// exactly as it did before: what it cannot catch is the one who re-stamps.
	_, err = store.db.ExecContext(ctx, `DELETE FROM queries WHERE uid = ?`, queries[2].UID)
	require.NoError(t, err)

	tolerated, err = store.VerifyQueryChain(ctx, conn.UID, AllowLegacyStamps())
	require.NoError(t, err)
	require.NotNil(t, tolerated.Break)
	require.Contains(t, tolerated.Break.Reason, "removed from the end")
}

// TestQueryChainDetectsAnEditedStampLength covers the other half of the stamp:
// query_chain_len is compared against what the surviving statements say, not
// just carried in the break message.
func TestQueryChainDetectsAnEditedStampLength(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, _ := createChainTestConnection(t, ctx, store, "stamplen", 3)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	_, err := store.db.ExecContext(ctx,
		`UPDATE connections SET query_chain_len = 99 WHERE uid = ?`, conn.UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "the recorded length must be checked, not just printed")
	require.Contains(t, result.Break.Reason, "recorded 99 statements but the surviving statements end at 3")
}

// TestQueryChainStampFromANewerBuildIsNotCalledTampering keeps the failure mode
// honest for the opposite of an upgrade: a binary rolled back under a store a
// newer one wrote. Refusing to verify is right; calling it a forgery is not.
func TestQueryChainStampFromANewerBuildIsNotCalledTampering(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, _ := createChainTestConnection(t, ctx, store, "newer", 2)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	_, err := store.db.ExecContext(ctx,
		`UPDATE connections SET query_chain_stamp_version = 7 WHERE uid = ?`, conn.UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break)
	require.Contains(t, result.Break.Reason, "written by a newer dbbat")
}

// TestQueryChainStampSurvivesRetentionTruncatingThePrefix is why the stamp
// seals the head's chain_seq and not a count of survivors. Retention reaps the
// oldest statements of a long-lived session; a stamp that moved with them would
// report a break on every deployment that sets DBB_QUERY_STORAGE_RETENTION.
func TestQueryChainStampSurvivesRetentionTruncatingThePrefix(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "stamp-truncated", 4)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	_, err := store.db.ExecContext(ctx,
		`DELETE FROM queries WHERE uid IN (?, ?)`, queries[0].UID, queries[1].UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break, "a reaped prefix must not break the stamp: %v", result.Break)
	require.True(t, result.TruncatedPrefix)
	require.Equal(t, int64(2), result.Verified)
	require.Equal(t, int64(4), result.HeadSeq)

	// And the tail is still sealed underneath the truncation.
	_, err = store.db.ExecContext(ctx, `DELETE FROM queries WHERE uid = ?`, queries[3].UID)
	require.NoError(t, err)

	result, err = store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "a trailing deletion must still be caught on a truncated chain")
}

// TestQueryChainRetentionKeepsOtherConnectionsVerifiable is the property that
// dictated per-connection chaining: retention deletes whole connections, and a
// single global chain would break on the first sweep.
func TestQueryChainRetentionKeepsOtherConnectionsVerifiable(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	oldConn, _ := createChainTestConnection(t, ctx, store, "reaped", 3)
	keptConn, _ := createChainTestConnection(t, ctx, store, "kept", 3)

	require.NoError(t, store.CloseConnection(ctx, oldConn.UID))
	require.NoError(t, store.CloseConnection(ctx, keptConn.UID))

	// Age the first connection past the cutoff and let the real sweep run.
	_, err := store.db.ExecContext(ctx,
		`UPDATE connections SET disconnected_at = NOW() - INTERVAL '30 days' WHERE uid = ?`, oldConn.UID)
	require.NoError(t, err)

	swept, err := store.CleanupOldQueryRows(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(1), swept.Connections)

	result, err := store.VerifyQueryChains(ctx, nil)
	require.NoError(t, err)
	require.True(t, result.OK(), "the surviving connection must still verify: %v", result.Break)
	require.Equal(t, int64(1), result.Connections)
	require.Equal(t, int64(3), result.Verified)
	require.Equal(t, int64(0), result.Truncated)
}

// TestQueryChainRetentionPrefixIsNotABreak covers the other half of the
// retention story: an open, long-lived session loses its oldest statements to
// the second sweep. That is expected housekeeping, not tampering, and must not
// be reported as a break.
func TestQueryChainRetentionPrefixIsNotABreak(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "prefix", 4)

	// Age the two oldest statements; the connection stays open, so only the
	// query-level sweep touches them.
	_, err := store.db.ExecContext(ctx,
		`UPDATE queries SET executed_at = NOW() - INTERVAL '30 days' WHERE uid IN (?, ?)`,
		queries[0].UID, queries[1].UID)
	require.NoError(t, err)

	swept, err := store.CleanupOldQueryRows(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(2), swept.Queries)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break, "a retention-truncated prefix is not a break: %v", result.Break)
	require.True(t, result.TruncatedPrefix)
	require.Equal(t, int64(2), result.Verified)

	all, err := store.VerifyQueryChains(ctx, nil)
	require.NoError(t, err)
	require.True(t, all.OK())
	require.Equal(t, int64(1), all.Truncated)
}

// TestQueryChainDetectsWipedSession covers the case the enumeration has to
// reach out for, the query-chain counterpart of TestRowChainDetectsWipedCapture:
// every statement of a session deleted, so `queries` has nothing left to
// enumerate from and only the stamp remembers the session ran anything.
func TestQueryChainDetectsWipedSession(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, _ := createChainTestConnection(t, ctx, store, "wiped", 3)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	_, err := store.db.ExecContext(ctx, `DELETE FROM queries WHERE connection_id = ?`, conn.UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChains(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "a wiped session must not disappear silently")
	require.Equal(t, int64(1), result.Connections, "the stamp alone must put the session back in the walk")
	require.Contains(t, result.Break.Reason, "recorded 3 statements and none survive")
	require.NotNil(t, result.Break.ConnectionUID)
	require.Equal(t, conn.UID, *result.Break.ConnectionUID)
}

// TestQueryChainDetectsWipedOpenSession is the same wipe against a session that
// is still open, which the periodic sweep has stamped. Deleting the statements
// of a live session used to be doubly invisible: nothing enumerated it, and the
// zero-survivor case was excused outright as retention housekeeping.
func TestQueryChainDetectsWipedOpenSession(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, _ := createChainTestConnection(t, ctx, store, "wiped-open", 3)

	stamped, err := store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), stamped)

	_, err = store.db.ExecContext(ctx, `DELETE FROM queries WHERE connection_id = ?`, conn.UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "an open session emptied of its statements is a break too")
	require.Contains(t, result.Break.Reason, "none survive")
}

// TestQueryChainClearedStampOnAClosedSessionIsABreak covers the cheapest attack
// on the stamp: not forging it, deleting it. The tail of the session goes, and
// then the one thing that would have noticed goes too — a single UPDATE, no key
// needed. The walk used to read that as "never stamped" and verify clean.
func TestQueryChainClearedStampOnAClosedSessionIsABreak(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "cleared-stamp", 3)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	_, err := store.db.ExecContext(ctx, `DELETE FROM queries WHERE uid = ?`, queries[2].UID)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx,
		`UPDATE connections SET query_chain_mac = NULL, query_chain_len = 0 WHERE uid = ?`, conn.UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "clearing the stamp must not be cheaper than defeating it")
	require.Contains(t, result.Break.Reason, "carries no head stamp")
	require.NotNil(t, result.Break.ConnectionUID)
	require.Equal(t, conn.UID, *result.Break.ConnectionUID)

	all, err := store.VerifyQueryChains(ctx, nil)
	require.NoError(t, err)
	require.False(t, all.OK(), "the sweep must report it too, not just a scoped walk")
	require.Equal(t, int64(1), all.Connections)
}

// TestQueryChainClearedStampMACKeepsItsLength is the sloppier version of the
// same edit, and it is caught on an *open* session too: all three writers set
// the MAC, the length and the version in one statement, so a length that
// outlived its MAC is a row somebody wrote to.
func TestQueryChainClearedStampMACKeepsItsLength(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, _ := createChainTestConnection(t, ctx, store, "cleared-mac", 3)

	stamped, err := store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), stamped)

	_, err = store.db.ExecContext(ctx,
		`UPDATE connections SET query_chain_mac = NULL WHERE uid = ?`, conn.UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "a stamped length without a stamp is an edited row")
	require.Contains(t, result.Break.Reason, "records a chain of 3 statements")
	require.Equal(t, int64(3), result.Break.ChainSeq)
}

// TestQueryChainClosedSilentSessionIsNotABreak is the first no-cry-wolf half of
// the rule above: a session that logged nothing is stamped NULL on purpose, and
// a scoped walk enumerates a connection whatever it carries, so the judgment
// cannot lean on the enumeration to keep it out.
func TestQueryChainClosedSilentSessionIsNotABreak(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "chain_closed_silent")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	require.NoError(t, err)
	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	row, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, row.DisconnectedAt)
	require.Nil(t, row.QueryChainMAC, "a session that logged nothing has no head to seal")

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break, "a silent session is not tampering: %v", result.Break)
}

// TestQueryChainOpenSessionWithoutAStampIsNotABreak is the second: a live
// session younger than one reclaim tick has no stamp through nobody's fault,
// and the sweep is what will give it one.
func TestQueryChainOpenSessionWithoutAStampIsNotABreak(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, _ := createChainTestConnection(t, ctx, store, "unswept-open", 3)

	row, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, row.QueryChainMAC, "no sweep has run yet")

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break, "an unswept live session is not tampering: %v", result.Break)
	require.Equal(t, int64(3), result.Verified)
}

// TestQueryChainRetentionNeverClearsTheStamp is the third: retention only ever
// deletes rows, so it cannot produce the unstamped-but-chained state above. A
// closed session it emptied keeps its stamp and stays checkEmptiedChain's
// business — which is what lets the missing-stamp rule key off surviving
// statements without re-deriving the retention argument.
func TestQueryChainRetentionNeverClearsTheStamp(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	store.queryRetention = 24 * time.Hour

	conn, _ := createChainTestConnection(t, ctx, store, "retention-keeps-stamp", 3)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	_, err := store.db.ExecContext(ctx,
		`UPDATE connections SET connected_at = NOW() - INTERVAL '30 days' WHERE uid = ?`, conn.UID)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx,
		`UPDATE queries SET executed_at = NOW() - INTERVAL '30 days' WHERE connection_id = ?`, conn.UID)
	require.NoError(t, err)

	swept, err := store.CleanupOldQueryRows(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(3), swept.Queries)

	row, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, row.QueryChainMAC, "the sweep deletes statements, never a stamp")
	require.Equal(t, int64(3), row.QueryChainLen)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break, "retention emptying a session is not tampering: %v", result.Break)
	require.True(t, result.TruncatedPrefix)
}

// TestQueryChainSilentSessionIsNotWalked pins the other side of the widened
// enumeration: it reaches sessions that carry a stamp, not every connection ever
// opened. A session that logged nothing has no stamp and is not evidence of
// anything.
func TestQueryChainSilentSessionIsNotWalked(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "chain_silent_walk")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	require.NoError(t, err)
	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	result, err := store.VerifyQueryChains(ctx, nil)
	require.NoError(t, err)
	require.True(t, result.OK(), "a session that logged nothing is not a break: %v", result.Break)
	require.Equal(t, int64(0), result.Connections, "there is nothing to walk")
}

// TestQueryChainRetentionEmptyingASessionIsNotABreak is the no-cry-wolf half of
// the wiped-session check. A pooled session that ran its statements a month ago
// and only closed just now survives the connection sweep (its disconnected_at is
// recent) while the query sweep takes every statement it ran. That is
// housekeeping, and the walk must say so — not report the deployment's whole
// connection pool as tampering.
func TestQueryChainRetentionEmptyingASessionIsNotABreak(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	// What DBB_QUERY_STORAGE_RETENTION is set to, which is what verification
	// judges an emptied session against.
	store.queryRetention = 24 * time.Hour

	conn, _ := createChainTestConnection(t, ctx, store, "retention-emptied", 3)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	_, err := store.db.ExecContext(ctx,
		`UPDATE connections SET connected_at = NOW() - INTERVAL '30 days' WHERE uid = ?`, conn.UID)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx,
		`UPDATE queries SET executed_at = NOW() - INTERVAL '30 days' WHERE connection_id = ?`, conn.UID)
	require.NoError(t, err)

	swept, err := store.CleanupOldQueryRows(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(0), swept.Connections, "the session closed too recently to be reaped whole")
	require.Equal(t, int64(3), swept.Queries)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break, "retention emptying a session is not tampering: %v", result.Break)
	require.True(t, result.TruncatedPrefix, "a fully reaped chain is the extreme of a truncated prefix")

	all, err := store.VerifyQueryChains(ctx, nil)
	require.NoError(t, err)
	require.True(t, all.OK(), "%v", all.Break)
	require.Equal(t, int64(1), all.Connections)
	require.Equal(t, int64(1), all.Truncated, "the session is counted, not silently skipped")
}

// TestQueryChainWipeInsideTheRetentionWindowIsABreak is what keeps the excuse
// above from becoming a blanket one: retention only accounts for statements it
// could have reaped, and it cannot have reaped any statement of a session that
// began inside the retention window.
func TestQueryChainWipeInsideTheRetentionWindowIsABreak(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	store.queryRetention = 30 * 24 * time.Hour

	conn, _ := createChainTestConnection(t, ctx, store, "retention-window", 2)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	_, err := store.db.ExecContext(ctx, `DELETE FROM queries WHERE connection_id = ?`, conn.UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "a session too young for the sweep cannot blame it")
	require.Contains(t, result.Break.Reason, "retention window")
	require.False(t, result.TruncatedPrefix)
}

// TestQueryChainWipedLegacySessionStaysALegacyCaveat keeps the pre-0.24 stamp
// out of the new check. Without the flag such a session is already a break for
// its own reason. With it, an emptied one is tolerated like every other legacy
// stamp: the unkeyed value attests to nothing either way, so a session retention
// emptied and one somebody emptied are the same bytes, and no key separates
// them.
func TestQueryChainWipedLegacySessionStaysALegacyCaveat(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "wiped-legacy", 3)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	// Exactly what a 0.23.x CloseConnection left behind, then wiped.
	_, err := store.db.ExecContext(ctx,
		`UPDATE connections
		    SET query_chain_mac = ?, query_chain_len = ?, query_chain_stamp_version = 0
		  WHERE uid = ?`,
		queries[2].MAC, 3, conn.UID)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx, `DELETE FROM queries WHERE connection_id = ?`, conn.UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "an unkeyed stamp is a break whatever survived")
	require.Contains(t, result.Break.Reason, "stamp was not keyed")

	tolerated, err := store.VerifyQueryChain(ctx, conn.UID, AllowLegacyStamps())
	require.NoError(t, err)
	require.Nil(t, tolerated.Break, "the flag must not turn the legacy caveat into a new break: %v", tolerated.Break)
	require.True(t, tolerated.LegacyStamp)
}

// TestQueryChainConcurrentAppends checks the per-connection serialization the
// advisory lock provides, with several connections writing at once.
func TestQueryChainConcurrentAppends(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	const (
		connections = 3
		each        = 6
	)

	user, database := createTestUserAndDatabase(t, ctx, store, "chain_concurrent")

	conns := make([]*Connection, connections)
	for i := range conns {
		conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.2")
		require.NoError(t, err)

		conns[i] = conn
	}

	var wg sync.WaitGroup

	errs := make(chan error, connections*each)

	for _, conn := range conns {
		for i := range each {
			wg.Add(1)

			go func() {
				defer wg.Done()

				_, err := store.CreateQuery(ctx, &Query{
					ConnectionID: conn.UID,
					SQLText:      fmt.Sprintf("SELECT %d", i),
				})
				if err != nil {
					errs <- err
				}
			}()
		}
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err, "concurrent CreateQuery()")
	}

	result, err := store.VerifyQueryChains(ctx, nil)
	require.NoError(t, err)
	require.True(t, result.OK(), "concurrent statements must form valid per-connection chains: %v", result.Break)
	require.Equal(t, int64(connections), result.Connections)
	require.Equal(t, int64(connections*each), result.Verified)
}

// TestQueryChainOutcomeColumnsAreNotSealed documents the deliberate limit: the
// columns written after the insert are not covered, because covering them would
// require recomputing a MAC every successor already points at.
func TestQueryChainOutcomeColumnsAreNotSealed(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "outcome", 2)

	duration := 12.5
	rows := int64(7)

	require.NoError(t, store.UpdateQueryCompletion(ctx, queries[0].UID, &duration, &rows, nil, false, false))

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break, "recording an outcome must not break the chain")
}

func TestVerifyQueryChainsScopedToOneConnection(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	first, _ := createChainTestConnection(t, ctx, store, "scope_a", 2)
	second, queries := createChainTestConnection(t, ctx, store, "scope_b", 2)

	_, err := store.db.ExecContext(ctx,
		`UPDATE queries SET sql_text = 'tampered' WHERE uid = ?`, queries[0].UID)
	require.NoError(t, err)

	clean, err := store.VerifyQueryChains(ctx, &first.UID)
	require.NoError(t, err)
	require.True(t, clean.OK(), "the untouched connection must verify on its own")

	broken, err := store.VerifyQueryChains(ctx, &second.UID)
	require.NoError(t, err)
	require.False(t, broken.OK())
	require.NotNil(t, broken.Break.ConnectionUID)
	require.Equal(t, second.UID, *broken.Break.ConnectionUID)
	require.Contains(t, broken.Break.String(), second.UID.String())
}

func TestChainBreakStringMentionsTheRow(t *testing.T) {
	t.Parallel()

	uid := uuid.MustParse("018f0000-0000-7000-8000-00000000abcd")
	brk := ChainBreak{UID: uid, ChainSeq: 4, Reason: "boom"}

	require.Contains(t, brk.String(), uid.String())
	require.Contains(t, brk.String(), "boom")
	require.Contains(t, brk.String(), "4")
}

func TestChainKeyIsNotTheMasterKey(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)

	require.True(t, store.ChainEnabled())
	require.NotEqual(t, testChainMasterKey, store.chainKey,
		"the chain key must be derived, never the master key itself")
}

// TestAuditChainSurvivesAPeerAppend is the multi-replica case: two processes
// share the store, so one process's cached head goes stale the moment the other
// appends. The append must retry against a re-read head instead of losing the
// event to a unique-index collision.
func TestAuditChainSurvivesAPeerAppend(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	peer := &Store{db: store.db, queryChains: newQueryChains(), chainKey: store.chainKey}

	// Both processes warm their caches on the same head.
	require.NoError(t, store.LogAuditEvent(ctx, &AuditEvent{EventType: "first"}))
	require.NoError(t, peer.LogAuditEvent(ctx, &AuditEvent{EventType: "peer"}))

	// store still believes the head is 1; the peer moved it to 2.
	require.NoError(t, store.LogAuditEvent(ctx, &AuditEvent{EventType: "third"}))

	rows := readChainedAuditRows(t, ctx, store)
	require.Len(t, rows, 3, "no event may be lost to a stale head")

	for i, row := range rows {
		require.Equal(t, int64(i+1), *row.ChainSeq)
	}

	result, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	require.True(t, result.OK(), "the chain must stay valid across replicas: %v", result.Break)
}

// TestQueryChainSurvivesAPeerAppend is the same case on a connection, which a
// failover can move between replicas mid-session.
func TestQueryChainSurvivesAPeerAppend(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, _ := createChainTestConnection(t, ctx, store, "peer", 1)

	peer := &Store{db: store.db, queryChains: newQueryChains(), chainKey: store.chainKey}

	_, err := peer.CreateQuery(ctx, &Query{ConnectionID: conn.UID, SQLText: "SELECT 'peer'"})
	require.NoError(t, err)

	_, err = store.CreateQuery(ctx, &Query{ConnectionID: conn.UID, SQLText: "SELECT 'third'"})
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break, "the chain must stay valid across replicas")
	require.Equal(t, int64(3), result.Verified)
}

// --- Result row chains --------------------------------------------------
//
// One chain per capture, positioned by row_number, sealed at the flush
// barrier. See docs/audit-chain.md.

// captureQuery creates a connection with one statement on it, which is the
// parent every captured row hangs from.
func captureQuery(t *testing.T, ctx context.Context, store *Store, suffix string) *Query {
	t.Helper()

	_, queries := createChainTestConnection(t, ctx, store, suffix, 1)

	return &queries[0]
}

// storeChainedRows writes captured rows through the real store path, with the
// given row numbers.
func storeChainedRows(t *testing.T, ctx context.Context, store *Store, queryUID uuid.UUID, numbers ...int) {
	t.Helper()

	rows := make([]PendingQueryRow, 0, len(numbers))

	for _, n := range numbers {
		data := json.RawMessage(fmt.Sprintf(`{"n":%d,"b":"x","a":1.0}`, n))
		rows = append(rows, PendingQueryRow{
			QueryID:      queryUID,
			RowNumber:    n,
			RowData:      data,
			RowSizeBytes: int64(len(data)),
		})
	}

	require.NoError(t, store.StoreQueryRows(ctx, rows), "StoreQueryRows()")
}

func readChainedRows(t *testing.T, ctx context.Context, store *Store, queryUID uuid.UUID) []QueryRowModel {
	t.Helper()

	var rows []QueryRowModel

	err := store.db.NewSelect().
		Model(&rows).
		Where("query_id = ?", queryUID).
		Order("row_number ASC").
		Scan(ctx)
	require.NoError(t, err, "reading captured rows")

	return rows
}

func TestRowChainVerifiesClean(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	query := captureQuery(t, ctx, store, "rows-clean")
	storeChainedRows(t, ctx, store, query.UID, 1, 2, 3)
	require.NoError(t, store.SealQueryRowChain(ctx, query.UID))

	rows := readChainedRows(t, ctx, store, query.UID)
	require.Len(t, rows, 3)

	for _, row := range rows {
		require.NotEmpty(t, row.MAC)
		require.NotEmpty(t, row.PrevMAC)
	}

	one, err := store.VerifyRowChain(ctx, query.UID)
	require.NoError(t, err)
	require.Nil(t, one.Break, "the capture should verify")
	require.Equal(t, int64(3), one.Verified)
	require.Equal(t, rows[2].MAC, one.HeadMAC)

	all, err := store.VerifyRowChains(ctx, nil)
	require.NoError(t, err)
	require.True(t, all.OK())
	require.Equal(t, int64(1), all.Captures)
	require.Equal(t, int64(3), all.Verified)
	require.Equal(t, int64(0), all.Unchained)
}

func TestRowChainStampsHeadOnSeal(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	query := captureQuery(t, ctx, store, "rows-stamp")
	storeChainedRows(t, ctx, store, query.UID, 1, 2, 3)

	// Not stamped until the capture reaches its barrier.
	unsealed, err := store.GetQuery(ctx, query.UID)
	require.NoError(t, err)
	require.Nil(t, unsealed.RowChainMAC)

	require.NoError(t, store.SealQueryRowChain(ctx, query.UID))

	sealed, err := store.GetQuery(ctx, query.UID)
	require.NoError(t, err)

	rows := readChainedRows(t, ctx, store, query.UID)
	require.Equal(t, int64(3), sealed.RowChainLen)
	require.Equal(t, store.rowChainStampMAC(query.UID, 3, rows[2].MAC), sealed.RowChainMAC,
		"the stamp must seal the capture head")

	// The security property: the stamp is not a copy of a value that is
	// readable from query_rows, so it cannot be recomputed without the key.
	require.NotEqual(t, rows[2].MAC, sealed.RowChainMAC,
		"a stamp that echoes the head MAC could be rewritten by anyone who can read the rows")
}

func TestRowChainDetectsModifiedRow(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	query := captureQuery(t, ctx, store, "rows-modified")
	storeChainedRows(t, ctx, store, query.UID, 1, 2, 3)
	require.NoError(t, store.SealQueryRowChain(ctx, query.UID))

	rows := readChainedRows(t, ctx, store, query.UID)

	_, err := store.db.ExecContext(ctx,
		`UPDATE query_rows SET row_data = '{"n":0,"b":"innocent","a":1.0}'::jsonb WHERE uid = ?`, rows[1].UID)
	require.NoError(t, err)

	result, err := store.VerifyRowChain(ctx, query.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break)
	require.Equal(t, rows[1].UID, result.Break.UID)
	require.Equal(t, query.UID, *result.Break.QueryUID)
	require.Contains(t, result.Break.Reason, "modified")
}

func TestRowChainDetectsDeletedRow(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	query := captureQuery(t, ctx, store, "rows-deleted")
	storeChainedRows(t, ctx, store, query.UID, 1, 2, 3, 4)
	require.NoError(t, store.SealQueryRowChain(ctx, query.UID))

	rows := readChainedRows(t, ctx, store, query.UID)

	_, err := store.db.ExecContext(ctx, `DELETE FROM query_rows WHERE uid = ?`, rows[1].UID)
	require.NoError(t, err)

	// A gap in row_number is not what catches this — the prev_mac linkage is.
	result, err := store.VerifyRowChain(ctx, query.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break)
	require.Equal(t, rows[2].UID, result.Break.UID)
	require.Contains(t, result.Break.Reason, "prev_mac")
}

func TestRowChainDetectsDeletedFirstRow(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	query := captureQuery(t, ctx, store, "rows-first")
	storeChainedRows(t, ctx, store, query.UID, 1, 2, 3)
	require.NoError(t, store.SealQueryRowChain(ctx, query.UID))

	rows := readChainedRows(t, ctx, store, query.UID)

	_, err := store.db.ExecContext(ctx, `DELETE FROM query_rows WHERE uid = ?`, rows[0].UID)
	require.NoError(t, err)

	// Retention never removes individual captured rows, so unlike a session's
	// statements a capture missing its start is tampering, not housekeeping.
	result, err := store.VerifyRowChain(ctx, query.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break)
	require.Contains(t, result.Break.Reason, "removed from the start")
}

// TestRowChainDetectsTrailingDeletion is what the row_chain_mac stamp exists
// for: without it the surviving prefix would still verify end to end.
func TestRowChainDetectsTrailingDeletion(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	query := captureQuery(t, ctx, store, "rows-trailing")
	storeChainedRows(t, ctx, store, query.UID, 1, 2, 3)
	require.NoError(t, store.SealQueryRowChain(ctx, query.UID))

	rows := readChainedRows(t, ctx, store, query.UID)

	_, err := store.db.ExecContext(ctx, `DELETE FROM query_rows WHERE uid = ?`, rows[2].UID)
	require.NoError(t, err)

	result, err := store.VerifyRowChain(ctx, query.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "deleting the last captured row must be detected")
	require.Contains(t, result.Break.Reason, "removed from the end")
}

// TestRowChainDetectsRestampedTrailingDeletion is the case that actually
// matters. An attacker with write access to the store does not stop at deleting
// the last rows — they fix the stamp up afterwards. That works against a stamp
// that merely echoes the head MAC, because the head is readable from
// query_rows. It must not work here: the stamp is keyed, so correcting it needs
// the chain key.
func TestRowChainDetectsRestampedTrailingDeletion(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	query := captureQuery(t, ctx, store, "rows-restamped")
	storeChainedRows(t, ctx, store, query.UID, 1, 2, 3)
	require.NoError(t, store.SealQueryRowChain(ctx, query.UID))

	rows := readChainedRows(t, ctx, store, query.UID)

	// Delete the last row and re-stamp the query with everything the attacker
	// can see: the new last row's MAC and the new count.
	_, err := store.db.ExecContext(ctx, `DELETE FROM query_rows WHERE uid = ?`, rows[2].UID)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx,
		`UPDATE queries SET row_chain_mac = ?, row_chain_len = ? WHERE uid = ?`,
		rows[1].MAC, 2, query.UID)
	require.NoError(t, err)

	result, err := store.VerifyRowChain(ctx, query.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "a re-stamped truncation must still be detected")
	require.Contains(t, result.Break.Reason, "the stamp was rewritten")
}

// TestRowChainDetectsAnEditedStampLength covers the other half of the stamp:
// row_chain_len is compared against what survives, not just carried in the
// break message.
func TestRowChainDetectsAnEditedStampLength(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	query := captureQuery(t, ctx, store, "rows-stamplen")
	storeChainedRows(t, ctx, store, query.UID, 1, 2, 3)
	require.NoError(t, store.SealQueryRowChain(ctx, query.UID))

	_, err := store.db.ExecContext(ctx,
		`UPDATE queries SET row_chain_len = 99 WHERE uid = ?`, query.UID)
	require.NoError(t, err)

	result, err := store.VerifyRowChain(ctx, query.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "the recorded length must be checked, not just printed")
	require.Contains(t, result.Break.Reason, "recorded 99 captured rows but 3 survive")
}

// TestRowChainDetectsWipedCapture covers the case the enumeration has to reach
// out for: every row of a capture deleted, so query_rows has nothing left to
// walk and only the stamp remembers the capture existed.
func TestRowChainDetectsWipedCapture(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	query := captureQuery(t, ctx, store, "rows-wiped")
	storeChainedRows(t, ctx, store, query.UID, 1, 2, 3)
	require.NoError(t, store.SealQueryRowChain(ctx, query.UID))

	_, err := store.db.ExecContext(ctx, `DELETE FROM query_rows WHERE query_id = ?`, query.UID)
	require.NoError(t, err)

	result, err := store.VerifyRowChains(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "a wiped capture must not disappear silently")
	require.Equal(t, int64(1), result.Captures)
	require.Contains(t, result.Break.Reason, "removed from the end")
}

// TestRowChainGapsAreNotABreak pins the reason row_number is an ordering and
// not a dense sequence: the batched writer drops rows when its queue is full,
// and an unencodable row is skipped outright.
func TestRowChainGapsAreNotABreak(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	query := captureQuery(t, ctx, store, "rows-gaps")
	storeChainedRows(t, ctx, store, query.UID, 1, 4, 9)
	require.NoError(t, store.SealQueryRowChain(ctx, query.UID))

	result, err := store.VerifyRowChain(ctx, query.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break, "a capture with dropped rows still verifies: %v", result.Break)
	require.Equal(t, int64(3), result.Verified)

	sealed, err := store.GetQuery(ctx, query.UID)
	require.NoError(t, err)
	require.Equal(t, int64(3), sealed.RowChainLen, "row_chain_len counts rows, not row numbers")
}

// TestRowChainBatchSpansSeveralQueries is the property that shaped the write
// path: one INSERT carries rows for several captures, so the batch has to be
// split per query before the MACs are computed — and each capture must come out
// with its own independent chain.
func TestRowChainBatchSpansSeveralQueries(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	_, queries := createChainTestConnection(t, ctx, store, "rows-batch", 2)
	first, second := queries[0], queries[1]

	data := func(n int) json.RawMessage { return json.RawMessage(fmt.Sprintf(`{"n":%d}`, n)) }

	// Interleaved, the way a shared writer drains two concurrent sessions.
	mixed := []PendingQueryRow{
		{QueryID: first.UID, RowNumber: 1, RowData: data(1), RowSizeBytes: 8},
		{QueryID: second.UID, RowNumber: 1, RowData: data(2), RowSizeBytes: 8},
		{QueryID: first.UID, RowNumber: 2, RowData: data(3), RowSizeBytes: 8},
		{QueryID: second.UID, RowNumber: 2, RowData: data(4), RowSizeBytes: 8},
	}
	require.NoError(t, store.StoreQueryRows(ctx, mixed))

	// A second batch continues both chains from the heads the first one left.
	more := []PendingQueryRow{
		{QueryID: second.UID, RowNumber: 3, RowData: data(5), RowSizeBytes: 8},
		{QueryID: first.UID, RowNumber: 3, RowData: data(6), RowSizeBytes: 8},
	}
	require.NoError(t, store.StoreQueryRows(ctx, more))

	require.NoError(t, store.SealQueryRowChain(ctx, first.UID))
	require.NoError(t, store.SealQueryRowChain(ctx, second.UID))

	firstRows := readChainedRows(t, ctx, store, first.UID)
	secondRows := readChainedRows(t, ctx, store, second.UID)
	require.Len(t, firstRows, 3)
	require.Len(t, secondRows, 3)

	// Independent chains: neither capture's first row links to the other's.
	require.NotEqual(t, firstRows[0].PrevMAC, secondRows[0].PrevMAC)

	all, err := store.VerifyRowChains(ctx, nil)
	require.NoError(t, err)
	require.True(t, all.OK(), "both captures must verify: %v", all.Break)
	require.Equal(t, int64(2), all.Captures)
	require.Equal(t, int64(6), all.Verified)
}

// TestRowChainRetentionCascadeIsNotABreak is the property that dictated a chain
// per query: retention deletes whole queries and query_rows cascades, so a
// reaped capture takes its chain with it instead of severing a shared one.
func TestRowChainRetentionCascadeIsNotABreak(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	oldConn, oldQueries := createChainTestConnection(t, ctx, store, "rows-reaped", 1)
	keptConn, keptQueries := createChainTestConnection(t, ctx, store, "rows-kept", 1)

	storeChainedRows(t, ctx, store, oldQueries[0].UID, 1, 2, 3)
	storeChainedRows(t, ctx, store, keptQueries[0].UID, 1, 2, 3)
	require.NoError(t, store.SealQueryRowChain(ctx, oldQueries[0].UID))
	require.NoError(t, store.SealQueryRowChain(ctx, keptQueries[0].UID))

	require.NoError(t, store.CloseConnection(ctx, oldConn.UID))
	require.NoError(t, store.CloseConnection(ctx, keptConn.UID))

	_, err := store.db.ExecContext(ctx,
		`UPDATE connections SET disconnected_at = NOW() - INTERVAL '30 days' WHERE uid = ?`, oldConn.UID)
	require.NoError(t, err)

	swept, err := store.CleanupOldQueryRows(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(1), swept.Connections)

	result, err := store.VerifyRowChains(ctx, nil)
	require.NoError(t, err)
	require.True(t, result.OK(), "retention must not look like tampering: %v", result.Break)
	require.Equal(t, int64(1), result.Captures)
	require.Equal(t, int64(3), result.Verified)
}

// TestRowChainOutcomeFlagsAreNotSealed states the decision the row chain
// inherited from the query chain: results_truncated and results_dropped are
// written after the fact by UpdateQueryCompletion, so sealing them would mean
// re-MACing a record every successor already points at.
func TestRowChainOutcomeFlagsAreNotSealed(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	query := captureQuery(t, ctx, store, "rows-outcome")
	storeChainedRows(t, ctx, store, query.UID, 1, 2)
	require.NoError(t, store.SealQueryRowChain(ctx, query.UID))

	require.NoError(t, store.UpdateQueryCompletion(ctx, query.UID, nil, nil, nil, true, true))

	result, err := store.VerifyRowChain(ctx, query.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break, "completing a query must not break its row chain: %v", result.Break)
}

func TestVerifyRowChainsScopedToOneConnection(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	connA, queriesA := createChainTestConnection(t, ctx, store, "rows-scope-a", 1)
	_, queriesB := createChainTestConnection(t, ctx, store, "rows-scope-b", 1)

	storeChainedRows(t, ctx, store, queriesA[0].UID, 1, 2)
	storeChainedRows(t, ctx, store, queriesB[0].UID, 1, 2, 3)

	scoped, err := store.VerifyRowChains(ctx, &connA.UID)
	require.NoError(t, err)
	require.True(t, scoped.OK())
	require.Equal(t, int64(1), scoped.Captures)
	require.Equal(t, int64(2), scoped.Verified)
}

func TestRowChainDisabledWithoutKey(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	query := captureQuery(t, ctx, store, "rows-unkeyed")

	unkeyed := &Store{db: store.db, queryChains: newQueryChains(), rowChains: newQueryChains()}

	storeChainedRows(t, ctx, unkeyed, query.UID, 1, 2)
	require.NoError(t, unkeyed.SealQueryRowChain(ctx, query.UID))

	for _, row := range readChainedRows(t, ctx, store, query.UID) {
		require.Empty(t, row.MAC, "an unkeyed store must not chain captured rows")
	}

	_, err := unkeyed.VerifyRowChains(ctx, nil)
	require.ErrorIs(t, err, ErrChainKeyUnavailable)

	// The rows are still there, they are just unverifiable — and counted as
	// such rather than silently ignored.
	result, err := store.VerifyRowChains(ctx, nil)
	require.NoError(t, err)
	require.True(t, result.OK())
	require.Equal(t, int64(2), result.Unchained)
	require.Equal(t, int64(0), result.Captures)
}

// TestRowChainResumesFromTheStoredHead exercises the cold-cache path: a store
// that never wrote to a capture reads its head back out of query_rows and
// continues the chain from there. It is what a restarted process — or a peer
// replica — lands on.
func TestRowChainResumesFromTheStoredHead(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	query := captureQuery(t, ctx, store, "rows-resume")
	storeChainedRows(t, ctx, store, query.UID, 1, 2)

	peer := &Store{
		db:          store.db,
		queryChains: newQueryChains(),
		rowChains:   newQueryChains(),
		chainKey:    store.chainKey,
	}

	storeChainedRows(t, ctx, peer, query.UID, 3, 4)
	require.NoError(t, peer.SealQueryRowChain(ctx, query.UID))

	result, err := store.VerifyRowChain(ctx, query.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break, "a cold store must extend the chain, not restart it: %v", result.Break)
	require.Equal(t, int64(4), result.Verified)

	sealed, err := store.GetQuery(ctx, query.UID)
	require.NoError(t, err)
	require.Equal(t, int64(4), sealed.RowChainLen, "the stamp must count the rows the cold store did not write")
}

// createOrphanedChainConnection opens a chained connection under a run that
// never registered — the crash case — and writes it `statements` statements.
// The store's own identity is left alone, so the reconcile sees the rows as
// somebody else's dead run.
func createOrphanedChainConnection(
	t *testing.T, ctx context.Context, store *Store, suffix string, statements int,
) (*Connection, []Query) {
	t.Helper()

	user, database := createTestUserAndDatabase(t, ctx, store, "orphan_chain_"+suffix)

	var conn *Connection

	asRun(t, store, "dead-instance-"+suffix, "dead-run-"+suffix, func() {
		var err error

		conn, err = store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
		require.NoError(t, err, "CreateConnection()")
	})

	queries := make([]Query, 0, statements)

	for i := range statements {
		created, err := store.CreateQuery(ctx, &Query{
			ConnectionID: conn.UID,
			SQLText:      fmt.Sprintf("SELECT %d", i),
			Parameters:   &QueryParameters{Values: []string{fmt.Sprintf("v%d", i)}},
		})
		require.NoError(t, err, "CreateQuery()")

		queries = append(queries, *created)
	}

	return conn, queries
}

// TestQueryChainStampsHeadOnOrphanReconcile is the crash half of what
// TestQueryChainStampsHeadOnClose covers for a clean teardown: a session whose
// process died must still end up with its tail sealed, using only what the
// database can recover.
func TestQueryChainStampsHeadOnOrphanReconcile(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createOrphanedChainConnection(t, ctx, store, "reconcile", 3)

	store.SetInstanceID("live-instance")
	store.SetRunID("live-run")
	require.NoError(t, store.RegisterInstance(ctx))

	reclaimed, err := store.ReclaimDeadInstanceConnections(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), reclaimed, "the dead run's connection must be reconciled")

	closed, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, closed.DisconnectedAt, "the orphan should no longer look open")
	require.Equal(t, int64(3), closed.QueryChainLen)
	require.Equal(t, queryChainStampKeyed, closed.QueryChainStampVersion,
		"the reconcile must seal, not copy — it is the same evidence a clean close produces")
	require.Equal(t, store.queryChainStampMAC(conn.UID, queryChainStampKeyed, 3, queries[2].MAC),
		closed.QueryChainMAC, "the reconcile must seal the head the surviving statements compute")

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break, "stamping must not itself break the chain")

	// The point of the stamp: a trailing deletion after the reconcile is now
	// caught, exactly as it is for a cleanly closed session.
	_, err = store.db.ExecContext(ctx, `DELETE FROM queries WHERE uid = ?`, queries[2].UID)
	require.NoError(t, err)

	result, err = store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "deleting the last statement of a reconciled session must be detected")
	require.Contains(t, result.Break.Reason, "removed from the end")
}

// TestQueryChainOrphanReconcileLeavesSilentSessionUnstamped pins the NULL case:
// a session that logged nothing has no head, and a NULL stamp must never be
// read as a break.
func TestQueryChainOrphanReconcileLeavesSilentSessionUnstamped(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, _ := createOrphanedChainConnection(t, ctx, store, "silent", 0)

	store.SetInstanceID("live-instance")
	store.SetRunID("live-run")
	require.NoError(t, store.RegisterInstance(ctx))

	reclaimed, err := store.ReclaimDeadInstanceConnections(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), reclaimed)

	closed, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, closed.DisconnectedAt)
	require.Nil(t, closed.QueryChainMAC, "a session that logged nothing has no head to seal")
	require.Equal(t, int64(0), closed.QueryChainLen)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break)
}

// TestQueryChainOrphanReconcileDoesNotRestampAClosedSession guards the scope:
// the reconcile only ever touches rows whose disconnected_at is NULL, so a head
// CloseConnection already stamped is never recomputed from whatever survives
// today.
func TestQueryChainOrphanReconcileDoesNotRestampAClosedSession(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createOrphanedChainConnection(t, ctx, store, "closed", 3)
	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	// A trailing deletion between the clean close and the reconcile.
	_, err := store.db.ExecContext(ctx, `DELETE FROM queries WHERE uid = ?`, queries[2].UID)
	require.NoError(t, err)

	store.SetInstanceID("live-instance")
	store.SetRunID("live-run")
	require.NoError(t, store.RegisterInstance(ctx))

	reclaimed, err := store.ReclaimDeadInstanceConnections(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), reclaimed, "an already-closed connection is out of the reconcile's scope")

	closed, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.Equal(t, store.queryChainStampMAC(conn.UID, queryChainStampKeyed, 3, queries[2].MAC),
		closed.QueryChainMAC, "the original stamp must survive the reconcile")

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "the reconcile must not launder a trailing deletion")
}

// TestQueryChainOrphanStampCostScalesWithOrphans is the cost guard the spec
// asked for: the reconcile must cost one head lookup per *closed* row, not one
// per connection in the table.
//
// The shape it guards changed when the stamp became keyed. It used to be a
// correlated subquery in the reconcile's SET clause, and the guarantee came
// from PostgreSQL only evaluating a SET expression for rows that pass the
// WHERE. A keyed stamp cannot be computed in SQL at all, so the close now
// returns the uids it took and the heads are read back for exactly those — and
// the guarantee comes from the input list instead of from the planner. Both
// halves are asserted against a real plan: the close touches `queries` not at
// all, and the head lookup enters it once per closed row.
func TestQueryChainOrphanStampCostScalesWithOrphans(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "orphan_cost")

	const (
		liveSessions = 40
		orphans      = 3
	)

	store.SetInstanceID("live-instance")
	store.SetRunID("live-run")
	require.NoError(t, store.RegisterInstance(ctx))

	writeSession := func(instanceID, runID string) {
		var conn *Connection

		asRun(t, store, instanceID, runID, func() {
			var err error

			conn, err = store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
			require.NoError(t, err)
		})

		for i := range 5 {
			_, err := store.CreateQuery(ctx, &Query{
				ConnectionID: conn.UID,
				SQLText:      fmt.Sprintf("SELECT %d", i),
			})
			require.NoError(t, err)
		}
	}

	// Open sessions this run still owns: in scope for the scan, out of scope
	// for the update.
	for range liveSessions {
		writeSession("live-instance", "live-run")
	}

	for i := range orphans {
		writeSession("dead-instance", fmt.Sprintf("dead-run-%d", i))
	}

	orphanUIDs := openConnectionUIDs(t, ctx, store, "dead-instance")
	require.Len(t, orphanUIDs, orphans)

	// The close half: EXPLAIN the statement the reconcile actually runs, not a
	// lookalike. EXPLAIN ANALYZE executes it, which is also how the rows get
	// closed for the assertions below.
	closeLoops := explainQueriesLoops(t, ctx, store,
		store.deadRunScope(store.orphanCloseQuery(store.db)).Returning("uid").String())
	require.Empty(t, closeLoops,
		"the close no longer reads queries at all: a keyed stamp cannot be computed in SQL")

	// The seal half: one index lookup per closed row, and none for the 40 live
	// sessions the scan walked past.
	headLoops := explainQueriesLoops(t, ctx, store, store.orphanHeadSelect(store.db, orphanUIDs).String())
	require.NotEmpty(t, headLoops, "the plan must contain the head lookup over queries")

	for _, l := range headLoops {
		require.Equal(t, orphans, l, "the head lookup must run once per closed row, not once per connection")
	}

	// And the real path does the work: the rows above were closed by the
	// EXPLAIN ANALYZE, so re-open them and let the reconcile itself run.
	_, err := store.db.ExecContext(ctx,
		`UPDATE connections SET disconnected_at = NULL WHERE instance_id = 'dead-instance'`)
	require.NoError(t, err)

	reclaimed, err := store.ReclaimDeadInstanceConnections(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(orphans), reclaimed)

	var stamped int

	require.NoError(t, store.db.QueryRowContext(ctx,
		"SELECT count(*) FROM connections WHERE query_chain_mac IS NOT NULL AND query_chain_stamp_version = 1").
		Scan(&stamped))
	require.Equal(t, orphans, stamped)
}

// openConnectionUIDs lists the still-open connections of one instance id.
func openConnectionUIDs(t *testing.T, ctx context.Context, store *Store, instanceID string) []uuid.UUID {
	t.Helper()

	var uids []uuid.UUID

	err := store.db.NewSelect().
		Model((*Connection)(nil)).
		Column("uid").
		Where("instance_id = ?", instanceID).
		Where("disconnected_at IS NULL").
		Order("uid ASC").
		Scan(ctx, &uids)
	require.NoError(t, err)

	return uids
}

// explainQueriesLoops EXPLAIN ANALYZEs a statement and returns the loop count
// of every plan node that reads the queries table.
func explainQueriesLoops(t *testing.T, ctx context.Context, store *Store, statement string) []int {
	t.Helper()

	var plan string

	require.NoError(t, store.db.QueryRowContext(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+statement).Scan(&plan),
		"EXPLAIN of %s", statement)

	// Walked as untyped maps rather than a tagged struct: the key names are
	// PostgreSQL's ("Node Type", "Actual Loops", …) and modeling them as Go
	// fields buys nothing here.
	var explained []map[string]any

	require.NoError(t, json.Unmarshal([]byte(plan), &explained))
	require.Len(t, explained, 1)

	return queriesScanLoops(explained[0]["Plan"])
}

// queriesScanLoops walks an EXPLAIN plan tree and collects the loop count of
// every node that reads the queries table. Whether the planner picks the partial
// index or a sequential scan depends on the table's size, but either way the
// node is entered once per evaluation of the correlated subquery — which is the
// number this test is about.
func queriesScanLoops(node any) []int {
	plan, ok := node.(map[string]any)
	if !ok {
		return nil
	}

	var loops []int

	if relation, _ := plan["Relation Name"].(string); relation == "queries" {
		actual, ok := plan["Actual Loops"].(float64)
		if ok {
			loops = append(loops, int(actual))
		}
	}

	children, _ := plan["Plans"].([]any)
	for _, child := range children {
		loops = append(loops, queriesScanLoops(child)...)
	}

	return loops
}

// appendChainStatements adds n more statements to an existing connection's
// chain and returns them, so a test can let a session move on after a sweep has
// already stamped it.
func appendChainStatements(
	t *testing.T, ctx context.Context, store *Store, connectionUID uuid.UUID, n int,
) []Query {
	t.Helper()

	appended := make([]Query, 0, n)

	for i := range n {
		created, err := store.CreateQuery(ctx, &Query{
			ConnectionID: connectionUID,
			SQLText:      fmt.Sprintf("SELECT later %d", i),
		})
		require.NoError(t, err, "CreateQuery()")

		appended = append(appended, *created)
	}

	return appended
}

// TestQueryChainRefreshStampsAnOpenSession is the whole point of the sweep: the
// stamp used to be written only by a close, so a session that never ends — a
// psql window left open all day, a pooled connection, an approval hold waiting
// on a human — had nothing sealing its tail for its entire life.
func TestQueryChainRefreshStampsAnOpenSession(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "refresh-open", 3)

	before, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, before.QueryChainMAC, "an open session starts unstamped")

	stamped, err := store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), stamped)

	after, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, after.DisconnectedAt, "the refresh must not close the session")
	require.Equal(t, int64(3), after.QueryChainLen)
	require.Equal(t, queryChainStampKeyed, after.QueryChainStampVersion,
		"the sweep must write the keyed stamp version like the other two writers")
	require.Equal(t, store.queryChainStampMAC(conn.UID, queryChainStampKeyed, 3, queries[2].MAC),
		after.QueryChainMAC)
	require.NotEqual(t, queries[2].MAC, after.QueryChainMAC,
		"the stamp must not be a copy of the head MAC: that value is readable from queries")

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break, "a freshly stamped open session must verify: %v", result.Break)
}

// TestQueryChainOpenSessionMayLagBehindItsStamp is the substantive half of the
// refresh, and the failure mode that would make it worse than useless: between
// two sweeps a busy session runs statements the stamp does not cover. If the
// stamp were still judged exactly, every active session in the store would read
// as tampered with.
func TestQueryChainOpenSessionMayLagBehindItsStamp(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, _ := createChainTestConnection(t, ctx, store, "refresh-lag", 3)

	_, err := store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)

	later := appendChainStatements(t, ctx, store, conn.UID, 2)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break,
		"an open session whose stamp is merely behind is not a break: %v", result.Break)
	require.Equal(t, int64(5), result.Verified)
	require.Equal(t, int64(5), result.HeadSeq)

	stale, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.Equal(t, int64(3), stale.QueryChainLen, "the stamp is a prefix, not the head")

	// The next sweep catches it up, and the close seals it exactly.
	_, err = store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)

	caught, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.Equal(t, int64(5), caught.QueryChainLen)
	require.Equal(t, store.queryChainStampMAC(conn.UID, queryChainStampKeyed, 5, later[1].MAC),
		caught.QueryChainMAC)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	result, err = store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break, "%v", result.Break)
}

// TestQueryChainClosedSessionStampStaysExact pins the other side of the fork: a
// prefix is only ever acceptable while disconnected_at IS NULL. Once the session
// is closed the stamp is final, and one that names fewer statements than survive
// is a trailing deletion — the closed row's rule must not soften just because
// the open one's did.
func TestQueryChainClosedSessionStampStaysExact(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, _ := createChainTestConnection(t, ctx, store, "refresh-closed-exact", 3)

	_, err := store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)

	appendChainStatements(t, ctx, store, conn.UID, 2)

	// Close the row behind the store's back, leaving the lagging stamp in
	// place. This is exactly what the stampOnlyOpen guard exists to prevent a
	// concurrent sweep from producing.
	_, err = store.db.ExecContext(ctx,
		`UPDATE connections SET disconnected_at = NOW() WHERE uid = ?`, conn.UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "a closed session's stamp must name the head exactly")
	require.Contains(t, result.Break.Reason, "recorded 3 statements but the surviving statements end at 5")
}

// TestQueryChainRefreshGuardSkipsAClosedSession is that guard on its own: a
// sweep that read a head before a concurrent CloseConnection committed must not
// land afterwards and overwrite the exact final stamp with an older one.
func TestQueryChainRefreshGuardSkipsAClosedSession(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "refresh-guard", 3)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	sealed := store.queryChainStampMAC(conn.UID, queryChainStampKeyed, 3, queries[2].MAC)

	stamped, err := store.stampChainHeads(ctx, store.db, []uuid.UUID{conn.UID}, stampOnlyOpen)
	require.NoError(t, err)
	require.Equal(t, int64(0), stamped, "a closed row is out of the refresh's reach")

	closed, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.Equal(t, sealed, closed.QueryChainMAC, "the close's stamp must survive a late sweep")

	// And the sweep proper never selects it in the first place.
	swept, err := store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), swept)
}

// TestQueryChainRefreshDetectsTrailingDeletionOnAnOpenSession is what the sweep
// buys. Deleting the last statements of a live session used to be undetectable
// until it closed — at which point the close blessed the truncated chain.
func TestQueryChainRefreshDetectsTrailingDeletionOnAnOpenSession(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "refresh-trailing", 5)

	_, err := store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx,
		`DELETE FROM queries WHERE uid IN (?, ?)`, queries[3].UID, queries[4].UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "deleting below an open session's stamp must be detected")
	require.Contains(t, result.Break.Reason, "removed from the end")
}

// TestQueryChainRefreshDetectsARestampedOpenSession is the same attack with the
// attacker doing their homework: delete the tail, then rewrite the stamp with
// the values that are readable straight out of `queries`. The prefix rule
// loosens *which position* the stamp may name; it does not loosen the seal.
func TestQueryChainRefreshDetectsARestampedOpenSession(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "refresh-restamp", 5)

	_, err := store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx,
		`DELETE FROM queries WHERE uid IN (?, ?)`, queries[3].UID, queries[4].UID)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx,
		`UPDATE connections SET query_chain_mac = ?, query_chain_len = ? WHERE uid = ?`,
		queries[2].MAC, 3, conn.UID)
	require.NoError(t, err)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "a re-stamped truncation must be caught on an open session too")
	require.Contains(t, result.Break.Reason, "the stamp was rewritten")
	require.False(t, result.LegacyStamp, "a forged stamp is a break, not a legacy stamp")
}

// TestQueryChainRefreshSurvivesRetentionReapingTheStampedStatement is the
// cry-wolf case the prefix rule has to get right in the other direction.
// DBB_QUERY_STORAGE_RETENTION reaps the oldest statements of an open,
// long-lived session — including, eventually, the one an earlier sweep sealed.
// Nothing is left to check the stamp against, and that is housekeeping, not
// tampering.
func TestQueryChainRefreshSurvivesRetentionReapingTheStampedStatement(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "refresh-reaped", 2)

	_, err := store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)

	appendChainStatements(t, ctx, store, conn.UID, 2)

	// Age the two statements the stamp covers; the connection stays open, so
	// only the query-level sweep touches them.
	_, err = store.db.ExecContext(ctx,
		`UPDATE queries SET executed_at = NOW() - INTERVAL '30 days' WHERE uid IN (?, ?)`,
		queries[0].UID, queries[1].UID)
	require.NoError(t, err)

	swept, err := store.CleanupOldQueryRows(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(2), swept.Queries)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, result.Break,
		"a stamp whose statement retention reaped is unverifiable, not a break: %v", result.Break)
	require.True(t, result.TruncatedPrefix)
	require.Equal(t, int64(3), result.FirstSeq)
	require.Equal(t, int64(2), result.Verified)

	// The next sweep re-seals it against what survives, and the tail is
	// protected again.
	_, err = store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx, `DELETE FROM queries WHERE chain_seq = 4 AND connection_id = ?`,
		conn.UID)
	require.NoError(t, err)

	result, err = store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "a trailing deletion below the fresh stamp must be caught")
}

// TestQueryChainRefreshOnlyTouchesThisRunsSessions keeps replicas out of each
// other's way: a row's head is best known to the process serving it, and two
// replicas rewriting the same stamps every pass would be contention for nothing.
func TestQueryChainRefreshOnlyTouchesThisRunsSessions(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "refresh_scope")

	mine, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	require.NoError(t, err)

	var theirs *Connection

	asRun(t, store, "other-instance", "other-run", func() {
		theirs, err = store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.2")
		require.NoError(t, err)
	})

	for _, conn := range []*Connection{mine, theirs} {
		_, err = store.CreateQuery(ctx, &Query{ConnectionID: conn.UID, SQLText: "SELECT 1"})
		require.NoError(t, err)
	}

	stamped, err := store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), stamped, "only the sessions this run owns are swept")

	ours, err := store.GetConnectionByUID(ctx, mine.UID)
	require.NoError(t, err)
	require.NotNil(t, ours.QueryChainMAC)

	peer, err := store.GetConnectionByUID(ctx, theirs.UID)
	require.NoError(t, err)
	require.Nil(t, peer.QueryChainMAC, "another run's live session is not ours to stamp")
}

// TestQueryChainRefreshLeavesSilentSessionUnstamped mirrors the reconcile's
// rule: there is no head to seal, and checkMissingStamp reads a NULL stamp on a
// session with no surviving statements as "logged nothing" rather than as a
// break.
func TestQueryChainRefreshLeavesSilentSessionUnstamped(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "refresh_silent")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	require.NoError(t, err)

	stamped, err := store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), stamped)

	row, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.Nil(t, row.QueryChainMAC)
	require.Equal(t, int64(0), row.QueryChainLen)
	require.Equal(t, queryChainStampLegacy, row.QueryChainStampVersion)
}

// TestQueryChainRefreshCostScalesWithOpenSessions is the cost guard the spec
// asked for, and the counterpart of
// TestQueryChainOrphanStampCostScalesWithOrphans. The sweep runs on every
// reclaim tick for the life of the process, so its cost has to track *open
// sessions this run owns* — concurrency — and never the size of the query
// history. Both halves are asserted against a real plan: picking the rows never
// reads `queries` at all, and the head lookup enters it once per open session.
func TestQueryChainRefreshCostScalesWithOpenSessions(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "refresh_cost")

	const (
		open        = 4
		closed      = 20
		otherRun    = 15
		perSession  = 5
		ourInstance = "live-instance"
		ourRun      = "live-run"
	)

	store.SetInstanceID(ourInstance)
	store.SetRunID(ourRun)

	writeSession := func(instanceID, runID string, andClose bool) {
		var conn *Connection

		asRun(t, store, instanceID, runID, func() {
			var err error

			conn, err = store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
			require.NoError(t, err)
		})

		for i := range perSession {
			_, err := store.CreateQuery(ctx, &Query{
				ConnectionID: conn.UID,
				SQLText:      fmt.Sprintf("SELECT %d", i),
			})
			require.NoError(t, err)
		}

		if andClose {
			require.NoError(t, store.CloseConnection(ctx, conn.UID))
		}
	}

	for range open {
		writeSession(ourInstance, ourRun, false)
	}

	// History the sweep must never pay for: our own closed sessions, and
	// another replica's live ones.
	for range closed {
		writeSession(ourInstance, ourRun, true)
	}

	for i := range otherRun {
		writeSession("other-instance", fmt.Sprintf("other-run-%d", i), false)
	}

	pickLoops := explainQueriesLoops(t, ctx, store, store.openChainStampSelect(store.db).String())
	require.Empty(t, pickLoops, "picking the rows to sweep must not read queries at all")

	var uids []uuid.UUID

	require.NoError(t, store.openChainStampSelect(store.db).Scan(ctx, &uids))
	require.Len(t, uids, open, "the sweep covers this run's open sessions and nothing else")

	headLoops := explainQueriesLoops(t, ctx, store, store.orphanHeadSelect(store.db, uids).String())
	require.NotEmpty(t, headLoops, "the plan must contain the head lookup over queries")

	for _, l := range headLoops {
		require.Equal(t, open, l,
			"the head lookup must run once per open session this run owns, not once per connection")
	}

	stamped, err := store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(open), stamped)
}

// TestQueryChainStampNeverMovesBackwards is the interaction the refresh sweep
// creates between the three stamp writers. A live session now carries a stamp
// from the last sweep; if its process then crashes, the reconcile is the next
// writer and recovers the head from whatever is left in the database. Someone
// who deleted the tail in between would otherwise get the reconcile to
// overwrite the sweep's higher stamp with their truncated one — laundering
// exactly the deletion the stamp exists to catch.
//
// Retention cannot lower a connection's highest chain_seq without removing
// every statement (in which case there is no head to stamp with at all), so a
// recovered head below the stored stamp has only one meaning.
func TestQueryChainStampNeverMovesBackwards(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createOrphanedChainConnection(t, ctx, store, "no-regress", 5)

	// The sweep that stamped it ran as the process that owned the session.
	asRun(t, store, "dead-instance-no-regress", "dead-run-no-regress", func() {
		stamped, err := store.RefreshOpenChainStamps(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(1), stamped)
	})

	swept, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.Equal(t, int64(5), swept.QueryChainLen)

	// The process crashes, and the tail is deleted before anyone notices.
	_, err = store.db.ExecContext(ctx,
		`DELETE FROM queries WHERE uid IN (?, ?)`, queries[3].UID, queries[4].UID)
	require.NoError(t, err)

	store.SetInstanceID("live-instance")
	store.SetRunID("live-run")
	require.NoError(t, store.RegisterInstance(ctx))

	reclaimed, err := store.ReclaimDeadInstanceConnections(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), reclaimed)

	closed, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, closed.DisconnectedAt, "the reconcile must still close the row")
	require.Equal(t, int64(5), closed.QueryChainLen,
		"the reconcile must not lower a stamp the sweep already wrote")
	require.Equal(t, swept.QueryChainMAC, closed.QueryChainMAC)

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "the reconcile must not launder a deletion the sweep had sealed")
	require.Contains(t, result.Break.Reason, "removed from the end")
}

// TestQueryChainCloseDoesNotLowerASweptStamp is the same invariant on the clean
// teardown path. CloseConnection normally stamps from the head this process
// holds in memory, which is authoritative — but it falls back to the database
// when this process never wrote to the chain (a replica closing a session
// another opened, a store restarted mid-session), and that fallback sees only
// what survives.
func TestQueryChainCloseDoesNotLowerASweptStamp(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, queries := createChainTestConnection(t, ctx, store, "close-no-regress", 5)

	_, err := store.RefreshOpenChainStamps(ctx)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx,
		`DELETE FROM queries WHERE uid IN (?, ?)`, queries[3].UID, queries[4].UID)
	require.NoError(t, err)

	// Drop the in-process head so the close takes the database fallback, as a
	// replica that did not serve this session would.
	store.queryChains.forget(conn.UID)

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	closed, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, closed.DisconnectedAt)
	require.Equal(t, int64(5), closed.QueryChainLen, "the close must not lower the swept stamp")

	result, err := store.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break)
	require.Contains(t, result.Break.Reason, "removed from the end")
}

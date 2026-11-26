package store

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// ArchiveLease is how long a claim on a connection holds. A crash anywhere
// leaves the rows in PostgreSQL; the lease expires and the next pass redoes the
// connection, overwriting the same object key.
const ArchiveLease = 15 * time.Minute

// ArchiveCandidate is a closed connection due for archival, claimed by this
// process.
type ArchiveCandidate struct {
	UID            uuid.UUID
	ServerUID      uuid.UUID
	DisconnectedAt time.Time
	// ArchiveSeconds is the resolved archive window that made it due.
	ArchiveWindow RetentionWindow
}

// ClaimArchiveCandidates claims up to limit closed, due, not-yet-archived
// connections that captured at least one result row, with a conditional UPDATE
// on archive_claimed_at (FOR UPDATE SKIP LOCKED, so replicas never take the
// same one). The archive window is resolved per server, live.
func (s *Store) ClaimArchiveCandidates(
	ctx context.Context, instance InstanceRetention, limit int, now time.Time,
) ([]ArchiveCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}

	var serverUIDs []uuid.UUID

	err := s.db.NewSelect().
		Model((*Connection)(nil)).
		ColumnExpr("DISTINCT database_id").
		Where("disconnected_at IS NOT NULL").
		Where("archived_at IS NULL").
		Scan(ctx, &serverUIDs)
	if err != nil {
		return nil, fmt.Errorf("list archive servers: %w", err)
	}

	resolved, err := s.ResolveRetentionByServers(ctx, serverUIDs, instance)
	if err != nil {
		return nil, err
	}

	var claimed []ArchiveCandidate

	for _, serverUID := range serverUIDs {
		window := resolved[serverUID].Archive
		if window.Duration <= 0 || len(claimed) >= limit {
			continue
		}

		var rows []struct {
			UID            uuid.UUID `bun:"uid"`
			DisconnectedAt time.Time `bun:"disconnected_at"`
		}

		err := s.db.NewRaw(`UPDATE connections SET archive_claimed_at = ?
			WHERE uid IN (
				SELECT c.uid FROM connections c
				WHERE c.database_id = ? AND c.disconnected_at IS NOT NULL
				  AND c.archived_at IS NULL
				  AND c.disconnected_at < ?
				  AND (c.archive_claimed_at IS NULL OR c.archive_claimed_at < ?)
				  AND `+hasCapturedRows("c")+`
				ORDER BY c.disconnected_at
				LIMIT ?
				FOR UPDATE OF c SKIP LOCKED
			)
			RETURNING uid, disconnected_at`,
			now, serverUID, now.Add(-window.Duration), now.Add(-ArchiveLease), limit-len(claimed),
		).Scan(ctx, &rows)
		if err != nil {
			return claimed, fmt.Errorf("claim archive candidates: %w", err)
		}

		for _, r := range rows {
			claimed = append(claimed, ArchiveCandidate{
				UID: r.UID, ServerUID: serverUID, DisconnectedAt: r.DisconnectedAt, ArchiveWindow: window,
			})
		}
	}

	return claimed, nil
}

// ArchivedRow is one captured result row, with the identity the archive needs.
type ArchivedRow struct {
	QueryUID     uuid.UUID
	RowUID       uuid.UUID
	RowNumber    int
	RowData      []byte
	RowSizeBytes int64
}

// StreamConnectionRows calls fn for every captured row of a connection, in
// archive order: statement order (executed_at, uid), then (row_number, uid).
//
// It reads one statement at a time and sorts that statement's rows in Go. A
// single ORDER BY over the whole connection made PostgreSQL read and sort every
// row before sending the first one: on a store where a connection's rows are
// spread over the disk (about one page read per row) that took over a minute,
// past the driver's 10 s read timeout, and the abandoned query kept running.
// On connections with millions of rows the planner even chose a full scan of
// query_rows. Per statement, the lookup always goes through
// idx_query_rows_query_id, and memory holds at most one statement's rows.
func (s *Store) StreamConnectionRows(ctx context.Context, connectionUID uuid.UUID, fn func(ArchivedRow) error) error {
	var queryUIDs []uuid.UUID

	if err := s.db.NewSelect().Table("queries").Column("uid").
		Where("connection_id = ?", connectionUID).
		Order("executed_at", "uid").
		Scan(ctx, &queryUIDs); err != nil {
		return fmt.Errorf("list connection statements: %w", err)
	}

	var batch []ArchivedRow

	for _, queryUID := range queryUIDs {
		var err error

		batch, err = s.statementRows(ctx, queryUID, batch[:0])
		if err != nil {
			return err
		}

		for _, r := range batch {
			if err := fn(r); err != nil {
				return err
			}
		}
	}

	return nil
}

// statementRows appends the captured rows of one statement to dst, sorted by
// (row_number, uid). uid ties are compared bytewise, which is PostgreSQL's
// uuid ordering.
func (s *Store) statementRows(ctx context.Context, queryUID uuid.UUID, dst []ArchivedRow) ([]ArchivedRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT uid, row_number, row_data::text, row_size_bytes
		FROM query_rows WHERE query_id = ?`, queryUID)
	if err != nil {
		return nil, fmt.Errorf("stream statement rows: %w", err)
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		r := ArchivedRow{QueryUID: queryUID}

		var data string

		if err := rows.Scan(&r.RowUID, &r.RowNumber, &data, &r.RowSizeBytes); err != nil {
			return nil, fmt.Errorf("scan statement row: %w", err)
		}

		r.RowData = []byte(data)
		dst = append(dst, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stream statement rows: %w", err)
	}

	slices.SortFunc(dst, func(a, b ArchivedRow) int {
		if c := cmp.Compare(a.RowNumber, b.RowNumber); c != 0 {
			return c
		}

		return bytes.Compare(a.RowUID[:], b.RowUID[:])
	})

	return dst, nil
}

// ArchiveSeal is what the archiver records about one written object.
type ArchiveSeal struct {
	ConnectionUID uuid.UUID `json:"connection_uid"`
	ServerUID     uuid.UUID `json:"server_uid"`
	Key           string    `json:"key"`
	Bytes         int64     `json:"bytes"`
	SHA256        string    `json:"sha256"`
	Rows          int64     `json:"rows"`
	Statements    int64     `json:"statements"`
	// ChunkBytes and ChunkSHA256 are the SHA-256 of every ChunkBytes-long
	// chunk of an object larger than the whole-fetch limit, so a range read
	// checks what it touches. Absent from smaller objects and from entries
	// sealed before they existed.
	ChunkBytes  int64    `json:"chunk_bytes,omitempty"`
	ChunkSHA256 []string `json:"chunk_sha256,omitempty"`
	// Policy says which window applied and from which level.
	PolicySeconds int64       `json:"policy_archive_after_seconds"`
	PolicySource  string      `json:"policy_source"`
	PolicyGroups  []uuid.UUID `json:"policy_server_group_uids,omitempty"`
}

// SealAndCommitArchive appends the chained connection.archived audit entry and,
// in one transaction, marks the connection archived and deletes its captured
// rows. A crash before the transaction leaves the rows in PostgreSQL.
func (s *Store) SealAndCommitArchive(ctx context.Context, seal ArchiveSeal) error {
	details, err := json.Marshal(seal)
	if err != nil {
		return fmt.Errorf("encode archive seal: %w", err)
	}

	if err := s.LogAuditEvent(ctx, &AuditEvent{
		EventType: AuditEventConnectionArchived,
		Details:   details,
	}); err != nil {
		return err
	}

	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model((*Connection)(nil)).
			Set("archived_at = ?", time.Now()).
			Set("archive_key = ?", seal.Key).
			Set("archive_rows_sha256 = ?", seal.SHA256).
			Set("archive_rows_bytes = ?", seal.Bytes).
			Set("archive_rows_count = ?", seal.Rows).
			Set("archive_claimed_at = NULL").
			Where("uid = ?", seal.ConnectionUID).
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("mark connection archived: %w", err)
		}

		if n, _ := res.RowsAffected(); n == 0 {
			return ErrConnectionNotFound
		}

		return deleteConnectionRows(ctx, tx, seal.ConnectionUID)
	})
}

// archiveDeleteBatch bounds one DELETE of archived rows. Rows of a connection
// are spread over the disk, about one page read each, so a batch stays well
// under the driver's 10 s read timeout even on a loaded store.
const archiveDeleteBatch = 2000

// deleteConnectionRows deletes the captured rows of a connection inside tx,
// statement by statement and in batches. One DELETE over the whole connection
// ran past the driver's read timeout on connections with millions of rows, and
// the abandoned statement kept running on the server. All batches share the
// transaction, so the connection still flips to archived atomically.
func deleteConnectionRows(ctx context.Context, tx bun.Tx, connectionUID uuid.UUID) error {
	var queryUIDs []uuid.UUID

	if err := tx.NewSelect().Table("queries").Column("uid").
		Where("connection_id = ?", connectionUID).
		Scan(ctx, &queryUIDs); err != nil {
		return fmt.Errorf("list archived statements: %w", err)
	}

	for _, queryUID := range queryUIDs {
		for {
			res, err := tx.ExecContext(ctx, `DELETE FROM query_rows WHERE uid IN (
				SELECT uid FROM query_rows WHERE query_id = ? LIMIT ?)`, queryUID, archiveDeleteBatch)
			if err != nil {
				return fmt.Errorf("delete archived rows: %w", err)
			}

			if n, _ := res.RowsAffected(); n < archiveDeleteBatch {
				break
			}
		}
	}

	return nil
}

// ReleaseArchiveClaim drops the lease of a connection whose archive failed, so
// the next pass retries without waiting out the lease.
func (s *Store) ReleaseArchiveClaim(ctx context.Context, connectionUID uuid.UUID) error {
	_, err := s.db.NewUpdate().Model((*Connection)(nil)).
		Set("archive_claimed_at = NULL").
		Where("uid = ?", connectionUID).
		Exec(ctx)

	return err
}

// ConnectionArchiveRef identifies the object of an archived connection.
type ConnectionArchiveRef struct {
	ConnectionUID uuid.UUID
	Key           string
}

// ListArchivesDueForDeletion returns archived, not yet deleted connections
// whose resolved delete window (disconnected_at + delete_after) has passed.
func (s *Store) ListArchivesDueForDeletion(
	ctx context.Context, instance InstanceRetention, limit int, now time.Time,
) ([]ConnectionArchiveRef, error) {
	var conns []Connection

	err := s.db.NewSelect().Model(&conns).
		Column("uid", "database_id", "archive_key", "disconnected_at").
		Where("archived_at IS NOT NULL").
		Where("archive_deleted_at IS NULL").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("list live archives: %w", err)
	}

	serverSet := map[uuid.UUID]struct{}{}
	for i := range conns {
		serverSet[conns[i].DatabaseID] = struct{}{}
	}

	uids := make([]uuid.UUID, 0, len(serverSet))
	for u := range serverSet {
		uids = append(uids, u)
	}

	resolved, err := s.ResolveRetentionByServers(ctx, uids, instance)
	if err != nil {
		return nil, err
	}

	var due []ConnectionArchiveRef

	for i := range conns {
		window := resolved[conns[i].DatabaseID].Delete.Duration
		if window <= 0 || conns[i].DisconnectedAt == nil || conns[i].ArchiveKey == nil {
			continue
		}

		if conns[i].DisconnectedAt.Before(now.Add(-window)) {
			due = append(due, ConnectionArchiveRef{ConnectionUID: conns[i].UID, Key: *conns[i].ArchiveKey})

			if len(due) >= limit {
				break
			}
		}
	}

	return due, nil
}

// ArchiveStillDue re-resolves the delete window of one archived connection at
// this instant and reports whether its archive is still due for deletion. The
// sweeper calls it right before deleting each object: the list it iterates was
// read earlier in the pass, and an operator may have lengthened (or set to
// never) the window since. An object is irreversible, so the policy is read
// again, live, at the last moment.
func (s *Store) ArchiveStillDue(ctx context.Context, ref ConnectionArchiveRef, now time.Time) (bool, error) {
	var conn Connection

	err := s.db.NewSelect().Model(&conn).
		Column("uid", "database_id", "disconnected_at").
		Where("uid = ?", ref.ConnectionUID).
		Where("archived_at IS NOT NULL").
		Where("archive_deleted_at IS NULL").
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}

		return false, fmt.Errorf("re-read archived connection: %w", err)
	}

	instance, err := s.InstanceRetention(ctx)
	if err != nil {
		return false, err
	}

	resolved, err := s.ResolveRetentionByServers(ctx, []uuid.UUID{conn.DatabaseID}, instance)
	if err != nil {
		return false, err
	}

	window := resolved[conn.DatabaseID].Delete.Duration

	return window > 0 && conn.DisconnectedAt != nil && conn.DisconnectedAt.Before(now.Add(-window)), nil
}

// MarkArchiveDeleted appends the chained connection.archive_deleted entry and
// records the deletion on the connection row.
func (s *Store) MarkArchiveDeleted(ctx context.Context, ref ConnectionArchiveRef) error {
	details, err := json.Marshal(map[string]any{"connection_uid": ref.ConnectionUID, "key": ref.Key})
	if err != nil {
		return fmt.Errorf("encode archive deletion: %w", err)
	}

	if err := s.LogAuditEvent(ctx, &AuditEvent{
		EventType: AuditEventConnectionArchiveDeleted,
		Details:   details,
	}); err != nil {
		return err
	}

	_, err = s.db.NewUpdate().Model((*Connection)(nil)).
		Set("archive_deleted_at = ?", time.Now()).
		Set("archive_key = NULL").
		Where("uid = ?", ref.ConnectionUID).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("mark archive deleted: %w", err)
	}

	return nil
}

// ErrArchiveNotFound is returned when a connection has no live archive.
var ErrArchiveNotFound = errors.New("connection has no archive")

// ConnectionArchive is what a reader needs to open a connection's archive.
type ConnectionArchive struct {
	Key    string
	Bytes  int64
	SHA256 string
	Rows   int64
	// ChunkBytes and ChunkSHA256 are the sealed per-chunk digests, empty when
	// the entry has none (see ArchiveSeal).
	ChunkBytes  int64
	ChunkSHA256 []string
}

// GetConnectionArchive returns the archive reference of a connection, or
// ErrArchiveNotFound when its rows are not in the archive. The reference is
// taken from the chained connection.archived entry, which must agree with the
// connection row: ErrArchiveSealMismatch otherwise.
func (s *Store) GetConnectionArchive(ctx context.Context, connectionUID uuid.UUID) (*ConnectionArchive, error) {
	conn := new(Connection)

	err := s.db.NewSelect().Model(conn).
		Column("archived_at", "archive_deleted_at", "archive_key", "archive_rows_bytes", "archive_rows_sha256", "archive_rows_count").
		Where("uid = ?", connectionUID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConnectionNotFound
		}

		return nil, fmt.Errorf("get connection archive: %w", err)
	}

	if conn.ArchivedAt == nil || conn.ArchiveDeletedAt != nil || conn.ArchiveKey == nil {
		return nil, ErrArchiveNotFound
	}

	conn.UID = connectionUID

	// The reference comes from the sealed entry, never from the unsealed
	// columns alone: see archive_seal.go.
	return s.sealedConnectionArchive(ctx, conn)
}

package store

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// The archive excuse is only as strong as its seal.
//
// archived_at, archive_key and archive_rows_sha256 are plain columns on
// connections, which carries no MAC. Anyone who can write dbbat's store could
// set them on a past connection and then delete its captured rows. What the
// archiver writes that such an actor cannot forge is the chained
// connection.archived entry (and, at deletion, connection.archive_deleted). So
// a connection's missing rows are excused, and its archive is read, only when
// such an entry exists, carries a MAC valid under the chain key, and agrees
// with the row.

// ErrArchiveSealMismatch is returned when an archived connection's row has no
// authentic connection.archived entry, or one that disagrees with its columns.
var ErrArchiveSealMismatch = errors.New("archive seal does not match the connection row")

// archiveSeals is what the audit chain holds about one connection's archive.
type archiveSeals struct {
	// entry is the latest authentic connection.archived entry, seal its details.
	entry *AuditLog
	seal  ArchiveSeal
	// deletedKeys are the keys named by authentic connection.archive_deleted
	// entries.
	deletedKeys map[string]struct{}
}

// archiveDeletion is the details of a connection.archive_deleted entry.
type archiveDeletion struct {
	ConnectionUID uuid.UUID `json:"connection_uid"`
	Key           string    `json:"key"`
}

// authenticAuditEntry reports whether an audit entry's MAC matches its own
// content under the chain key. Its position in the chain is what `audit
// verify` checks; this only proves the entry was written by a key holder.
// Without a key nothing can be authenticated, and every entry is taken as is.
func (s *Store) authenticAuditEntry(row *AuditLog) bool {
	if !s.ChainEnabled() {
		return true
	}

	if row.ChainSeq == nil || len(row.MAC) == 0 {
		return false
	}

	payload, err := auditChainPayload(
		*row.ChainSeq, row.UID, row.EventType, row.UserID, row.PerformedBy,
		row.Details, row.CreatedAt, row.PrevMAC,
	)
	if err != nil {
		return false
	}

	return hmac.Equal(row.MAC, s.chainMAC(payload))
}

// loadArchiveSeals reads the authentic archive entries, of every connection or
// of one, in a single query.
func (s *Store) loadArchiveSeals(ctx context.Context, connectionUID *uuid.UUID) (map[uuid.UUID]*archiveSeals, error) {
	var rows []AuditLog

	q := s.db.NewSelect().
		Model(&rows).
		Where("event_type IN (?)", bun.List([]string{AuditEventConnectionArchived, AuditEventConnectionArchiveDeleted})).
		OrderExpr("chain_seq ASC NULLS FIRST, created_at ASC")

	if connectionUID != nil {
		q = q.Where("details->>'connection_uid' = ?", connectionUID.String())
	}

	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to read archive seals: %w", err)
	}

	out := map[uuid.UUID]*archiveSeals{}

	get := func(uid uuid.UUID) *archiveSeals {
		rec, ok := out[uid]
		if !ok {
			rec = &archiveSeals{deletedKeys: map[string]struct{}{}}
			out[uid] = rec
		}

		return rec
	}

	for i := range rows {
		row := &rows[i]

		if !s.authenticAuditEntry(row) {
			continue
		}

		switch row.EventType {
		case AuditEventConnectionArchived:
			var seal ArchiveSeal
			if err := json.Unmarshal(row.Details, &seal); err != nil {
				continue
			}

			rec := get(seal.ConnectionUID)
			rec.entry, rec.seal = row, seal
		case AuditEventConnectionArchiveDeleted:
			var del archiveDeletion
			if err := json.Unmarshal(row.Details, &del); err != nil {
				continue
			}

			get(del.ConnectionUID).deletedKeys[del.Key] = struct{}{}
		}
	}

	return out, nil
}

// archivedConnection is the archive state of one connection row, and how many
// of its statements carry a row chain head.
type archivedConnection struct {
	UID       uuid.UUID  `bun:"uid"`
	Key       *string    `bun:"archive_key"`
	SHA256    *string    `bun:"archive_rows_sha256"`
	Bytes     *int64     `bun:"archive_rows_bytes"`
	Rows      *int64     `bun:"archive_rows_count"`
	DeletedAt *time.Time `bun:"archive_deleted_at"`
	Captures  int64      `bun:"captures"`
}

// archiveVerdict is what the row walk makes of an archived connection.
type archiveVerdict int

const (
	// archiveUnsealed: no authentic entry excuses it; walk it as if archived_at
	// were NULL.
	archiveUnsealed archiveVerdict = iota
	// archiveLive: its rows are in a sealed archive object.
	archiveLive
	// archiveDeleted: its sealed archive object was deleted, and the deletion
	// is sealed too.
	archiveDeleted
)

// sealMismatch names what differs between a connection row and its seal, or
// "" when they agree. The key is only compared while the archive is live:
// deleting the object nulls archive_key.
func sealMismatch(c *archivedConnection, seal *ArchiveSeal, live bool) string {
	switch {
	case c.SHA256 == nil || *c.SHA256 != seal.SHA256:
		return "sha256"
	case c.Bytes == nil || *c.Bytes != seal.Bytes:
		return "size"
	case c.Rows == nil || *c.Rows != seal.Rows:
		return "row count"
	case live && (c.Key == nil || *c.Key != seal.Key):
		return "key"
	case !live && c.Key != nil && *c.Key != seal.Key:
		return "key"
	}

	return ""
}

// judgeArchive decides whether an archived connection's missing rows are
// excused. A seal that disagrees with the row is a break of its own.
func judgeArchive(c *archivedConnection, rec *archiveSeals) (archiveVerdict, *ChainBreak) {
	if rec == nil || rec.entry == nil {
		return archiveUnsealed, nil
	}

	live := c.DeletedAt == nil

	if !live {
		if _, ok := rec.deletedKeys[rec.seal.Key]; !ok {
			return archiveUnsealed, nil
		}
	}

	if what := sealMismatch(c, &rec.seal, live); what != "" {
		connUID := c.UID

		var seq int64
		if rec.entry.ChainSeq != nil {
			seq = *rec.entry.ChainSeq
		}

		return archiveUnsealed, &ChainBreak{
			UID:           rec.entry.UID,
			ChainSeq:      seq,
			ConnectionUID: &connUID,
			Reason:        fmt.Sprintf("%s: %s differs", ErrArchiveSealMismatch.Error(), what),
		}
	}

	if live {
		return archiveLive, nil
	}

	return archiveDeleted, nil
}

// archiveExcuses is the outcome of judging every archived connection in scope.
type archiveExcuses struct {
	// excused are the connections whose captures are not walked.
	excused        map[uuid.UUID]struct{}
	archived       int64
	archiveDeleted int64
	brk            *ChainBreak
}

// judgeArchivedConnections reads every archived connection in scope and its
// seals (two queries), and decides which are excused from the row walk.
func (s *Store) judgeArchivedConnections(ctx context.Context, connectionUID *uuid.UUID) (archiveExcuses, error) {
	out := archiveExcuses{excused: map[uuid.UUID]struct{}{}}

	var conns []archivedConnection

	q := s.db.NewSelect().
		TableExpr("connections AS c").
		Column("c.uid", "c.archive_key", "c.archive_rows_sha256", "c.archive_rows_bytes",
			"c.archive_rows_count", "c.archive_deleted_at").
		ColumnExpr("(SELECT count(*) FROM queries q WHERE q.connection_id = c.uid AND q.row_chain_mac IS NOT NULL) AS captures").
		Where("c.archived_at IS NOT NULL").
		Order("c.uid ASC")

	if connectionUID != nil {
		q = q.Where("c.uid = ?", *connectionUID)
	}

	if err := q.Scan(ctx, &conns); err != nil {
		return out, fmt.Errorf("failed to list archived connections: %w", err)
	}

	if len(conns) == 0 {
		return out, nil
	}

	seals, err := s.loadArchiveSeals(ctx, connectionUID)
	if err != nil {
		return out, err
	}

	for i := range conns {
		c := &conns[i]

		verdict, brk := judgeArchive(c, seals[c.UID])
		if brk != nil && out.brk == nil {
			out.brk = brk
		}

		switch verdict {
		case archiveLive:
			out.excused[c.UID] = struct{}{}
			out.archived += c.Captures
		case archiveDeleted:
			out.excused[c.UID] = struct{}{}
			out.archiveDeleted += c.Captures
		case archiveUnsealed:
		}
	}

	return out, nil
}

// sealedConnectionArchive checks a connection row's archive columns against
// its authentic connection.archived entry and returns the reference from the
// entry. A missing entry, or one that disagrees, is ErrArchiveSealMismatch.
func (s *Store) sealedConnectionArchive(ctx context.Context, conn *Connection) (*ConnectionArchive, error) {
	seals, err := s.loadArchiveSeals(ctx, &conn.UID)
	if err != nil {
		return nil, err
	}

	rec := seals[conn.UID]
	if rec == nil || rec.entry == nil {
		return nil, fmt.Errorf("%w: connection %s has no sealed connection.archived entry", ErrArchiveSealMismatch, conn.UID)
	}

	row := &archivedConnection{
		UID: conn.UID, Key: conn.ArchiveKey, SHA256: conn.ArchiveRowsSHA256,
		Bytes: conn.ArchiveRowsBytes, Rows: conn.ArchiveRowsCount,
	}

	if what := sealMismatch(row, &rec.seal, true); what != "" {
		return nil, fmt.Errorf("%w: connection %s, %s differs", ErrArchiveSealMismatch, conn.UID, what)
	}

	return &ConnectionArchive{
		Key:         rec.seal.Key,
		Bytes:       rec.seal.Bytes,
		SHA256:      rec.seal.SHA256,
		Rows:        rec.seal.Rows,
		ChunkBytes:  rec.seal.ChunkBytes,
		ChunkSHA256: rec.seal.ChunkSHA256,
	}, nil
}

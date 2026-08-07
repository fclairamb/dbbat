package mssql

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// maxAccountBuffer caps what the accountant will hold while a single token is
// still incomplete. A ROW carrying a varbinary(max) column can be arbitrarily
// large, and the accountant is observational: past this point it gives up on
// the walk for that message rather than growing without bound. The relayed
// bytes are unaffected — they were forwarded packet by packet as they arrived.
const maxAccountBuffer = 8 * 1024 * 1024

// errNeedMore means a token is not complete yet, so the walk must wait for the
// next packet. It is the incremental counterpart of ErrTokenTruncated.
var errNeedMore = errors.New("mssql: incomplete TDS token")

// column is one column of the result set currently being read.
type column struct {
	name string
	info typeInfo
}

// resultAccountant walks the response token stream a statement produced.
//
// It has to be incremental: the upstream→client pump forwards a packet at a
// time (a result set is one logical message and may be arbitrarily large), so
// tokens routinely straddle packet boundaries.
//
// It is also strictly observational. Nothing it does can alter the bytes the
// client receives, which is why a stream it cannot follow is a lost measurement
// rather than a broken session: it marks itself desynchronised, and a 13-byte
// rolling tail still recovers the row count, because the last token of any TDS
// response is a DONE-family token.
type resultAccountant struct {
	session *session

	buf      []byte
	desynced bool

	cols []column

	rows          []store.QueryRow
	capturedBytes int64
	truncated     bool
	captureOff    bool

	rowsSeen  int64
	doneRows  int64
	haveCount bool

	failure *tdsMessage

	// handle is the first integer RETURNVALUE of the message, which for a
	// prepare form is the prepared-statement handle.
	handle    int64
	haveHandl bool

	tail    [doneTokenLen]byte
	tailLen int
}

// newResultAccountant starts accounting for one response message.
func (s *session) newResultAccountant() *resultAccountant {
	return &resultAccountant{
		session:    s,
		captureOff: !s.server.queryStorage.StoreResults,
	}
}

// accountServerPacket is the serverPacketHook: every response packet passes
// through here on its way to the client, with eom marking the last packet of a
// logical message.
func (s *session) accountServerPacket(ctx context.Context, msgType byte, payload []byte, eom bool) {
	if msgType != packetTypeReply {
		return
	}

	if s.accountant == nil {
		s.accountant = s.newResultAccountant()
	}

	s.accountant.feed(ctx, payload)

	if !eom {
		return
	}

	acc := s.accountant
	s.accountant = nil

	acc.finish(ctx)
}

// feed consumes one response packet's payload.
func (a *resultAccountant) feed(ctx context.Context, payload []byte) {
	a.recordTail(payload)

	if a.desynced {
		return
	}

	a.buf = append(a.buf, payload...)

	for len(a.buf) > 0 {
		next, err := a.consumeToken(a.buf)

		switch {
		case err == nil:
			a.buf = a.buf[next:]

		case errors.Is(err, errNeedMore):
			if len(a.buf) > maxAccountBuffer {
				a.desync(ctx, err)
			}

			return

		default:
			a.desync(ctx, err)

			return
		}
	}
}

// desync abandons the forward walk for this message.
func (a *resultAccountant) desync(ctx context.Context, err error) {
	a.desynced = true
	a.buf = nil
	a.rows = nil

	a.session.logger.DebugContext(ctx, "MSSQL result accounting desynchronised; "+
		"falling back to the DONE token at the end of the message", slog.Any("error", err))
}

// recordTail keeps the last doneTokenLen bytes of the message, which is exactly
// its final DONE-family token.
func (a *resultAccountant) recordTail(payload []byte) {
	if len(payload) >= doneTokenLen {
		a.tailLen = copy(a.tail[:], payload[len(payload)-doneTokenLen:])

		return
	}

	keep := min(a.tailLen, doneTokenLen-len(payload))
	copy(a.tail[:], a.tail[a.tailLen-keep:a.tailLen])
	copy(a.tail[keep:], payload)
	a.tailLen = keep + len(payload)
}

// consumeToken advances past one token, returning how many bytes it took.
func (a *resultAccountant) consumeToken(buf []byte) (int, error) {
	token := buf[0]

	switch token {
	case tokenError, tokenInfo, tokenLoginAck, tokenEnvChange,
		tokenTabName, tokenColInfo, tokenOrder, tokenSSPI:
		return a.consumeUShortToken(token, buf)

	case tokenSessionState, tokenFedAuthInfo:
		return consumeULongToken(buf)

	case tokenDone, tokenDoneProc, tokenDoneInProc:
		return a.consumeDone(token, buf)

	case tokenReturnStatus:
		return needAtLeast(buf, 1+4)

	case tokenOffset:
		return needAtLeast(buf, 1+4)

	case tokenFeatureExtAck:
		return consumeFeatureExtAck(buf)

	case tokenColMetadata:
		return a.consumeColMetadata(buf)

	case tokenRow:
		return a.consumeRow(buf, false)

	case tokenNBCRow:
		return a.consumeRow(buf, true)

	case tokenReturnValue:
		return a.consumeReturnValue(buf)

	default:
		// ALTMETADATA / ALTROW (COMPUTE BY) and anything unmodelled. Stopping
		// is the honest outcome: guessing a length would mis-read everything
		// after it.
		return 0, ErrUnknownDataType
	}
}

// needAtLeast returns size when the buffer holds it, and errNeedMore otherwise.
func needAtLeast(buf []byte, size int) (int, error) {
	if len(buf) < size {
		return 0, errNeedMore
	}

	return size, nil
}

// consumeUShortToken handles the tokens framed as type + USHORT length + body.
func (a *resultAccountant) consumeUShortToken(token byte, buf []byte) (int, error) {
	if len(buf) < 3 {
		return 0, errNeedMore
	}

	length := int(binary.LittleEndian.Uint16(buf[1:3]))

	total := 3 + length
	if len(buf) < total {
		return 0, errNeedMore
	}

	if token == tokenError && a.failure == nil {
		if msg, err := parseInfoBody(buf[3:total]); err == nil {
			a.failure = &msg
		}
	}

	return total, nil
}

// consumeULongToken handles the two tokens framed with a DWORD length.
func consumeULongToken(buf []byte) (int, error) {
	if len(buf) < 5 {
		return 0, errNeedMore
	}

	length := int(binary.LittleEndian.Uint32(buf[1:5]))
	if length < 0 {
		return 0, ErrTokenTruncated
	}

	total := 5 + length
	if len(buf) < total {
		return 0, errNeedMore
	}

	return total, nil
}

// consumeDone folds one DONE-family token into the counters.
//
// DONEPROC's count duplicates the DONEINPROC that preceded it inside the same
// procedure, so only DONE and DONEINPROC contribute — otherwise every
// sp_executesql would report its rows twice.
func (a *resultAccountant) consumeDone(token byte, buf []byte) (int, error) {
	total, err := needAtLeast(buf, doneTokenLen)
	if err != nil {
		return 0, err
	}

	done, err := parseDoneBody(buf[1:total])
	if err != nil {
		return 0, err
	}

	if done.HasCount && token != tokenDoneProc {
		a.doneRows += done.RowCount
		a.haveCount = true
	}

	return total, nil
}

// consumeFeatureExtAck skips a feature-acknowledgement list.
func consumeFeatureExtAck(buf []byte) (int, error) {
	pos := 1

	for {
		if pos >= len(buf) {
			return 0, errNeedMore
		}

		id := buf[pos]
		pos++

		if id == featureExtAckTerminator {
			return pos, nil
		}

		if pos+4 > len(buf) {
			return 0, errNeedMore
		}

		length := int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
		pos += 4

		if length < 0 {
			return 0, ErrTokenTruncated
		}

		if pos+length > len(buf) {
			return 0, errNeedMore
		}

		pos += length
	}
}

// consumeColMetadata parses the column shape of a result set.
func (a *resultAccountant) consumeColMetadata(buf []byte) (int, error) {
	if len(buf) < 3 {
		return 0, errNeedMore
	}

	count := int(binary.LittleEndian.Uint16(buf[1:3]))
	pos := 3

	// 0xFFFF means "no metadata", which keeps whatever was in force.
	if count == 0xFFFF {
		return pos, nil
	}

	cols := make([]column, 0, count)

	for range count {
		col, next, err := parseColumnData(buf, pos)
		if err != nil {
			return 0, incompleteAs(err)
		}

		cols = append(cols, col)
		pos = next
	}

	a.cols = cols
	a.rows = nil
	a.capturedBytes = 0

	return pos, nil
}

// parseColumnData decodes one COLMETADATA column entry.
func parseColumnData(buf []byte, pos int) (column, int, error) {
	const userTypeAndFlags = 6 // ULONG UserType + USHORT Flags on TDS 7.2+

	if pos+userTypeAndFlags > len(buf) {
		return column{}, 0, ErrTokenTruncated
	}

	pos += userTypeAndFlags

	info, pos, err := parseTypeInfo(buf, pos)
	if err != nil {
		return column{}, 0, err
	}

	// The legacy LOBs carry the table they came from before their name.
	if info.id == typeText || info.id == typeNText || info.id == typeImage {
		if pos >= len(buf) {
			return column{}, 0, ErrTokenTruncated
		}

		parts := int(buf[pos])
		pos++

		for range parts {
			if _, pos, err = readUSVarchar(buf, pos); err != nil {
				return column{}, 0, err
			}
		}
	}

	name, pos, err := readBVarchar(buf, pos)
	if err != nil {
		return column{}, 0, err
	}

	return column{name: name, info: info}, pos, nil
}

// consumeRow parses a ROW (or NBCROW) token, capturing it when result storage
// is on.
func (a *resultAccountant) consumeRow(buf []byte, nbc bool) (int, error) {
	if len(a.cols) == 0 {
		return 0, ErrUnknownDataType
	}

	pos := 1

	var nullBitmap []byte

	if nbc {
		size := (len(a.cols) + 7) / 8
		if pos+size > len(buf) {
			return 0, errNeedMore
		}

		nullBitmap = buf[pos : pos+size]
		pos += size
	}

	values := make([]any, len(a.cols))

	for i, col := range a.cols {
		if nbc && nullBitmap[i/8]&(1<<(i%8)) != 0 {
			values[i] = nil

			continue
		}

		raw, isNull, next, err := readValue(col.info, buf, pos)
		if err != nil {
			return 0, incompleteAs(err)
		}

		pos = next

		if !a.captureOff {
			values[i] = jsonValueFor(col.info, raw, isNull)
		}
	}

	a.rowsSeen++

	if !a.captureOff {
		a.capture(values)
	}

	return pos, nil
}

// incompleteAs maps a truncation reported by the value/type decoders onto
// "wait for the next packet". Only the end of the *message* can tell the two
// apart, and finish() is where that is decided.
func incompleteAs(err error) error {
	if errors.Is(err, ErrTokenTruncated) {
		return errNeedMore
	}

	return err
}

// capture serializes one row for storage, honoring the query-storage limits.
func (a *resultAccountant) capture(values []any) {
	limits := a.session.server.queryStorage

	if a.truncated {
		return
	}

	if limits.MaxResultRows > 0 && len(a.rows) >= limits.MaxResultRows {
		a.truncated = true

		return
	}

	encoded, err := json.Marshal(values)
	if err != nil {
		return
	}

	size := int64(len(encoded))
	if limits.MaxResultBytes > 0 && a.capturedBytes+size > limits.MaxResultBytes {
		a.truncated = true

		return
	}

	a.rows = append(a.rows, store.QueryRow{
		RowNumber:    len(a.rows) + 1,
		RowData:      json.RawMessage(encoded),
		RowSizeBytes: size,
	})
	a.capturedBytes += size
}

// consumeReturnValue parses a RETURNVALUE token, keeping the first integer one
// because that is where a prepare form returns its statement handle.
func (a *resultAccountant) consumeReturnValue(buf []byte) (int, error) {
	const ordinalLen = 2

	pos := 1 + ordinalLen

	if pos > len(buf) {
		return 0, errNeedMore
	}

	_, pos, err := readBVarchar(buf, pos)
	if err != nil {
		return 0, incompleteAs(err)
	}

	// Status (1) + UserType (4) + Flags (2).
	const statusUserTypeFlags = 7

	if pos+statusUserTypeFlags > len(buf) {
		return 0, errNeedMore
	}

	pos += statusUserTypeFlags

	info, pos, err := parseTypeInfo(buf, pos)
	if err != nil {
		return 0, incompleteAs(err)
	}

	raw, isNull, pos, err := readValue(info, buf, pos)
	if err != nil {
		return 0, incompleteAs(err)
	}

	if !a.haveHandl && !isNull {
		if v, ok := integerValue(raw).(int64); ok {
			a.handle = v
			a.haveHandl = true
		}
	}

	return pos, nil
}

// finish records the statement this response answered.
func (a *resultAccountant) finish(ctx context.Context) {
	s := a.session

	if len(a.buf) > 0 && !a.desynced {
		// The message ended mid-token: the walk was wrong somewhere, so trust
		// the tail rather than the counters.
		a.desync(ctx, ErrTokenTruncated)
	}

	// The tail backstop: the last token of a TDS response is always a
	// DONE-family token, so a row count survives even a walk that gave up.
	if !a.haveCount {
		a.readTailDone()
	}

	pending := s.takePending()
	if pending == nil {
		return
	}

	if pending.prepareFor != "" && a.haveHandl {
		s.rememberPrepared(a.handle, pending.prepareFor)
	}

	outcome := queryOutcome{
		rows:      a.rows,
		truncated: a.truncated,
	}

	if a.haveCount {
		rows := a.doneRows
		outcome.rowsAffected = &rows
	} else if a.rowsSeen > 0 {
		rows := a.rowsSeen
		outcome.rowsAffected = &rows
	}

	if a.failure != nil {
		text := a.failure.String()
		outcome.queryError = &text
	}

	s.recordQuery(ctx, pending, outcome)
}

// readTailDone decodes the DONE token sitting at the end of the message.
func (a *resultAccountant) readTailDone() {
	if a.tailLen < doneTokenLen {
		return
	}

	switch a.tail[0] {
	case tokenDone, tokenDoneInProc:
	default:
		return
	}

	done, err := parseDoneBody(a.tail[1:])
	if err != nil || !done.HasCount {
		return
	}

	a.doneRows = done.RowCount
	a.haveCount = true
}

// setPending registers the statement whose response is about to arrive.
func (s *session) setPending(pending *pendingQuery) {
	s.pendingMu.Lock()
	s.pending = pending
	s.pendingMu.Unlock()
}

// takePending removes and returns the in-flight statement, if any.
func (s *session) takePending() *pendingQuery {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	pending := s.pending
	s.pending = nil

	return pending
}

// queryOutcome is everything the response said about one statement.
type queryOutcome struct {
	rows         []store.QueryRow
	rowsAffected *int64
	queryError   *string
	truncated    bool
}

// recordQuery writes one query log row (asynchronously) with its completion
// fields, stores the captured rows, and bumps the connection and grant counters.
//
// It mirrors the MySQL and MongoDB proxies down to the ordering: a row with
// captured results is inserted bare and completed only once the rows have
// landed, so a query never reads as finished while its rows are still arriving.
func (s *session) recordQuery(ctx context.Context, pending *pendingQuery, outcome queryOutcome) {
	if s.connection == nil || s.server.store == nil {
		return
	}

	// Last gate before the store: never persist an "error" that is not readable
	// text. A TDS ERROR token's message is decoded UCS-2, but the sanitiser is
	// what guarantees no decoder mistake ever reaches queries.error as bytes.
	queryError := shared.SanitizeQueryError(ctx, s.logger, outcome.queryError)

	bytesTransferred := s.takeByteDelta()

	durationMs := float64(time.Since(pending.start).Microseconds()) / 1000.0

	record := &store.Query{
		ConnectionID:     s.connection.UID,
		SQLText:          pending.sqlText,
		Parameters:       pending.params,
		ExecutedAt:       pending.start,
		DurationMs:       &durationMs,
		RowsAffected:     outcome.rowsAffected,
		Error:            queryError,
		ResultsTruncated: outcome.truncated,
	}

	hasRows := len(outcome.rows) > 0

	go s.persistQuery(ctx, pending.approvalUID, record, outcome, hasRows, bytesTransferred)

	s.chargeGrant(bytesTransferred)
}

// persistQuery is the asynchronous half of recordQuery.
func (s *session) persistQuery(
	ctx context.Context,
	approvalUID uuid.UUID,
	record *store.Query,
	outcome queryOutcome,
	hasRows bool,
	bytesTransferred int64,
) {
	sink := s.server.rowWriter.NewSink()

	queryUID := approvalUID
	held := queryUID != uuid.Nil

	if !held {
		insert := record

		if hasRows {
			bare := *record
			bare.DurationMs = nil
			bare.RowsAffected = nil
			bare.Error = nil
			bare.ResultsTruncated = false
			insert = &bare
		}

		created, err := s.server.store.CreateQuery(ctx, insert)
		if err != nil {
			s.logger.ErrorContext(ctx, "MSSQL create query log failed", slog.Any("error", err))
			sink.Fail()

			return
		}

		queryUID = created.UID
	}

	sink.Resolve(queryUID)

	// Blocking hand-off: the rows are already resident, so queueing them costs
	// no extra memory and dropping them would lose a capture that used to be
	// stored whole.
	sink.AddAll(ctx, outcome.rows)
	sink.Flush(ctx)

	record.ResultsDropped = sink.Dropped()

	if held || hasRows || record.ResultsDropped {
		if err := s.server.store.UpdateQueryCompletion(
			ctx, queryUID, record.DurationMs, record.RowsAffected, record.Error,
			outcome.truncated, record.ResultsDropped,
		); err != nil {
			s.logger.ErrorContext(ctx, "MSSQL complete query log failed", slog.Any("error", err))
		}
	}

	s.publisher.Query(queryUID, record)

	if bytesTransferred > 0 {
		if err := s.server.store.IncrementConnectionStats(ctx, s.connection.UID, bytesTransferred); err != nil {
			s.logger.DebugContext(ctx, "MSSQL increment connection stats failed", slog.Any("error", err))
		}
	}
}

// completeHeldQuery records the outcome on the row an approval hold already
// created, rather than inserting a duplicate.
func (s *session) completeHeldQuery(ctx context.Context, queryUID uuid.UUID, errText string) {
	if s.server.store == nil {
		return
	}

	go func() {
		if err := s.server.store.UpdateQueryCompletion(ctx, queryUID, nil, nil, &errText, false, false); err != nil {
			s.logger.DebugContext(ctx, "MSSQL failed to complete a held query", slog.Any("error", err))
		}
	}()
}

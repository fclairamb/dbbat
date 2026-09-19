package oracle

import "strings"

// Learning the cursor id of a `SYS_REFCURSOR` a stored procedure hands back.
//
// Every other cursor id dbbat knows is read off the OER that ends the call
// (findCursorIDInResponse). A REF cursor has none: `OPEN p FOR …` runs inside
// the procedure body, so the statement it runs never crosses the wire as a
// parse, and the id the server allotted for it comes back in the call's
// **bind-output** data instead. Until this existed, every fetch the client then
// drove on that cursor named an id the tracker did not hold, and
// refuseUnknownCursor turned it into ORA-01031 under any grant carrying
// read_only, block_ddl or approval patterns — ordinary application code refused.
//
// The walk below is deterministic, never a scan: a wrong id planted in the
// tracker would let a fetch resolve against the wrong statement's grant and
// text, which is worse than the refusal it replaces. See docs/oracle.md,
// "Learning a REF cursor's id".

// TTC message types the bind-output walk needs. They are *message* types inside
// a server payload, which is a different namespace from the client function
// codes in ttc.go — 0x0b is the IO vector here, not OVERSION.
const (
	// ttcMsgBindOutput is go-ora's `case 7`: the block carrying the values of a
	// call's OUT parameters (and, for a query, its row data).
	ttcMsgBindOutput = 0x07

	// ttcMsgIOVector is go-ora's `case 11`: the per-bind direction vector the
	// server sends ahead of the bind-output block on a call's *first* execution.
	ttcMsgIOVector = 0x0b
)

// refCursorMaxOutBinds bounds how many REF cursor descriptors one bind-output
// block may hold. A procedure returning more than a handful of cursors is not a
// shape any recording shows; the bound is here so a drifting walk stops rather
// than looping over whatever the payload happens to contain.
const refCursorMaxOutBinds = 16

// refCursorMaxColumns bounds a REF cursor's column count, mirroring the bound
// parseColumnDescribesMode puts on a describe.
const refCursorMaxColumns = 1000

// refCursorIDsInBindOutput returns the cursor ids the server reported as
// `SYS_REFCURSOR` out-binds in this server payload, in wire order, or nil when
// the payload is not a bind-output block dbbat can walk end to end.
//
// ttcPayload starts at the TTC message-type byte (what extractTTCPayload
// returns). Two shapes reach here, and both are measured rather than assumed
// (testdata/{go_ora,python_thin,jdbc_thin}_refcursor.pcapng):
//
//   - the **first** execution of a call, where the server sends the IO vector
//     (`0x0b`) naming each bind's direction and then the bind-output block; and
//   - a re-execution, where the bind-output block (`0x07`) is the payload's
//     leading byte.
//
// Anything else returns nil, and so does a walk that does not land cleanly on
// the message that follows the block. Returning nil costs only the pre-existing
// behavior — the drive stays an untracked cursor — while a wrong id costs a
// mis-gated statement, so every bound here is deliberately the strict one.
func refCursorIDsInBindOutput(ttcPayload []byte) []uint16 {
	start, ok := bindOutputBodyStart(ttcPayload)
	if !ok {
		return nil
	}

	// The per-column record has a version-dependent tail, exactly as a describe
	// does; try the classic layout first so nothing regresses for a client that
	// negotiates it, then the modern one (what 23ai sends every thin client in
	// the corpus). Same order, and the same reason, as parseColumnDescribes.
	if ids := refCursorIDsAt(ttcPayload, start, false); ids != nil {
		return ids
	}

	return refCursorIDsAt(ttcPayload, start, true)
}

// bindOutputBodyStart returns the offset of the first byte *inside* the
// bind-output block, walking the IO vector ahead of it when one is present.
func bindOutputBodyStart(ttcPayload []byte) (int, bool) {
	if len(ttcPayload) == 0 {
		return 0, false
	}

	switch ttcPayload[0] {
	case ttcMsgBindOutput:
		return 1, true
	case ttcMsgIOVector:
		return afterIOVector(ttcPayload)
	default:
		return 0, false
	}
}

// afterIOVector walks the `0x0b` bind-direction vector and returns the offset
// just past the `0x07` byte that must follow it.
//
// The vector is go-ora's ResultSet.load followed by one direction byte per
// bind:
//
//	[0x0b] skip:byte count:cint(2) hi:cint(4) rows:cint(4) uac:cint(2)
//	       bitvector:dlc  spare:dlc
//	       count x direction:byte   [0x07]
//
// The count is the *bind* count, so it is small; a payload that does not put a
// bind-output message right after that many bytes has not been understood and
// is refused.
func afterIOVector(ttc []byte) (int, bool) {
	c := &dcursor{buf: ttc, pos: 1}

	c.byte()

	count := c.cint()
	count += c.cint() * 0x100

	c.cint() // row count
	c.cint() // UAC buffer length
	c.dlc()  // bit vector
	c.dlc()

	if c.err || count < 0 || count > refCursorMaxColumns {
		return 0, false
	}

	for range count {
		c.byte() // this bind's direction (32 in, 16 out, 48 in/out)
	}

	if c.err || c.pos >= len(ttc) || ttc[c.pos] != ttcMsgBindOutput {
		return 0, false
	}

	return c.pos + 1, true
}

// refCursorIDsAt walks the descriptors in a bind-output body under one of the
// two column-record layouts, and returns their cursor ids — or nil if anything
// about the walk is not fully accounted for.
func refCursorIDsAt(ttc []byte, start int, modern bool) []uint16 {
	c := &dcursor{buf: ttc, pos: start}

	var ids []uint16

	for len(ids) < refCursorMaxOutBinds {
		id, ok := readRefCursorDescriptor(c, modern)
		if !ok {
			break
		}

		ids = append(ids, id)

		// The block is finished the moment the walk lands on the next TTC
		// message. PL/SQL puts one more compressed int between a REF cursor's
		// descriptor and whatever follows it (go-ora reads it as `GetInt(2)`
		// right after cursor.load), so the landing is checked on both sides of
		// that int rather than assuming it is there.
		if landedAfterBindOutput(ttc, c.pos) {
			return ids
		}

		after := c.pos

		c.cint()

		if c.err {
			return nil
		}

		if landedAfterBindOutput(ttc, c.pos) {
			return ids
		}

		if c.pos == after {
			return nil
		}
	}

	return nil
}

// landedAfterBindOutput reports whether pos is where the bind-output block ends:
// the end of the payload, or the first byte of the message that follows it.
//
// The accepted set is the two measured across every REF-cursor recording — the
// summary object (`0x04`) on a re-execution, the return-parameter message
// (`0x08`) on a first execution. It is deliberately not "any plausible message
// type": this is the one check that catches a walk which consumed the wrong
// number of version-gated trailing fields, and a wide set would let exactly that
// drift through. A shape that lands somewhere else learns nothing and keeps the
// behavior it had before this existed — which is the safe direction.
func landedAfterBindOutput(ttc []byte, pos int) bool {
	if pos == len(ttc) {
		return true
	}

	if pos < 0 || pos > len(ttc) {
		return false
	}

	return ttc[pos] == byte(TTCFuncOERR) || ttc[pos] == byte(TTCFuncResponse)
}

// readRefCursorDescriptor reads one `SYS_REFCURSOR` out-bind descriptor at the
// cursor and returns the id it ends with.
//
// The field list is go-ora's RefCursor.load, which is the describe body
// (`case 16:` in the same file, and describeColumnLayout here) plus the cursor
// id:
//
//	len:byte maxRowSize:cint colCount:cint
//	[1 byte] colCount x column-describe record
//	dlc
//	two cints          TTCVersion >= 3
//	two more           TTCVersion >= 4
//	dlc                TTCVersion >= 5
//	cursorID:cint
//
// Every column type must be a known TNSType — the same alignment proof
// parseColumnDescribesMode relies on, and the reason a run of bytes that is not
// a descriptor is rejected before the id is ever read — and the id itself must
// be a plausible 16-bit cursor. The walk is left where it stopped on failure;
// the caller discards the whole result rather than reusing a partial one.
//
// **A descriptor with no columns is refused**, and that bound is doing real
// work rather than tidying up. go-ora's RefCursor.load tolerates `colCount == 0`
// (the column loop is simply skipped), but a zero there would skip the only
// structural proof this walk has: without a single column record to align on,
// what remains is "a short run of small integers ending on a nonzero one that
// lands on 0x04/0x08" — which the bind output of a call with **scalar** OUT
// parameters can satisfy, and which the session gate admits (that call is a
// PL/SQL block, so learnRefCursorIDs offers it here). Planting the id that would
// come out of it is the wrong-entry failure this whole file is bounded against:
// rememberCursor overwrites, so a collision with a tracked cursor would replace
// a real statement's text with the call's — and an anonymous PL/SQL block passes
// `read_only`, where the statement it displaced might not have.
//
// It costs nothing measurable: a `SYS_REFCURSOR` is a query's result set, and no
// recording holds one with zero columns.
// TestScalarOutBindsYieldNoRefCursorID is the other half of this bound.
func readRefCursorDescriptor(c *dcursor, modern bool) (uint16, bool) {
	c.byte() // descriptor length, informational: the fields below are self-sizing
	c.cint() // max row size

	colCount := c.cint()
	if c.err || colCount <= 0 || colCount > refCursorMaxColumns {
		return 0, false
	}

	c.byte()

	for range colCount {
		_, typ := parseColumnDescribe(c, modern)
		if c.err || !isKnownTNSType(typ) {
			return 0, false
		}
	}

	c.dlc()

	// TTCVersion >= 3 and >= 4. Every client in the corpus negotiates past both;
	// one that did not would leave the walk short and fail the landing check
	// rather than reading an id out of the wrong field.
	c.cint()
	c.cint()
	c.cint()
	c.cint()

	c.dlc() // TTCVersion >= 5

	cursorID := c.cint()
	if c.err || cursorID <= 0 || cursorID > cursorReexecMaxID {
		return 0, false
	}

	return uint16(cursorID), true
}

// plsqlCallPrefixes are the statement openings that can return an out-bind. A
// `SYS_REFCURSOR` comes back from an anonymous block or a CALL and from nothing
// else, so a statement that does not open with one of these has no bind output
// for the walk above to be offered.
var plsqlCallPrefixes = []string{"BEGIN", "DECLARE", "CALL"}

// statementIsAPLSQLCall reports whether sql is a statement that could hand back
// a REF cursor out-bind. It is a cheap gate, not a parser: its job is to keep
// refCursorIDsInBindOutput away from the responses of ordinary queries, whose
// row data travels in the very same `0x07` message.
//
// Leading comments are skipped the way the proxy's own comment-aware scanner
// does — a statement dbbat itself annotated, or one a client prefixed with a
// hint, must still be recognized.
func statementIsAPLSQLCall(sql string) bool {
	rest := strings.ToUpper(strings.TrimSpace(sql))

	for {
		switch {
		case strings.HasPrefix(rest, "--"):
			if i := strings.IndexAny(rest, "\r\n"); i >= 0 {
				rest = strings.TrimSpace(rest[i+1:])

				continue
			}

			return false
		case strings.HasPrefix(rest, "/*"):
			i := strings.Index(rest[2:], "*/")
			if i < 0 {
				return false
			}

			rest = strings.TrimSpace(rest[2+i+2:])

			continue
		}

		break
	}

	for _, prefix := range plsqlCallPrefixes {
		if !strings.HasPrefix(rest, prefix) {
			continue
		}

		after := rest[len(prefix):]
		if after == "" || after[0] == ' ' || after[0] == '\t' || after[0] == '\n' || after[0] == '\r' {
			return true
		}
	}

	return false
}

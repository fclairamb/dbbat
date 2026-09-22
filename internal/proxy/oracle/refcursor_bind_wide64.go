package oracle

import "encoding/binary"

// Reading a `SYS_REFCURSOR`'s id out of a call's bind output on the **64-bit**
// OCI dialect — the sqlplus bundled in gvenzl/oracle-free:23-slim, and the
// client CI runs (see isCloseCursorsWide8Header for why "the OCI encoding" is
// two encodings).
//
// The 4-byte dialect's walk in refcursor_bind.go does not fit these bytes, and
// the reason is worth writing down because it is also why this file does not
// simply widen it. Measured 2026-09-20 against
// testdata/oci64_refcursor_bind_output.hex and testdata/oci64_describe.hex,
// both recorded from one live session (capture_oci_fixtures_integration_test.go):
//
//   - the IO vector's fixed header is **50 bytes** where the 4-byte dialect
//     spends 22, and its bind count sits at a different offset;
//   - the descriptor header is the same field list at the same widths, plus one
//     byte between the marker and the first column record — which turned out to
//     be the first record's own lead byte rather than a header field, see
//     describeColumnLayoutWide64;
//   - the descriptor's trailing block — the describe timestamp, four integers,
//     an empty DLC, the cursor id — is **byte-for-byte identical** to the
//     4-byte dialect's;
//   - and the per-column record in between is 25 bytes longer, in the four
//     places parseColumnDescribeWide64 spells out.
//
// That last line used to say the 25 bytes could not be placed, and the walk
// below used to route around the column records because of it: it anchored the
// trailing block on a signature — a DLC of exactly seven bytes carrying an
// Oracle DATE — and validated by landing. What closed it was a second describe
// in the same session (ociDescribeTypedQuery): three object columns at three
// type-name lengths, a SYS.XMLTYPE at a fourth schema length, a CLOB, and an
// ordinary NUMBER placed **last** — which is what separated "the object column"
// from "the column with a type OID" from "the last column"
// (specs/todos/2026-09-21-01-oracle-wide64-column-record-layout.md).
//
// So the middle is now walked rather than scanned, and the walk is the same
// walk the describe path runs — one `parseColumnDescribe` per column, each
// column's type checked against isKnownTNSType, which is the alignment proof
// the anchored version could not use. What has not changed is what happens when
// it is wrong: everything after the columns must still decode and the whole
// block must still **land** on the message that follows it, so a layout this
// file has wrong yields *no id*, never a different number.
//
// It still reads **one** descriptor and requires it to land. The reason is no
// longer the one above — a field walk has no second signature to re-sync on, so
// the `[wrong id, real id]` hazard the single-descriptor rule was written
// against is gone with the scan — but the bound costs nothing measurable and is
// kept: a `SYS_REFCURSOR` is one cursor and no recording holds a call returning
// two. See TestOCI64DecoyTailSignatureIsWalkedStraightPast.
//
// The cross-check is the same one, and it is what says the field is the right
// one rather than a consistently decoded one:
// TestDumpReplay_OCI64RefCursorIDsMatchTheCursorsTheClientDrives pairs the ids
// read here with the ids the client's very next frame drives.

// wide64IOVectorHeaderLen is how many bytes the 64-bit IO vector spends before
// its per-bind direction run. The 4-byte dialect spends 22 on the same fields
// (afterIOVector walks them); this dialect writes wider integers and a pair of
// pointer flags, and every byte between the count and the direction run is zero
// in both recorded shapes — a one-bind REF cursor and a three-bind scalar call —
// so the constant is measured rather than decomposed into fields it cannot be
// decomposed into.
const wide64IOVectorHeaderLen = 50

// wide64IOVectorCountAt is where that header declares how many binds follow, as
// a two-byte little-endian count. It is the one field in the header that varies
// across the recordings (1 for the REF cursor call, 3 for the scalar one),
// which is how it was located.
const wide64IOVectorCountAt = 4

// The per-bind direction byte, as the server reports it. The values are the
// dialect-independent ones go-ora reads (32 in, 16 out, 48 in/out); requiring
// the run to be made of them is what stops a header of the wrong shape from
// being read as one.
const (
	bindDirectionIn    = 32
	bindDirectionOut   = 16
	bindDirectionInOut = 48
)

// wide64TailSignature opens the descriptor's trailing block: a DLC declaring
// seven bytes (four-byte little-endian length) followed by its seven-byte CLR,
// carrying the describe timestamp as an Oracle DATE.
//
// Nothing scans for it any more — the column walk arrives at it — and it is
// kept for the one test that plants a **decoy** copy of it inside the records
// and requires the walk to go straight past.
var wide64TailSignature = []byte{0x07, 0x00, 0x00, 0x00, 0x07}

// wide64TailCursorIDAt is where the cursor id sits relative to that signature's
// first byte, and wide64TailLen how long the whole block is. Same test, same
// reason: it is how the decoy is given a plausible id to be mistaken for.
//
//	+0   07 00 00 00     DLC length: 7
//	+4   07              CLR marker
//	+5   7 bytes         the describe timestamp, an Oracle DATE
//	+12  4 x ub4         the version-gated trailing integers
//	+28  ub4             a DLC length, zero on every recorded descriptor
//	+32  ub4             the cursor id
const (
	wide64TailDateAt     = 5
	wide64TailCursorIDAt = 32
	wide64TailLen        = wide64TailCursorIDAt + 4
)

// refCursorIDsInBindOutputWide64 is refCursorIDsInBindOutput for the 64-bit OCI
// dialect. It returns the one id the block carries, or nil when the payload is
// not a bind-output block this walk can account for end to end.
//
// One id, not a list: see the note at the top of this file for why a walk
// anchored on a scan must not take a second chance after a descriptor that did
// not land.
func refCursorIDsInBindOutputWide64(ttcPayload []byte) []uint16 {
	start, ok := bindOutputBodyStartWide64(ttcPayload)
	if !ok {
		return nil
	}

	id, next, ok := wide64RefCursorDescriptor(ttcPayload, start)
	if !ok {
		return nil
	}

	// The block is finished the moment the walk lands on the next TTC message,
	// with or without the one integer PL/SQL puts between a REF cursor's
	// descriptor and whatever follows it — the same two-sided landing check
	// refCursorIDsAt makes. Anything else means the anchor was not the real
	// trailing block, and the id it produced is discarded rather than kept.
	if !landedAfterBindOutput(ttcPayload, next) && !landedAfterBindOutput(ttcPayload, next+2) {
		return nil
	}

	return []uint16{id}
}

// bindOutputBodyStartWide64 returns the offset of the first byte inside the
// bind-output block, walking the 64-bit IO vector ahead of it when one is
// there.
//
// The header is a fixed span rather than a field walk (see
// wide64IOVectorHeaderLen), so what validates it is what comes after: the bind
// count the header declares must be followed by exactly that many direction
// bytes, each a direction the server actually sends, and then by the
// bind-output message byte. A payload that does not end that way has not been
// understood and is refused.
func bindOutputBodyStartWide64(ttc []byte) (int, bool) {
	if len(ttc) == 0 {
		return 0, false
	}

	if ttc[0] == ttcMsgBindOutput {
		return 1, true
	}

	if ttc[0] != ttcMsgIOVector || len(ttc) < wide64IOVectorHeaderLen {
		return 0, false
	}

	count := int(binary.LittleEndian.Uint16(ttc[wide64IOVectorCountAt : wide64IOVectorCountAt+2]))
	if count <= 0 || count > refCursorMaxColumns {
		return 0, false
	}

	end := wide64IOVectorHeaderLen + count
	if end >= len(ttc) {
		return 0, false
	}

	for _, direction := range ttc[wide64IOVectorHeaderLen:end] {
		switch direction {
		case bindDirectionIn, bindDirectionOut, bindDirectionInOut:
		default:
			return 0, false
		}
	}

	if ttc[end] != ttcMsgBindOutput {
		return 0, false
	}

	return end + 1, true
}

// wide64RefCursorDescriptor reads one `SYS_REFCURSOR` descriptor starting at
// the cursor and returns its id plus the offset just past the descriptor.
//
// It is readRefCursorDescriptor's field list at this dialect's widths, and it
// reaches the cursor id **through** the column records rather than around them:
// the descriptor's own header, then one parseColumnDescribe per column, then
// the trailing block the 4-byte dialect shares byte for byte.
//
//	len:byte maxRowSize:ub4 colCount:ub4 [1 marker byte]
//	colCount x column-describe record     (parseColumnDescribeWide64)
//	dlc                                   the describe timestamp
//	four ub4                              the version-gated trailing integers
//	dlc                                   empty on every recorded descriptor
//	cursorID:ub4
//
// Note what that last empty DLC costs: four bytes and nothing else. The
// eleven-byte pad an absent type OID carries inside a column record
// (wide64TypeOID) is that field's, not every empty DLC's — which is measured
// here, on this descriptor, and is why the pad lives in one function rather
// than in dcursor.dlc.
func wide64RefCursorDescriptor(ttc []byte, start int) (uint16, int, bool) {
	if start < 0 || start > len(ttc) {
		return 0, 0, false
	}

	c := &dcursor{buf: ttc, pos: start, wide: true, wide64: true}

	c.byte()  // descriptor length, informational: the fields below are self-sizing
	c.intw(4) // max row size

	colCount := c.intw(4)

	// The same bound the 4-byte walk puts on a descriptor, and doing the same
	// work: a REF cursor is a query's result set and no recording holds one with
	// zero columns, while a zero here would skip the column records — and with
	// them the only structural proof this walk has. See readRefCursorDescriptor.
	if c.err || colCount <= 0 || colCount > refCursorMaxColumns {
		return 0, 0, false
	}

	c.byte() // marker

	for range colCount {
		_, typ := parseColumnDescribe(c, false)
		if c.err || !isKnownTNSType(typ) {
			return 0, 0, false
		}
	}

	c.dlc() // the describe timestamp

	// TTCVersion >= 3 and >= 4, exactly as in the 4-byte dialect.
	c.intw(4)
	c.intw(4)
	c.intw(4)
	c.intw(4)

	c.dlc() // TTCVersion >= 5

	cursorID := c.intw(4)
	if c.err || cursorID <= 0 || cursorID > cursorReexecMaxID {
		return 0, 0, false
	}

	return uint16(cursorID), c.pos, true
}

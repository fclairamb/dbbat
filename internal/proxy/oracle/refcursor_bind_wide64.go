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
//     byte between the marker and the first column record;
//   - the descriptor's trailing block — the describe timestamp, four integers,
//     an empty DLC, the cursor id — is **byte-for-byte identical** to the
//     4-byte dialect's;
//   - but the per-column record in between is 25 bytes longer, and *where*
//     those 25 bytes sit cannot be decided from the recordings in hand. Seven
//     of them are ahead of the type OID (the object column pins that), eleven
//     appear only when that OID is absent, and the remaining seven only when
//     the schema and type names are absent too — and the corpus holds exactly
//     one column with any of those three non-empty, so the three effects cannot
//     be separated. Widening the record walk would mean shipping offsets no
//     recording can falsify, which is the one thing this whole area refuses to
//     do.
//
// So the column records are not parsed here at all. The walk is header-driven
// at both ends and anchors the middle on the trailing block's own signature:
// the describe timestamp, which is a DLC of exactly seven bytes carrying an
// Oracle DATE. What makes that safe is the same thing that makes the 4-byte
// walk safe — everything after the anchor must decode and the whole block must
// **land** on the message that follows it. A layout this file has wrong yields
// *no id*, which is the behaviour an OCI session had before any of this
// existed; it does not yield a different number.
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

// wide64DescriptorHeaderLen is the bind-output descriptor's header: the length
// byte, the four-byte max row size, the four-byte column count, the marker
// byte, and the one further byte this dialect writes before the first column
// record.
const wide64DescriptorHeaderLen = 1 + 4 + 4 + 1 + 1

// wide64DescriptorColCountAt is where that header declares the cursor's column
// count, relative to the descriptor's first byte.
const wide64DescriptorColCountAt = 5

// wide64TailScanLimit bounds how far past a descriptor's header the trailing
// block may be looked for. A REF cursor's whole descriptor is a few hundred
// bytes; the bound is here so a payload that is not one stops the walk rather
// than making it read to the end.
const wide64TailScanLimit = 8192

// wide64TailSignature opens the descriptor's trailing block: a DLC declaring
// seven bytes (four-byte little-endian length) followed by its seven-byte CLR.
// What it carries is the describe timestamp, an Oracle DATE — which is checked
// for being one, so the anchor is seven constrained bytes rather than five.
var wide64TailSignature = []byte{0x07, 0x00, 0x00, 0x00, 0x07}

// wide64TailCursorIDAt is where the cursor id sits, relative to the signature's
// first byte: past the seven-byte DATE, the four integers the descriptor ends
// with, and the empty DLC before it. Identical to the 4-byte dialect's trailing
// block, walked there by readRefCursorDescriptor and spelled out here because
// this walk does not reach it through the column records.
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
// dialect. It returns the ids in wire order, or nil when the payload is not a
// bind-output block this walk can account for end to end.
func refCursorIDsInBindOutputWide64(ttcPayload []byte) []uint16 {
	start, ok := bindOutputBodyStartWide64(ttcPayload)
	if !ok {
		return nil
	}

	var ids []uint16

	for len(ids) < refCursorMaxOutBinds {
		id, next, ok := wide64RefCursorDescriptor(ttcPayload, start)
		if !ok {
			return nil
		}

		ids = append(ids, id)

		// The block is finished the moment the walk lands on the next TTC
		// message, with or without the one integer PL/SQL puts between a REF
		// cursor's descriptor and whatever follows it — the same two-sided
		// landing check refCursorIDsAt makes.
		if landedAfterBindOutput(ttcPayload, next) {
			return ids
		}

		if landedAfterBindOutput(ttcPayload, next+2) {
			return ids
		}

		start = next
	}

	return nil
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
// The header is read at pinned offsets and the trailing block is found by its
// signature — the **first** match, never the first one that happens to work. A
// walk that retried later matches until something decoded would be a search for
// a plausible number, which is precisely the failure this file is bounded
// against.
func wide64RefCursorDescriptor(ttc []byte, start int) (uint16, int, bool) {
	if start < 0 || start+wide64DescriptorHeaderLen > len(ttc) {
		return 0, 0, false
	}

	colCount := int(binary.LittleEndian.Uint32(
		ttc[start+wide64DescriptorColCountAt : start+wide64DescriptorColCountAt+4]))

	// The same bound the 4-byte walk puts on a descriptor, and doing the same
	// work: a REF cursor is a query's result set and no recording holds one with
	// zero columns, while a zero here would let a run of small integers stand in
	// for a descriptor. See readRefCursorDescriptor.
	if colCount <= 0 || colCount > refCursorMaxColumns {
		return 0, 0, false
	}

	tail, ok := wide64DescriptorTailAt(ttc, start+wide64DescriptorHeaderLen)
	if !ok {
		return 0, 0, false
	}

	cursorID := int(binary.LittleEndian.Uint32(
		ttc[tail+wide64TailCursorIDAt : tail+wide64TailCursorIDAt+4]))
	if cursorID <= 0 || cursorID > cursorReexecMaxID {
		return 0, 0, false
	}

	return uint16(cursorID), tail + wide64TailLen, true
}

// wide64DescriptorTailAt finds the descriptor's trailing block: the first
// offset at or after from where the signature sits and the DATE behind it is a
// plausible one.
func wide64DescriptorTailAt(ttc []byte, from int) (int, bool) {
	if from < 0 {
		return 0, false
	}

	limit := from + wide64TailScanLimit
	if limit > len(ttc) {
		limit = len(ttc)
	}

	for at := from; at+wide64TailLen <= limit; at++ {
		if !matchesWide64TailSignature(ttc, at) {
			continue
		}

		return at, true
	}

	return 0, false
}

// matchesWide64TailSignature reports whether the descriptor's trailing block
// starts at offset at.
func matchesWide64TailSignature(ttc []byte, at int) bool {
	for i, b := range wide64TailSignature {
		if ttc[at+i] != b {
			return false
		}
	}

	return isOracleDateRun(ttc[at+wide64TailDateAt : at+wide64TailDateAt+7])
}

// isOracleDateRun reports whether seven bytes are an Oracle DATE as the server
// writes one: excess-100 century and year, then month, day, and hour/minute/
// second stored one greater than they are.
//
// It is the half of the anchor that does the work. Five signature bytes alone
// are a run a payload could hold by accident; five plus seven bytes that have
// to spell a real date is not.
func isOracleDateRun(b []byte) bool {
	if len(b) != 7 {
		return false
	}

	century, year, month, day, hour, minute, second := b[0], b[1], b[2], b[3], b[4], b[5], b[6]

	return century >= 100 && century <= 200 &&
		year >= 1 && year <= 200 &&
		month >= 1 && month <= 12 &&
		day >= 1 && day <= 31 &&
		hour >= 1 && hour <= 24 &&
		minute >= 1 && minute <= 60 &&
		second >= 1 && second <= 60
}

package oracle

import (
	"testing"
)

// TestDecodeExecSQL_SQLclLowercase verifies that SQLcl/JDBC thin's func=0x11
// exec — whose SQL follows a run of zero bytes (no length prefix) and is sent
// verbatim in lowercase — is extracted by the case-insensitive keyword
// fallback. Before the fix, findSQLInPayload matched only uppercase keywords,
// so the SQL was lost ("could not find SQL text in JDBC exec payload").
//
// Fixture: real "select 'sqlcl-ok' as m, 7*6 as a from dual" exec from SQLcl
// 26.1.2 against Oracle 23ai, taken as the TTC payload (after the 2-byte data
// flags, starting at the 0x11 func code).
func TestDecodeExecSQL_SQLclLowercase(t *testing.T) {
	t.Parallel()

	ttc := mustHex(t, "116910000101010101035e11000280210001012a01010d000004ffffffff0132047fffffff00000000000000000000000100000000000000000000000000000073656c656374202773716c636c2d6f6b27206173206d2c20372a3620617320612066726f6d206475616c0101000000000000010100028000000000")

	res, err := decodeExecSQL(ttc)
	if err != nil {
		t.Fatalf("decodeExecSQL failed: %v", err)
	}

	want := "select 'sqlcl-ok' as m, 7*6 as a from dual"
	if res.SQL != want {
		t.Fatalf("SQL mismatch:\n got=%q\nwant=%q", res.SQL, want)
	}
}

// modernSQLclDescribeFixture is a real func=0x10 QueryResult describe for
// "select 'XX' as m, 7*6 as a from dual" (2 columns) from SQLcl 26.1.2 against
// Oracle 23ai, taken as the TTC payload. Its column records carry the modern
// (TTCVersion ≥ 20) trailing layout — data-use-case domain DLCs, an annotations
// count, and three further ints — that the classic parser misaligns on.
const modernSQLclDescribeFixture = "10173d20afa10f9cb3a32fc3f5f88e89224b787e06140a081f010401028260800000010200000000020369010102023ffe01010101014d00000000000000000000020000817f010200000000000000000101010101410000010100000000000000010707787e06140a081f00021fe80102010200062201020001320000000702585802c12b08010603227f8e0001030000000000000401010138010102057b00000103012003000000000000000000000000110001010000000002057b0101010300194f52412d30313430333a206e6f206461746120666f756e640a"

// TestDecodeQueryResultV2_ModernDescribeNoPanic guards the dlc() negative-length
// fix: a misaligned modern describe must never panic (`data[:-127]` once crashed
// the whole proxy process).
func TestDecodeQueryResultV2_ModernDescribeNoPanic(t *testing.T) {
	t.Parallel()

	_ = decodeQueryResultV2(mustHex(t, modernSQLclDescribeFixture))
}

// TestDecodeQueryResultV2_ModernDescribeColumns verifies the modern-layout
// describe parse recovers the column names and types — without it,
// parseColumnDescribes bailed out and SQLcl SELECTs captured no columns (and
// therefore no rows). Expects M (CHAR/96) and A (NUMBER/2).
func TestDecodeQueryResultV2_ModernDescribeColumns(t *testing.T) {
	t.Parallel()

	res := decodeQueryResultV2(mustHex(t, modernSQLclDescribeFixture))
	if res == nil {
		t.Fatal("decodeQueryResultV2 returned nil")
	}

	wantNames := []string{"M", "A"}
	if len(res.Columns) != len(wantNames) {
		t.Fatalf("columns = %v, want %v", res.Columns, wantNames)
	}

	for i, want := range wantNames {
		if res.Columns[i] != want {
			t.Fatalf("column %d = %q, want %q (all: %v)", i, res.Columns[i], want, res.Columns)
		}
	}

	wantTypes := []int{tnsTypeCHAR, tnsTypeNUMBER}
	if len(res.ColumnTypes) != len(wantTypes) {
		t.Fatalf("column types = %v, want %v", res.ColumnTypes, wantTypes)
	}

	for i, want := range wantTypes {
		if res.ColumnTypes[i] != want {
			t.Fatalf("column %d type = %d, want %d (all: %v)", i, res.ColumnTypes[i], want, res.ColumnTypes)
		}
	}
}

// modernSQLclRowFixture is a real func=0x10 QueryResult for
// "select 'sqlcl-ok' as m, 7*6 as a from dual" from SQLcl 26.1.2 against Oracle
// 23ai. The first column value "sqlcl-ok" is 8 bytes, so its length prefix is
// 0x08 — which the row scanner once mistook for the end-of-rows footer, dropping
// the row entirely.
const modernSQLclRowFixture = "10172a3e2cd14bedc60281caf2ab09f03cc0787e0614170610010a01028260800000010800000000020369010108023ffe01010101014d00000000000000000000020000817f010200000000000000000101010101410000010100000000000000010707787e0614171d1c00021fe8010201020006220102000132000000070873716c636c2d6f6b02c12b0801060323f9e500010300000000000004010101af010102057b000001030003000000000000000000000000110001010000000002057b0101010300194f52412d30313430333a206e6f206461746120666f756e640a"

// TestDecodeQueryResultV2_ModernDescribeRows verifies an 8-byte first column
// value (length prefix 0x08) is captured as a row rather than being swallowed by
// the end-of-rows-footer check. Expects one row [sqlcl-ok, 42].
func TestDecodeQueryResultV2_ModernDescribeRows(t *testing.T) {
	t.Parallel()

	res := decodeQueryResultV2(mustHex(t, modernSQLclRowFixture))
	if res == nil {
		t.Fatal("decodeQueryResultV2 returned nil")
	}

	if len(res.Rows) != 1 {
		t.Fatalf("rows = %v, want one row", res.Rows)
	}

	want := []string{"sqlcl-ok", "42"}
	for i, w := range want {
		if i >= len(res.Rows[0]) || res.Rows[0][i] != w {
			t.Fatalf("row = %v, want %v", res.Rows[0], want)
		}
	}
}

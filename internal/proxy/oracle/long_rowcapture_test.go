package oracle

// A genuine LONG / LONG RAW column, which is the family the LOB reading walked
// past without noticing.
//
// go-ora's default LOB policy works by re-declaring a CLOB as a LONG VARCHAR
// and a BLOB as a LONG RAW in its define block, so what the inline LOB shape
// reads has always *been* a LONG column's value — see readInlineLongColumn.
// Nothing in the corpus, though, selected a column the **describe** itself
// reported as LONG (8) or LONG RAW (24), so whether such a column carries the
// same indicator-and-return-code trailer was an inference rather than a
// measurement. These are the recordings that measure it.
//
// The table is not optional. Oracle allows at most one LONG column per table
// and refuses a LONG in the select list of a `FROM dual` expression query, so
// unlike every other fixture in this package this one has DDL in front of it —
// two tables, one per type, each with a NULL row so the trailer's NULL spelling
// is in the recording as well.

// goOraLongFixture and pythonThinLongFixture are one recording per thin driver,
// each carrying both queries below. Two drivers because the two disagree about
// LOB framing, so there was no reason to assume they agree here.
//
// Regenerate with:
//
//	go test -tags capture -timeout 300s -run 'TestCapture_(GoOra|PythonThin)Long' ./internal/proxy/oracle/
const (
	goOraLongFixture      = "go_ora_long.pcapng"
	pythonThinLongFixture = "python_thin_long.pcapng"
)

// longTableDropDDL and longTableDDL build the two tables the queries below
// select from. They run on the **same session** the fetch does, which is why
// the recordings carry their setup: go-ora opens exactly one working connection
// per process against this server (a second one is answered with EOF, measured
// 2026-09-22 on gvenzl/oracle-free:23-slim), so a tidier setup connection of its
// own would cost the capture the session it is there to record. The replay
// picks the fetch out by SQL marker, so the extra frames cost nothing.
//
// The drops are a list of their own because their failure is expected — the
// capture is re-run against a container that may or may not already hold the
// tables.
var (
	longTableDropDDL = []string{
		`DROP TABLE dbbat_cap_long`,
		`DROP TABLE dbbat_cap_longraw`,
	}

	longTableDDL = []string{
		`CREATE TABLE dbbat_cap_long (n NUMBER, l1 LONG)`,
		`CREATE TABLE dbbat_cap_longraw (n NUMBER, r1 LONG RAW)`,
		`INSERT INTO dbbat_cap_long VALUES (1, 'longvalue-0123456789')`,
		`INSERT INTO dbbat_cap_long VALUES (2, NULL)`,
		`INSERT INTO dbbat_cap_longraw VALUES (1, HEXTORAW('DEADBEEF'))`,
		`INSERT INTO dbbat_cap_longraw VALUES (2, NULL)`,
	}
)

// longQuery and longRawQuery put the LONG column **between** ordinary CHAR
// columns, the same alternation goOraLOBQuery uses and for the same reason: a
// column read at the wrong width drifts the two behind it, and the row is then
// refused rather than captured wrong (rowEndsAtMarker).
//
// `ORDER BY n` sorts on a column that is not in the select list, which is
// allowed — what Oracle refuses is a LONG column in the ORDER BY itself. It is
// there so the value row and the NULL row arrive in a fixed order.
const (
	longQuery = `SELECT 'aaaaaa' AS c1, l1, 'bbbbbb' AS c2, 'cccccc' AS c3
  FROM dbbat_cap_long ORDER BY n`

	longRawQuery = `SELECT 'dddddd' AS c4, r1, 'eeeeee' AS c5, 'ffffff' AS c6
  FROM dbbat_cap_longraw ORDER BY n`
)

// longSQLMarker and longRawSQLMarker pick each statement out of a recording
// that carries both. Each stops at its first column, so it is a substring of
// the text on the wire, and the two share no prefix.
const (
	longSQLMarker    = "'aaaaaa' AS c1"
	longRawSQLMarker = "'dddddd' AS c4"
)

// longColumns and longRawColumns are the two describes, name and TTC type code
// in wire order. The middle column is the one the fixtures exist for: a type
// code the describe itself reports as LONG (8) or LONG RAW (24), which is what
// no other recording in the corpus has.
var (
	longColumns = []columnDesc{
		{Name: "C1", Type: tnsTypeCHAR},
		{Name: "L1", Type: tnsTypeLONG},
		{Name: "C2", Type: tnsTypeCHAR},
		{Name: "C3", Type: tnsTypeCHAR},
	}

	longRawColumns = []columnDesc{
		{Name: "C4", Type: tnsTypeCHAR},
		{Name: "R1", Type: tnsTypeLONGRAW},
		{Name: "C5", Type: tnsTypeCHAR},
		{Name: "C6", Type: tnsTypeCHAR},
	}
)

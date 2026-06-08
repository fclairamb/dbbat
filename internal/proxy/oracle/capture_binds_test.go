package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// TestDumpReplay_Binds checks that bind values are extracted from a piggyback
// exec (the path modern clients like go-ora use for parameterized queries). The
// fixture binds 42 (NUMBER) and "hello" (VARCHAR) to
// `SELECT :1 || '-' || :2 AS v FROM dual`; the fixture is regenerated with:
//
//	go test -tags capture -timeout 120s -run TestCapture_GoOraBinds ./internal/proxy/oracle/
func TestDumpReplay_Binds(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "go_ora_binds.dbbat-dump")

	var got []string

	for _, pkt := range td.Packets {
		if pkt.Direction != dump.DirClientToServer {
			continue
		}

		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData || len(tns.Payload) < ttcDataFlagsSize+1 {
			continue
		}

		fc, _ := parseTTCFunctionCode(tns.Payload)
		if fc != TTCFuncPiggyback {
			continue
		}

		ttc := extractTTCPayload(tns.Payload)
		if !IsPiggybackExecSQL(ttc) {
			continue
		}

		res, derr := decodePiggybackExecSQL(ttc)
		if derr != nil || len(res.BindValues) == 0 {
			continue
		}

		got = res.BindValues
	}

	require.Equal(t, []string{"42", "hello"}, got,
		"piggyback bind values should decode by content (NUMBER 42, string hello)")
}

// TestDecodeBindValue covers the shared bind-value renderer used by both the
// OALL8 and piggyback paths: readable text (incl. UTF-8) stays text, NUMBER
// bytes render as decimal, and other binary falls back to hex.
func TestDecodeBindValue(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "NULL", decodeBindValue(nil))
	assert.Equal(t, "hello", decodeBindValue([]byte("hello")))
	assert.Equal(t, "Éric", decodeBindValue([]byte("Éric")), "valid UTF-8 must not become hex")
	assert.Equal(t, "42", decodeBindValue([]byte{0xc1, 0x2b}), "NUMBER bind decodes to decimal")
	assert.Equal(t, "deadbeef", decodeBindValue([]byte{0xde, 0xad, 0xbe, 0xef}), "other binary is hex")
}

// TestCountBindPlaceholders covers the bind-count heuristic.
func TestCountBindPlaceholders(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 2, countBindPlaceholders("SELECT :1 || '-' || :2 AS v FROM dual"))
	assert.Equal(t, 1, countBindPlaceholders("SELECT * FROM t WHERE a = :1 AND b = :1"), "repeated placeholder is one bind")
	assert.Equal(t, 2, countBindPlaceholders("UPDATE t SET x = :val WHERE id = :id"))
	assert.Equal(t, 0, countBindPlaceholders("SELECT 1 FROM dual"))
}

//go:build capture

package oracle

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// TestAnalyzeDump prints a packet-by-packet protocol summary of a dump file.
// Set DUMP_PATH to choose the dump (default: testdata/go_ora_dml.dbbat-dump).
//
//	go test -tags capture -run TestAnalyzeDump -v ./internal/proxy/oracle/
func TestAnalyzeDump(t *testing.T) {
	path := captureEnv("DUMP_PATH", "testdata/go_ora_dml.dbbat-dump")

	r, err := dump.OpenReader(path)
	if err != nil {
		t.Skipf("cannot open dump %s: %v", path, err)
	}

	defer func() { _ = r.Close() }()

	maxHex := 220

	for i := 0; ; i++ {
		pkt, err := r.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			require.NoError(t, err)
		}

		dir := "C->S"
		if pkt.Direction == dump.DirServerToClient {
			dir = "S->C"
		}

		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil {
			t.Logf("#%03d %s len=%d UNPARSEABLE: %v", i, dir, len(pkt.Data), err)
			continue
		}

		desc := tns.Type.String()

		if tns.Type == TNSPacketTypeData && len(tns.Payload) >= ttcDataFlagsSize+1 {
			fc, _ := parseTTCFunctionCode(tns.Payload)
			desc += " ttc=" + fc.String()

			ttcPayload := extractTTCPayload(tns.Payload)
			if len(ttcPayload) > 1 {
				desc += fmt.Sprintf(" sub=0x%02x", ttcPayload[1])
			}

			if sql := findSQLInPayload(ttcPayload); sql != "" && pkt.Direction == dump.DirClientToServer {
				desc += " sql=" + truncateSQL(strings.ReplaceAll(sql, "\n", " "), 80)
			}
		}

		hexLen := len(tns.Payload)
		if hexLen > maxHex {
			hexLen = maxHex
		}

		t.Logf("#%03d %s len=%d %s\n%s", i, dir, len(pkt.Data), desc, hex.Dump(tns.Payload[:hexLen]))
	}

	_ = os.Stdout.Sync()
}

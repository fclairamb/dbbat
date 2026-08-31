package mongodb

import (
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// attachDump gives a pump session a capture, and returns a reader for it.
func attachDump(t *testing.T, s *Session) func() []dump.Packet {
	t.Helper()

	path := filepath.Join(t.TempDir(), uuid.New().String()+dump.FileExt)

	dw, err := dump.NewWriter(path, dump.Header{
		SessionID:  uuid.New().String(),
		Protocol:   dump.ProtocolMongo,
		StartTime:  time.Now(),
		Connection: map[string]any{"database": "app"},
	}, 10<<20)
	require.NoError(t, err)

	s.dumpWriter = dw

	return func() []dump.Packet {
		require.NoError(t, dw.Close())

		r, err := dump.OpenReader(path)
		require.NoError(t, err)

		defer func() { _ = r.Close() }()

		var out []dump.Packet

		for {
			pkt, err := r.ReadPacket()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}

				require.NoError(t, err)
			}

			out = append(out, *pkt)
		}

		return out
	}
}

// TestDumpCapturesRefusedLegacyOpCode covers both halves of the MongoDB gap on
// one refusal: the client frame used to be recorded only by forward(), so a
// command dbbat blocked never reached the capture at all, and the reply used to
// be recorded only by the upstream pump, so the answer dbbat synthesized did
// not either. A capture of a blocked command was therefore empty in both
// directions — it read as a dropped connection rather than an enforced control.
func TestDumpCapturesRefusedLegacyOpCode(t *testing.T) {
	t.Parallel()

	conn := postAuthSession(legacyFrame(opCodeQuery, legacyOpQueryBody(t)))
	s := newPumpSession(conn)
	read := attachDump(t, s)

	require.ErrorIs(t, s.pumpClientToUpstream(), io.EOF)

	pkts := read()
	require.Len(t, pkts, 2, "the blocked command and the refusal, each recorded once")

	assert.Equal(t, dump.DirClientToServer, pkts[0].Direction)
	assert.Contains(t, string(pkts[0].Data), "otherdb.$cmd", "the blocked command is in the capture")

	assert.Equal(t, dump.DirServerToClient, pkts[1].Direction)
	assert.Contains(t, string(pkts[1].Data), "legacy wire protocol", "the refusal is in the capture")
}

// TestDumpRecordsSynthesizedReplyOnce guards against the recording point moving
// into writeClient without the old call being removed: a duplicated frame in a
// forensic capture is its own bug.
func TestDumpRecordsSynthesizedReplyOnce(t *testing.T) {
	t.Parallel()

	conn := postAuthSession(nil)
	s := newPumpSession(conn)
	read := attachDump(t, s)

	require.NoError(t, s.replyOpMsg(11, unauthorizedDoc("write operations not permitted")))

	pkts := read()
	require.Len(t, pkts, 1)
	assert.Equal(t, dump.DirServerToClient, pkts[0].Direction)
	assert.Contains(t, string(pkts[0].Data), "write operations not permitted")
}

// TestForwardDoesNotDoubleRecord pins that the forwarded path relies on the
// read in pumpClientToUpstream and does not record a second copy of its own.
func TestForwardDoesNotDoubleRecord(t *testing.T) {
	t.Parallel()

	conn := postAuthSession(nil)
	s := newPumpSession(conn)
	s.upstream = &UpstreamConn{conn: &scriptedConn{}}
	read := attachDump(t, s)

	require.NoError(t, s.forward(&message{raw: legacyFrame(opCodeMsg, []byte{1, 2, 3, 4})}))

	assert.Empty(t, read(), "forward records nothing; the read already did")
}

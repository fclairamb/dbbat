package oracle

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

const pipelineWait = 3 * time.Second

type pipelineProbe struct {
	t        *testing.T
	s        *session
	client   net.Conn
	upstream net.Conn
	relay    chan error
	recorder *recordingCompletionStore
}

func newPipelineProbe(t *testing.T) *pipelineProbe {
	t.Helper()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	recorder := newRecordingCompletionStore()
	s.completionStore = recorder

	clientEnd, proxyClient := net.Pipe()
	upstreamEnd, proxyUpstream := net.Pipe()
	s.clientConn = proxyClient
	s.upstreamConn = proxyUpstream
	s.callTurn.upstreamDone = make(chan struct{})

	p := &pipelineProbe{
		t: t, s: s, client: clientEnd, upstream: upstreamEnd,
		relay: make(chan error, 1), recorder: recorder,
	}

	go func() { p.relay <- s.clientToUpstream() }()

	t.Cleanup(func() {
		for _, c := range []net.Conn{clientEnd, proxyClient, upstreamEnd, proxyUpstream} {
			_ = c.Close()
		}
	})

	return p
}

func (p *pipelineProbe) send(pkt *TNSPacket) {
	p.t.Helper()
	require.NoError(p.t, p.client.SetWriteDeadline(time.Now().Add(pipelineWait)))
	require.NoError(p.t, writeTNSPacket(p.client, pkt))
}

func (p *pipelineProbe) sendCall(sql string, cursor uint16) {
	p.t.Helper()
	p.send(&TNSPacket{Type: TNSPacketTypeData, Payload: append([]byte{0, 0}, buildOALL8(sql, nil, cursor)...)})
}

func (p *pipelineProbe) read(timeout time.Duration) (*TNSPacket, error) {
	if err := p.upstream.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}

	return readTNSPacket(p.upstream)
}

// answer plays the upstream: it ends the in-flight statement with a result and
// then reports that it started answering, as upstreamToClient does.
func (p *pipelineProbe) answer(rows int64, errText *string) {
	p.s.trackerMu.Lock()
	p.s.completeQuery(&rows, errText)
	p.s.trackerMu.Unlock()
	p.s.noteUpstreamResponse()
}

func TestPipelinedCall_WaitsForPreviousAnswerAndKeepsItsOwnResult(t *testing.T) {
	t.Parallel()

	p := newPipelineProbe(t)

	p.sendCall("DELETE FROM emp", 1)

	first, err := p.read(pipelineWait)
	require.NoError(t, err)
	assert.Contains(t, string(first.Payload), "DELETE FROM emp")

	// Pipelined: sent before the DELETE was answered.
	p.sendCall("SELECT 1 FROM DUAL", 2)

	_, err = p.read(300 * time.Millisecond)
	require.Error(t, err, "a call sent before the previous answer must be held back")

	errText := "ORA-02292: integrity constraint violated"
	p.answer(0, &errText)

	failed := p.recorder.awaitCreated(t)
	assert.Equal(t, "DELETE FROM emp", failed.SQLText)
	require.NotNil(t, failed.Error)
	assert.Equal(t, errText, *failed.Error, "the DELETE keeps its own error")

	second, err := p.read(pipelineWait)
	require.NoError(t, err)
	assert.Contains(t, string(second.Payload), "SELECT 1 FROM DUAL")

	// The second answer comes much later than the first one.
	const secondAnswerDelay = 1500 * time.Millisecond

	time.Sleep(secondAnswerDelay)
	p.answer(1, nil)

	ok := p.recorder.awaitCreated(t)
	assert.Equal(t, "SELECT 1 FROM DUAL", ok.SQLText)

	require.NotNil(t, failed.DurationMs)
	require.NotNil(t, ok.DurationMs)
	assert.Less(t, *failed.DurationMs, float64(secondAnswerDelay.Milliseconds()),
		"the DELETE's duration stops at its own answer, not at the later one")
	assert.GreaterOrEqual(t, *ok.DurationMs, float64(secondAnswerDelay.Milliseconds())*0.8,
		"the SELECT's duration covers its own wait")
	assert.Nil(t, ok.Error, "the SELECT is not charged the DELETE's error")
	require.NotNil(t, ok.RowsAffected)
	assert.EqualValues(t, 1, *ok.RowsAffected)
}

func TestPipelinedCall_SequentialClientIsNotHeld(t *testing.T) {
	t.Parallel()

	p := newPipelineProbe(t)

	p.sendCall("SELECT 1 FROM DUAL", 1)
	_, err := p.read(pipelineWait)
	require.NoError(t, err)

	// Answered, but the statement stays pending (no end-of-call detected).
	p.s.noteUpstreamResponse()

	p.sendCall("SELECT 2 FROM DUAL", 2)
	second, err := p.read(pipelineWait)
	require.NoError(t, err)
	assert.Contains(t, string(second.Payload), "SELECT 2 FROM DUAL")
}

func TestPipelinedCall_BreakGoesThroughAtOnce(t *testing.T) {
	t.Parallel()

	p := newPipelineProbe(t)

	p.sendCall("SELECT 1 FROM DUAL", 1)
	_, err := p.read(pipelineWait)
	require.NoError(t, err)

	// A break while the call runs, with nothing answered yet.
	marker := &TNSPacket{Type: TNSPacketTypeMarker, Payload: []byte{1, 0, 1}}
	p.send(marker)

	got, err := p.read(pipelineWait)
	require.NoError(t, err)
	assert.Equal(t, TNSPacketTypeMarker, got.Type)
}

func TestPipelinedCall_UpstreamHangupReleasesWaitingCall(t *testing.T) {
	t.Parallel()

	p := newPipelineProbe(t)

	p.sendCall("SELECT 1 FROM DUAL", 1)
	_, err := p.read(pipelineWait)
	require.NoError(t, err)

	p.sendCall("SELECT 2 FROM DUAL", 2)

	time.Sleep(100 * time.Millisecond)
	p.s.callTurn.noteUpstreamGone()

	select {
	case err := <-p.relay:
		require.NoError(t, err)
	case <-time.After(pipelineWait):
		t.Fatal("the relay stayed parked on a call waiting for an upstream that is gone")
	}
}

func TestPipelinedCall_ClientHangupEndsSession(t *testing.T) {
	t.Parallel()

	p := newPipelineProbe(t)

	p.sendCall("SELECT 1 FROM DUAL", 1)
	_, err := p.read(pipelineWait)
	require.NoError(t, err)

	// Unanswered, so the next read is the background one.
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, p.client.Close())

	select {
	case err := <-p.relay:
		require.NoError(t, err)
	case <-time.After(pipelineWait):
		t.Fatal("the relay stayed parked after the client left")
	}
}

func TestPipelinedCall_ClientHangupWhileCallWaitsEndsSession(t *testing.T) {
	t.Parallel()

	p := newPipelineProbe(t)

	p.sendCall("SELECT 1 FROM DUAL", 1)
	_, err := p.read(pipelineWait)
	require.NoError(t, err)

	p.sendCall("SELECT 2 FROM DUAL", 2)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, p.client.Close())

	select {
	case err := <-p.relay:
		require.NoError(t, err)
	case <-time.After(pipelineWait):
		t.Fatal("the relay stayed parked on a held call after the client left")
	}
}

func TestPipelinedCall_BreakBehindWaitingCallStillGoesThrough(t *testing.T) {
	t.Parallel()

	p := newPipelineProbe(t)

	p.sendCall("SELECT 1 FROM DUAL", 1)
	_, err := p.read(pipelineWait)
	require.NoError(t, err)

	p.sendCall("SELECT 2 FROM DUAL", 2)
	p.send(&TNSPacket{Type: TNSPacketTypeMarker, Payload: []byte{1, 0, 1}})

	got, err := p.read(pipelineWait)
	require.NoError(t, err)
	assert.Equal(t, TNSPacketTypeMarker, got.Type, "the break overtakes the held call")

	p.answer(0, nil)

	next, err := p.read(pipelineWait)
	require.NoError(t, err)
	assert.Contains(t, string(next.Payload), "SELECT 2 FROM DUAL")
}

// The tail of a statement too long for reassembly to read arrives as Data
// packets of its own. They are the rest of the call in flight, not a new call,
// so they go upstream at once: holding them for an answer the upstream gives
// only once it has the whole message hung the session.
func TestPipelinedCall_StatementTailIsNotHeld(t *testing.T) {
	t.Parallel()

	p := newPipelineProbe(t)

	p.sendCall("SELECT 1 FROM DUAL", 1)
	_, err := p.read(pipelineWait)
	require.NoError(t, err)

	tail := append([]byte{0, 0}, []byte(strings.Repeat("x", 64))...)
	p.send(&TNSPacket{Type: TNSPacketTypeData, Payload: tail})

	got, err := p.read(pipelineWait)
	require.NoError(t, err, "the tail must not wait for the call's answer")
	assert.Equal(t, tail, got.Payload)
}

func TestIsGatedCall_OnlyCallOpenersAreHeld(t *testing.T) {
	t.Parallel()

	data := func(b ...byte) *TNSPacket {
		return &TNSPacket{Type: TNSPacketTypeData, Payload: append([]byte{0, 0}, b...)}
	}

	assert.True(t, isGatedCall(data(0x03, 0x5e, 0x01)), "an exec")
	assert.True(t, isGatedCall(data(0x0e, 0x01)), "a legacy OALL8")
	assert.True(t, isGatedCall(data(0x11, 0x69, 0x01)), "a call behind a close list")
	assert.False(t, isGatedCall(data('x', 'x', 'x')), "statement text continuing a call")
	assert.False(t, isGatedCall(data()), "an empty Data packet")
	assert.False(t, isGatedCall(&TNSPacket{Type: TNSPacketTypeMarker, Payload: []byte{1, 0, 2}}), "a marker")
}

package oracle

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// bindHeavySelect is the statement whose cursor id Oracle 18c's describe made
// dbbat mis-learn (see TestDumpReplay_18cBindHeavySelectLearnsItsOwnCursorID).
const bindHeavySelect = "SELECT :1 AS a, :2 AS b, :3 AS c, :4 AS d, :5 AS e FROM dual"

// replayCursorLearning18c replays goOra18cCursorLearningDump, both legs, through
// one session with a no-control grant, and hands back what it logged.
func replayCursorLearning18c(t *testing.T) *countingHandler {
	t.Helper()

	logs := newCountingHandler()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	s.logger = slog.New(logs)
	s.clientConn = drainedPipe(t)

	for _, pkt := range loadTestDump(t, goOra18cCursorLearningDump).Packets {
		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData {
			continue
		}

		if pkt.Direction == dump.DirClientToServer {
			s.interceptClientMessage(tns)

			continue
		}

		s.interceptUpstreamMessage(tns)
	}

	return logs
}

// TestDumpReplay_18cCursorLearningNamesNoUnknownCursor is the offline half of
// TestIntegration_CursorIDLearningMissRate on its 18c XE leg, replayed from the
// session that leg recorded.
//
// The nightly of 2026-11-25 failed it with 4 of 81 re-executions naming an
// unknown cursor, all four the re-runs of bindHeavySelect. The mis-learning was
// older than that: those re-executions carry a bind row, and until
// decodeCursorReexec read trailing bind rows (#529) they failed to decode and
// went upstream unlogged, so nothing counted them. Under a statement-shaped
// control each one is an ORA-01031, so the figure is zero, not a rate.
func TestDumpReplay_18cCursorLearningNamesNoUnknownCursor(t *testing.T) {
	t.Parallel()

	logs := replayCursorLearning18c(t)

	assert.Zero(t, logs.count(logMsgUntrackedCursorForwarded),
		"no re-execution in the recorded workload may name a cursor dbbat did not learn")

	// The live leg's own figure once nothing is missed: 77 resolved + the 4 that
	// were not.
	assert.Equal(t, 81, logs.count(logMsgReexecGated))

	runs := 0

	for _, sql := range logs.sqlsFor(logMsgReexecGated) {
		if sql == bindHeavySelect {
			runs++
		}
	}

	assert.Equal(t, 4, runs, "the bind-heavy SELECT is re-executed 4 times after its parse")
}

// TestDumpReplay_18cBindHeavySelectLearnsItsOwnCursorID pins where the wrong id
// came from. 18c's QueryResult for bindHeavySelect carries, inside the describe
// of its fifth column, a 0x04 whose compressed reading passes every other bound
// a status OER is held to:
//
//	04 | 00 | 01 07 | 07 78 7e 0b 19 05 1b 05 | 00 | 02 1f e8 | 01 02 | 01 02
//	   status 0, seq 7, rows (a date), error 0, …, cursor 2
//
// The scan accepted it, 70 bytes ahead of the call's real OER (status 1, seq
// 0x17, ORA-01403, cursor 8), and filed the statement under cursor 2, which
// belonged to another statement. A real OER never reports a zero call status;
// the fixed-width reading already refused one (decodeOERFieldsAtLayout), and
// plausibleStatusOER now does for both encodings.
func TestDumpReplay_18cBindHeavySelectLearnsItsOwnCursorID(t *testing.T) {
	t.Parallel()

	logs := replayCursorLearning18c(t)

	var ids []int64

	for i, sql := range logs.sqlsFor(logMsgLearnedCursorID) {
		if sql == bindHeavySelect {
			ids = append(ids, logs.intsFor(logMsgLearnedCursorID, "cursor_id")[i])
		}
	}

	assert.Equal(t, []int64{8}, ids, "the SELECT's id is the one its OER and its re-executions name")
}

// TestPlausibleStatusOERRefusesAZeroCallStatus is the bound on its own, on the
// exact bytes 18c's describe carried.
func TestPlausibleStatusOERRefusesAZeroCallStatus(t *testing.T) {
	t.Parallel()

	describeBytes := []byte{
		0x04, 0x00, 0x01, 0x07, 0x07, 0x78, 0x7e, 0x0b, 0x19, 0x05, 0x1b, 0x05,
		0x00, 0x02, 0x1f, 0xe8, 0x01, 0x02, 0x01, 0x02,
	}

	info, _ := decodeOERFieldsAt(describeBytes, 0)
	require.NotNil(t, info, "the bytes do decode as the seven OER fields")
	assert.Zero(t, info.CallStatus)
	assert.Equal(t, 2, info.CursorID)
	assert.False(t, plausibleStatusOER(info), "a zero call status is not an OER")

	// The call's real OER from the same packet.
	realOER := []byte{0x04, 0x01, 0x01, 0x01, 0x17, 0x01, 0x01, 0x02, 0x05, 0x7b, 0x00, 0x00, 0x01, 0x08}

	info, _ = decodeOERFieldsAt(realOER, 0)
	require.NotNil(t, info)
	assert.True(t, plausibleStatusOER(info))
	assert.Equal(t, 8, info.CursorID)
}

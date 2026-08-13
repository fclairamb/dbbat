//go:build integration

package oracle

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	go_ora "github.com/sijms/go-ora/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// killSettle is how long the capture waits after issuing the kill before it
// decides the server pushed nothing. Generous: a plain KILL SESSION does
// process cleanup before it returns, and the question being asked is "did
// anything arrive", which only a wait can answer negatively.
const killSettle = 5 * time.Second

// sessionIDPattern reads the `session <sid> <serial>` line the probe prints.
var sessionIDPattern = regexp.MustCompile(`(?m)^session (\d+) (\d+)$`)

// TestIntegration_RealOracleSessionTermination captures what a *real* Oracle
// does to a session it terminates, which is the reference dbbat's own
// asynchronous refusals are judged against
// (TestIntegration_AsyncRefusalAgainstJDBCThin, and docs/oracle.md under "An
// asynchronous refusal: which call number, and whether to send one at all").
//
// The claim being checked is the one the code comment in onLimitViolation
// makes: that TTC has no unsolicited server message, so a server which decides
// to end a session either waits to answer the client's *next* call or drops the
// socket — and never pushes an error at a client that is not listening.
//
// No proxy is involved: ojdbc talks to Oracle through a recording tap, and the
// kill is issued from a second connection. What the tap holds afterwards is
// what Oracle actually sent.
func TestIntegration_RealOracleSessionTermination(t *testing.T) {
	java, jar := requireOJDBC(t)

	container, host, port := startOracleContainer(t)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	service := oracleTestService()

	admin, err := sql.Open("oracle", go_ora.BuildUrl(host, port, service, "system", "oracle", nil))
	require.NoError(t, err)

	t.Cleanup(func() { _ = admin.Close() })
	require.NoError(t, admin.PingContext(context.Background()))

	program := filepath.Join(t.TempDir(), "AsyncProbe.java")
	require.NoError(t, os.WriteFile(program, []byte(asyncProbeProgram), 0o600))

	for _, tc := range []struct {
		name string
		// statement is the ALTER SYSTEM form, with %s for 'sid,serial#'.
		statement string
		// dropsSocket is what the form was measured doing to an idle session:
		// hold the socket open and answer the next call, or hang up.
		dropsSocket bool
		// clientCode is the vendor code ojdbc then reports — 28 for the answered
		// call, 3113 for the hang-up.
		clientCode int
	}{
		// The polite form: Oracle marks the session and lets it discover that
		// at its next call.
		{name: "KILL SESSION", statement: "ALTER SYSTEM KILL SESSION '%s'", clientCode: 28},
		// The impolite ones, which are what dbbat's watchdog imitates.
		{
			name: "KILL SESSION IMMEDIATE", statement: "ALTER SYSTEM KILL SESSION '%s' IMMEDIATE",
			dropsSocket: true, clientCode: 3113,
		},
		{
			name: "DISCONNECT SESSION IMMEDIATE", statement: "ALTER SYSTEM DISCONNECT SESSION '%s' IMMEDIATE",
			dropsSocket: true, clientCode: 3113,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tap := startRecordingTap(t, host, port)

			var (
				pushedWhileIdle int
				closedWhileIdle bool
				killErr         error
				idleMark        int
				record          *tapRecord
			)

			run := jdbcProbeRun{
				java: java, jar: jar, program: program,
				host: tap.host, port: tap.port,
				service: service, user: "system", password: "oracle",
				mode: "session",
			}

			run.onIdle = func(printedSoFar string) {
				record = tap.lastRecord(t)
				idleMark = record.bytesSeen()

				sid, serial := probeSessionID(t, printedSoFar)

				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()

				_, killErr = admin.ExecContext(ctx, fmt.Sprintf(tc.statement, sid+","+serial))

				time.Sleep(killSettle)

				pushedWhileIdle = record.bytesSeen() - idleMark
				closedWhileIdle = record.closed()
			}

			output := run.run(t)

			t.Logf("%s: kill returned %v; while the client was idle Oracle pushed %d bytes "+
				"and %s the socket", tc.name, killErr, pushedWhileIdle,
				map[bool]string{true: "dropped", false: "kept"}[closedWhileIdle])

			// Everything Oracle sent from the moment the client went idle: the
			// silence during the kill, and then whatever answers the client's
			// next call.
			afterIdle := record.bytesFromServer()[idleMark:]
			frames := tappedOERs(t, afterIdle)

			for _, f := range frames {
				t.Logf("%s: OER after the kill: ORA-%05d call=%d (readable: %t) %q",
					tc.name, f.errorCode, f.callNumber, f.callNumberOK, f.message)
			}

			t.Logf("%s: packets after the kill: %v", tc.name, tappedPacketTypes(afterIdle))
			t.Logf("%s: the client reported:\n%s", tc.name, output)

			// The claim every form has to satisfy, and the one dbbat's watchdog
			// rests on: whatever Oracle decides about the session, it writes
			// nothing at a client that is not in a call. A frame pushed here
			// would be consumed as the answer to the *next* call, carrying the
			// previous call's number.
			assert.Zero(t, pushedWhileIdle,
				"a real Oracle pushed %d bytes at an idle client", pushedWhileIdle)

			assert.Equal(t, tc.dropsSocket, closedWhileIdle,
				"measured: the IMMEDIATE forms hang up on an idle session and the plain KILL "+
					"holds the socket to answer the next call")

			assert.Contains(t, output, fmt.Sprintf("after-idle: code=%d ", tc.clientCode),
				"what the client makes of it:\n%s", output)

			if tc.dropsSocket {
				assert.Empty(t, frames,
					"a session dropped while idle gets no frame at all — the client meets a "+
						"closed socket, which is exactly what dbbat's onLimitViolation does")

				return
			}

			// The plain KILL is the reference for dbbat's *answering* refusals:
			// the error is delivered as the answer to the call the client sends
			// next, stamped with that call's own sequence number — never with
			// the number of the call that was in flight when the kill landed.
			require.Len(t, frames, 1, "one OER answers the next call")
			assert.Equal(t, 28, frames[0].errorCode)
			assert.True(t, frames[0].callNumberOK, "the summary object must walk cleanly")
			assert.NotZero(t, frames[0].callNumber,
				"a real server stamps the client's own call number, which is what dbbat copies")
			// Measured shape, and a detail worth keeping: the answer is not a
			// bare Data packet. Oracle 23ai Free sends two Control packets
			// (type 12) ahead of it — ojdbc logs "Break received from server.
			// Responding with reset..." and sends a break marker back — and only
			// then the Data packet carrying the OER. dbbat synthesizes the Data
			// packet alone, which every client tested reads for the refusals
			// that *answer* a call; the break exchange is Oracle interrupting a
			// session, which dbbat has no equivalent of.
			assert.Equal(t, []TNSPacketType{TNSPacketTypeControl, TNSPacketTypeControl, TNSPacketTypeData},
				tappedPacketTypes(afterIdle),
				"measured: two Control packets (the break) then the Data packet with the OER")
		})
	}
}

// probeSessionID reads the sid and serial# the probe printed for its own
// session.
func probeSessionID(t *testing.T, output string) (sid, serial string) {
	t.Helper()

	m := sessionIDPattern.FindStringSubmatch(output)
	require.NotNil(t, m, "the probe never reported its session id:\n%s", output)

	return m[1], m[2]
}

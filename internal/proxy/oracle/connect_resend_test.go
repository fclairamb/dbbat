package oracle

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeResendListener answers every Connect it reads with Resend, resends times,
// then with Accept. It records how many Connects it read.
func fakeResendListener(t *testing.T, resends int) (net.Conn, <-chan int) {
	t.Helper()

	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, server.SetDeadline(time.Now().Add(5*time.Second)))

	seen := make(chan int, 1)

	go func() {
		connects := 0

		defer func() { seen <- connects }()

		for {
			pkt, err := readTNSPacket(server)
			if err != nil || pkt.Type != TNSPacketTypeConnect {
				return
			}

			connects++

			answer := encodeTNSPacket(TNSPacketTypeResend, nil)
			if connects > resends {
				answer = encodeTNSPacket(TNSPacketTypeAccept, []byte{0x01, 0x3b, 0x00, 0x00})
			}

			if _, err := server.Write(answer); err != nil {
				return
			}

			if connects > resends {
				return
			}
		}
	}()

	return client, seen
}

// TestExchangeConnect_FollowsResend is the 18c XE listener: it answers a first
// Connect with Resend, and the same Connect sent again is accepted.
func TestExchangeConnect_FollowsResend(t *testing.T) {
	t.Parallel()

	for _, resends := range []int{0, 1, maxConnectResends} {
		conn, seen := fakeResendListener(t, resends)

		connect := encodeTNSPacket(TNSPacketTypeConnect, []byte("CONNECT"))

		pkt, followed, err := exchangeConnect(conn, connect)
		require.NoError(t, err)
		assert.Equal(t, TNSPacketTypeAccept, pkt.Type)
		assert.Equal(t, resends, followed)
		assert.Equal(t, resends+1, <-seen, "the Connect is re-sent once per Resend")
	}
}

// TestExchangeConnect_GivesUpOnAListenerThatKeepsAskingAgain bounds the loop:
// a peer answering Resend forever must not hold the pre-auth slot.
func TestExchangeConnect_GivesUpOnAListenerThatKeepsAskingAgain(t *testing.T) {
	t.Parallel()

	conn, _ := fakeResendListener(t, 100)

	_, _, err := exchangeConnect(conn, encodeTNSPacket(TNSPacketTypeConnect, []byte("CONNECT")))
	require.ErrorIs(t, err, ErrTooManyConnectResends)
}

package shared_test

import (
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
)

// errMalformed stands in for any failure that is not a hang-up.
var errMalformed = errors.New("malformed startup packet")

func TestClientDisconnectedBeforeStartup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		bytesFromClient int64
		err             error
		wantSentinel    bool
	}{
		{
			name: "clean hang-up having said nothing is the health-probe case",
			err:  io.EOF, wantSentinel: true,
		},
		{
			name: "wrapped EOF still matches",
			err:  fmt.Errorf("peek startup header: %w", io.EOF), wantSentinel: true,
		},
		{
			name: "a reset probe counts too",
			err:  fmt.Errorf("read: %w", syscall.ECONNRESET), wantSentinel: true,
		},
		{
			name: "a socket closed under us during shutdown counts too",
			err:  fmt.Errorf("read: %w", net.ErrClosed), wantSentinel: true,
		},
		{
			name:            "a client that sent bytes is truncated, not silent",
			bytesFromClient: 3, err: io.EOF, wantSentinel: false,
		},
		{
			name: "an unexpected EOF is a truncated frame, not a hang-up",
			err:  io.ErrUnexpectedEOF, wantSentinel: false,
		},
		{
			name: "an unrelated failure is never demoted",
			err:  errMalformed, wantSentinel: false,
		},
		{
			name: "no error, nothing to classify",
			err:  nil, wantSentinel: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := shared.ClientDisconnectedBeforeStartup(tc.bytesFromClient, tc.err)

			if !tc.wantSentinel {
				assert.NoError(t, got)

				return
			}

			require.Error(t, got)
			require.ErrorIs(t, got, shared.ErrClientDisconnectedBeforeStartup)
			// The cause is kept so a DEBUG line can still say what the
			// transport reported.
			require.ErrorIs(t, got, tc.err)
		})
	}
}

func TestIsClientHangup(t *testing.T) {
	t.Parallel()

	assert.True(t, shared.IsClientHangup(io.EOF))
	assert.True(t, shared.IsClientHangup(fmt.Errorf("wrapped: %w", syscall.ECONNRESET)))
	assert.True(t, shared.IsClientHangup(net.ErrClosed))
	assert.False(t, shared.IsClientHangup(io.ErrUnexpectedEOF),
		"a peer that stopped mid-frame is a truncated client, not a hang-up")
	assert.False(t, shared.IsClientHangup(fmt.Errorf("wrapped: %w", errMalformed)))
	assert.False(t, shared.IsClientHangup(nil))
}

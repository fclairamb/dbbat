package postgresql

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// relayParameterStatus drives the real upstream→client relay with one
// ParameterStatus followed by a ReadyForQuery, and returns what the relay
// returned and every message the client received.
func relayParameterStatus(t *testing.T, controls []string, name, value string) ([]pgproto3.BackendMessage, error) {
	t.Helper()

	clientProxyEnd, clientTestEnd := net.Pipe()
	upstreamProxyEnd, upstreamTestEnd := net.Pipe()

	var fromClient, toClient atomic.Int64

	countedClient := shared.NewCountingConn(clientProxyEnd, &fromClient, &toClient)
	grant := &store.Grant{
		ExpiresAt:  time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{Controls: controls},
	}

	s := &Session{
		clientConn:       countedClient,
		clientBackend:    pgproto3.NewBackend(countedClient, countedClient),
		upstreamFrontend: pgproto3.NewFrontend(upstreamProxyEnd, upstreamProxyEnd),
		grant:            grant,
		bytesFromClient:  &fromClient,
		bytesToClient:    &toClient,
		guard:            shared.NewLimitGuard(grant, &fromClient, &toClient),
		extendedState: &extendedQueryState{
			preparedStatements: make(map[string]*preparedStatement),
			portals:            make(map[string]*portalState),
		},
		logger: slog.New(slog.DiscardHandler),
		ctx:    context.Background(),
	}

	go func() {
		var buf []byte

		buf, err := (&pgproto3.ParameterStatus{Name: name, Value: value}).Encode(buf)
		if err != nil {
			return
		}

		buf, err = (&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(buf)
		if err != nil {
			return
		}

		_, _ = upstreamTestEnd.Write(buf)
		_ = upstreamTestEnd.Close()
	}()

	received := make(chan []pgproto3.BackendMessage, 1)

	go func() {
		fe := pgproto3.NewFrontend(clientTestEnd, clientTestEnd)

		var msgs []pgproto3.BackendMessage

		for {
			msg, err := fe.Receive()
			if err != nil {
				received <- msgs

				return
			}

			// Receive reuses its message structs; keep a copy of what matters.
			switch m := msg.(type) {
			case *pgproto3.ErrorResponse:
				c := *m
				msgs = append(msgs, &c)
			case *pgproto3.ParameterStatus:
				c := *m
				msgs = append(msgs, &c)
			case *pgproto3.ReadyForQuery:
				c := *m
				msgs = append(msgs, &c)
			}
		}
	}()

	relayErr := s.proxyUpstreamToClient()

	_ = upstreamProxyEnd.Close()
	_ = clientProxyEnd.Close()
	_ = clientTestEnd.Close()

	return <-received, relayErr
}

// A read-only session whose upstream reports the pin gone is ended before the
// client gets the ReadyForQuery that would let it open an unpinned transaction.
func TestSession_ReadOnlyPinLost_TerminatesSession(t *testing.T) {
	t.Parallel()

	msgs, relayErr := relayParameterStatus(t, []string{store.ControlReadOnly}, "default_transaction_read_only", "off")

	if !errors.Is(relayErr, ErrReadOnlyPinLost) {
		t.Fatalf("proxyUpstreamToClient() = %v, want ErrReadOnlyPinLost", relayErr)
	}

	if len(msgs) != 1 {
		t.Fatalf("client received %d messages (%v), want only the FATAL error", len(msgs), msgs)
	}

	errResp, ok := msgs[0].(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("client received %T, want *pgproto3.ErrorResponse", msgs[0])
	}

	if errResp.Severity != "FATAL" || errResp.Code != "25006" {
		t.Fatalf("error = %s %s, want FATAL 25006", errResp.Severity, errResp.Code)
	}
}

// The tripwire only fires on a read-only grant losing the pin: a report that
// keeps it on, another setting, and a grant without read_only all relay as
// before.
func TestSession_ReadOnlyPinLost_RelaysEverythingElse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		controls []string
		param    string
		value    string
	}{
		{"read-only, pin kept", []string{store.ControlReadOnly}, "default_transaction_read_only", "on"},
		{"read-only, other setting", []string{store.ControlReadOnly}, "application_name", "off"},
		{"full write, pin off", nil, "default_transaction_read_only", "off"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msgs, relayErr := relayParameterStatus(t, tt.controls, tt.param, tt.value)
			// The relay ends when the simulated upstream closes its pipe, which
			// pgproto3 reports as an unexpected EOF; only the tripwire matters.
			if errors.Is(relayErr, ErrReadOnlyPinLost) {
				t.Fatalf("proxyUpstreamToClient() = %v, want the session relayed", relayErr)
			}

			if len(msgs) != 2 {
				t.Fatalf("client received %d messages (%v), want ParameterStatus + ReadyForQuery", len(msgs), msgs)
			}

			if _, ok := msgs[0].(*pgproto3.ParameterStatus); !ok {
				t.Fatalf("first message = %T, want *pgproto3.ParameterStatus", msgs[0])
			}

			if _, ok := msgs[1].(*pgproto3.ReadyForQuery); !ok {
				t.Fatalf("second message = %T, want *pgproto3.ReadyForQuery", msgs[1])
			}
		})
	}
}

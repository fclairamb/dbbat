package oracle

import (
	"fmt"
)

// upstreamAuth performs Oracle authentication on the relay-phase upstream
// socket using stored database credentials. The socket has already
// negotiated TNS Connect / Accept / Set Protocol / Set Data Types with the
// real upstream — keeping that exact socket through AUTH ensures the TTC
// capability levels stay aligned with the client's view, so caps-rich
// drivers (SQLcl JDBC thin) can send OALL8 messages that upstream parses
// correctly.
func (s *session) upstreamAuth() error {
	if err := s.runUpstreamClientAuth(); err != nil {
		return fmt.Errorf("upstream auth failed: %w", err)
	}

	s.logger.InfoContext(s.ctx, "upstream Oracle authentication complete")

	return nil
}

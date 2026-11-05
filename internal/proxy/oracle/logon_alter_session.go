package oracle

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// authKeyAlterSession is the AUTH Phase 2 key whose value the server runs as an
// ALTER SESSION while it establishes the session.
const authKeyAlterSession = "AUTH_ALTER_SESSION"

// ErrLogonAlterSessionRefused — the client's AUTH_ALTER_SESSION failed the gate
// a mid-session ALTER SESSION goes through, or could not be read. The login is
// refused before the client's Phase 2 reaches the upstream.
var ErrLogonAlterSessionRefused = errors.New("logon AUTH_ALTER_SESSION refused")

// logonAlterSessionError carries the reason a logon AUTH_ALTER_SESSION was
// refused, so the client message can name it without parsing error strings.
// It matches ErrLogonAlterSessionRefused under errors.Is.
type logonAlterSessionError struct {
	reason error
}

func (e *logonAlterSessionError) Error() string {
	return ErrLogonAlterSessionRefused.Error() + ": " + e.reason.Error()
}

func (e *logonAlterSessionError) Unwrap() []error {
	return []error{ErrLogonAlterSessionRefused, e.reason}
}

func refuseLogonAlterSession(reason error) error {
	return &logonAlterSessionError{reason: reason}
}

// errLogonAlterSessionUnreadable is the reason when the value cannot be located
// to the byte: dbbat would otherwise forward a statement it never checked.
var errLogonAlterSessionUnreadable = errors.New("dbbat could not read the session settings sent at logon")

// errLogonAlterSessionApproval is the reason when the statement matches one of
// the grant's approval patterns: a login cannot be parked on a human.
var errLogonAlterSessionApproval = errors.New(
	"the session settings sent at logon match an approval pattern, and a login cannot be held for approval")

// logonAlterSessionValues returns every AUTH_ALTER_SESSION value in a client's
// AUTH Phase 2 body (what follows the 2-byte data flags).
//
// The scan is fail-closed rather than best-effort, because whatever it misses
// reaches the server unchecked:
//
//   - every occurrence of the key name, in any letter case, is examined — not
//     just the first, so a decoy inside an earlier value cannot hide the real
//     pair behind it;
//   - each occurrence must sit on a key boundary (its CLR length byte right
//     before it) and its value must decode with a declared length that agrees
//     with the decoded one. Anything else is an error, not a skip.
//
// wide selects the OCI 4-byte little-endian length encoding, bigChunks the
// session's negotiated CLR long form — the same two switches the rewrite that
// forwards the body uses, so the gate reads the value the way it is forwarded.
func logonAlterSessionValues(body []byte, wide, bigChunks bool) ([]string, error) {
	upper := asciiUpper(body)
	key := []byte(authKeyAlterSession)

	var values []string

	for from := 0; ; {
		rel := indexOf(upper[from:], key)
		if rel < 0 {
			return values, nil
		}

		idx := from + rel
		from = idx + 1

		if idx < 1 || int(body[idx-1]) != len(key) {
			return nil, fmt.Errorf("%w: %s outside a key boundary at offset %d",
				errLogonAlterSessionUnreadable, authKeyAlterSession, idx)
		}

		value, err := readLogonAlterSessionValue(body[idx+len(key):], wide, bigChunks)
		if err != nil {
			return nil, err
		}

		values = append(values, value)
	}
}

// readLogonAlterSessionValue decodes the value side of one KV pair — the
// declared length, then the CLR value — and checks the two agree. The thin
// form declares the plain byte count; OCI declares either that or a 3x UTF-8
// expansion (see replaceAuthKVValueWide).
func readLogonAlterSessionValue(tail []byte, wide, bigChunks bool) (string, error) {
	var (
		declared int
		lenSize  int
	)

	if wide {
		if len(tail) < 4 {
			return "", fmt.Errorf("%w: truncated length", errLogonAlterSessionUnreadable)
		}

		declared = int(binary.LittleEndian.Uint32(tail[:4]))
		lenSize = 4
	} else {
		declared, lenSize = readCompressedInt(tail)
		if lenSize == 0 {
			return "", fmt.Errorf("%w: truncated length", errLogonAlterSessionUnreadable)
		}
	}

	if declared == 0 {
		return "", nil
	}

	value, clrN := readCLRVariant(tail[lenSize:], bigChunks)
	if clrN == 0 {
		return "", fmt.Errorf("%w: truncated value", errLogonAlterSessionUnreadable)
	}

	plain := declared == len(value)
	expanded := wide && declared == 3*len(value)

	if !plain && !expanded {
		return "", fmt.Errorf("%w: declared length %d, value length %d",
			errLogonAlterSessionUnreadable, declared, len(value))
	}

	return string(value), nil
}

// asciiUpper returns a copy of b with a-z upper-cased and every other byte,
// binary ones included, left where it was — so offsets in the copy are offsets
// in b. bytes.ToUpper would rewrite invalid UTF-8 and shift them.
func asciiUpper(b []byte) []byte {
	out := make([]byte, len(b))

	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}

		out[i] = c
	}

	return out
}

// checkLogonAlterSession runs every AUTH_ALTER_SESSION value through the gate a
// mid-session statement gets and returns the statements that will be applied.
//
// A value is one statement with trailing NULs (every recorded client ends it
// with one): those and surrounding whitespace are trimmed, an embedded NUL is
// unreadable, an empty value applies nothing. grant nil is checked as a grant
// with no definition, which reports every control — the fail-closed default.
// patterns is the grant's compiled approval patterns when approval holds are
// enabled, nil otherwise.
func checkLogonAlterSession(values []string, grant *store.Grant, patterns *shared.CompiledApprovalPatterns) ([]string, error) {
	if grant == nil {
		grant = &store.Grant{}
	}

	var applied []string

	for _, value := range values {
		stmt := strings.TrimSpace(strings.TrimRight(value, "\x00"))
		if stmt == "" {
			continue
		}

		if strings.ContainsRune(stmt, 0) {
			return nil, refuseLogonAlterSession(fmt.Errorf("%w: embedded NUL", errLogonAlterSessionUnreadable))
		}

		if err := shared.ValidateOracleLogonAlterSession(stmt, grant); err != nil {
			return nil, refuseLogonAlterSession(err)
		}

		if patterns != nil {
			if _, held := patterns.Match(shared.NormalizeSQL(stmt)); held {
				return nil, refuseLogonAlterSession(errLogonAlterSessionApproval)
			}
		}

		applied = append(applied, stmt)
	}

	return applied, nil
}

// gateLogonAlterSession is the session-level entry point, called on the client
// Phase 2 body right before it is rewritten and forwarded upstream. On success
// it remembers the statements so recordLogonAlterSession can put them on file
// once the connection row exists.
func (s *session) gateLogonAlterSession(clientBody []byte) error {
	values, err := logonAlterSessionValues(clientBody, payloadUsesWideKVEncoding(clientBody), s.clientBigClrChunks)
	if err != nil {
		err = refuseLogonAlterSession(err)
	} else {
		var patterns *shared.CompiledApprovalPatterns
		if s.approvalDeps.Enabled && s.grant != nil {
			patterns = shared.CompileApprovalPatterns(s.grant.ApprovalPatterns())
		}

		s.logonAlterSession, err = checkLogonAlterSession(values, s.grant, patterns)
	}

	if err != nil {
		s.logonAlterSession = nil
		s.logger.WarnContext(s.ctx, "refusing Oracle login: AUTH_ALTER_SESSION failed the statement gate",
			slog.Any("error", err))

		return err
	}

	return nil
}

// logonAlterSessionRefusal maps a refused logon AUTH_ALTER_SESSION to the ORA
// code and message the client gets. The reason is dbbat's own text (the grant
// control, the outright block, or "could not read"), never the upstream's.
// ok is false when err is not such a refusal.
func logonAlterSessionRefusal(err error) (uint16, string, bool) {
	var refused *logonAlterSessionError
	if !errors.As(err, &refused) {
		return 0, "", false
	}

	return ORA01031, "dbbat refused the session settings your client sends at logon " +
		"(AUTH_ALTER_SESSION): " + refused.reason.Error(), true
}

// recordLogonAlterSession writes one queries row per statement the logon
// applied, so the session's starting state is on file, in the connection's
// query chain, ahead of anything the client runs. Written synchronously so it
// is the chain's first link.
func (s *session) recordLogonAlterSession() {
	if len(s.logonAlterSession) == 0 || s.completionStore == nil || s.connectionUID == uuid.Nil {
		return
	}

	for _, stmt := range s.logonAlterSession {
		duration := float64(0)

		query := &store.Query{
			ConnectionID: s.connectionUID,
			SQLText:      stmt,
			ExecutedAt:   time.Now(),
			DurationMs:   &duration,
		}

		if _, err := s.completionStore.CreateQuery(s.ctx, query); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to log the logon AUTH_ALTER_SESSION", slog.Any("error", err))

			continue
		}

		if err := s.completionStore.IncrementConnectionStats(s.ctx, s.connectionUID, 0); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to increment connection stats", slog.Any("error", err))
		}
	}
}

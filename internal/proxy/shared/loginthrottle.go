package shared

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/store"
)

// LoginAuditStore is the slice of the store a LoginThrottle writes its audit
// entries through: the event itself, and the user the username names (so the
// entry carries a user_id when the account exists).
type LoginAuditStore interface {
	LogAuditEvent(ctx context.Context, event *store.AuditEvent) error
	GetUserByUsername(ctx context.Context, username string) (*store.User, error)
}

// LoginThrottle puts the REST API's login throttle in front of a proxy
// listener's password check. It wraps the process-wide
// cache.AuthFailureTracker the API uses, so a dbbat password guessed on a
// database port and on /api/v1/auth/login is counted once:
//
//   - Allow, before the hash check: false while the (username, source IP)
//     pair is in backoff. The listener then answers its ordinary
//     authentication error without verifying anything, whatever the password.
//   - Failure, on a wrong password or an unknown user: counts it, and writes
//     the auth.failure_ceiling audit entry (with a listener field) when it
//     brings the username to its ceiling.
//   - Success, on a correct password: clears the source's backoff and the
//     username's ceiling, auditing a success that ends a guessing burst.
//
// API-key logins are not throttled, as on the API: a dbb_ key has far too much
// entropy to guess.
//
// A nil *LoginThrottle throttles nothing, so sessions built by tests without
// one keep working. The tracker is single-process, as on the API.
type LoginThrottle struct {
	tracker  *cache.AuthFailureTracker
	store    LoginAuditStore
	logger   *slog.Logger
	listener string
	enforce  bool
}

// NewLoginThrottle builds the throttle of one listener. listener is the name
// written in the audit entries (postgresql, mysql, mssql, mongodb). enforce
// false keeps counting but never refuses an attempt: it is how test mode runs,
// as the API skips its backoff there. dataStore may be nil (no audit entry).
func NewLoginThrottle(
	tracker *cache.AuthFailureTracker,
	dataStore LoginAuditStore,
	logger *slog.Logger,
	listener string,
	enforce bool,
) *LoginThrottle {
	if logger == nil {
		logger = slog.Default()
	}

	return &LoginThrottle{
		tracker:  tracker,
		store:    dataStore,
		logger:   logger,
		listener: listener,
		enforce:  enforce,
	}
}

// Allow reports whether username may have a password checked from ip.
func (l *LoginThrottle) Allow(ctx context.Context, username, ip string) bool {
	if l == nil || l.tracker == nil || !l.enforce {
		return true
	}

	allowed, retryAfter := l.tracker.CheckRateLimit(username, ip)
	if !allowed {
		l.logger.InfoContext(ctx, "proxy login refused: source in login backoff",
			slog.String("listener", l.listener),
			slog.String("username", username),
			slog.String("client_ip", ip),
			slog.Int("retry_after_seconds", retryAfter))
	}

	return allowed
}

// Failure records a wrong password (or an unknown user) for username from ip.
func (l *LoginThrottle) Failure(ctx context.Context, username, ip string) {
	if l == nil || l.tracker == nil {
		return
	}

	if !l.tracker.RecordFailure(username, ip) {
		return
	}

	l.logger.WarnContext(ctx, "username reached the login failure ceiling; wrong passwords are refused from every source",
		slog.String("listener", l.listener),
		slog.String("username", username),
		slog.String("client_ip", ip),
		slog.Int("failures", cache.AuthUsernameCeiling),
		slog.Duration("window", cache.AuthUsernameCeilingWindow))

	l.audit(ctx, store.AuditEventAuthFailureCeiling, username, map[string]any{
		"username":       username,
		"failures":       cache.AuthUsernameCeiling,
		"window_seconds": int(cache.AuthUsernameCeilingWindow.Seconds()),
		"last_client_ip": ip,
		"listener":       l.listener,
	})
}

// Success clears ip's backoff and username's ceiling after a correct password.
func (l *LoginThrottle) Success(ctx context.Context, username, ip string) {
	if l == nil || l.tracker == nil {
		return
	}

	failures := l.tracker.ResetFailures(username, ip)
	if failures == 0 {
		return
	}

	l.logger.WarnContext(ctx, "correct password during an active guessing burst; login failure ceiling lifted",
		slog.String("listener", l.listener),
		slog.String("username", username),
		slog.String("client_ip", ip),
		slog.Int("failures", failures))

	l.audit(ctx, store.AuditEventAuthSuccessDuringCeiling, username, map[string]any{
		"username":  username,
		"failures":  failures,
		"client_ip": ip,
		"listener":  l.listener,
	})
}

func (l *LoginThrottle) audit(ctx context.Context, eventType, username string, details map[string]any) {
	if l.store == nil {
		return
	}

	raw, err := json.Marshal(details)
	if err != nil {
		return
	}

	event := &store.AuditEvent{EventType: eventType, Details: raw}
	if user, err := l.store.GetUserByUsername(ctx, username); err == nil && user != nil {
		event.UserID = &user.UID
	}

	if err := l.store.LogAuditEvent(ctx, event); err != nil {
		l.logger.ErrorContext(ctx, "failed to audit a proxy login throttle event",
			slog.String("listener", l.listener), slog.Any("error", err))
	}
}

// SourceIP is the address a listener keys the login throttle on: the host part
// of the client's remote address. Behind a TCP load balancer (dbbat does not
// speak the PROXY protocol) every client shares the balancer's address.
func SourceIP(addr net.Addr) string {
	if addr == nil {
		return ""
	}

	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.IP.String()
	}

	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}

	return host
}

// NewDefaultLoginThrottle is the throttle a listener starts with before the
// process wiring hands it the tracker it shares with the API: its own tracker,
// enforced, auditing through dataStore when there is one. It keeps a listener
// built outside main (tests, tools) from being an unthrottled oracle.
func NewDefaultLoginThrottle(dataStore *store.Store, logger *slog.Logger, listener string) *LoginThrottle {
	var auditStore LoginAuditStore
	if dataStore != nil {
		auditStore = dataStore
	}

	return NewLoginThrottle(cache.NewAuthFailureTracker(), auditStore, logger, listener, true)
}

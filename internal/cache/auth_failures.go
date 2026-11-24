package cache

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fclairamb/dbbat/internal/safe"
)

// AuthFailureTracker throttles password guessing on the credential-checking
// endpoints. It keeps two counters:
//
//   - a backoff per (username, client IP): the escalating delays of
//     authBackoffDelays. Keying on the source as well as the account is what
//     stops a stranger who knows a username (admin always exists) from keeping
//     that account locked out with one wrong password every five minutes: their
//     failures lock out their own address, not the account.
//   - a coarse ceiling per username alone (AuthUsernameCeiling failures per
//     AuthUsernameCeilingWindow), the backstop against guessing spread across
//     many addresses. Crossing it blocks the username from every source until
//     the window ends, and is logged and audited by the caller (RecordFailure
//     reports the crossing) rather than happening silently.
type AuthFailureTracker struct {
	mu       sync.RWMutex
	failures map[authFailureKey]*failureRecord
	ceilings map[string]*ceilingRecord
}

// authFailureKey is the backoff key: one account as seen from one address.
type authFailureKey struct {
	username string
	ip       string
}

type failureRecord struct {
	count       int
	lastFailure time.Time
}

// ceilingRecord counts a username's failures, from any source, in a fixed
// window starting at its first failure.
type ceilingRecord struct {
	count       int
	windowStart time.Time
}

// Auth failure rate limiting constants
const (
	AuthFailureResetDuration   = 15 * time.Minute
	authFailureCleanupInterval = 5 * time.Minute

	// AuthUsernameCeiling failures against one username within
	// AuthUsernameCeilingWindow, summed over every source address, put that
	// username under a guessing burst until the window ends: every wrong
	// password, from any source, is answered 429 rather than 401, so the
	// failure feedback stops being useful at scale. The password is still
	// checked, and a correct one is let through and lifts the ceiling, so the
	// burst is a backstop against distributed guessing and not a lockout
	// lever.
	AuthUsernameCeiling       = 100
	AuthUsernameCeilingWindow = time.Hour
)

// goroutineNameAuthFailureSweep is what a panic in the stale-entry sweep is
// logged under. The tracker predates the server's logger and has no access to
// it, so the sweep reports through slog.Default().
const goroutineNameAuthFailureSweep = "auth failure tracker sweep"

// Backoff delays based on failure count
var authBackoffDelays = []struct {
	minFailures int
	delay       time.Duration
}{
	{10, 5 * time.Minute},
	{7, 2 * time.Minute},
	{5, 30 * time.Second},
	{3, 5 * time.Second},
}

// NewAuthFailureTracker creates a new auth failure tracker
func NewAuthFailureTracker() *AuthFailureTracker {
	tracker := &AuthFailureTracker{
		failures: make(map[authFailureKey]*failureRecord),
		ceilings: make(map[string]*ceilingRecord),
	}
	go tracker.cleanup()
	return tracker
}

// cleanup periodically removes stale entries
func (t *AuthFailureTracker) cleanup() {
	ticker := time.NewTicker(authFailureCleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		// Per turn, so a panic neither ends the process nor retires the sweep:
		// a tracker that stopped expiring entries would lock users out on stale
		// failure counts. See safe.RunMaintenance.
		safe.RunMaintenance(context.Background(), slog.Default(), goroutineNameAuthFailureSweep, func() {
			t.sweep(time.Now())
		})
	}
}

// sweep drops the entries that no longer throttle anything at now.
func (t *AuthFailureTracker) sweep(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for key, record := range t.failures {
		if now.Sub(record.lastFailure) > AuthFailureResetDuration {
			delete(t.failures, key)
		}
	}

	for username, record := range t.ceilings {
		if now.Sub(record.windowStart) > AuthUsernameCeilingWindow {
			delete(t.ceilings, username)
		}
	}
}

// CheckRateLimit reports whether username may attempt a password check from
// ip, under the per-source backoff. Returns (allowed, retryAfter in seconds).
// The username-wide ceiling is not consulted here: it never stops the
// password from being checked (see CeilingRetryAfter).
func (t *AuthFailureTracker) CheckRateLimit(username, ip string) (bool, int) {
	now := time.Now()

	t.mu.RLock()
	record, exists := t.failures[authFailureKey{username: username, ip: ip}]
	var recordCopy failureRecord
	if exists {
		recordCopy = *record
	}
	t.mu.RUnlock()

	if !exists {
		return true, 0
	}

	// Auto-reset after reset duration
	if now.Sub(recordCopy.lastFailure) > AuthFailureResetDuration {
		return true, 0
	}

	// Find applicable backoff delay
	for _, backoff := range authBackoffDelays {
		if recordCopy.count >= backoff.minFailures {
			blockedUntil := recordCopy.lastFailure.Add(backoff.delay)
			if now.Before(blockedUntil) {
				return false, retryAfterSeconds(blockedUntil.Sub(now))
			}
			break
		}
	}

	return true, 0
}

// CeilingRetryAfter returns the seconds left in username's guessing burst, or
// 0 when the username is under its ceiling. During a burst a wrong password
// is answered 429 with this Retry-After, from every source.
func (t *AuthFailureTracker) CeilingRetryAfter(username string) int {
	now := time.Now()

	t.mu.RLock()
	ceiling, exists := t.ceilings[username]
	var ceilingCopy ceilingRecord
	if exists {
		ceilingCopy = *ceiling
	}
	t.mu.RUnlock()

	if !exists || ceilingCopy.count < AuthUsernameCeiling {
		return 0
	}

	blockedUntil := ceilingCopy.windowStart.Add(AuthUsernameCeilingWindow)
	if !now.Before(blockedUntil) {
		return 0
	}

	return retryAfterSeconds(blockedUntil.Sub(now))
}

// retryAfterSeconds rounds a remaining block to whole seconds, at least one.
func retryAfterSeconds(d time.Duration) int {
	retryAfter := int(d.Seconds())
	if retryAfter < 1 {
		retryAfter = 1
	}

	return retryAfter
}

// RecordFailure records a failed authentication attempt for username from ip.
// It returns true exactly once per window: on the failure that brings the
// username to AuthUsernameCeiling, so the caller can log and audit it.
func (t *AuthFailureTracker) RecordFailure(username, ip string) bool {
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	key := authFailureKey{username: username, ip: ip}
	record, exists := t.failures[key]
	switch {
	case !exists:
		t.failures[key] = &failureRecord{count: 1, lastFailure: now}
	case now.Sub(record.lastFailure) > AuthFailureResetDuration:
		record.count = 1
		record.lastFailure = now
	default:
		record.count++
		record.lastFailure = now
	}

	ceiling, exists := t.ceilings[username]
	if !exists || now.Sub(ceiling.windowStart) > AuthUsernameCeilingWindow {
		ceiling = &ceilingRecord{windowStart: now}
		t.ceilings[username] = ceiling
	}
	ceiling.count++

	return ceiling.count == AuthUsernameCeiling
}

// ResetFailures clears, on a successful login, the backoff of username from
// ip and the username-wide ceiling: whoever holds the password has nothing
// left to guess, so a burst against the account ends with its owner's next
// correct login. The backoff of the other sources is left alone. It returns
// the number of failures the ceiling had counted when it was reached, or 0
// when no guessing burst was active, so the caller can audit the success.
func (t *AuthFailureTracker) ResetFailures(username, ip string) int {
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, authFailureKey{username: username, ip: ip})

	ceiling, exists := t.ceilings[username]
	if !exists {
		return 0
	}
	delete(t.ceilings, username)

	if ceiling.count >= AuthUsernameCeiling && now.Before(ceiling.windowStart.Add(AuthUsernameCeilingWindow)) {
		return ceiling.count
	}

	return 0
}

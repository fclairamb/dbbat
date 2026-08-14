package mssql

import (
	"context"
	"strings"
	"time"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// authenticate turns a parsed LOGIN7 into an authorized session: the dbbat user
// behind the credentials, the target database, and the grant that links them.
//
// It is the SQL Server side of the same three steps every other proxy takes —
// see internal/proxy/mysql/auth.go — with one difference forced by TDS: there is
// no separate "use database" round trip, so the database comes from the LOGIN7
// database field and everything is settled in one shot.
//
// Only SQL authentication reaches this point. Integrated (NTLM/Kerberos/SSPI)
// and federated logins are refused earlier, by Login7.Validate and by the
// PRELOGIN FEDAUTHREQUIRED answer.
func (s *session) authenticate(ctx context.Context, login *Login7) error {
	// A proxy built without a store can authenticate nobody. It only happens in
	// tests that exercise the handshake on its own, but failing closed is the
	// only defensible behavior for an auth path.
	if s.server.store == nil {
		return ErrAuthFailed
	}

	user, err := s.verifyCredentials(ctx, login.UserName, login.Password)
	if err != nil {
		return err
	}

	s.user = user

	database, err := s.resolveDatabase(ctx, login.Database)
	if err != nil {
		return err
	}

	s.database = database

	grant, err := s.server.store.GetActiveGrant(ctx, user.UID, database.UID)
	if err != nil {
		return ErrNoActiveGrant
	}

	s.grant = grant

	// A grant that is already over quota (or expired) must not open a session
	// at all — the same check the per-statement path re-runs on every message.
	return checkQuotas(grant)
}

// checkQuotas verifies the grant's expiry and its count / byte quotas.
func checkQuotas(grant *store.Grant) error {
	if grant == nil {
		return nil
	}

	if !grant.ExpiresAt.IsZero() && !time.Now().Before(grant.ExpiresAt) {
		return shared.ErrGrantExpired
	}

	if maxQueries := grant.MaxQueryCounts(); maxQueries != nil && grant.QueryCount >= *maxQueries {
		return ErrQueryLimitExceeded
	}

	if maxBytes := grant.MaxBytesTransferred(); maxBytes != nil && grant.BytesTransferred >= *maxBytes {
		return ErrDataLimitExceeded
	}

	return nil
}

// verifyCredentials checks the LOGIN7 password against the user's Argon2id
// hash, or interprets it as an API key when it carries the dbb_ prefix.
//
// Every failure — unknown user, wrong password, store outage — comes back as
// the same ErrAuthFailed, and the caller renders one message for all of them.
// Telling a caller which half was wrong is the enumeration oracle dbbat does
// not give anywhere else either.
func (s *session) verifyCredentials(ctx context.Context, username, password string) (*store.User, error) {
	if isAPIKey(password) {
		return s.authenticateAPIKey(ctx, username, password)
	}

	user, err := s.server.store.GetUserByUsername(ctx, username)
	if err != nil {
		return nil, ErrAuthFailed
	}

	var (
		valid    bool
		verifErr error
	)

	if s.server.authCache != nil {
		valid, verifErr = s.server.authCache.VerifyPassword(ctx, user.UID.String(), password, user.PasswordHash)
	} else {
		valid, verifErr = crypto.VerifyPassword(user.PasswordHash, password)
	}

	if verifErr != nil || !valid {
		return nil, ErrAuthFailed
	}

	return user, nil
}

// authenticateAPIKey accepts a dbb_ key in place of a password, provided it
// belongs to the user named in the login.
func (s *session) authenticateAPIKey(ctx context.Context, username, key string) (*store.User, error) {
	verified, err := s.server.store.VerifyAPIKey(ctx, key)
	if err != nil {
		return nil, ErrAuthFailed
	}

	user, err := s.server.store.GetUserByUsername(ctx, username)
	if err != nil || user.UID != verified.UserID {
		return nil, ErrAPIKeyOwnerMismatch
	}

	// Usage accounting must not sit in the login's latency path, and it must
	// outlive the request context, which is canceled the moment the session
	// ends.
	shared.BumpAPIKeyUsage(ctx, s.logger, s.server.store, verified.ID)

	return user, nil
}

// resolveDatabase maps the LOGIN7 database field onto a dbbat server row.
//
// The convention is the one all the other proxies use: what the client puts in
// the "database" slot of its connection string is the *dbbat* entry's name, not
// a database name on the target — the real database comes from the row. The row
// must also be a SQL Server target, so a name that happens to match a
// PostgreSQL entry is not a way to reach it through the wrong listener.
func (s *session) resolveDatabase(ctx context.Context, requested string) (*store.Server, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil, ErrNoDatabaseRequested
	}

	database, err := s.server.store.GetServerByName(ctx, requested)
	if err != nil {
		return nil, ErrServerNotFound
	}

	if database.Protocol != store.ProtocolMSSQL {
		return nil, ErrServerNotFound
	}

	return database, nil
}

// isAPIKey reports whether the given password looks like a dbbat API key.
func isAPIKey(password string) bool {
	return len(password) >= store.APIKeyPrefixLength &&
		strings.HasPrefix(password, store.APIKeyPrefix)
}

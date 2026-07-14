package mongodb

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// authFailDelay slows brute-force attempts: replies to failed auth are held
// briefly before the connection is torn down (contract §5).
const authFailDelay = 250 * time.Millisecond

// handleSaslStart performs the server side of SASL PLAIN (contract §5): parse
// the RFC 4616 payload, verify the credential (Argon2id or dbb_ API key),
// resolve the target database + grant, register revocation, build the limit
// guard, dial the upstream, and record the connection.
//
// Returns done=true only on full success.
func (s *Session) handleSaslStart(responseTo int32, body bson.Raw) (bool, error) {
	if mech := lookupString(body, "mechanism"); mech != "PLAIN" {
		s.logger.InfoContext(s.ctx, "MongoDB unsupported SASL mechanism", "mechanism", mech)

		return false, s.failAuth(responseTo)
	}

	// PLAIN must run over TLS, unless the operator disabled TLS entirely on
	// this listener (server.tlsConfig == nil) — then plaintext is a choice.
	if s.server.tlsConfig != nil && !s.tlsActive {
		s.logger.WarnContext(s.ctx, "MongoDB PLAIN auth refused on non-TLS connection")
		_ = s.replyOpMsg(responseTo, errorDoc(codeAuthenticationFailed, codeNameAuthenticationFailed, ErrTLSRequired.Error()))
		time.Sleep(authFailDelay)

		return false, ErrTLSRequired
	}

	rawUser, password, ok := parsePlainPayload(body)
	if !ok {
		return false, s.failAuth(responseTo)
	}

	s.authSource = lookupString(body, "$db")

	// A username may carry a "user#database" hint (resolution order #2).
	bareUser, userDBHint := splitUserDBHint(rawUser)

	user, err := s.verifyCredentials(bareUser, password)
	if err != nil {
		return false, s.failAuth(responseTo)
	}

	s.user = user

	db, err := s.resolveDatabase(user, userDBHint)
	if err != nil {
		s.logger.InfoContext(s.ctx, "MongoDB database resolution failed",
			"user", bareUser, "auth_source", s.authSource, "error", err)
		_ = s.replyOpMsg(responseTo, errorDoc(codeAuthenticationFailed, codeNameAuthenticationFailed, err.Error()))
		time.Sleep(authFailDelay)

		return false, err
	}

	s.database = db

	grant, err := s.server.store.GetActiveGrant(s.ctx, user.UID, db.UID)
	if err != nil {
		_ = s.replyOpMsg(responseTo, errorDoc(codeAuthenticationFailed, codeNameAuthenticationFailed, ErrNoActiveGrant.Error()))
		time.Sleep(authFailDelay)

		return false, ErrNoActiveGrant
	}

	s.grant = grant

	if err := checkQuotas(grant); err != nil {
		_ = s.replyOpMsg(responseTo, errorDoc(codeAuthenticationFailed, codeNameAuthenticationFailed, err.Error()))
		time.Sleep(authFailDelay)

		return false, err
	}

	if err := s.completeAuth(responseTo, grant); err != nil {
		return false, err
	}

	return true, nil
}

// completeAuth wires up the authenticated session: revocation registration,
// limit guard, upstream dial, connection record, and the success reply.
func (s *Session) completeAuth(responseTo int32, grant *store.Grant) error {
	// Register the live session so an admin revoke can signal it.
	s.revocation = s.server.store.Revocations().Register(grant.UID)
	s.guard = shared.NewLimitGuard(grant, s.bytesFromClient, s.bytesToClient).
		WithRevocation(s.revocation.Flag())

	if err := s.connectUpstream(); err != nil {
		s.deregisterRevocation()
		s.grant = nil
		_ = s.replyOpMsg(responseTo, errorDoc(codeAuthenticationFailed, codeNameAuthenticationFailed, ErrUpstreamConnect.Error()))
		time.Sleep(authFailDelay)

		return err
	}

	if err := s.recordConnection(); err != nil {
		// Don't fail the session if the audit insert fails — log and continue.
		s.logger.WarnContext(s.ctx, "MongoDB connection insert failed", slog.Any("error", err))
	}

	s.authenticated = true

	// SASL PLAIN is single-step: reply done:true with an empty payload.
	reply := bson.D{
		{Key: "conversationId", Value: int32(1)},
		{Key: "done", Value: true},
		{Key: "payload", Value: bson.Binary{Subtype: 0, Data: []byte{}}},
		{Key: "ok", Value: 1.0},
	}

	return s.replyOpMsg(responseTo, reply)
}

// failAuth sends the standard authentication-failure reply, then holds briefly
// before the session is torn down.
func (s *Session) failAuth(responseTo int32) error {
	_ = s.replyOpMsg(responseTo, authFailedDoc())
	time.Sleep(authFailDelay)

	return ErrAuthenticationFailed
}

// parsePlainPayload decodes an RFC 4616 SASL PLAIN payload
// ([authzid] \0 authcid \0 password). Drivers send an empty authzid; we parse
// defensively, taking the last two NUL-separated fields.
func parsePlainPayload(body bson.Raw) (username, password string, ok bool) {
	_, data, hasBin := body.Lookup("payload").BinaryOK()
	if !hasBin {
		return "", "", false
	}

	fields := bytes.Split(data, []byte{0})
	if len(fields) < 2 {
		return "", "", false
	}

	username = string(fields[len(fields)-2])
	password = string(fields[len(fields)-1])

	if username == "" {
		return "", "", false
	}

	return username, password, true
}

// splitUserDBHint splits a "user#database" username into its parts. When there
// is no '#', the hint is empty.
func splitUserDBHint(raw string) (user, dbHint string) {
	if idx := strings.LastIndex(raw, "#"); idx >= 0 {
		return raw[:idx], raw[idx+1:]
	}

	return raw, ""
}

// verifyCredentials checks the cleartext against the user's Argon2id hash, or
// interprets it as a dbb_ API key.
func (s *Session) verifyCredentials(username, password string) (*store.User, error) {
	if isAPIKey(password) {
		return s.authenticateAPIKey(username, password)
	}

	user, err := s.server.store.GetUserByUsername(s.ctx, username)
	if err != nil {
		return nil, ErrAuthenticationFailed
	}

	var (
		valid bool
		verr  error
	)

	if s.server.authCache != nil {
		valid, verr = s.server.authCache.VerifyPassword(s.ctx, user.UID.String(), password, user.PasswordHash)
	} else {
		valid, verr = crypto.VerifyPassword(user.PasswordHash, password)
	}

	if verr != nil || !valid {
		return nil, ErrAuthenticationFailed
	}

	return user, nil
}

// authenticateAPIKey validates a dbb_ API key and checks it belongs to the
// authenticating user.
func (s *Session) authenticateAPIKey(username, key string) (*store.User, error) {
	verified, err := s.server.store.VerifyAPIKey(s.ctx, key)
	if err != nil {
		return nil, ErrAuthenticationFailed
	}

	user, err := s.server.store.GetUserByUsername(s.ctx, username)
	if err != nil || user.UID != verified.UserID {
		return nil, ErrAuthenticationFailed
	}

	go func() { _ = s.server.store.IncrementAPIKeyUsage(context.Background(), verified.ID) }()

	return user, nil
}

// resolveDatabase resolves the target dbbat database (contract §5) in order:
//  1. saslStart.$db not in {$external, admin} → that's the database name;
//  2. username "user#database" hint;
//  3. the user's single active grant;
//  4. otherwise fail.
func (s *Session) resolveDatabase(user *store.User, userDBHint string) (*store.Database, error) {
	name := ""

	switch {
	case s.authSource != "" && s.authSource != "$external" && s.authSource != "admin":
		name = s.authSource
	case userDBHint != "":
		name = userDBHint
	}

	if name != "" {
		db, err := s.server.store.GetDatabaseByName(s.ctx, name)
		if err != nil || db.Protocol != store.ProtocolMongoDB {
			return nil, ErrDatabaseNotResolvable
		}

		return db, nil
	}

	return s.resolveSingleGrantDatabase(user)
}

// resolveSingleGrantDatabase returns the database of the user's single active
// MongoDB grant, or an error if there is not exactly one.
func (s *Session) resolveSingleGrantDatabase(user *store.User) (*store.Database, error) {
	grants, err := s.server.store.ListGrants(s.ctx, store.GrantFilter{UserID: &user.UID, ActiveOnly: true})
	if err != nil {
		return nil, ErrDatabaseNotResolvable
	}

	var resolved *store.Database

	for i := range grants {
		db, err := s.server.store.GetDatabaseByUID(s.ctx, grants[i].DatabaseID)
		if err != nil || db.Protocol != store.ProtocolMongoDB {
			continue
		}

		if resolved != nil && resolved.UID != db.UID {
			// More than one distinct MongoDB target — ambiguous.
			return nil, ErrDatabaseNotResolvable
		}

		resolved = db
	}

	if resolved == nil {
		return nil, ErrDatabaseNotResolvable
	}

	return resolved, nil
}

// isAPIKey reports whether the given password looks like a DBBat API key.
func isAPIKey(password string) bool {
	return len(password) >= store.APIKeyPrefixLength &&
		strings.HasPrefix(password, store.APIKeyPrefix)
}

// checkQuotas verifies the grant's expiry and count/byte quotas.
func checkQuotas(grant *store.Grant) error {
	if !grant.ExpiresAt.IsZero() && !time.Now().Before(grant.ExpiresAt) {
		return shared.ErrGrantExpired
	}

	if grant.MaxQueryCounts != nil && grant.QueryCount >= *grant.MaxQueryCounts {
		return ErrQueryLimitExceeded
	}

	if grant.MaxBytesTransferred != nil && grant.BytesTransferred >= *grant.MaxBytesTransferred {
		return ErrDataLimitExceeded
	}

	return nil
}

package mongodb

import (
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/xdg-go/scram"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// handleScramStart begins the server side of a SCRAM-SHA-256 exchange (item 1),
// letting a client authenticate to dbbat with the driver-default mechanism
// instead of PLAIN — no cleartext password on the wire. It steps the client's
// first message and replies with the server-first (salt, iterations, nonce);
// the exchange completes on saslContinue.
func (s *Session) handleScramStart(responseTo int32, body bson.Raw) (bool, error) {
	_, clientFirst, ok := body.Lookup("payload").BinaryOK()
	if !ok {
		return false, s.failAuth(responseTo)
	}

	srv, err := scram.SHA256.NewServer(s.scramCredentialLookup)
	if err != nil {
		return false, s.failAuth(responseTo)
	}

	conv := srv.NewConversation()

	serverFirst, err := conv.Step(string(clientFirst))
	if err != nil {
		s.logger.InfoContext(s.ctx, "MongoDB SCRAM client-first rejected", slog.Any("error", err))

		return false, s.failAuth(responseTo)
	}

	s.scramConv = conv

	return false, s.replyOpMsg(responseTo, scramReply(serverFirst, false))
}

// handleScramContinue completes a SCRAM-SHA-256 exchange: it steps the client's
// final message, validates the proof, and — on success — authorizes and
// establishes the session before replying with the server-final signature.
func (s *Session) handleScramContinue(responseTo int32, body bson.Raw) (bool, error) {
	if s.scramConv == nil {
		return false, s.failAuth(responseTo)
	}

	_, clientFinal, ok := body.Lookup("payload").BinaryOK()
	if !ok {
		return false, s.failAuth(responseTo)
	}

	serverFinal, err := s.scramConv.Step(string(clientFinal))
	if err != nil || !s.scramConv.Valid() {
		s.logger.InfoContext(s.ctx, "MongoDB SCRAM authentication failed", slog.Any("error", err))

		return false, s.failAuth(responseTo)
	}

	if s.scramUser == nil {
		return false, s.failAuth(responseTo)
	}

	if err := s.authorizeAndEstablish(responseTo, s.scramUser, s.scramUserDBHint); err != nil {
		return false, err
	}

	return true, s.replyOpMsg(responseTo, scramReply(serverFinal, true))
}

// scramReply builds a SASL reply document carrying a SCRAM payload.
func scramReply(payload string, done bool) bson.D {
	return bson.D{
		{Key: "conversationId", Value: int32(1)},
		{Key: "done", Value: done},
		{Key: "payload", Value: bson.Binary{Subtype: 0, Data: []byte(payload)}},
		{Key: "ok", Value: 1.0},
	}
}

// scramCredentialLookup is the SCRAM server's credential callback: it resolves
// the dbbat user named in the client-first message and returns their stored
// SCRAM-SHA-256 credentials (decrypting the stored/server keys). The resolved
// user is recorded so the continue step can authorize the session. Any failure
// returns an error, which the library surfaces as an authentication failure.
func (s *Session) scramCredentialLookup(username string) (scram.StoredCredentials, error) {
	bareUser, hint := splitUserDBHint(username)

	user, err := s.server.store.GetUserByUsername(s.ctx, bareUser)
	if err != nil {
		return scram.StoredCredentials{}, ErrAuthenticationFailed
	}

	creds := user.MongoSCRAMCredentials()
	if creds == nil {
		return scram.StoredCredentials{}, ErrAuthenticationFailed
	}

	storedKey, serverKey, err := s.decryptMongoSCRAM(user.UID, creds)
	if err != nil {
		s.logger.WarnContext(s.ctx, "MongoDB SCRAM verifier decrypt failed", slog.Any("error", err))

		return scram.StoredCredentials{}, ErrAuthenticationFailed
	}

	s.scramUser = user
	s.scramUserDBHint = hint

	return scram.StoredCredentials{
		KeyFactors: scram.KeyFactors{Salt: string(creds.Salt), Iters: creds.Iterations},
		StoredKey:  storedKey,
		ServerKey:  serverKey,
	}, nil
}

// decryptMongoSCRAM decrypts the stored/server keys (AAD-bound to the user UID).
func (s *Session) decryptMongoSCRAM(userUID uuid.UUID, creds *store.MongoSCRAMCredentials) ([]byte, []byte, error) {
	aad := crypto.UserAAD(userUID.String())

	storedKey, err := crypto.Decrypt(creds.StoredKey, s.server.encryptionKey, aad)
	if err != nil {
		return nil, nil, fmt.Errorf("decrypt stored key: %w", err)
	}

	serverKey, err := crypto.Decrypt(creds.ServerKey, s.server.encryptionKey, aad)
	if err != nil {
		return nil, nil, fmt.Errorf("decrypt server key: %w", err)
	}

	return storedKey, serverKey, nil
}

package oracle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// speedyKeyClient answers an 18453 challenge the way a real client does, with
// dbbat's own upstream-side implementation, and returns its Phase 2 secrets.
func speedyKeyClient(t *testing.T, server *O5LogonServer, encSessKey, vfrData, password string) *upstreamAuthSecrets {
	t.Helper()

	salt, err := hex.DecodeString(vfrData)
	require.NoError(t, err)

	chkSalt, err := hex.DecodeString(server.PBKDF2ChkSalt())
	require.NoError(t, err)

	sec := &upstreamAuthSecrets{
		verifierType:    VerifierType18453,
		customHash:      true,
		salt:            salt,
		pbkdf2ChkSalt:   chkSalt,
		pbkdf2VgenCount: server.PBKDF2VgenCount(),
		pbkdf2SderCount: server.PBKDF2SderCount(),
	}
	require.NoError(t, computeUpstreamAuthSecrets(password, encSessKey, sec))
	require.NotEmpty(t, sec.eSpeedyKey)

	return sec
}

func TestVerifySpeedyKey(t *testing.T) {
	t.Parallel()

	userSalt := make([]byte, 16)
	_, err := rand.Read(userSalt)
	require.NoError(t, err)

	key := "dbb_speedy_aaaa11112222333344445555"

	server := NewO5LogonServer(nil, nil)
	server.UseVerifier18453(userSalt, dbbcryptoDerive18453(key, userSalt))

	encSessKey, vfrData, err := server.GenerateChallenge()
	require.NoError(t, err)

	right := speedyKeyClient(t, server, encSessKey, vfrData, key)
	wrong := speedyKeyClient(t, server, encSessKey, vfrData, "dbb_speedy_wrong_9999888877776666")

	// Positive control: the client holding the key proves it.
	require.NoError(t, server.VerifySpeedyKey(right.encClientSessKey, right.eSpeedyKey))
	assert.NotEmpty(t, server.CombinedKey)

	// A different key, no speedy key, or random bytes prove nothing.
	require.ErrorIs(t, server.VerifySpeedyKey(wrong.encClientSessKey, wrong.eSpeedyKey), ErrSpeedyKeyMismatch)
	require.ErrorIs(t, server.VerifySpeedyKey(right.encClientSessKey, ""), ErrSpeedyKeyMismatch)
	require.ErrorIs(t, server.VerifySpeedyKey(right.encClientSessKey, strings.Repeat("AB", 80)), ErrSpeedyKeyMismatch)
}

func TestVerifySpeedyKey_Refuses6949(t *testing.T) {
	t.Parallel()

	salt, verifier, err := GenerateO5LogonVerifier("dbb_speedy_legacy_key_0000111122")
	require.NoError(t, err)

	server := NewO5LogonServer(salt, verifier)

	encSessKey, _, err := server.GenerateChallenge()
	require.NoError(t, err)

	require.ErrorIs(t, server.VerifySpeedyKey(encSessKey, strings.Repeat("AB", 80)), ErrSpeedyKeyMismatch)
}

// A Phase 2 without AUTH_PASSWORD must prove the key with its speedy key: it
// is the only thing standing between a client that knows a username and that
// user's session.
func TestResolveAPIKeyFromPhase2_EmptyPasswordNeedsProof(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataStore := newOracleTestStore(t)

	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(0xA0 + i)
	}

	hash, err := crypto.HashPassword("unused-password")
	require.NoError(t, err)

	user, err := dataStore.CreateUser(ctx, "speedyvictim", hash, []string{store.RoleConnector})
	require.NoError(t, err)

	// Two keys: the older one is the one the client holds, so a match also has
	// to pick the right candidate rather than the newest.
	held, heldPlain, err := dataStore.CreateAPIKey(ctx, user.UID, "held", nil, masterKey)
	require.NoError(t, err)
	_, _, err = dataStore.CreateAPIKey(ctx, user.UID, "newest", nil, masterKey)
	require.NoError(t, err)

	s := &session{
		store:         dataStore,
		encryptionKey: masterKey,
		logger:        slog.New(slog.DiscardHandler),
		ctx:           ctx,
	}

	verifiers, _, err := s.loadO5LogonVerifiers(user.UID)
	require.NoError(t, err)
	require.Len(t, verifiers, 2)

	challenge := func() (*O5LogonServer, string, string) {
		o5 := NewO5LogonServer(verifiers[0].O5LogonSalt, verifiers[0].decryptedVerifier)
		o5.UseVerifier18453(verifiers[0].salt18453, verifiers[0].decryptedVerifier18453)

		encSessKey, vfrData, err := o5.GenerateChallenge()
		require.NoError(t, err)

		return o5, encSessKey, vfrData
	}

	// No AUTH_PASSWORD and no speedy key: refused.
	o5, encSessKey, vfrData := challenge()
	anyone := speedyKeyClient(t, o5, encSessKey, vfrData, "dbb_not_the_key_000011112222333344")

	_, err = s.resolveAPIKeyFromPhase2(o5, verifiers, anyone.encClientSessKey, "", "", encSessKey)
	require.ErrorIs(t, err, ErrAPIKeyVerification)

	// A speedy key derived from a key the user does not have: refused.
	_, err = s.resolveAPIKeyFromPhase2(o5, verifiers, anyone.encClientSessKey, "", anyone.eSpeedyKey, encSessKey)
	require.ErrorIs(t, err, ErrAPIKeyVerification)

	// Positive control: the holder of a real key gets that key, not the newest.
	o5, encSessKey, vfrData = challenge()
	holder := speedyKeyClient(t, o5, encSessKey, vfrData, heldPlain)

	got, err := s.resolveAPIKeyFromPhase2(o5, verifiers, holder.encClientSessKey, "", holder.eSpeedyKey, encSessKey)
	require.NoError(t, err)
	assert.Equal(t, held.ID, got.ID)
}

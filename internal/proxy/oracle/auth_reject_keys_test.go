package oracle

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// oracleMasterKey is any 32-byte master key: these tests only need the verifiers
// they mint to decrypt under the same key the session runs with.
func oracleMasterKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0x11 * (i%15 + 1))
	}

	return key
}

// TestNoteKeysWithoutVerifier pins what the annotation will and will not attach
// itself to. It must not borrow a store failure or a parse error — those are not
// explained by an inventory of the user's keys — and it must keep the sentinel
// every other caller classifies the failure by.
func TestNoteKeysWithoutVerifier(t *testing.T) {
	t.Parallel()

	t.Run("no unusable keys leaves the error alone", func(t *testing.T) {
		t.Parallel()

		err := fmt.Errorf("%w: no candidate key decrypted AUTH_PASSWORD (3 tried)", ErrAPIKeyVerification)
		assert.Equal(t, err, noteKeysWithoutVerifier(err, 0))
	})

	t.Run("an unrelated failure is not annotated", func(t *testing.T) {
		t.Parallel()

		err := fmt.Errorf("failed to list API keys: %w", context.Canceled)
		assert.Equal(t, err, noteKeysWithoutVerifier(err, 2))
	})

	t.Run("the wrapped sentinel survives", func(t *testing.T) {
		t.Parallel()

		annotated := noteKeysWithoutVerifier(
			fmt.Errorf("%w: no candidate key decrypted AUTH_PASSWORD (3 tried)", ErrAPIKeyVerification), 2)

		require.ErrorIs(t, annotated, ErrAPIKeyVerification)
		assert.Contains(t, annotated.Error(), "2 of the user's API keys")
	})

	t.Run("no candidate at all is annotated too", func(t *testing.T) {
		t.Parallel()

		annotated := noteKeysWithoutVerifier(
			fmt.Errorf("failed to load O5LOGON verifier: %w", ErrNoO5LogonVerifier), 1)

		require.ErrorIs(t, annotated, ErrNoO5LogonVerifier)

		_, message := authRejectFor(annotated)
		assert.Contains(t, message, "1 of your dbbat API keys")
	})
}

// TestAuthRejectFor_KeysWithoutVerifier is the point-of-failure half of the bug:
// the same ORA-01017 (the credential really was refused, and O5LOGON never
// reveals which key was typed) but a message that names the trap instead of
// leaving "invalid username/password" to be read as a typo.
func TestAuthRejectFor_KeysWithoutVerifier(t *testing.T) {
	t.Parallel()

	err := noteKeysWithoutVerifier(
		fmt.Errorf("%w: no candidate key decrypted AUTH_PASSWORD (3 tried)", ErrAPIKeyVerification), 2)

	code, message := authRejectFor(err)

	assert.Equal(t, ORA01017, code,
		"the credential was refused; only the explanation improves")
	assert.Contains(t, message, "invalid username/password",
		"dbbat cannot know the presented key was one of the unusable ones, so the hedge stays first")
	assert.Contains(t, message, "2 of your dbbat API keys")
	assert.Contains(t, message, "mint a new key")

	for _, secret := range []string{"dbb_", "verifier", "O5LOGON"} {
		assert.NotContains(t, message, secret,
			"a pre-auth message carries counts and advice, never key material or prefixes")
	}
}

// TestSendAuthFailed_KeysWithoutVerifierReachesTheClient reads the refusal off
// the wire the way a client's parser does. The message is longer than the three
// it joins, and the frame is the same OER mechanism, so what this actually pins
// is that a variable-length message still lands where the client looks for it.
func TestSendAuthFailed_KeysWithoutVerifierReachesTheClient(t *testing.T) {
	t.Parallel()

	for _, count := range []int{1, 12} {
		t.Run(fmt.Sprintf("%d unusable keys", count), func(t *testing.T) {
			t.Parallel()

			client, server := newPipeConns(t)

			s := newTestErrorSession(t, server)
			s.upstreamCustomHash = true

			code, message := authRejectFor(noteKeysWithoutVerifier(ErrAPIKeyVerification, count))

			go s.sendAuthFailed(code, message)

			body := readAuthRejectFrame(t, client)

			shape, _, _ := s.authRefusalOERShape()
			gotCode, gotMessage, _ := decodeAuthRejectOER(t, shape, body)

			assert.Equal(t, int(ORA01017), gotCode)
			assert.Equal(t, fmt.Sprintf("ORA-%05d: %s", ORA01017, message), gotMessage)
			assert.Contains(t, gotMessage, fmt.Sprintf("%d of your dbbat API keys", count))
		})
	}
}

// TestLoadO5LogonVerifiers_CountsKeysThatCannotDoOracle drives the real inventory
// against a real store, which is where the count comes from — the reported
// production case exactly: three usable keys, and one older key that is the one
// actually being typed.
//
// dbbat's own PostgreSQL is the only database involved: nothing here reaches an
// Oracle, so it stays out of the integration suite.
func TestLoadO5LogonVerifiers_CountsKeysThatCannotDoOracle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataStore := newOracleTestStore(t)
	master := oracleMasterKey()

	newSession := func(t *testing.T) *session {
		t.Helper()

		s := newTestErrorSession(t, nil)
		s.store = dataStore
		s.encryptionKey = master

		return s
	}

	newUser := func(t *testing.T, username string) *store.User {
		t.Helper()

		user, err := dataStore.CreateUser(ctx, username,
			"$argon2id$v=19$m=4096,t=3,p=1$salt$hash", []string{"connector"})
		require.NoError(t, err)

		return user
	}

	t.Run("usable keys alongside keys that predate Oracle", func(t *testing.T) {
		t.Parallel()

		user := newUser(t, "verifiermixed")

		// Minted without a master key: no verifier, and none can be recovered.
		_, _, err := dataStore.CreateAPIKey(ctx, user.UID, "Legacy Key", nil)
		require.NoError(t, err)

		for _, name := range []string{"New Key A", "New Key B"} {
			_, _, err := dataStore.CreateAPIKey(ctx, user.UID, name, nil, master)
			require.NoError(t, err)
		}

		candidates, unusable, err := newSession(t).loadO5LogonVerifiers(user.UID)
		require.NoError(t, err)
		assert.Len(t, candidates, 2, "only the verifier-bearing keys can answer a challenge")
		assert.Equal(t, 1, unusable)
	})

	t.Run("only keys that predate Oracle", func(t *testing.T) {
		t.Parallel()

		user := newUser(t, "verifiernone")

		_, _, err := dataStore.CreateAPIKey(ctx, user.UID, "Legacy Key", nil)
		require.NoError(t, err)

		candidates, unusable, err := newSession(t).loadO5LogonVerifiers(user.UID)
		require.ErrorIs(t, err, ErrNoO5LogonVerifier)
		assert.Empty(t, candidates)
		assert.Equal(t, 1, unusable,
			"the count has to survive the error: this is the case it explains best")

		_, message := authRejectFor(noteKeysWithoutVerifier(err, unusable))
		assert.Contains(t, message, "1 of your dbbat API keys")
	})

	t.Run("a verifier this process cannot decrypt counts as unusable", func(t *testing.T) {
		t.Parallel()

		user := newUser(t, "verifierstranded")

		otherKey := make([]byte, 32)
		for i := range otherKey {
			otherKey[i] = 0x5e
		}

		_, _, err := dataStore.CreateAPIKey(ctx, user.UID, "Stranded Key", nil, otherKey)
		require.NoError(t, err)

		candidates, unusable, err := newSession(t).loadO5LogonVerifiers(user.UID)
		require.ErrorIs(t, err, ErrNoO5LogonVerifier)
		assert.Empty(t, candidates)
		assert.Equal(t, 1, unusable)
	})

	t.Run("no keys at all says nothing about keys", func(t *testing.T) {
		t.Parallel()

		user := newUser(t, "verifierkeyless")

		_, unusable, err := newSession(t).loadO5LogonVerifiers(user.UID)
		require.ErrorIs(t, err, ErrNoO5LogonVerifier)
		assert.Zero(t, unusable)

		_, message := authRejectFor(noteKeysWithoutVerifier(err, unusable))
		assert.Equal(t, "invalid username/password; logon denied", message,
			"with no keys to explain, the refusal must stay the generic one")
	})
}

package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oracleCapableTestKey is a key with freshly derived (and encrypted) O5LOGON
// material, i.e. what every key minted since Oracle support looks like.
func oracleCapableTestKey(t *testing.T, master []byte) *APIKey {
	t.Helper()

	key := &APIKey{KeyPrefix: "dbb_8kre"}
	require.NoError(t, key.computeO5LogonVerifier("dbb_8kreqwertyuiopasdfghjklzxcvbnm", master))

	return key
}

// TestAPIKeyOracleLoginCapable pins the predicate `GET /api/v1/keys` reports as
// oracle_capable — and, being the same call the Oracle proxy makes to build a
// challenge, pins that the two agree.
//
// The wrong-master-key case is why the predicate is a decryption and not a
// "column is non-empty" test: that key holds verifier bytes, and they are just as
// unusable as no bytes at all.
func TestAPIKeyOracleLoginCapable(t *testing.T) {
	t.Parallel()

	master := bytes.Repeat([]byte{0x2b}, 32)
	otherMaster := bytes.Repeat([]byte{0x7c}, 32)

	t.Run("a key minted before Oracle support", func(t *testing.T) {
		t.Parallel()

		key := &APIKey{KeyPrefix: "dbb_3js0"}

		assert.False(t, key.OracleLoginCapable(master))

		_, err := key.DecryptedO5LogonVerifier6949(master)
		assert.ErrorIs(t, err, ErrAPIKeyNoOracleVerifier,
			"no material must be distinguishable from material that failed to decrypt")
	})

	t.Run("a key with verifier material", func(t *testing.T) {
		t.Parallel()

		key := oracleCapableTestKey(t, master)

		assert.True(t, key.OracleLoginCapable(master))

		verifier, err := key.DecryptedO5LogonVerifier6949(master)
		require.NoError(t, err)
		assert.NotEmpty(t, verifier)
	})

	t.Run("material that does not decrypt under this master key", func(t *testing.T) {
		t.Parallel()

		key := oracleCapableTestKey(t, master)

		assert.False(t, key.OracleLoginCapable(otherMaster),
			"a verifier the running process cannot decrypt is as unusable as a missing one")
	})

	t.Run("material transplanted onto another key prefix", func(t *testing.T) {
		t.Parallel()

		key := oracleCapableTestKey(t, master)
		key.KeyPrefix = "dbb_sjzu"

		assert.False(t, key.OracleLoginCapable(master),
			"the AAD binds the verifier to its own key prefix")
	})

	t.Run("a salt with no verifier", func(t *testing.T) {
		t.Parallel()

		key := oracleCapableTestKey(t, master)
		key.ProtocolData.Oracle.O5LogonVerifier6949 = nil

		assert.False(t, key.OracleLoginCapable(master))

		_, err := key.DecryptedO5LogonVerifier6949(master)
		assert.ErrorIs(t, err, ErrAPIKeyNoOracleVerifier)
	})

	t.Run("no master key at all", func(t *testing.T) {
		t.Parallel()

		key := oracleCapableTestKey(t, master)

		assert.False(t, key.OracleLoginCapable(nil))
	})
}

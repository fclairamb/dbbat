package mssql

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleLogin7 is a login shaped like the ones real drivers send.
func sampleLogin7() *Login7 {
	return &Login7{
		TDSVersion:     tdsVersion74,
		PacketSize:     4096,
		ClientProgVer:  0x07000000,
		ClientPID:      4242,
		ClientTimeZone: -60,
		ClientLCID:     1033,
		OptionFlags1:   0xE0,
		OptionFlags2:   0x03,
		OptionFlags3:   0x00,
		HostName:       "workstation-01",
		UserName:       "florent",
		Password:       "s3cr3t-p@ssw0rd",
		AppName:        "dbbat-test",
		ServerName:     "dbbat-proxy",
		CltIntName:     "go-mssqldb",
		Language:       "us_english",
		Database:       "reporting",
		ClientID:       [6]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01},
	}
}

func TestPasswordScrambleRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []string{
		"",
		"a",
		"password",
		"s3cr3t-p@ssw0rd",
		"héllo wörld",      // non-ASCII, still one UTF-16 unit each
		"emoji \U0001F510", // surrogate pair
		strings.Repeat("x", 128),
	}

	for _, password := range tests {
		t.Run(password, func(t *testing.T) {
			t.Parallel()

			scrambled := scramblePassword(password)
			assert.Equal(t, password, descramblePassword(scrambled))

			if password != "" {
				assert.NotEqual(t, stringToUCS2(password), scrambled,
					"the scramble must actually change the bytes")
			}
		})
	}
}

// TestPasswordScrambleKnownVector pins the transform to bytes taken off the
// wire, so a refactor cannot quietly change the algorithm.
func TestPasswordScrambleKnownVector(t *testing.T) {
	t.Parallel()

	// "abc" in UCS-2LE is 61 00 62 00 63 00.
	// Swap nibbles: 16 00 26 00 36 00. XOR 0xA5: b3 a5 83 a5 93 a5.
	assert.Equal(t,
		[]byte{0xB3, 0xA5, 0x83, 0xA5, 0x93, 0xA5},
		scramblePassword("abc"))

	assert.Equal(t, "abc", descramblePassword([]byte{0xB3, 0xA5, 0x83, 0xA5, 0x93, 0xA5}))
}

// TestPasswordScrambleIsNotAnInvolution guards the trap the doc comment warns
// about: applying the same transform twice does not give the password back.
func TestPasswordScrambleIsNotAnInvolution(t *testing.T) {
	t.Parallel()

	once := scramblePassword("secret")

	twice := make([]byte, len(once))
	for i, b := range once {
		twice[i] = (b<<4 | b>>4) ^ 0xA5
	}

	assert.NotEqual(t, stringToUCS2("secret"), twice)
}

func TestLogin7RoundTrip(t *testing.T) {
	t.Parallel()

	original := sampleLogin7()
	payload := original.serialize()

	parsed, err := parseLogin7(payload)
	require.NoError(t, err)

	assert.Equal(t, original, parsed)

	// And the bytes themselves round-trip, which is what stage 2's replay
	// depends on.
	assert.Equal(t, payload, parsed.serialize())
}

func TestLogin7SerializeMatchesTheDeclaredLength(t *testing.T) {
	t.Parallel()

	payload := sampleLogin7().serialize()

	assert.GreaterOrEqual(t, len(payload), login7FixedLen)
	assert.Equal(t, uint32(len(payload)), binary.LittleEndian.Uint32(payload[0:4]))
	assert.Equal(t, tdsVersion74, binary.LittleEndian.Uint32(payload[4:8]))
}

func TestLogin7RewriteCredentialsAndReserialize(t *testing.T) {
	t.Parallel()

	login, err := parseLogin7(sampleLogin7().serialize())
	require.NoError(t, err)

	login.UserName = "upstream_service_account"
	login.Password = "a-much-longer-upstream-password"
	login.Database = "master"

	replayed, err := parseLogin7(login.serialize())
	require.NoError(t, err)

	assert.Equal(t, "upstream_service_account", replayed.UserName)
	assert.Equal(t, "a-much-longer-upstream-password", replayed.Password)
	assert.Equal(t, "master", replayed.Database)
	assert.Equal(t, "workstation-01", replayed.HostName, "untouched fields survive the rewrite")
	assert.Equal(t, "go-mssqldb", replayed.CltIntName)
}

func TestLogin7EmptyFields(t *testing.T) {
	t.Parallel()

	login := &Login7{TDSVersion: tdsVersion74, PacketSize: defaultPacketSize}

	parsed, err := parseLogin7(login.serialize())
	require.NoError(t, err)

	assert.Empty(t, parsed.UserName)
	assert.Empty(t, parsed.Password)
	assert.Empty(t, parsed.Database)
	assert.Empty(t, parsed.SSPI)
	assert.False(t, parsed.IntegratedSecurity())
	require.NoError(t, parsed.Validate())
}

func TestLogin7FeatureExtSurvivesReserialize(t *testing.T) {
	t.Parallel()

	login := sampleLogin7()
	login.OptionFlags3 |= optionFlags3Extension
	// A FEATUREEXT block: feature id 0x0A (UTF-8 support), 1 data byte, then
	// the 0xFF terminator.
	login.FeatureExt = []byte{0x0A, 0x01, 0x00, 0x00, 0x00, 0x01, 0xFF}

	payload := login.serialize()

	parsed, err := parseLogin7(payload)
	require.NoError(t, err)
	assert.Equal(t, login.FeatureExt, parsed.FeatureExt)
	assert.Equal(t, payload, parsed.serialize())

	// The extension pointer really does point at the block.
	pointerOffset := int(binary.LittleEndian.Uint16(payload[tablePos(pairExtension):]))
	blockOffset := int(binary.LittleEndian.Uint32(payload[pointerOffset : pointerOffset+4]))
	assert.Equal(t, login.FeatureExt, payload[blockOffset:])
}

func TestParseLogin7Errors(t *testing.T) {
	t.Parallel()

	valid := sampleLogin7().serialize()

	t.Run("shorter than the fixed header", func(t *testing.T) {
		t.Parallel()

		_, err := parseLogin7(valid[:login7FixedLen-1])
		require.ErrorIs(t, err, ErrLogin7TooShort)
	})

	t.Run("declared length disagrees with the payload", func(t *testing.T) {
		t.Parallel()

		bad := append([]byte(nil), valid...)
		binary.LittleEndian.PutUint32(bad[0:4], uint32(len(bad)+16))

		_, err := parseLogin7(bad)
		require.ErrorIs(t, err, ErrLogin7BadLength)
	})

	t.Run("blob offset runs past the payload", func(t *testing.T) {
		t.Parallel()

		bad := append([]byte(nil), valid...)
		binary.LittleEndian.PutUint16(bad[tablePos(pairUserName):], uint16(len(bad)-2))
		binary.LittleEndian.PutUint16(bad[tablePos(pairUserName)+2:], 32)

		_, err := parseLogin7(bad)
		require.ErrorIs(t, err, ErrLogin7BadOffset)
	})

	t.Run("blob offset inside the fixed header", func(t *testing.T) {
		t.Parallel()

		bad := append([]byte(nil), valid...)
		binary.LittleEndian.PutUint16(bad[tablePos(pairDatabase):], 4)
		binary.LittleEndian.PutUint16(bad[tablePos(pairDatabase)+2:], 2)

		_, err := parseLogin7(bad)
		require.ErrorIs(t, err, ErrLogin7BadOffset)
	})

	t.Run("feature extension pointer out of range", func(t *testing.T) {
		t.Parallel()

		login := sampleLogin7()
		login.OptionFlags3 |= optionFlags3Extension
		login.FeatureExt = []byte{0xFF}

		bad := login.serialize()
		pointerOffset := int(binary.LittleEndian.Uint16(bad[tablePos(pairExtension):]))
		binary.LittleEndian.PutUint32(bad[pointerOffset:pointerOffset+4], uint32(len(bad)+64))

		_, err := parseLogin7(bad)
		require.ErrorIs(t, err, ErrLogin7BadOffset)
	})
}

func TestLogin7ValidateEnforcesTheTDS74Floor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version uint32
		wantErr bool
	}{
		{"7.1", tdsVersion71, true},
		{"7.2", tdsVersion72, true},
		{"7.3A", tdsVersion73A, true},
		{"7.3B", tdsVersion73B, true},
		{"7.4", tdsVersion74, false},
		{"newer than 7.4", 0x75000000, false},
		{"zero means the server picks", 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			login := sampleLogin7()
			login.TDSVersion = tc.version

			err := login.Validate()
			if tc.wantErr {
				require.ErrorIs(t, err, ErrLogin7Unsupported)
				assert.Contains(t, err.Error(), "7.4")

				return
			}

			require.NoError(t, err)
		})
	}
}

func TestLogin7ValidateRejectsUnsupportedAuth(t *testing.T) {
	t.Parallel()

	t.Run("integrated security flag", func(t *testing.T) {
		t.Parallel()

		login := sampleLogin7()
		login.OptionFlags2 |= optionFlags2IntSecurity

		require.ErrorIs(t, login.Validate(), ErrLogin7Unsupported)
		assert.True(t, login.IntegratedSecurity())
	})

	t.Run("sspi blob present", func(t *testing.T) {
		t.Parallel()

		login := sampleLogin7()
		login.SSPI = []byte{0x4E, 0x54, 0x4C, 0x4D} // "NTLM"

		require.ErrorIs(t, login.Validate(), ErrLogin7Unsupported)

		// It survives a round-trip, so stage 2 can report it precisely.
		parsed, err := parseLogin7(login.serialize())
		require.NoError(t, err)
		assert.Equal(t, login.SSPI, parsed.SSPI)
	})

	t.Run("password change", func(t *testing.T) {
		t.Parallel()

		login := sampleLogin7()
		login.ChangePassword = "new-password"

		require.ErrorIs(t, login.Validate(), ErrLogin7Unsupported)

		parsed, err := parseLogin7(login.serialize())
		require.NoError(t, err)
		assert.Equal(t, "new-password", parsed.ChangePassword)
	})
}

// featureExtEntry renders one {feature id, DWORD length, data} FEATUREEXT entry.
func featureExtEntry(id byte, data ...byte) []byte {
	entry := make([]byte, 5, 5+len(data))
	entry[0] = id
	binary.LittleEndian.PutUint32(entry[1:5], uint32(len(data)))

	return append(entry, data...)
}

// featureExtBlock renders a whole FEATUREEXT block: the entries, then the
// terminator.
func featureExtBlock(entries ...[]byte) []byte {
	block := []byte{}
	for _, entry := range entries {
		block = append(block, entry...)
	}

	return append(block, featureExtTerminator)
}

// loginWithFeatureExt is a sample login carrying the given FEATUREEXT block.
func loginWithFeatureExt(block []byte) *Login7 {
	login := sampleLogin7()
	login.OptionFlags3 |= optionFlags3Extension
	login.FeatureExt = block

	return login
}

// TestLogin7ValidateRejectsFederatedAuthInFeatureExt is the guard for dbbat's v1
// position: SQL authentication only. The proxy declines FEDAUTH in PRELOGIN, but
// a client can still ask for it in the LOGIN7 feature extension block, and that
// request must not reach the upstream.
func TestLogin7ValidateRejectsFederatedAuthInFeatureExt(t *testing.T) {
	t.Parallel()

	t.Run("fedauth alone", func(t *testing.T) {
		t.Parallel()

		// FEDAUTH's data is the library byte plus a token; its content does not
		// matter here, only that the feature was asked for.
		login := loginWithFeatureExt(featureExtBlock(featureExtEntry(featureExtFedAuth, 0x02, 0x01)))

		assert.True(t, login.FederatedAuthRequested())

		err := login.Validate()
		require.ErrorIs(t, err, ErrLogin7Unsupported)
		assert.Contains(t, err.Error(), "SQL login")
	})

	t.Run("fedauth hidden behind an allowed feature", func(t *testing.T) {
		t.Parallel()

		login := loginWithFeatureExt(featureExtBlock(
			featureExtEntry(0x0A, 0x01),                         // UTF-8 support
			featureExtEntry(0x01, 0xFF, 0xFF, 0xFF, 0xFF),       // session recovery, 0xFF bytes inside
			featureExtEntry(featureExtFedAuth, 0x02, 0x00),      // FEDAUTH
			featureExtEntry(0x04, 0x01, 0x00, 0x00, 0x00, 0x01), // column encryption
		))

		assert.True(t, login.FederatedAuthRequested())
		require.ErrorIs(t, login.Validate(), ErrLogin7Unsupported)
	})

	t.Run("the refusal survives a parse of the real bytes", func(t *testing.T) {
		t.Parallel()

		login := loginWithFeatureExt(featureExtBlock(featureExtEntry(featureExtFedAuth, 0x02, 0x01)))

		parsed, err := parseLogin7(login.serialize())
		require.NoError(t, err)

		assert.True(t, parsed.FederatedAuthRequested())
		require.ErrorIs(t, parsed.Validate(), ErrLogin7Unsupported)
	})
}

// TestLogin7ValidateFailsClosedOnAnUndecodableFeatureExt pins the other half of
// the rule: a block the proxy cannot walk is refused, because a walker that gave
// up quietly is exactly how a FEDAUTH request would slip through.
func TestLogin7ValidateFailsClosedOnAnUndecodableFeatureExt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block []byte
	}{
		{"no terminator", []byte{0x0A, 0x01, 0x00, 0x00, 0x00, 0x01}},
		{"truncated length field", []byte{0x0A, 0x01, 0x00}},
		{"data shorter than the declared length", []byte{0x0A, 0xFF, 0x00, 0x00, 0x00, 0x01, 0x02}},
		{"absurd declared length", []byte{0x0A, 0xFF, 0xFF, 0xFF, 0x7F, 0x01, 0xFF}},
		{"terminator eaten by the previous entry", []byte{0x0A, 0x02, 0x00, 0x00, 0x00, 0x01, 0xFF}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			login := loginWithFeatureExt(tc.block)

			_, err := featureExtRequests(tc.block)
			require.ErrorIs(t, err, ErrLogin7BadFeatureExt)

			// It is refused, and refused as an unsupported login so the client
			// gets the same well-formed TDS error as any other refusal.
			validateErr := login.Validate()
			require.ErrorIs(t, validateErr, ErrLogin7Unsupported)
			require.ErrorIs(t, validateErr, ErrLogin7BadFeatureExt)

			// The undecodable block never reads as "no federated auth here".
			assert.False(t, login.FederatedAuthRequested(),
				"detection alone must not be trusted on a block that does not decode")
		})
	}
}

// TestLogin7ValidateAcceptsAllowedFeatureExtensions is the regression guard for
// the pass-through stage 2 depends on: everything that is not FEDAUTH is still
// accepted and still replayed byte for byte.
func TestLogin7ValidateAcceptsAllowedFeatureExtensions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block []byte
	}{
		{"utf-8 support", featureExtBlock(featureExtEntry(0x0A, 0x01))},
		{"session recovery", featureExtBlock(featureExtEntry(0x01, 0x00, 0x00, 0x00, 0x00))},
		{"column encryption", featureExtBlock(featureExtEntry(0x04, 0x01))},
		{"several at once", featureExtBlock(
			featureExtEntry(0x01, 0x00),
			featureExtEntry(0x04, 0x01),
			featureExtEntry(0x08),
			featureExtEntry(0x0A, 0x01),
		)},
		{"terminator only", featureExtBlock()},
		{"padding past the terminator", append(featureExtBlock(featureExtEntry(0x0A, 0x01)), 0x00, 0x00)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			login := loginWithFeatureExt(tc.block)

			assert.False(t, login.FederatedAuthRequested())
			require.NoError(t, login.Validate())

			// And the block still round-trips byte for byte, which is what the
			// upstream replay depends on.
			payload := login.serialize()

			parsed, err := parseLogin7(payload)
			require.NoError(t, err)
			assert.Equal(t, tc.block, parsed.FeatureExt)
			assert.Equal(t, payload, parsed.serialize())
			require.NoError(t, parsed.Validate())
		})
	}
}

// TestLogin7StringNeverLeaksThePassword is the guard for the one rule in this
// file that is a security property rather than a correctness one.
func TestLogin7StringNeverLeaksThePassword(t *testing.T) {
	t.Parallel()

	login := sampleLogin7()
	login.Password = "correct-horse-battery-staple"
	login.ChangePassword = "another-secret-entirely"

	rendered := login.String()
	assert.NotContains(t, rendered, "correct-horse-battery-staple")
	assert.NotContains(t, rendered, "another-secret-entirely")
	assert.Contains(t, rendered, "florent")
	assert.Contains(t, rendered, "reporting")
	assert.Contains(t, rendered, "7.4")
}

func TestTDSVersionName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "7.4", tdsVersionName(tdsVersion74))
	assert.Equal(t, "7.3A", tdsVersionName(tdsVersion73A))
	assert.Equal(t, "7.3B", tdsVersionName(tdsVersion73B))
	assert.Equal(t, "7.2", tdsVersionName(tdsVersion72))
	assert.Equal(t, "7.1", tdsVersionName(tdsVersion71))
	assert.Contains(t, tdsVersionName(0x12345678), "unknown")
}

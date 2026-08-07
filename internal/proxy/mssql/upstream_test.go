package mssql

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/upstream"
)

// errBastionDown stands in for whatever the SSH bastion chain hands back when
// the transport cannot be opened at all.
var errBastionDown = errors.New("bastion is down")

// upstreamConfigFor builds a connector config pointing at a fake server.
func upstreamConfigFor(fake *fakeUpstream, sslMode string) upstream.MSSQLConfig {
	host, port := fake.addr()

	return upstream.MSSQLConfig{
		Host:     host,
		Port:     port,
		Username: "upstream-user",
		Password: "upstream-secret",
		Database: "AdventureWorks",
		AppName:  "dbbat/test @florent",
		SSLMode:  sslMode,
	}
}

func TestConnectUpstreamPlaintext(t *testing.T) {
	t.Parallel()

	fake := newFakeUpstream(t, encryptNotSup)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn, err := ConnectUpstream(ctx, fake.dialFunc(), upstreamConfigFor(fake, "disable"), nil)
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })

	assert.False(t, conn.TLS, "ssl_mode=disable must never encrypt")
	assert.False(t, fake.negotiatedTLS())
	assert.NotEmpty(t, conn.LoginResponse, "the client is served the upstream's own login response")

	login := fake.lastLogin()
	require.NotNil(t, login)
	assert.Equal(t, "upstream-user", login.UserName)
	assert.Equal(t, "upstream-secret", login.Password, "the scrambled password must survive the replay")
	assert.Equal(t, "AdventureWorks", login.Database)
	assert.Equal(t, "dbbat/test @florent", login.AppName)
	assert.Equal(t, tdsVersion74, login.TDSVersion, "a login with no template defaults to the 7.4 floor")
}

func TestConnectUpstreamEncrypted(t *testing.T) {
	t.Parallel()

	fake := newFakeUpstream(t, encryptOn)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn, err := ConnectUpstream(ctx, fake.dialFunc(), upstreamConfigFor(fake, "require"), nil)
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })

	assert.True(t, conn.TLS, "ssl_mode=require must encrypt, and upstream_tls must say so")
	assert.True(t, fake.negotiatedTLS())
	assert.Equal(t, 1, fake.loginCount(), "require has a single attempt: no redial")
}

// TestConnectUpstreamPreferFallsBackToPlaintext is the redial case: prefer
// offers TLS first, the server cannot do it, and the chain tries again in
// cleartext on a fresh socket.
func TestConnectUpstreamPreferFallsBackToPlaintext(t *testing.T) {
	t.Parallel()

	fake := newFakeUpstream(t, encryptNotSup)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn, err := ConnectUpstream(ctx, fake.dialFunc(), upstreamConfigFor(fake, "prefer"), nil)
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })

	assert.False(t, conn.TLS)
	assert.Equal(t, 1, fake.loginCount(),
		"the encrypted attempt is abandoned at PRELOGIN, so only the plaintext one ever logs in")
}

// TestConnectUpstreamAllowUpgradesWhenTheServerInsists is the mirror image:
// allow prefers plaintext, the server requires encryption, and the retry
// encrypts.
func TestConnectUpstreamAllowUpgradesWhenTheServerInsists(t *testing.T) {
	t.Parallel()

	fake := newFakeUpstream(t, encryptReq)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn, err := ConnectUpstream(ctx, fake.dialFunc(), upstreamConfigFor(fake, "allow"), nil)
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })

	assert.True(t, conn.TLS)
}

// TestConnectUpstreamRefusesToDowngradeUnderRequire pins the direction that
// matters for security: require never accepts a server that will not encrypt,
// and does not quietly try again in cleartext.
func TestConnectUpstreamRefusesToDowngradeUnderRequire(t *testing.T) {
	t.Parallel()

	fake := newFakeUpstream(t, encryptNotSup)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := ConnectUpstream(ctx, fake.dialFunc(), upstreamConfigFor(fake, "require"), nil)
	require.ErrorIs(t, err, upstream.ErrMSSQLEncryptionMismatch)
	assert.Zero(t, fake.loginCount(), "no credential may be sent over a transport the mode forbids")
}

// TestConnectUpstreamRefusesEncryptionUnderDisable is the other end of the
// policy: disable means plaintext, and a server that requires TLS is a failure
// rather than a silent upgrade.
func TestConnectUpstreamRefusesEncryptionUnderDisable(t *testing.T) {
	t.Parallel()

	fake := newFakeUpstream(t, encryptReq)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := ConnectUpstream(ctx, fake.dialFunc(), upstreamConfigFor(fake, "disable"), nil)
	require.ErrorIs(t, err, upstream.ErrMSSQLEncryptionMismatch)
}

func TestConnectUpstreamSurfacesALoginRejection(t *testing.T) {
	t.Parallel()

	fake := newFakeUpstream(t, encryptNotSup)
	fake.rejectLogin = true

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := ConnectUpstream(ctx, fake.dialFunc(), upstreamConfigFor(fake, "prefer"), nil)
	require.ErrorIs(t, err, upstream.ErrMSSQLLoginRejected)
	assert.Contains(t, err.Error(), "Login failed for user 'upstream-user'")
	assert.Equal(t, 1, fake.loginCount(),
		"a rejected password ends the chain — retrying it over another transport would be a downgrade")
}

func TestConnectUpstreamReportsADialFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := ConnectUpstream(ctx, func(context.Context) (net.Conn, error) { return nil, errBastionDown },
		upstream.MSSQLConfig{Host: "nowhere", Port: 1433, SSLMode: "disable"}, nil)

	require.ErrorIs(t, err, errBastionDown)
}

// TestBuildUpstreamLoginReplaysTheClientShape is the reason the client's own
// LOGIN7 is used as a template: everything that decides the shape of the token
// stream coming back has to be the client's, because dbbat forwards that
// stream to the client untouched.
func TestBuildUpstreamLoginReplaysTheClientShape(t *testing.T) {
	t.Parallel()

	client := sampleLogin7()
	client.OptionFlags2 |= optionFlags2IntSecurity
	client.SSPI = []byte{0x01, 0x02}
	client.ChangePassword = "new-password"
	client.AtchDBFile = "C:\\db.mdf"
	client.OptionFlags3 |= optionFlags3Extension
	client.FeatureExt = []byte{0x0A, 0x01, 0x00, 0x00, 0x00, 0x01, 0xFF}

	login := buildUpstreamLogin(client, upstream.MSSQLConfig{
		Username: "sa",
		Password: "upstream-secret",
		Database: "master",
		AppName:  "dbbat @florent",
	})

	// Kept: the negotiation the client made.
	assert.Equal(t, client.TDSVersion, login.TDSVersion)
	assert.Equal(t, client.PacketSize, login.PacketSize)
	assert.Equal(t, client.OptionFlags1, login.OptionFlags1)
	assert.Equal(t, client.ClientLCID, login.ClientLCID)
	assert.Equal(t, client.HostName, login.HostName)
	assert.Equal(t, client.ClientID, login.ClientID)
	assert.Equal(t, client.FeatureExt, login.FeatureExt,
		"the FEATUREEXT block must survive, or the forwarded FEATUREEXTACK answers nothing")

	// Swapped: the credentials.
	assert.Equal(t, "sa", login.UserName)
	assert.Equal(t, "upstream-secret", login.Password)
	assert.Equal(t, "master", login.Database)
	assert.Equal(t, "dbbat @florent", login.AppName)

	// Dropped: anything that would negotiate an auth scheme dbbat does not
	// speak, or act on the target's filesystem.
	assert.Zero(t, login.OptionFlags2&optionFlags2IntSecurity)
	assert.Nil(t, login.SSPI)
	assert.Empty(t, login.ChangePassword)
	assert.Empty(t, login.AtchDBFile)

	// And the client's own struct is untouched.
	assert.Equal(t, "florent", client.UserName)
}

// TestBuildUpstreamLoginDropsAFederatedFeatureExt is belt and braces on the
// rule Login7.Validate already enforces: no federated-auth request, and no
// block dbbat could not read, is ever what the upstream negotiates on.
func TestBuildUpstreamLoginDropsAFederatedFeatureExt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block []byte
	}{
		{"fedauth requested", featureExtBlock(featureExtEntry(featureExtFedAuth, 0x02, 0x01))},
		{"block does not decode", []byte{0x0A, 0x02, 0x00, 0x00, 0x00, 0x01, 0xFF}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client := loginWithFeatureExt(tc.block)

			login := buildUpstreamLogin(client, upstream.MSSQLConfig{Username: "sa", Password: "x"})

			assert.Nil(t, login.FeatureExt)
			assert.Zero(t, login.OptionFlags3&optionFlags3Extension)
			assert.False(t, login.FederatedAuthRequested())
			require.NoError(t, login.Validate())

			// The client's own struct is untouched.
			assert.Equal(t, tc.block, client.FeatureExt)
		})
	}
}

func TestBuildUpstreamLoginClampsThePacketSize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   uint32
		want uint32
	}{
		{"below the floor falls back to the default", 0, defaultPacketSize},
		{"tiny falls back to the default", 128, defaultPacketSize},
		{"in range is kept", 8192, 8192},
		{"above the ceiling is clamped", 1 << 20, maxPacketSize},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			login := sampleLogin7()
			login.PacketSize = tc.in

			assert.Equal(t, tc.want, buildUpstreamLogin(login, upstream.MSSQLConfig{}).PacketSize)
		})
	}
}

func TestUpstreamCloseIsSafeTwiceAndOnNil(t *testing.T) {
	t.Parallel()

	var nilConn *UpstreamConn

	require.NoError(t, nilConn.Close())

	fake := newFakeUpstream(t, encryptNotSup)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn, err := ConnectUpstream(ctx, fake.dialFunc(), upstreamConfigFor(fake, "disable"), nil)
	require.NoError(t, err)

	require.NoError(t, conn.Close())
	require.NoError(t, conn.Close(), "the relay teardown closes it twice")
}

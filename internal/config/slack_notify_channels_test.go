package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSlackNotifyParseChannels(t *testing.T) {
	t.Parallel()

	got, err := SlackNotifyConfig{}.ParseChannels()
	require.NoError(t, err)
	assert.Empty(t, got)

	got, err = SlackNotifyConfig{Channels: "prod=#dbbat-prod, nonprod = C0123ABC ,a_b-1=#x"}.ParseChannels()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"prod": "#dbbat-prod", "nonprod": "C0123ABC", "a_b-1": "#x"}, got)

	for _, bad := range []string{"prod", "prod=", "=#x", "Prod=#x", "-a=#x", "none=#x", "a=#x,,b"} {
		_, err = SlackNotifyConfig{Channels: bad}.ParseChannels()
		require.ErrorIs(t, err, ErrSlackNotifyChannelsInvalid, bad)
	}
}

func setSlackBase(t *testing.T) {
	t.Helper()

	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_PUBLIC_URL", "https://dbbat.example.com")
	t.Setenv("DBB_SLACK_NOTIFY_BOT_TOKEN", "xoxb-test")
	t.Setenv("DBB_SLACK_NOTIFY_CHANNEL", "#dbbat")
}

func TestSlackNotifyChannelsEnv(t *testing.T) {
	setSlackBase(t)
	t.Setenv("DBB_SLACK_NOTIFY_CHANNELS", "prod=#dbbat-prod")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	got, err := cfg.SlackNotify.ParseChannels()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"prod": "#dbbat-prod"}, got)
}

func TestSlackNotifyChannelsMalformedFailsStartup(t *testing.T) {
	setSlackBase(t)
	t.Setenv("DBB_SLACK_NOTIFY_CHANNELS", "none=#x")

	_, err := Load(LoadOptions{})
	require.ErrorIs(t, err, ErrSlackNotifyChannelsInvalid)
}

func TestSlackNotifyChannelsFile(t *testing.T) { //nolint:paralleltest // t.Setenv in setSlackBase
	setSlackBase(t)

	path := filepath.Join(t.TempDir(), "dbbat.yml")
	require.NoError(t, os.WriteFile(path, []byte("slack_notify:\n  channels: \"prod=#dbbat-prod,dev=#dbbat-dev\"\n"), 0o600))

	cfg, err := Load(LoadOptions{ConfigFile: path})
	require.NoError(t, err)

	got, err := cfg.SlackNotify.ParseChannels()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"prod": "#dbbat-prod", "dev": "#dbbat-dev"}, got)
}

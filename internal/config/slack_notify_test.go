package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSlackNotifyAppTokenEnv verifies DBB_SLACK_NOTIFY_APP_TOKEN maps to
// SlackNotify.AppToken (the slack_notify_* env prefix) and enables Socket Mode.
func TestSlackNotifyAppTokenEnv(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_PUBLIC_URL", "https://dbbat.example.com")
	t.Setenv("DBB_SLACK_NOTIFY_BOT_TOKEN", "xoxb-test")
	t.Setenv("DBB_SLACK_NOTIFY_CHANNEL", "#dbbat")
	t.Setenv("DBB_SLACK_NOTIFY_APP_TOKEN", "xapp-test")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.Equal(t, "xapp-test", cfg.SlackNotify.AppToken)
	assert.True(t, cfg.SlackNotify.SocketMode(), "app token + bot token should enable Socket Mode")
	assert.True(t, cfg.SlackNotify.Interactive(), "an app token should render buttons")
}

// TestSlackSigningSecretEnv verifies the canonical documented name
// DBB_SLACK_SIGNING_SECRET populates SlackNotify.SigningSecret and enables
// interactivity (the cross-check the completeness audit lacked).
func TestSlackSigningSecretEnv(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_PUBLIC_URL", "https://dbbat.example.com")
	t.Setenv("DBB_SLACK_NOTIFY_BOT_TOKEN", "xoxb-test")
	t.Setenv("DBB_SLACK_SIGNING_SECRET", "canonical-secret")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.Equal(t, "canonical-secret", cfg.SlackNotify.SigningSecret)
	assert.True(t, cfg.SlackNotify.Interactive(), "signing secret + bot token should enable interactivity")
}

// TestSlackNotifySigningSecretLegacyEnv verifies the legacy alias
// DBB_SLACK_NOTIFY_SIGNING_SECRET (still set on live deployments) keeps
// populating SlackNotify.SigningSecret.
func TestSlackNotifySigningSecretLegacyEnv(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_PUBLIC_URL", "https://dbbat.example.com")
	t.Setenv("DBB_SLACK_NOTIFY_BOT_TOKEN", "xoxb-test")
	t.Setenv("DBB_SLACK_NOTIFY_SIGNING_SECRET", "legacy-secret")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.Equal(t, "legacy-secret", cfg.SlackNotify.SigningSecret)
	assert.True(t, cfg.SlackNotify.Interactive(), "signing secret + bot token should enable interactivity")
}

// TestSlackSigningSecretPrecedence verifies the canonical
// DBB_SLACK_SIGNING_SECRET deterministically wins over the legacy alias when
// both are set (env provider map order is not guaranteed).
func TestSlackSigningSecretPrecedence(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_PUBLIC_URL", "https://dbbat.example.com")
	t.Setenv("DBB_SLACK_NOTIFY_BOT_TOKEN", "xoxb-test")
	t.Setenv("DBB_SLACK_SIGNING_SECRET", "canonical-secret")
	t.Setenv("DBB_SLACK_NOTIFY_SIGNING_SECRET", "legacy-secret")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.Equal(t, "canonical-secret", cfg.SlackNotify.SigningSecret,
		"canonical DBB_SLACK_SIGNING_SECRET must win over the legacy alias")
}

// TestSlackNotifySocketModeAndInteractive covers the SocketMode() gate and the
// widened Interactive() gate across field combinations.
func TestSlackNotifySocketModeAndInteractive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		cfg         SlackNotifyConfig
		socketMode  bool
		interactive bool
	}{
		{"empty", SlackNotifyConfig{}, false, false},
		{"bot only", SlackNotifyConfig{BotToken: "xoxb"}, false, false},
		{"bot+signing (HTTP)", SlackNotifyConfig{BotToken: "xoxb", SigningSecret: "s"}, false, true},
		{"bot+app (socket)", SlackNotifyConfig{BotToken: "xoxb", AppToken: "xapp"}, true, true},
		{"bot+signing+app", SlackNotifyConfig{BotToken: "xoxb", SigningSecret: "s", AppToken: "xapp"}, true, true},
		{"app without bot", SlackNotifyConfig{AppToken: "xapp"}, false, false},
		{"signing without bot", SlackNotifyConfig{SigningSecret: "s"}, false, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.socketMode, tc.cfg.SocketMode())
			assert.Equal(t, tc.interactive, tc.cfg.Interactive())
		})
	}
}

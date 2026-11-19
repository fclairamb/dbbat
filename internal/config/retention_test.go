package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadRetention loads a config with the given environment and optional YAML.
func loadRetention(t *testing.T, env map[string]string, yaml string) *Config {
	t.Helper()

	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	for k, v := range env {
		t.Setenv(k, v)
	}

	opts := LoadOptions{}

	if yaml != "" {
		path := filepath.Join(t.TempDir(), "config.yml")
		require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

		opts.ConfigFile = path
	}

	cfg, err := Load(opts)
	require.NoError(t, err)

	return cfg
}

// Each row of the names table: the variable sets the key, and the key alone
// (in the config file) sets the same window.
func TestRetentionNames(t *testing.T) { //nolint:paralleltest // t.Setenv cannot run in parallel
	rows := []struct {
		env  string
		yaml string
		get  func(RetentionWindows) time.Duration
	}{
		{
			"DBB_RETENTION_QUERIES_DELETE_AFTER", "retention:\n  queries:\n    delete_after: 720h\n",
			func(w RetentionWindows) time.Duration { return w.Queries },
		},
		{
			"DBB_RETENTION_QUERIES_ARCHIVE_AFTER", "retention:\n  queries:\n    archive_after: 720h\n",
			func(w RetentionWindows) time.Duration { return w.QueriesArchive },
		},
		{
			"DBB_RETENTION_DUMPS_DELETE_AFTER", "retention:\n  dumps:\n    delete_after: 720h\n",
			func(w RetentionWindows) time.Duration { return w.Dumps },
		},
	}

	for _, r := range rows { //nolint:paralleltest // t.Setenv cannot run in parallel
		t.Run(r.env, func(t *testing.T) {
			w := loadRetention(t, map[string]string{r.env: "720h"}, "").RetentionWindows()
			assert.Empty(t, w.Misconfiguration)
			assert.Equal(t, 720*time.Hour, r.get(w))

			w = loadRetention(t, nil, r.yaml).RetentionWindows()
			assert.Empty(t, w.Misconfiguration)
			assert.Equal(t, 720*time.Hour, r.get(w))
		})
	}

	// connections need queries to be kept at most as long.
	env := map[string]string{
		"DBB_RETENTION_QUERIES_DELETE_AFTER":     "720h",
		"DBB_RETENTION_CONNECTIONS_DELETE_AFTER": "8760h",
	}
	w := loadRetention(t, env, "").RetentionWindows()
	assert.Empty(t, w.Misconfiguration)
	assert.Equal(t, 8760*time.Hour, w.Connections)

	w = loadRetention(t, nil, "retention:\n  queries:\n    delete_after: 720h\n  connections:\n    delete_after: 8760h\n").RetentionWindows()
	assert.Empty(t, w.Misconfiguration)
	assert.Equal(t, 8760*time.Hour, w.Connections)
}

// Nothing set: statements and connections are never deleted, dumps at 24h.
func TestRetentionDefaults(t *testing.T) { //nolint:paralleltest // t.Setenv cannot run in parallel
	w := loadRetention(t, nil, "").RetentionWindows()

	assert.Empty(t, w.Misconfiguration)
	assert.Zero(t, w.Queries)
	assert.Zero(t, w.Connections)
	assert.Zero(t, w.QueriesArchive)
	assert.Equal(t, 24*time.Hour, w.Dumps)
}

func TestRetentionZeroMeansNever(t *testing.T) { //nolint:paralleltest // t.Setenv cannot run in parallel
	w := loadRetention(t, map[string]string{"DBB_RETENTION_DUMPS_DELETE_AFTER": "0"}, "").RetentionWindows()

	assert.Empty(t, w.Misconfiguration)
	assert.Zero(t, w.Dumps)
}

// Each kind of mistake: every window is zero (dumps included) and the problem
// names the culprit.
func TestRetentionMistakes(t *testing.T) { //nolint:paralleltest // t.Setenv cannot run in parallel
	cases := []struct {
		name  string
		env   map[string]string
		yaml  string
		culpr string
	}{
		{
			"not a duration",
			map[string]string{"DBB_RETENTION_QUERIES_DELETE_AFTER": "30d"},
			"",
			"DBB_RETENTION_QUERIES_DELETE_AFTER",
		},
		{
			"negative",
			map[string]string{"DBB_RETENTION_DUMPS_DELETE_AFTER": "-1h"},
			"",
			"DBB_RETENTION_DUMPS_DELETE_AFTER",
		},
		{
			"unknown name in env",
			map[string]string{"DBB_RETENTION_QUERY_DELETE_AFTER": "720h"},
			"",
			"retention_query_delete_after",
		},
		{
			"unknown name in file", nil, "retention:\n  queries:\n    delete_afte: 720h\n",
			"retention.queries.delete_afte",
		},
		{"connection shorter than queries", map[string]string{
			"DBB_RETENTION_QUERIES_DELETE_AFTER":     "720h",
			"DBB_RETENTION_CONNECTIONS_DELETE_AFTER": "24h",
		}, "", "shorter"},
		{"connection set, queries forever", map[string]string{
			"DBB_RETENTION_CONNECTIONS_DELETE_AFTER": "720h",
		}, "", "kept forever"},
	}

	for _, tc := range cases { //nolint:paralleltest // t.Setenv cannot run in parallel
		t.Run(tc.name, func(t *testing.T) {
			w := loadRetention(t, tc.env, tc.yaml).RetentionWindows()

			assert.Contains(t, w.Misconfiguration, tc.culpr)
			assert.Zero(t, w.Queries)
			assert.Zero(t, w.Connections)
			assert.Zero(t, w.QueriesArchive)
			assert.Zero(t, w.Dumps, "a mistake must not delete capture files either")
			assert.False(t, w.Enabled())
		})
	}
}

// Each removed name, in the environment and in the file: every window is zero,
// the problem gives the new name, and the old value is never read.
func TestRetentionRemovedNames(t *testing.T) { //nolint:paralleltest // t.Setenv cannot run in parallel
	cases := []struct {
		name        string
		env         map[string]string
		yaml        string
		replacement string
	}{
		{"query env", map[string]string{"DBB_QUERY_STORAGE_RETENTION": "720h"}, "", "DBB_RETENTION_QUERIES_DELETE_AFTER"},
		{"query file", nil, "query_storage:\n  retention: 720h\n", "retention.queries.delete_after"},
		{"connection env", map[string]string{"DBB_CONNECTION_RETENTION": "720h"}, "", "DBB_RETENTION_CONNECTIONS_DELETE_AFTER"},
		{"connection file", nil, "connection:\n  retention: 720h\n", "retention.connections.delete_after"},
		{"dump env", map[string]string{"DBB_DUMP_RETENTION": "720h"}, "", "DBB_RETENTION_DUMPS_DELETE_AFTER"},
		{"dump file", nil, "dump:\n  retention: 720h\n", "retention.dumps.delete_after"},
	}

	for _, tc := range cases { //nolint:paralleltest // t.Setenv cannot run in parallel
		t.Run(tc.name, func(t *testing.T) {
			w := loadRetention(t, tc.env, tc.yaml).RetentionWindows()

			assert.Contains(t, w.Misconfiguration, tc.replacement)
			assert.Zero(t, w.Queries)
			assert.Zero(t, w.Connections)
			assert.Zero(t, w.Dumps)
		})
	}
}

// The docs and the OpenAPI file never mention the three removed names. They
// may appear in the changelog, the guard's map and the tests.
func TestRemovedRetentionNamesAreGoneFromDocs(t *testing.T) { //nolint:paralleltest // t.Setenv cannot run in parallel
	removed := []string{"DBB_QUERY_STORAGE_RETENTION", "DBB_CONNECTION_RETENTION", "DBB_DUMP_RETENTION"}
	roots := []string{"../../CLAUDE.md", "../../README.md", "../../website/docs", "../../docs", "../api/openapi.yml"}

	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil //nolint:nilerr // a missing root is fine
			}

			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil //nolint:nilerr // unreadable files are not docs
			}

			for _, name := range removed {
				// The migration table in the docs is the one place the old
				// name may be quoted, behind the explicit "removed" marker.
				for _, line := range splitLines(string(data)) {
					if containsStr(line, name) && !containsStr(line, "removed") {
						t.Errorf("%s mentions %s: %s", path, name, line)
					}
				}
			}

			return nil
		})
	}
}

func splitLines(s string) []string { return strings.Split(s, "\n") }

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }

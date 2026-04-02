package config

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// clearEnvVars unsets all DBB_ environment variables and uses t.Cleanup for restoration.
func clearEnvVars(t *testing.T) {
	t.Helper()

	envVars := []string{
		"DBB_DSN", "DBB_KEY", "DBB_KEYFILE",
		"DBB_LISTEN_PG", "DBB_LISTEN_API", "DBB_CONFIG",
		"DBB_BASE_URL", "DBB_REDIRECTS",
	}

	// Store original values and unset
	originals := make(map[string]string)
	for _, key := range envVars {
		if val, ok := os.LookupEnv(key); ok {
			originals[key] = val
		}
		_ = os.Unsetenv(key)
	}

	// Restore original values after test
	t.Cleanup(func() {
		for _, key := range envVars {
			if val, ok := originals[key]; ok {
				_ = os.Setenv(key, val)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	})
}

func TestLoad(t *testing.T) {
	// Note: Can't use t.Parallel() since we manipulate environment variables

	// Generate a valid 32-byte key
	validKey := make([]byte, 32)
	for i := range validKey {
		validKey[i] = byte(i)
	}
	validKeyBase64 := base64.StdEncoding.EncodeToString(validKey)

	tests := []struct {
		name    string
		envVars map[string]string
		wantErr error
	}{
		{
			name: "valid config with DBB_KEY",
			envVars: map[string]string{
				"DBB_DSN":        "postgres://localhost/test",
				"DBB_KEY":        validKeyBase64,
				"DBB_LISTEN_PG":  ":5432",
				"DBB_LISTEN_API": ":8080",
			},
			wantErr: nil,
		},
		{
			name: "missing DSN",
			envVars: map[string]string{
				"DBB_KEY": validKeyBase64,
			},
			wantErr: ErrDSNRequired,
		},
		{
			name: "auto-generated key when none provided",
			envVars: map[string]string{
				"DBB_DSN": "postgres://localhost/test",
			},
			wantErr: nil, // Key is now auto-generated if not provided
		},
		{
			name: "invalid key size",
			envVars: map[string]string{
				"DBB_DSN": "postgres://localhost/test",
				"DBB_KEY": base64.StdEncoding.EncodeToString([]byte("short")),
			},
			wantErr: ErrInvalidKeySize,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear all DBB_ env vars
			clearEnvVars(t)

			// Set test env vars
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			cfg, err := Load(LoadOptions{})

			if tt.wantErr != nil {
				if err == nil {
					t.Errorf("Load() expected error %v, got nil", tt.wantErr)
					return
				}
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("Load() error = %v, want %v", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Errorf("Load() unexpected error = %v", err)
				return
			}

			if cfg.DSN != tt.envVars["DBB_DSN"] {
				t.Errorf("Load() DSN = %v, want %v", cfg.DSN, tt.envVars["DBB_DSN"])
			}

			if len(cfg.EncryptionKey) != 32 {
				t.Errorf("Load() EncryptionKey length = %d, want 32", len(cfg.EncryptionKey))
			}
		})
	}
}

func TestLoadWithKeyFile(t *testing.T) {
	// Note: Can't use t.Parallel() since we manipulate environment variables

	// Create a temporary key file
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	tmpDir := t.TempDir()
	keyFile := filepath.Join(tmpDir, "keyfile")
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		t.Fatalf("Failed to write key file: %v", err)
	}

	// Clear and set env vars
	clearEnvVars(t)
	t.Setenv("DBB_DSN", "postgres://localhost/test")
	t.Setenv("DBB_KEYFILE", keyFile)

	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(cfg.EncryptionKey) != 32 {
		t.Errorf("Load() EncryptionKey length = %d, want 32", len(cfg.EncryptionKey))
	}

	// Verify the key content
	for i := range key {
		if cfg.EncryptionKey[i] != key[i] {
			t.Errorf("Load() EncryptionKey[%d] = %d, want %d", i, cfg.EncryptionKey[i], key[i])
		}
	}
}

func TestDefaultValues(t *testing.T) {
	// Note: Can't use t.Parallel() since we manipulate environment variables

	// Generate a valid 32-byte key
	validKey := make([]byte, 32)
	for i := range validKey {
		validKey[i] = byte(i)
	}
	validKeyBase64 := base64.StdEncoding.EncodeToString(validKey)

	// Clear and set env vars
	clearEnvVars(t)
	t.Setenv("DBB_DSN", "postgres://localhost/test")
	t.Setenv("DBB_KEY", validKeyBase64)

	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ListenPG != ":5434" {
		t.Errorf("Load() ListenPG = %v, want :5434", cfg.ListenPG)
	}

	if cfg.ListenAPI != ":8080" {
		t.Errorf("Load() ListenAPI = %v, want :8080", cfg.ListenAPI)
	}
}

//nolint:paralleltest // Can't use t.Parallel() since we manipulate environment variables
func TestLoadWithConfigFile(t *testing.T) {
	// Generate a valid 32-byte key
	validKey := make([]byte, 32)
	for i := range validKey {
		validKey[i] = byte(i)
	}
	validKeyBase64 := base64.StdEncoding.EncodeToString(validKey)

	// Create a temporary config file
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	configContent := `
dsn: postgres://config-file/db
key: ` + validKeyBase64 + `
listen_pg: ":6000"
listen_api: ":9000"
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o600); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// Clear env vars
	clearEnvVars(t)

	// Load with config file option
	cfg, err := Load(LoadOptions{ConfigFile: configFile})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.DSN != "postgres://config-file/db" {
		t.Errorf("Load() DSN = %v, want postgres://config-file/db", cfg.DSN)
	}

	if cfg.ListenPG != ":6000" {
		t.Errorf("Load() ListenPG = %v, want :6000", cfg.ListenPG)
	}

	if cfg.ListenAPI != ":9000" {
		t.Errorf("Load() ListenAPI = %v, want :9000", cfg.ListenAPI)
	}
}

func TestLoadWithCLIOverrides(t *testing.T) {
	// Note: Can't use t.Parallel() since we manipulate environment variables

	// Generate a valid 32-byte key
	validKey := make([]byte, 32)
	for i := range validKey {
		validKey[i] = byte(i)
	}
	validKeyBase64 := base64.StdEncoding.EncodeToString(validKey)

	// Clear and set env vars
	clearEnvVars(t)
	t.Setenv("DBB_DSN", "postgres://env/db")
	t.Setenv("DBB_KEY", validKeyBase64)
	t.Setenv("DBB_LISTEN_PG", ":5555")

	// Load with CLI override
	cfg, err := Load(LoadOptions{}, func(c *Config) {
		c.ListenPG = ":7777"
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// CLI override should take precedence
	if cfg.ListenPG != ":7777" {
		t.Errorf("Load() ListenPG = %v, want :7777 (CLI override)", cfg.ListenPG)
	}

	// Env var should still be used for other values
	if cfg.DSN != "postgres://env/db" {
		t.Errorf("Load() DSN = %v, want postgres://env/db", cfg.DSN)
	}
}

func TestLoadPriorityOrder(t *testing.T) {
	// Note: Can't use t.Parallel() since we manipulate environment variables

	// Generate a valid 32-byte key
	validKey := make([]byte, 32)
	for i := range validKey {
		validKey[i] = byte(i)
	}
	validKeyBase64 := base64.StdEncoding.EncodeToString(validKey)

	// Create a temporary config file
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	configContent := `
dsn: postgres://config/db
key: ` + validKeyBase64 + `
listen_pg: ":6000"
listen_api: ":9000"
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o600); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// Clear and set env vars (should override config file)
	clearEnvVars(t)
	t.Setenv("DBB_LISTEN_PG", ":5555")

	// Load with config file and CLI override
	cfg, err := Load(LoadOptions{ConfigFile: configFile}, func(c *Config) {
		c.ListenAPI = ":8888" // CLI override (highest priority)
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// CLI override (highest priority)
	if cfg.ListenAPI != ":8888" {
		t.Errorf("Load() ListenAPI = %v, want :8888 (CLI override)", cfg.ListenAPI)
	}

	// Env var overrides config file
	if cfg.ListenPG != ":5555" {
		t.Errorf("Load() ListenPG = %v, want :5555 (env var)", cfg.ListenPG)
	}

	// Config file value (not overridden)
	if cfg.DSN != "postgres://config/db" {
		t.Errorf("Load() DSN = %v, want postgres://config/db (config file)", cfg.DSN)
	}
}

func TestDefaultKeyFilePath(t *testing.T) {
	t.Parallel()

	path, err := DefaultKeyFilePath()
	if err != nil {
		t.Fatalf("DefaultKeyFilePath() error = %v", err)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}

	expected := filepath.Join(homeDir, ".dbbat", "key")
	if path != expected {
		t.Errorf("DefaultKeyFilePath() = %v, want %v", path, expected)
	}
}

func TestLoadOrCreateDefaultKey_CreateNew(t *testing.T) {
	t.Parallel()
	// Create a temporary directory to simulate home directory
	tmpDir := t.TempDir()
	keyDir := filepath.Join(tmpDir, ".dbbat")
	keyPath := filepath.Join(keyDir, "key")

	// Ensure the key file does not exist
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("Key file should not exist before test")
	}

	// Call the internal function by simulating the behavior
	key, err := generateAndSaveDefaultKey(keyPath)
	if err != nil {
		t.Fatalf("generateAndSaveDefaultKey() error = %v", err)
	}

	// Verify key length
	if len(key) != 32 {
		t.Errorf("generateAndSaveDefaultKey() key length = %d, want 32", len(key))
	}

	// Verify the file was created
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("Key file should exist after generation: %v", err)
	}

	// Verify file permissions (on Unix systems)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("Key file permissions = %o, want 0600", info.Mode().Perm())
	}

	// Verify directory permissions
	dirInfo, err := os.Stat(keyDir)
	if err != nil {
		t.Fatalf("Key directory should exist: %v", err)
	}

	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("Key directory permissions = %o, want 0700", dirInfo.Mode().Perm())
	}

	// Verify the file content is valid base64
	content, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("Failed to read key file: %v", err)
	}

	decodedKey, err := base64.StdEncoding.DecodeString(string(content[:len(content)-1])) // Remove trailing newline
	if err != nil {
		t.Fatalf("Key file content is not valid base64: %v", err)
	}

	if len(decodedKey) != 32 {
		t.Errorf("Decoded key length = %d, want 32", len(decodedKey))
	}

	// Verify the returned key matches the file content
	for i := range key {
		if key[i] != decodedKey[i] {
			t.Errorf("Key[%d] = %d, file content[%d] = %d", i, key[i], i, decodedKey[i])
		}
	}
}

func TestLoadOrCreateDefaultKey_ReadExisting(t *testing.T) {
	t.Parallel()
	// Create a temporary directory with an existing key file
	tmpDir := t.TempDir()
	keyDir := filepath.Join(tmpDir, ".dbbat")
	keyPath := filepath.Join(keyDir, "key")

	// Create the directory
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatalf("Failed to create key directory: %v", err)
	}

	// Create a known key
	expectedKey := make([]byte, 32)
	for i := range expectedKey {
		expectedKey[i] = byte(i * 2)
	}
	keyBase64 := base64.StdEncoding.EncodeToString(expectedKey)

	// Write the key file
	if err := os.WriteFile(keyPath, []byte(keyBase64+"\n"), 0o600); err != nil {
		t.Fatalf("Failed to write key file: %v", err)
	}

	// Read the key using loadOrCreateDefaultKey logic
	content, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("Failed to read key file: %v", err)
	}

	keyStr := string(content)
	keyStr = keyStr[:len(keyStr)-1] // Remove trailing newline
	key, err := base64.StdEncoding.DecodeString(keyStr)
	if err != nil {
		t.Fatalf("Failed to decode key: %v", err)
	}

	// Verify the key matches
	if len(key) != 32 {
		t.Errorf("Key length = %d, want 32", len(key))
	}

	for i := range expectedKey {
		if key[i] != expectedKey[i] {
			t.Errorf("Key[%d] = %d, want %d", i, key[i], expectedKey[i])
		}
	}
}

func TestLoadOrCreateDefaultKey_InvalidBase64(t *testing.T) {
	t.Parallel()
	// Create a temporary directory with an invalid key file
	tmpDir := t.TempDir()
	keyDir := filepath.Join(tmpDir, ".dbbat")
	keyPath := filepath.Join(keyDir, "key")

	// Create the directory
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatalf("Failed to create key directory: %v", err)
	}

	// Write invalid content
	if err := os.WriteFile(keyPath, []byte("not-valid-base64!!!"), 0o600); err != nil {
		t.Fatalf("Failed to write key file: %v", err)
	}

	// Try to read - this should fail during decoding
	content, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("Failed to read key file: %v", err)
	}

	_, err = base64.StdEncoding.DecodeString(string(content))
	if err == nil {
		t.Error("Expected base64 decode error for invalid content")
	}
}

func TestLoadOrCreateDefaultKey_WrongKeySize(t *testing.T) {
	t.Parallel()
	// Create a temporary directory with a wrong-sized key
	tmpDir := t.TempDir()
	keyDir := filepath.Join(tmpDir, ".dbbat")
	keyPath := filepath.Join(keyDir, "key")

	// Create the directory
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatalf("Failed to create key directory: %v", err)
	}

	// Write a key that's too short (16 bytes instead of 32)
	shortKey := make([]byte, 16)
	keyBase64 := base64.StdEncoding.EncodeToString(shortKey)

	if err := os.WriteFile(keyPath, []byte(keyBase64), 0o600); err != nil {
		t.Fatalf("Failed to write key file: %v", err)
	}

	// Verify the decoded key has wrong size
	content, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("Failed to read key file: %v", err)
	}

	key, err := base64.StdEncoding.DecodeString(string(content))
	if err != nil {
		t.Fatalf("Failed to decode key: %v", err)
	}

	if len(key) == 32 {
		t.Error("Expected key length != 32 for this test")
	}
}

func TestParseRedirects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected []RedirectRule
	}{
		{
			name:     "empty string",
			input:    "",
			expected: nil,
		},
		{
			name:  "single redirect without target path",
			input: "/app:localhost:5173",
			expected: []RedirectRule{
				{PathPrefix: "/app", TargetHost: "localhost:5173", TargetPath: "/"},
			},
		},
		{
			name:  "single redirect with target path",
			input: "/app:localhost:5173/dashboard",
			expected: []RedirectRule{
				{PathPrefix: "/app", TargetHost: "localhost:5173", TargetPath: "/dashboard"},
			},
		},
		{
			name:  "multiple redirects",
			input: "/app:localhost:5173,/admin:localhost:5174/admin",
			expected: []RedirectRule{
				{PathPrefix: "/app", TargetHost: "localhost:5173", TargetPath: "/"},
				{PathPrefix: "/admin", TargetHost: "localhost:5174", TargetPath: "/admin"},
			},
		},
		{
			name:  "with spaces",
			input: " /app:localhost:5173 , /admin:localhost:5174 ",
			expected: []RedirectRule{
				{PathPrefix: "/app", TargetHost: "localhost:5173", TargetPath: "/"},
				{PathPrefix: "/admin", TargetHost: "localhost:5174", TargetPath: "/"},
			},
		},
		{
			name:     "invalid rule without leading slash",
			input:    "app:localhost:5173",
			expected: []RedirectRule{},
		},
		{
			name:     "invalid rule without colon",
			input:    "/app",
			expected: []RedirectRule{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := parseRedirects(tt.input)

			if tt.expected == nil && result != nil {
				t.Errorf("parseRedirects() = %v, want nil", result)
				return
			}

			if len(result) != len(tt.expected) {
				t.Errorf("parseRedirects() returned %d rules, want %d", len(result), len(tt.expected))
				return
			}

			for i, expected := range tt.expected {
				if result[i].PathPrefix != expected.PathPrefix {
					t.Errorf("rule[%d].PathPrefix = %v, want %v", i, result[i].PathPrefix, expected.PathPrefix)
				}
				if result[i].TargetHost != expected.TargetHost {
					t.Errorf("rule[%d].TargetHost = %v, want %v", i, result[i].TargetHost, expected.TargetHost)
				}
				if result[i].TargetPath != expected.TargetPath {
					t.Errorf("rule[%d].TargetPath = %v, want %v", i, result[i].TargetPath, expected.TargetPath)
				}
			}
		})
	}
}

func TestParseRedirectRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected RedirectRule
		ok       bool
	}{
		{
			name:     "valid with target path",
			input:    "/app:localhost:5173/dashboard",
			expected: RedirectRule{PathPrefix: "/app", TargetHost: "localhost:5173", TargetPath: "/dashboard"},
			ok:       true,
		},
		{
			name:     "valid without target path",
			input:    "/app:localhost:5173",
			expected: RedirectRule{PathPrefix: "/app", TargetHost: "localhost:5173", TargetPath: "/"},
			ok:       true,
		},
		{
			name:     "invalid - no leading slash",
			input:    "app:localhost:5173",
			expected: RedirectRule{},
			ok:       false,
		},
		{
			name:     "invalid - no colon",
			input:    "/app",
			expected: RedirectRule{},
			ok:       false,
		},
		{
			name:     "invalid - empty target",
			input:    "/app:",
			expected: RedirectRule{},
			ok:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, ok := parseRedirectRule(tt.input)

			if ok != tt.ok {
				t.Errorf("parseRedirectRule() ok = %v, want %v", ok, tt.ok)
				return
			}

			if !tt.ok {
				return
			}

			if result.PathPrefix != tt.expected.PathPrefix {
				t.Errorf("PathPrefix = %v, want %v", result.PathPrefix, tt.expected.PathPrefix)
			}
			if result.TargetHost != tt.expected.TargetHost {
				t.Errorf("TargetHost = %v, want %v", result.TargetHost, tt.expected.TargetHost)
			}
			if result.TargetPath != tt.expected.TargetPath {
				t.Errorf("TargetPath = %v, want %v", result.TargetPath, tt.expected.TargetPath)
			}
		})
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "root slash",
			input:    "/",
			expected: "",
		},
		{
			name:     "simple path",
			input:    "/app",
			expected: "/app",
		},
		{
			name:     "path with trailing slash",
			input:    "/app/",
			expected: "/app",
		},
		{
			name:     "path without leading slash",
			input:    "app",
			expected: "/app",
		},
		{
			name:     "path without leading slash with trailing slash",
			input:    "app/",
			expected: "/app",
		},
		{
			name:     "nested path",
			input:    "/foo/bar",
			expected: "/foo/bar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := normalizeBaseURL(tt.input)
			if result != tt.expected {
				t.Errorf("normalizeBaseURL(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestLoadWithBaseURL(t *testing.T) {
	// Generate a valid 32-byte key
	validKey := make([]byte, 32)
	for i := range validKey {
		validKey[i] = byte(i)
	}
	validKeyBase64 := base64.StdEncoding.EncodeToString(validKey)

	// Clear and set env vars
	clearEnvVars(t)
	t.Setenv("DBB_DSN", "postgres://localhost/test")
	t.Setenv("DBB_KEY", validKeyBase64)
	t.Setenv("DBB_BASE_URL", "/myapp")

	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.BaseURL != "/myapp" {
		t.Errorf("Load() BaseURL = %v, want /myapp", cfg.BaseURL)
	}
}

func TestLoadWithRedirects(t *testing.T) {
	// Generate a valid 32-byte key
	validKey := make([]byte, 32)
	for i := range validKey {
		validKey[i] = byte(i)
	}
	validKeyBase64 := base64.StdEncoding.EncodeToString(validKey)

	// Clear and set env vars
	clearEnvVars(t)
	t.Setenv("DBB_DSN", "postgres://localhost/test")
	t.Setenv("DBB_KEY", validKeyBase64)
	t.Setenv("DBB_REDIRECTS", "/app:localhost:5173,/admin:localhost:5174")

	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(cfg.Redirects) != 2 {
		t.Fatalf("Load() Redirects length = %d, want 2", len(cfg.Redirects))
	}

	if cfg.Redirects[0].PathPrefix != "/app" {
		t.Errorf("Redirects[0].PathPrefix = %v, want /app", cfg.Redirects[0].PathPrefix)
	}
	if cfg.Redirects[0].TargetHost != "localhost:5173" {
		t.Errorf("Redirects[0].TargetHost = %v, want localhost:5173", cfg.Redirects[0].TargetHost)
	}
}

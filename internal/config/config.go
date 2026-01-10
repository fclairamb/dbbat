package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// Configuration errors.
var (
	ErrDSNRequired    = errors.New("DBB_DSN environment variable is required")
	ErrKeyRequired    = errors.New("either DBB_KEY or DBB_KEYFILE must be set")
	ErrInvalidKeySize = errors.New("encryption key must be 32 bytes")
)

// RunMode represents the application run mode.
type RunMode string

const (
	// RunModeDefault is the default production mode.
	RunModeDefault RunMode = ""
	// RunModeTest provisions test data on startup.
	RunModeTest RunMode = "test"
)

// QueryStorageConfig holds configuration for query result storage.
type QueryStorageConfig struct {
	// MaxResultRows is the maximum number of rows to store per query.
	MaxResultRows int `koanf:"max_result_rows"`

	// MaxResultBytes is the maximum total bytes to store per query.
	MaxResultBytes int64 `koanf:"max_result_bytes"`

	// StoreResults enables/disables result storage globally.
	StoreResults bool `koanf:"store_results"`
}

// RateLimitConfig holds configuration for API rate limiting.
type RateLimitConfig struct {
	// Enabled enables/disables rate limiting.
	Enabled bool `koanf:"enabled"`

	// RequestsPerMinute is the rate limit for authenticated users.
	RequestsPerMinute int `koanf:"requests_per_minute"`

	// RequestsPerMinuteAnon is the rate limit for unauthenticated requests (by IP).
	RequestsPerMinuteAnon int `koanf:"requests_per_minute_anon"`

	// Burst allows short bursts above the rate limit.
	Burst int `koanf:"burst"`
}

// RedirectRule represents a path-based redirect for development proxying.
type RedirectRule struct {
	// PathPrefix is the path prefix to match (e.g., "/app").
	PathPrefix string
	// TargetHost is the target host to proxy to (e.g., "localhost:5173").
	TargetHost string
	// TargetPath is the path on the target (e.g., "/").
	TargetPath string
}

// Config holds the application configuration.
type Config struct {
	// Proxy listen address.
	ListenPG string `koanf:"listen_pg"`

	// REST API listen address.
	ListenAPI string `koanf:"listen_api"`

	// PostgreSQL DSN for DBBat storage.
	DSN string `koanf:"dsn"`

	// Base64-encoded encryption key (alternative to KeyFile).
	Key string `koanf:"key"`

	// Path to file containing encryption key (alternative to Key).
	KeyFile string `koanf:"keyfile"`

	// ConfigFile path (not loaded from config, set via CLI).
	ConfigFile string `koanf:"-"`

	// Encryption key for database credentials (32 bytes).
	// Populated from Key or KeyFile after loading.
	EncryptionKey []byte `koanf:"-"`

	// RunMode controls whether test data is provisioned on startup.
	RunMode RunMode `koanf:"run_mode"`

	// QueryStorage holds query result storage configuration.
	QueryStorage QueryStorageConfig `koanf:"query_storage"`

	// RateLimit holds rate limiting configuration.
	RateLimit RateLimitConfig `koanf:"rate_limit"`

	// BaseURL is the base URL path for the frontend app (default: "/app").
	BaseURL string `koanf:"base_url"`

	// Redirects contains dev redirect rules parsed from DBB_REDIRECTS env var.
	// Not loaded from config file, parsed from environment only.
	Redirects []RedirectRule `koanf:"-"`
}

// Default query storage limits.
const (
	DefaultMaxResultRows  = 100000
	DefaultMaxResultBytes = 100 * 1024 * 1024 // 100MB
)

// Default rate limiting settings.
const (
	DefaultRateLimitEnabled = true
	DefaultRateLimitRPM     = 60
	DefaultRateLimitRPMAnon = 10
	DefaultRateLimitBurst   = 10
)

const expectedKeySize = 32

// Default key file constants.
const (
	defaultKeyDirName  = ".dbbat"
	defaultKeyFileName = "key"
	defaultKeyDirPerm  = 0o700
	defaultKeyFilePerm = 0o600
)

// DefaultBaseURL is the default base URL path for the frontend.
const DefaultBaseURL = "/app"

// defaultConfig returns a Config with default values.
func defaultConfig() Config {
	return Config{
		ListenPG:  ":5432",
		ListenAPI: ":8080",
		BaseURL:   DefaultBaseURL,
		QueryStorage: QueryStorageConfig{
			MaxResultRows:  DefaultMaxResultRows,
			MaxResultBytes: DefaultMaxResultBytes,
			StoreResults:   true,
		},
		RateLimit: RateLimitConfig{
			Enabled:               DefaultRateLimitEnabled,
			RequestsPerMinute:     DefaultRateLimitRPM,
			RequestsPerMinuteAnon: DefaultRateLimitRPMAnon,
			Burst:                 DefaultRateLimitBurst,
		},
	}
}

// LoadOptions configures how configuration is loaded.
type LoadOptions struct {
	// ConfigFile is the path to a config file (YAML, JSON, or TOML).
	ConfigFile string
}

// koanfDelim is the delimiter used for nested config keys in koanf.
const koanfDelim = "."

// envTransform transforms environment variable names to koanf keys.
// DBB_LISTEN_PG -> listen_pg
// DBB_QUERY_STORAGE_MAX_RESULT_ROWS -> query_storage.max_result_rows
// DBB_RATE_LIMIT_ENABLED -> rate_limit.enabled
func envTransform(s string) string {
	key := strings.ToLower(strings.TrimPrefix(s, "DBB_"))
	// Map known prefixes to nested paths
	// query_storage_* -> query_storage.*
	if strings.HasPrefix(key, "query_storage_") {
		return "query_storage." + strings.TrimPrefix(key, "query_storage_")
	}
	// rate_limit_* -> rate_limit.*
	if strings.HasPrefix(key, "rate_limit_") {
		return "rate_limit." + strings.TrimPrefix(key, "rate_limit_")
	}
	return key
}

// Load reads configuration from environment variables and optional config file.
// Priority order: CLI overrides > Environment variables > Config file > Defaults
func Load(opts LoadOptions, cliOverrides ...func(*Config)) (*Config, error) {
	k := koanf.New(koanfDelim)

	// 1. Load defaults
	if err := k.Load(structs.Provider(defaultConfig(), "koanf"), nil); err != nil {
		return nil, fmt.Errorf("failed to load defaults: %w", err)
	}

	// 2. Determine config file path (CLI option takes precedence over DBB_CONFIG env var)
	configPath := opts.ConfigFile
	if configPath == "" {
		// Load env vars first just to check for DBB_CONFIG
		envK := koanf.New(koanfDelim)
		if err := envK.Load(env.Provider("DBB_", koanfDelim, envTransform), nil); err == nil {
			configPath = envK.String("config")
		}
	}

	// 3. Load config file if specified
	if configPath != "" {
		if err := loadConfigFile(k, configPath); err != nil {
			return nil, fmt.Errorf("failed to load config file: %w", err)
		}
	}

	// 4. Load environment variables (DBB_ prefix) - these override config file values
	if err := k.Load(env.Provider("DBB_", koanfDelim, envTransform), nil); err != nil {
		return nil, fmt.Errorf("failed to load environment variables: %w", err)
	}

	// Unmarshal into Config struct
	cfg := &Config{}
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// 4. Apply CLI overrides (highest priority)
	for _, override := range cliOverrides {
		override(cfg)
	}

	// Validate required fields
	if cfg.DSN == "" {
		return nil, ErrDSNRequired
	}

	// Load encryption key from Key or KeyFile
	key, err := loadEncryptionKey(cfg.Key, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load encryption key: %w", err)
	}

	cfg.EncryptionKey = key

	// Parse redirects from DBB_REDIRECTS environment variable
	cfg.Redirects = parseRedirects(os.Getenv("DBB_REDIRECTS"))

	// Normalize base URL
	cfg.BaseURL = normalizeBaseURL(cfg.BaseURL)

	return cfg, nil
}

// loadConfigFile loads configuration from a file based on its extension.
func loadConfigFile(k *koanf.Koanf, path string) error {
	var parser koanf.Parser

	switch {
	case strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml"):
		parser = yaml.Parser()
	case strings.HasSuffix(path, ".json"):
		parser = json.Parser()
	case strings.HasSuffix(path, ".toml"):
		parser = toml.Parser()
	default:
		// Default to YAML
		parser = yaml.Parser()
	}

	return k.Load(file.Provider(path), parser)
}

// loadEncryptionKey loads the encryption key from base64 string, file, or default location.
func loadEncryptionKey(keyStr, keyFile string) ([]byte, error) {
	// Try base64-encoded key first
	if keyStr != "" {
		key, err := base64.StdEncoding.DecodeString(keyStr)
		if err != nil {
			return nil, fmt.Errorf("failed to decode key: %w", err)
		}

		if len(key) != expectedKeySize {
			return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKeySize, len(key))
		}

		return key, nil
	}

	// Try key file
	if keyFile != "" {
		key, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read key file: %w", err)
		}

		if len(key) != expectedKeySize {
			return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKeySize, len(key))
		}

		return key, nil
	}

	// Fall back to default key file (~/.dbbat/key)
	return loadOrCreateDefaultKey()
}

// DefaultKeyFilePath returns the path to the default key file (~/.dbbat/key).
func DefaultKeyFilePath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	return filepath.Join(homeDir, defaultKeyDirName, defaultKeyFileName), nil
}

// loadOrCreateDefaultKey loads the key from the default location, creating it if necessary.
func loadOrCreateDefaultKey() ([]byte, error) {
	keyPath, err := DefaultKeyFilePath()
	if err != nil {
		return nil, err
	}

	// Try to read existing key file
	content, err := os.ReadFile(keyPath)
	if err == nil {
		// File exists, decode the base64 key
		keyStr := strings.TrimSpace(string(content))
		key, decodeErr := base64.StdEncoding.DecodeString(keyStr)
		if decodeErr != nil {
			return nil, fmt.Errorf("failed to decode key from %s: %w", keyPath, decodeErr)
		}

		if len(key) != expectedKeySize {
			return nil, fmt.Errorf("%w: got %d bytes from %s", ErrInvalidKeySize, len(key), keyPath)
		}

		return key, nil
	}

	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read key file %s: %w", keyPath, err)
	}

	// File doesn't exist, create a new key
	return generateAndSaveDefaultKey(keyPath)
}

// generateAndSaveDefaultKey generates a new encryption key and saves it to the default location.
func generateAndSaveDefaultKey(keyPath string) ([]byte, error) {
	// Create the directory if it doesn't exist
	keyDir := filepath.Dir(keyPath)
	if err := os.MkdirAll(keyDir, defaultKeyDirPerm); err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", keyDir, err)
	}

	// Ensure directory has correct permissions
	if err := os.Chmod(keyDir, defaultKeyDirPerm); err != nil {
		return nil, fmt.Errorf("failed to set permissions on %s: %w", keyDir, err)
	}

	// Generate a new random key
	key := make([]byte, expectedKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("failed to generate encryption key: %w", err)
	}

	// Encode as base64
	keyBase64 := base64.StdEncoding.EncodeToString(key)

	// Write to file
	if err := os.WriteFile(keyPath, []byte(keyBase64+"\n"), defaultKeyFilePerm); err != nil {
		return nil, fmt.Errorf("failed to write key file %s: %w", keyPath, err)
	}

	slog.Warn("generated new encryption key",
		"path", keyPath,
		"warning", "losing this key means encrypted credentials cannot be recovered")

	return key, nil
}

// parseRedirects parses the DBB_REDIRECTS environment variable.
// Format: /path:host:port/targetpath,/path2:host2:port2/targetpath2
// Example: /app:localhost:5173
// If the target path is omitted, it defaults to "/".
func parseRedirects(value string) []RedirectRule {
	if value == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	rules := make([]RedirectRule, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		rule, ok := parseRedirectRule(part)
		if !ok {
			slog.Warn("Invalid redirect rule, skipping", "rule", part)

			continue
		}

		rules = append(rules, rule)
	}

	if len(rules) > 0 {
		slog.Info("Loaded redirect rules", "count", len(rules))

		for i := range rules {
			r := &rules[i]
			slog.Debug("Redirect rule",
				"pathPrefix", r.PathPrefix,
				"targetHost", r.TargetHost,
				"targetPath", r.TargetPath)
		}
	}

	return rules
}

// parseRedirectRule parses a single redirect rule.
// Format: /path:host:port/targetpath or /path:host:port
func parseRedirectRule(rule string) (RedirectRule, bool) {
	if !strings.HasPrefix(rule, "/") {
		return RedirectRule{}, false
	}

	colonIdx := strings.Index(rule, ":")
	if colonIdx == -1 {
		return RedirectRule{}, false
	}

	pathPrefix := rule[:colonIdx]
	target := rule[colonIdx+1:]

	if target == "" {
		return RedirectRule{}, false
	}

	var targetHost, targetPath string

	slashIdx := strings.Index(target, "/")

	if slashIdx == -1 {
		// No path in target, e.g., "localhost:5173"
		targetHost = target
		targetPath = "/" // Default to root
	} else {
		// Has path, e.g., "localhost:5173/app"
		targetHost = target[:slashIdx]
		targetPath = target[slashIdx:]
	}

	if targetHost == "" {
		return RedirectRule{}, false
	}

	return RedirectRule{
		PathPrefix: pathPrefix,
		TargetHost: targetHost,
		TargetPath: targetPath,
	}, true
}

// normalizeBaseURL ensures the base URL starts with "/" and doesn't end with "/".
func normalizeBaseURL(baseURL string) string {
	if baseURL == "" || baseURL == "/" {
		return ""
	}

	// Ensure leading slash
	if !strings.HasPrefix(baseURL, "/") {
		baseURL = "/" + baseURL
	}

	// Remove trailing slash
	baseURL = strings.TrimSuffix(baseURL, "/")

	return baseURL
}

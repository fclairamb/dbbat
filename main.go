package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/fclairamb/dbbat/internal/api"
	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/proxy"
	"github.com/fclairamb/dbbat/internal/store"
)

const shutdownTimeout = 30 * time.Second

// setupLogger creates the logger, optionally writing to a file in test mode.
// Returns the logger and a cleanup function to close the log file (if any).
func setupLogger(runMode config.RunMode, level slog.Level) (*slog.Logger, func()) {
	var writer io.Writer = os.Stdout
	var cleanup func()

	if runMode == config.RunModeTest {
		writer, cleanup = setupTestLogFile()
	}

	logger := slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{
		Level: level,
	}))

	return logger, cleanup
}

// setupTestLogFile creates a log file for test mode and returns a writer and cleanup function.
func setupTestLogFile() (io.Writer, func()) {
	logDir := "logs"
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create logs directory: %v\n", err)
		return os.Stdout, nil
	}

	dateTimePrefix := time.Now().Format("2006-01-02_15-04-05")
	logFileName := filepath.Join(logDir, fmt.Sprintf("%s_dbbat.log", dateTimePrefix))

	logFile, err := os.OpenFile(logFileName, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create log file: %v\n", err)
		return os.Stdout, nil
	}

	return io.MultiWriter(os.Stdout, logFile), func() { _ = logFile.Close() }
}

// cliFlags holds CLI flag values that will override config.
type cliFlags struct {
	listenAddr string
	apiAddr    string
	dsn        string
	key        string
	keyFile    string
	configFile string
	logLevel   string
}

func main() {
	CmdRun()
}

func CmdRun() {
	flags := &cliFlags{}

	cmd := &cli.Command{
		Name:  "dbbat",
		Usage: "PostgreSQL observability proxy with controlled access",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "listen-addr",
				Aliases:     []string{"l"},
				Usage:       "Proxy listen address",
				Destination: &flags.listenAddr,
			},
			&cli.StringFlag{
				Name:        "api-addr",
				Aliases:     []string{"a"},
				Usage:       "REST API listen address",
				Destination: &flags.apiAddr,
			},
			&cli.StringFlag{
				Name:        "dsn",
				Aliases:     []string{"d"},
				Usage:       "PostgreSQL DSN for DBBat storage",
				Destination: &flags.dsn,
			},
			&cli.StringFlag{
				Name:        "key",
				Aliases:     []string{"k"},
				Usage:       "Base64-encoded AES-256 encryption key",
				Destination: &flags.key,
			},
			&cli.StringFlag{
				Name:        "keyfile",
				Usage:       "Path to file containing encryption key",
				Destination: &flags.keyFile,
			},
			&cli.StringFlag{
				Name:        "config",
				Aliases:     []string{"c"},
				Usage:       "Path to config file (YAML, JSON, or TOML)",
				Destination: &flags.configFile,
			},
			&cli.StringFlag{
				Name:        "log-level",
				Usage:       "Log level (debug, info, warn, error)",
				Sources:     cli.EnvVars("DBB_LOG_LEVEL"),
				Destination: &flags.logLevel,
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "serve",
				Usage: "Start DBBat server (default)",
				Action: func(ctx context.Context, _ *cli.Command) error {
					return runServer(ctx, flags)
				},
			},
			{
				Name:  "db",
				Usage: "Database migration commands",
				Commands: []*cli.Command{
					{
						Name:  "migrate",
						Usage: "Run pending migrations",
						Action: func(ctx context.Context, _ *cli.Command) error {
							return runMigrate(ctx, flags)
						},
					},
					{
						Name:  "rollback",
						Usage: "Rollback the last migration group",
						Action: func(ctx context.Context, _ *cli.Command) error {
							return runRollback(ctx, flags)
						},
					},
					{
						Name:  "status",
						Usage: "Show migration status",
						Action: func(ctx context.Context, _ *cli.Command) error {
							return runMigrationStatus(ctx, flags)
						},
					},
				},
			},
		},
		Action: func(ctx context.Context, _ *cli.Command) error {
			// Default action is to serve
			return runServer(ctx, flags)
		},
	}

	// Use a basic logger for CLI errors (before config is loaded)
	basicLogger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		basicLogger.ErrorContext(context.Background(), "Application error", slog.Any("error", err))
		os.Exit(1)
	}
}

// buildCLIOverrides creates a config override function from CLI flags.
func buildCLIOverrides(flags *cliFlags) func(*config.Config) {
	return func(cfg *config.Config) {
		if flags.listenAddr != "" {
			cfg.ListenPG = flags.listenAddr
		}
		if flags.apiAddr != "" {
			cfg.ListenAPI = flags.apiAddr
		}
		if flags.dsn != "" {
			cfg.DSN = flags.dsn
		}
		if flags.key != "" {
			cfg.Key = flags.key
		}
		if flags.keyFile != "" {
			cfg.KeyFile = flags.keyFile
		}
		if flags.configFile != "" {
			cfg.ConfigFile = flags.configFile
		}
		if flags.logLevel != "" {
			cfg.LogLevel = flags.logLevel
		}
	}
}

// loadConfigWithCLI loads configuration with CLI flag overrides.
func loadConfigWithCLI(flags *cliFlags) (*config.Config, error) {
	opts := config.LoadOptions{
		ConfigFile: flags.configFile,
	}
	return config.Load(opts, buildCLIOverrides(flags))
}

func runServer(ctx context.Context, flags *cliFlags) error {
	// Load configuration first
	cfg, err := loadConfigWithCLI(flags)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// Setup logger with run mode and log level from config
	logLevel := config.ParseLogLevel(cfg.LogLevel)
	logger, logCleanup := setupLogger(cfg.RunMode, logLevel)
	if logCleanup != nil {
		defer logCleanup()
	}
	slog.SetDefault(logger)

	logger.InfoContext(ctx, "Starting DBBat")
	logger.InfoContext(ctx, "Configuration loaded",
		slog.String("proxy_addr", cfg.ListenPG),
		slog.String("api_addr", cfg.ListenAPI),
		slog.Any("run_mode", cfg.RunMode),
		slog.String("log_level", cfg.LogLevel),
	)

	// Initialize store (with table drop if in test or demo mode)
	storeOpts := store.Options{
		DropTablesFirst: cfg.RunMode == config.RunModeTest || cfg.RunMode == config.RunModeDemo,
	}
	if cfg.RunMode == config.RunModeTest {
		logger.InfoContext(ctx, "Test mode enabled, will drop all tables before migration")
	}
	if cfg.RunMode == config.RunModeDemo {
		logger.WarnContext(ctx, "WARNING: Running in DEMO mode. Do not use in production environments.")
		logger.InfoContext(ctx, "Demo mode enabled, will drop all tables before migration")
	}

	dataStore, err := store.New(ctx, cfg.DSN, storeOpts)
	if err != nil {
		return fmt.Errorf("failed to initialize store: %w", err)
	}

	defer dataStore.Close()

	logger.InfoContext(ctx, "Database connection established")

	// Check for database configurations that match the storage DSN
	checkDatabaseConfigurations(ctx, dataStore, logger)

	// Ensure default admin exists
	defaultPassword := "admin"

	passwordHash, err := crypto.HashPassword(defaultPassword)
	if err != nil {
		return fmt.Errorf("failed to hash default admin password: %w", err)
	}

	if err := dataStore.EnsureDefaultAdmin(ctx, passwordHash); err != nil {
		return fmt.Errorf("failed to ensure default admin: %w", err)
	}

	logger.InfoContext(ctx, "Default admin user ensured (username: admin, password: admin)")

	// Provision test data if in test mode
	if cfg.RunMode == config.RunModeTest {
		if err := provisionTestData(ctx, dataStore, cfg.EncryptionKey, logger); err != nil {
			return fmt.Errorf("failed to provision test data: %w", err)
		}
	}

	// Provision demo data if in demo mode
	if cfg.RunMode == config.RunModeDemo {
		if err := provisionDemoData(ctx, dataStore, cfg, logger); err != nil {
			return fmt.Errorf("failed to provision demo data: %w", err)
		}
	}

	// Start API server
	apiServer := api.NewServer(dataStore, cfg.EncryptionKey, logger, cfg)

	go func() {
		if err := apiServer.Start(cfg.ListenAPI); err != nil {
			logger.ErrorContext(context.Background(), "API server error", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	logger.InfoContext(ctx, "API server started", slog.String("addr", cfg.ListenAPI))

	// Create auth cache for proxy server (shared cache config with API)
	proxyAuthCache := cache.NewAuthCache(cache.AuthCacheConfig{
		Enabled:    cfg.AuthCache.Enabled,
		TTLSeconds: cfg.AuthCache.TTLSeconds,
		MaxSize:    cfg.AuthCache.MaxSize,
	})

	// Start proxy server
	proxyServer := proxy.NewServer(dataStore, cfg.EncryptionKey, cfg.QueryStorage, proxyAuthCache, logger)

	go func() {
		if err := proxyServer.Start(cfg.ListenPG); err != nil {
			logger.ErrorContext(context.Background(), "Proxy server error", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	logger.InfoContext(ctx, "Proxy server started", slog.String("addr", cfg.ListenPG))

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan
	logger.InfoContext(ctx, "Shutdown signal received, gracefully shutting down...")

	// Graceful shutdown with timeout - use fresh context since main context may be canceled
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// Shutdown API server
	if err := apiServer.Shutdown(shutdownCtx); err != nil {
		logger.ErrorContext(shutdownCtx, "API server shutdown error", slog.Any("error", err))
	}

	// Shutdown proxy server
	if err := proxyServer.Shutdown(shutdownCtx); err != nil {
		logger.ErrorContext(shutdownCtx, "Proxy server shutdown error", slog.Any("error", err))
	}

	logger.InfoContext(shutdownCtx, "Shutdown complete")
	return nil
}

func runMigrate(ctx context.Context, flags *cliFlags) error {
	cfg, err := loadConfigWithCLI(flags)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	logLevel := config.ParseLogLevel(cfg.LogLevel)
	logger, logCleanup := setupLogger(cfg.RunMode, logLevel)
	if logCleanup != nil {
		defer logCleanup()
	}
	slog.SetDefault(logger)

	logger.InfoContext(ctx, "Running migrations")

	dataStore, err := store.New(ctx, cfg.DSN)
	if err != nil {
		return fmt.Errorf("failed to initialize store: %w", err)
	}
	defer dataStore.Close()

	if err := dataStore.Migrate(ctx); err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}

	logger.InfoContext(ctx, "Migrations completed successfully")
	return nil
}

func runRollback(ctx context.Context, flags *cliFlags) error {
	cfg, err := loadConfigWithCLI(flags)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	logLevel := config.ParseLogLevel(cfg.LogLevel)
	logger, logCleanup := setupLogger(cfg.RunMode, logLevel)
	if logCleanup != nil {
		defer logCleanup()
	}
	slog.SetDefault(logger)

	logger.InfoContext(ctx, "Rolling back migrations")

	dataStore, err := store.New(ctx, cfg.DSN)
	if err != nil {
		return fmt.Errorf("failed to initialize store: %w", err)
	}
	defer dataStore.Close()

	if err := dataStore.Rollback(ctx); err != nil {
		return fmt.Errorf("rollback failed: %w", err)
	}

	logger.InfoContext(ctx, "Rollback completed successfully")
	return nil
}

func runMigrationStatus(ctx context.Context, flags *cliFlags) error {
	cfg, err := loadConfigWithCLI(flags)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	logLevel := config.ParseLogLevel(cfg.LogLevel)
	logger, logCleanup := setupLogger(cfg.RunMode, logLevel)
	if logCleanup != nil {
		defer logCleanup()
	}
	slog.SetDefault(logger)

	dataStore, err := store.New(ctx, cfg.DSN)
	if err != nil {
		return fmt.Errorf("failed to initialize store: %w", err)
	}
	defer dataStore.Close()

	migrationInfos, err := dataStore.MigrationStatus(ctx)
	if err != nil {
		return fmt.Errorf("failed to get migration status: %w", err)
	}

	logger.InfoContext(ctx, "Migration status")
	for _, m := range migrationInfos {
		status := "pending"
		if !m.MigratedAt.IsZero() {
			status = fmt.Sprintf("applied at %s", m.MigratedAt.Format(time.RFC3339))
		}
		logger.InfoContext(ctx, "Migration", slog.String("name", m.Name), slog.String("status", status))
	}

	return nil
}

func provisionTestData(ctx context.Context, dataStore *store.Store, encryptionKey []byte, logger *slog.Logger) error {
	logger.InfoContext(ctx, "Test mode: provisioning test data...")

	// 1. Update admin password to "admintest" and mark as changed
	adminUser, err := dataStore.GetUserByUsername(ctx, "admin")
	if err != nil {
		return fmt.Errorf("failed to get admin user: %w", err)
	}

	adminTestPasswordHash, err := crypto.HashPassword("admintest")
	if err != nil {
		return fmt.Errorf("failed to hash admintest password: %w", err)
	}

	err = dataStore.UpdateUser(ctx, adminUser.UID, store.UserUpdate{
		PasswordHash: &adminTestPasswordHash,
	})
	if err != nil {
		return fmt.Errorf("failed to update admin password: %w", err)
	}
	logger.InfoContext(ctx, "Updated admin password to 'admintest'")

	// 2. Create viewer user (viewer role only)
	viewerPasswordHash, err := crypto.HashPassword("viewer")
	if err != nil {
		return fmt.Errorf("failed to hash viewer password: %w", err)
	}

	viewerUser, err := dataStore.CreateUser(ctx, "viewer", viewerPasswordHash, []string{store.RoleViewer})
	if err != nil {
		return fmt.Errorf("failed to create viewer user: %w", err)
	}
	// Mark password as changed so the user can log in immediately
	err = dataStore.UpdateUser(ctx, viewerUser.UID, store.UserUpdate{
		PasswordHash: &viewerPasswordHash,
	})
	if err != nil {
		return fmt.Errorf("failed to mark viewer password as changed: %w", err)
	}
	logger.InfoContext(ctx, "Created viewer user (username: viewer, password: viewer)")

	// 3. Create connector user (connector role only)
	connectorPasswordHash, err := crypto.HashPassword("connector")
	if err != nil {
		return fmt.Errorf("failed to hash connector password: %w", err)
	}

	connectorUser, err := dataStore.CreateUser(ctx, "connector", connectorPasswordHash, []string{store.RoleConnector})
	if err != nil {
		return fmt.Errorf("failed to create connector user: %w", err)
	}
	// Mark password as changed so the user can log in immediately
	err = dataStore.UpdateUser(ctx, connectorUser.UID, store.UserUpdate{
		PasswordHash: &connectorPasswordHash,
	})
	if err != nil {
		return fmt.Errorf("failed to mark connector password as changed: %w", err)
	}
	logger.InfoContext(ctx, "Created connector user (username: connector, password: connector)")

	// 4. Create proxy_target database configuration
	targetDB, err := dataStore.CreateDatabase(ctx, &store.Database{
		Name:         "proxy_target",
		Description:  "Target test database from docker-compose",
		Host:         "localhost",
		Port:         5002,
		DatabaseName: "target",
		Username:     "postgres",
		Password:     "postgres",
		SSLMode:      "disable",
		CreatedBy:    &adminUser.UID,
	}, encryptionKey)
	if err != nil {
		return fmt.Errorf("failed to create proxy_target database config: %w", err)
	}
	logger.InfoContext(ctx, "Created proxy_target database configuration")

	// 5. Create write grant for connector user (empty controls = full write access)
	_, err = dataStore.CreateGrant(ctx, &store.Grant{
		UserID:     connectorUser.UID,
		DatabaseID: targetDB.UID,
		Controls:   []string{}, // Empty = full write access
		GrantedBy:  adminUser.UID,
		StartsAt:   time.Now(),
		ExpiresAt:  time.Now().AddDate(10, 0, 0), // 10 years from now
	})
	if err != nil {
		return fmt.Errorf("failed to create write grant for connector user: %w", err)
	}
	logger.InfoContext(ctx, "Created write grant for connector user on proxy_target")

	// 6. Create read-only grant for viewer user
	_, err = dataStore.CreateGrant(ctx, &store.Grant{
		UserID:     viewerUser.UID,
		DatabaseID: targetDB.UID,
		Controls:   []string{store.ControlReadOnly}, // Read-only access
		GrantedBy:  adminUser.UID,
		StartsAt:   time.Now(),
		ExpiresAt:  time.Now().AddDate(10, 0, 0), // 10 years from now
	})
	if err != nil {
		return fmt.Errorf("failed to create read-only grant for viewer user: %w", err)
	}
	logger.InfoContext(ctx, "Created read-only grant for viewer user on proxy_target")

	logger.InfoContext(ctx, "Test data provisioning complete")
	return nil
}

func provisionDemoData(ctx context.Context, dataStore *store.Store, cfg *config.Config, logger *slog.Logger) error {
	logger.InfoContext(ctx, "Demo mode: provisioning demo data...")

	// Get demo target configuration
	demoTarget := cfg.GetDemoTarget()
	if demoTarget == nil {
		demoTarget = &config.DemoTarget{
			Username: "demo",
			Password: "demo",
			Host:     "localhost",
		}
	}
	logger.InfoContext(ctx, "Demo target", slog.String("user", demoTarget.Username), slog.String("host", demoTarget.Host))

	// 1. Get admin user and mark password as changed (password is already "admin" from EnsureDefaultAdmin)
	adminUser, err := dataStore.GetUserByUsername(ctx, "admin")
	if err != nil {
		return fmt.Errorf("failed to get admin user: %w", err)
	}

	// Update admin to mark password as changed so they can log in immediately
	adminPasswordHash, err := crypto.HashPassword("admin")
	if err != nil {
		return fmt.Errorf("failed to hash admin password: %w", err)
	}

	err = dataStore.UpdateUser(ctx, adminUser.UID, store.UserUpdate{
		PasswordHash: &adminPasswordHash,
	})
	if err != nil {
		return fmt.Errorf("failed to update admin password: %w", err)
	}
	logger.InfoContext(ctx, "Marked admin password as changed (username: admin, password: admin)")

	// 2. Create viewer user (viewer role only)
	viewerPasswordHash, err := crypto.HashPassword("viewer")
	if err != nil {
		return fmt.Errorf("failed to hash viewer password: %w", err)
	}

	viewerUser, err := dataStore.CreateUser(ctx, "viewer", viewerPasswordHash, []string{store.RoleViewer})
	if err != nil {
		return fmt.Errorf("failed to create viewer user: %w", err)
	}
	// Mark password as changed so the user can log in immediately
	err = dataStore.UpdateUser(ctx, viewerUser.UID, store.UserUpdate{
		PasswordHash: &viewerPasswordHash,
	})
	if err != nil {
		return fmt.Errorf("failed to mark viewer password as changed: %w", err)
	}
	logger.InfoContext(ctx, "Created viewer user (username: viewer, password: viewer)")

	// 3. Create connector user (connector role only)
	connectorPasswordHash, err := crypto.HashPassword("connector")
	if err != nil {
		return fmt.Errorf("failed to hash connector password: %w", err)
	}

	connectorUser, err := dataStore.CreateUser(ctx, "connector", connectorPasswordHash, []string{store.RoleConnector})
	if err != nil {
		return fmt.Errorf("failed to create connector user: %w", err)
	}
	// Mark password as changed so the user can log in immediately
	err = dataStore.UpdateUser(ctx, connectorUser.UID, store.UserUpdate{
		PasswordHash: &connectorPasswordHash,
	})
	if err != nil {
		return fmt.Errorf("failed to mark connector password as changed: %w", err)
	}
	logger.InfoContext(ctx, "Created connector user (username: connector, password: connector)")

	// 4. Create demo_db database configuration using demo target
	demoDB, err := dataStore.CreateDatabase(ctx, &store.Database{
		Name:         "demo_db",
		Description:  "Demo database",
		Host:         demoTarget.Host,
		Port:         5432,
		DatabaseName: "demo",
		Username:     demoTarget.Username,
		Password:     demoTarget.Password,
		SSLMode:      "disable",
		CreatedBy:    &adminUser.UID,
	}, cfg.EncryptionKey)
	if err != nil {
		return fmt.Errorf("failed to create demo_db database config: %w", err)
	}
	logger.InfoContext(ctx, "Created demo_db database configuration")

	// 5. Create write grant for connector user (empty controls = full write access)
	_, err = dataStore.CreateGrant(ctx, &store.Grant{
		UserID:     connectorUser.UID,
		DatabaseID: demoDB.UID,
		Controls:   []string{}, // Empty = full write access
		GrantedBy:  adminUser.UID,
		StartsAt:   time.Now(),
		ExpiresAt:  time.Now().AddDate(10, 0, 0), // 10 years from now
	})
	if err != nil {
		return fmt.Errorf("failed to create write grant for connector user: %w", err)
	}
	logger.InfoContext(ctx, "Created write grant for connector user on demo_db")

	// 6. Create read-only grant for viewer user
	_, err = dataStore.CreateGrant(ctx, &store.Grant{
		UserID:     viewerUser.UID,
		DatabaseID: demoDB.UID,
		Controls:   []string{store.ControlReadOnly}, // Read-only access
		GrantedBy:  adminUser.UID,
		StartsAt:   time.Now(),
		ExpiresAt:  time.Now().AddDate(10, 0, 0), // 10 years from now
	})
	if err != nil {
		return fmt.Errorf("failed to create read-only grant for viewer user: %w", err)
	}
	logger.InfoContext(ctx, "Created read-only grant for viewer user on demo_db")

	logger.InfoContext(ctx, "Demo data provisioning complete")
	return nil
}

// checkDatabaseConfigurations checks if any configured target databases match the storage DSN.
// Logs a warning for each match found. This handles databases that were configured
// before the storage DSN validation was added.
func checkDatabaseConfigurations(ctx context.Context, dataStore *store.Store, logger *slog.Logger) {
	databases, err := dataStore.ListDatabases(ctx)
	if err != nil {
		logger.WarnContext(ctx, "failed to check database configurations", slog.Any("error", err))
		return
	}

	for _, db := range databases {
		if dataStore.MatchesStorageDSN(db.Host, db.Port, db.DatabaseName) {
			logger.WarnContext(ctx, "SECURITY WARNING: database configuration matches storage DSN",
				slog.String("database_name", db.Name),
				slog.String("target", fmt.Sprintf("%s:%d/%s", db.Host, db.Port, db.DatabaseName)),
				slog.String("recommendation", "use a separate database for DBBat storage to prevent privilege escalation"))
		}
	}
}

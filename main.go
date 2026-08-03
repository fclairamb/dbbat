package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/urfave/cli/v3"

	"github.com/fclairamb/dbbat/internal/api"
	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/notify"
	"github.com/fclairamb/dbbat/internal/proxy/mongodb"
	"github.com/fclairamb/dbbat/internal/proxy/mysql"
	"github.com/fclairamb/dbbat/internal/proxy/oracle"
	"github.com/fclairamb/dbbat/internal/proxy/postgresql"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
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
				Usage: "Server migration commands",
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
			dumpCommand(),
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
		slog.String("instance_id", cfg.InstanceID),
	)

	// Initialize store (with table drop if in test or demo mode)
	storeOpts := store.Options{
		DropTablesFirst: cfg.RunMode == config.RunModeTest || cfg.RunMode == config.RunModeDemo,
		InstanceID:      cfg.InstanceID,
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

	logger.InfoContext(ctx, "Server connection established")

	// Check for database configurations that match the storage DSN
	checkDatabaseConfigurations(ctx, dataStore, logger)

	// Announce this process, then close what dead processes left open. Both
	// must happen before any proxy accepts.
	heartbeat := registerAndReconcile(ctx, dataStore, logger)

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

	apiServer, approvals, approvalDeps := buildEventPlumbing(ctx, cfg, dataStore, logger)

	go func() {
		if err := apiServer.Start(cfg.ListenAPI); err != nil {
			logger.ErrorContext(context.Background(), "API server error", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	logger.InfoContext(ctx, "API server started", slog.String("addr", cfg.ListenAPI))

	// One result-row writer for the whole process: batches span protocols,
	// sessions and queries (see shared.RowWriter).
	rowWriter := shared.NewRowWriter(dataStore, logger)

	// Create auth cache for proxy server (shared cache config with API)
	proxyAuthCache := cache.NewAuthCache(cache.AuthCacheConfig{
		Enabled:    cfg.AuthCache.Enabled,
		TTLSeconds: cfg.AuthCache.TTLSeconds,
		MaxSize:    cfg.AuthCache.MaxSize,
	})

	// Start proxy server
	proxyServer, err := postgresql.NewServer(dataStore, cfg.EncryptionKey, cfg.QueryStorage, cfg.Dump, proxyAuthCache, cfg.PG, logger)
	if err != nil {
		logger.ErrorContext(ctx, "PostgreSQL proxy server init failed", slog.Any("error", err))
		os.Exit(1)
	}

	proxyServer.SetApprovalDeps(approvalDeps)
	proxyServer.SetRowWriter(rowWriter)

	go func() {
		if err := proxyServer.Start(cfg.ListenPG); err != nil {
			logger.ErrorContext(context.Background(), "Proxy server error", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	logger.InfoContext(ctx, "Proxy server started",
		slog.String("addr", cfg.ListenPG),
		slog.Bool("tls", !cfg.PG.TLS.Disable))

	// Start Oracle proxy server (if configured)
	oracleServer := startOracleProxy(ctx, cfg, dataStore, proxyAuthCache, approvalDeps, rowWriter, logger)

	// Start MySQL proxy server (if configured)
	mysqlServer := startMySQLProxy(ctx, cfg, dataStore, proxyAuthCache, approvalDeps, rowWriter, logger)

	// Start MongoDB proxy server (if configured)
	mongoServer := startMongoProxy(ctx, cfg, dataStore, proxyAuthCache, approvalDeps, rowWriter, logger)

	// One retention sweep for the whole process (nil when disabled, the default).
	sweeper := startQueryRetentionSweep(ctx, cfg, dataStore, logger)

	// Draining releases parked queries first, then stops the servers.
	servers := collectServers(approvalDrain{approvals, logger}, apiServer, proxyServer,
		oracleServer, mysqlServer, mongoServer, rowWriter, sweeper, heartbeat)

	return awaitShutdown(ctx, logger, servers...)
}

// registerAndReconcile puts this process in the instance registry and then
// clears the connection rows left behind by processes that are gone.
//
// The order matters twice over. The reconcile reads the registry, so our own
// row has to be there first; and every other replica reads the registry to
// decide whether our connections are live, so the row must exist before this
// process opens any. Both therefore run before any proxy accepts — a session
// opened by this run can never be caught by its own reconcile.
//
// Returns the heartbeat that keeps our row fresh, for the shutdown sequence to
// stop last of all. Nil when there is no instance id to register.
func registerAndReconcile(ctx context.Context, dataStore *store.Store, logger *slog.Logger) *instanceHeartbeat {
	heartbeat := startInstanceHeartbeat(ctx, dataStore, logger)

	reconcileOrphanedConnections(ctx, dataStore, logger)

	return heartbeat
}

// reconcileOrphanedConnections closes the connection rows left open by a
// process that is no longer running. Without it a crash, a kill or a pod
// reschedule leaves disconnected_at NULL forever, and the retention sweep —
// which deliberately never reaps a connection that still looks open — can never
// remove those rows.
//
// It covers this instance id's own leftovers (DBB_INSTANCE_ID, defaulting to
// the hostname) plus those of any instance the registry proves is gone. dbbat
// runs with more than one replica against a shared store, so a blanket update
// would let a starting replica close another replica's live sessions; liveness,
// tracked by the instance heartbeat, is what makes widening it safe. See
// store.CloseOrphanedConnections.
//
// The two counts are logged separately, and reclaims are logged even when the
// own count is zero: connections reclaimed from another instance mean a process
// died without shutting down, which is worth noticing on its own.
//
// A failure here is logged, not fatal: stale bookkeeping must not stop the
// proxy from serving traffic.
func reconcileOrphanedConnections(ctx context.Context, dataStore *store.Store, logger *slog.Logger) {
	closed, err := dataStore.CloseOrphanedConnections(ctx)
	if err != nil {
		logger.ErrorContext(ctx, "failed to reconcile orphaned connections", slog.Any("error", err))

		return
	}

	if closed.Own > 0 {
		// Worth info level: a large count means the previous run did not shut
		// down cleanly.
		logger.InfoContext(ctx, "Closed connections left open by a previous run",
			slog.Int64("connections", closed.Own),
			slog.String("instance_id", dataStore.InstanceID()))
	}

	if closed.Reclaimed > 0 {
		logger.InfoContext(ctx, "Reclaimed connections left open by instances that are gone",
			slog.Int64("connections", closed.Reclaimed),
			slog.Duration("stale_after", store.InstanceStaleAfter))
	}

	// Housekeeping, after the reclaim so it still saw the rows it judged.
	if pruned, err := dataStore.PruneStaleInstances(ctx); err != nil {
		logger.WarnContext(ctx, "failed to prune stale instances", slog.Any("error", err))
	} else if pruned > 0 {
		logger.DebugContext(ctx, "Pruned stale instance registrations", slog.Int64("instances", pruned))
	}
}

// shutdownable is implemented by servers that support graceful shutdown.
type shutdownable interface {
	Shutdown(ctx context.Context) error
}

// awaitShutdown waits for an OS interrupt signal and then gracefully shuts down all servers.
func awaitShutdown(ctx context.Context, logger *slog.Logger, servers ...shutdownable) error {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan
	logger.InfoContext(ctx, "Shutdown signal received, gracefully shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.ErrorContext(shutdownCtx, "server shutdown error", slog.Any("error", err))
		}
	}

	logger.InfoContext(shutdownCtx, "Shutdown complete")

	return nil
}

func startOracleProxy(
	ctx context.Context,
	cfg *config.Config,
	dataStore *store.Store,
	authCache *cache.AuthCache,
	approvalDeps shared.ApprovalDeps,
	rowWriter *shared.RowWriter,
	logger *slog.Logger,
) *oracle.Server {
	if cfg.ListenOracle == "" {
		return nil
	}

	srv := oracle.NewServer(dataStore, cfg.EncryptionKey, authCache, cfg.QueryStorage, cfg.Dump, logger)
	srv.SetApprovalDeps(approvalDeps)
	srv.SetRowWriter(rowWriter)

	go func() {
		if err := srv.Start(cfg.ListenOracle); err != nil {
			logger.ErrorContext(context.Background(), "Oracle proxy server error", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	logger.InfoContext(ctx, "Oracle proxy server started", slog.String("addr", cfg.ListenOracle))

	return srv
}

func startMySQLProxy(
	ctx context.Context,
	cfg *config.Config,
	dataStore *store.Store,
	authCache *cache.AuthCache,
	approvalDeps shared.ApprovalDeps,
	rowWriter *shared.RowWriter,
	logger *slog.Logger,
) *mysql.Server {
	if cfg.ListenMySQL == "" {
		return nil
	}

	srv, err := mysql.NewServer(dataStore, cfg.EncryptionKey, cfg.QueryStorage, cfg.Dump, authCache, cfg.MySQL, logger)
	if err != nil {
		logger.ErrorContext(ctx, "MySQL proxy server init failed", slog.Any("error", err))
		os.Exit(1)
	}

	srv.SetApprovalDeps(approvalDeps)
	srv.SetRowWriter(rowWriter)

	go func() {
		if err := srv.Start(cfg.ListenMySQL); err != nil {
			logger.ErrorContext(context.Background(), "MySQL proxy server error", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	logger.InfoContext(ctx, "MySQL proxy server started",
		slog.String("addr", cfg.ListenMySQL),
		slog.Bool("tls", !cfg.MySQL.TLS.Disable))

	return srv
}

func startMongoProxy(
	ctx context.Context,
	cfg *config.Config,
	dataStore *store.Store,
	authCache *cache.AuthCache,
	approvalDeps shared.ApprovalDeps,
	rowWriter *shared.RowWriter,
	logger *slog.Logger,
) *mongodb.Server {
	if cfg.ListenMongo == "" {
		return nil
	}

	srv, err := mongodb.NewServer(dataStore, cfg.EncryptionKey, cfg.QueryStorage, cfg.Dump, authCache, cfg.Mongo, logger)
	if err != nil {
		logger.ErrorContext(ctx, "MongoDB proxy server init failed", slog.Any("error", err))
		os.Exit(1)
	}

	srv.SetApprovalDeps(approvalDeps)
	srv.SetRowWriter(rowWriter)

	go func() {
		if err := srv.Start(cfg.ListenMongo); err != nil {
			logger.ErrorContext(context.Background(), "MongoDB proxy server error", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	logger.InfoContext(ctx, "MongoDB proxy server started",
		slog.String("addr", cfg.ListenMongo),
		slog.Bool("tls", !cfg.Mongo.TLS.Disable))

	return srv
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
	targetDB, err := dataStore.CreateServer(ctx, &store.Server{
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

	// 6b. Create a quota-bounded grant for the admin user so the grants list
	// has a grant with applied limits (alongside the unlimited grants above).
	if err := seedQuotaGrant(ctx, dataStore, adminUser.UID, targetDB.UID); err != nil {
		return err
	}
	logger.InfoContext(ctx, "Created quota-bounded grant for admin user on proxy_target")

	// 6c. Seed a sample logged query (via a closed connection) so the queries
	// list has a row and the query-detail breadcrumb has real SQL text to
	// preview in test mode.
	if err := seedSampleQuery(ctx, dataStore, adminUser.UID, targetDB.UID); err != nil {
		return err
	}
	logger.InfoContext(ctx, "Created sample logged query on proxy_target")

	// 7. Create stable API keys for test users
	testKeys := []struct {
		user *store.User
		name string
		key  string
	}{
		{adminUser, "admin-test-key", "dbb_admin_key"},
		{connectorUser, "connector-test-key", "dbb_connector_key"},
		{viewerUser, "viewer-test-key", "dbb_viewer_key"},
	}
	for _, tk := range testKeys {
		if _, err := dataStore.CreateAPIKeyWithValue(ctx, tk.user.UID, tk.name, tk.key, nil, encryptionKey); err != nil {
			return fmt.Errorf("failed to create test API key for %s: %w", tk.user.Username, err)
		}
		logger.InfoContext(ctx, "Created test API key", slog.String("user", tk.user.Username), slog.String("key", tk.key))
	}

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
	demoDB, err := dataStore.CreateServer(ctx, &store.Server{
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

// seedQuotaGrant creates a read-only grant bounded by a query count and a
// data-transfer quota, so the test-mode grants list has a grant with applied
// limits to render (alongside the unlimited seed grants).
func seedQuotaGrant(ctx context.Context, dataStore *store.Store, userID, databaseID uuid.UUID) error {
	maxQueries := int64(100)
	maxBytes := int64(1024 * 1024 * 1024) // 1 GB
	_, err := dataStore.CreateGrant(ctx, &store.Grant{
		UserID:              userID,
		DatabaseID:          databaseID,
		Controls:            []string{store.ControlReadOnly},
		GrantedBy:           userID,
		StartsAt:            time.Now(),
		ExpiresAt:           time.Now().AddDate(10, 0, 0), // 10 years from now
		MaxQueryCounts:      &maxQueries,
		MaxBytesTransferred: &maxBytes,
	})
	if err != nil {
		return fmt.Errorf("failed to create quota grant: %w", err)
	}
	return nil
}

// seedSampleQuery records one historical query on a closed connection so the
// test-mode queries list has a clickable row and the query-detail breadcrumb
// has real SQL text (long enough to exercise the preview truncation) to render.
func seedSampleQuery(ctx context.Context, dataStore *store.Store, userID, databaseID uuid.UUID) error {
	conn, err := dataStore.CreateConnection(ctx, userID, databaseID, "127.0.0.1")
	if err != nil {
		return fmt.Errorf("failed to create sample connection: %w", err)
	}

	durationMs := 1.234
	rowsAffected := int64(3)
	if _, err := dataStore.CreateQuery(ctx, &store.Query{
		ConnectionID: conn.UID,
		// Deliberately avoids nav-section words (users, databases, grants,
		// connections, queries, audit) so the e2e navigation test's broad
		// `getByRole("link", { name: /users/i })`-style locators don't match
		// this query's row link on the dashboard's "Recent Queries" table.
		SQLText:      "SELECT order_id, total_amount, status FROM orders WHERE status = 'shipped' ORDER BY order_id DESC",
		ExecutedAt:   time.Now(),
		DurationMs:   &durationMs,
		RowsAffected: &rowsAffected,
	}); err != nil {
		return fmt.Errorf("failed to create sample query: %w", err)
	}

	// Close the connection so it doesn't linger as an "active" connection.
	if err := dataStore.CloseConnection(ctx, conn.UID); err != nil {
		return fmt.Errorf("failed to close sample connection: %w", err)
	}
	return nil
}

// dumpCommand builds the `dbbat dump` command tree.
func dumpCommand() *cli.Command {
	return &cli.Command{
		Name:  "dump",
		Usage: "Session capture (pcapng) commands",
		Commands: []*cli.Command{
			{
				Name:      "anonymise",
				Aliases:   []string{"anonymize"},
				Usage:     "Create an anonymised copy of a capture (strips session metadata and, by default, the synthesized addresses)",
				ArgsUsage: "<input-file> [output-file]",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:  "keep-addresses",
						Usage: "keep the synthesized IP addresses and ports (they may encode the real upstream)",
					},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return runDumpAnonymise(cmd)
				},
			},
		},
	}
}

var errDumpAnonymiseUsage = errors.New("usage: dbbat dump anonymise [--keep-addresses] <input-file> [output-file]")

func runDumpAnonymise(cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() < 1 {
		return errDumpAnonymiseUsage
	}

	inputPath := args.Get(0)

	outputPath := args.Get(1)
	if outputPath == "" {
		ext := filepath.Ext(inputPath)
		outputPath = inputPath[:len(inputPath)-len(ext)] + ".anonymised" + ext
	}

	rewriteAddresses := !cmd.Bool("keep-addresses")

	if err := dump.Anonymise(inputPath, outputPath, rewriteAddresses); err != nil {
		return fmt.Errorf("anonymise failed: %w", err)
	}

	slog.InfoContext(
		context.Background(),
		"Anonymised capture written",
		"path", outputPath,
		"addresses_rewritten", rewriteAddresses,
	)

	return nil
}

// checkDatabaseConfigurations checks if any configured target databases match the storage DSN.
// Logs a warning for each match found. This handles databases that were configured
// before the storage DSN validation was added.
func checkDatabaseConfigurations(ctx context.Context, dataStore *store.Store, logger *slog.Logger) {
	databases, err := dataStore.ListServers(ctx)
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

// escalatorAdapter bridges the proxy's escalator interface to the notify
// package's concrete type without either side importing the other.
type escalatorAdapter struct {
	inner *notify.ApprovalEscalator
}

func (a escalatorAdapter) Schedule(ctx context.Context, hold shared.ApprovalHoldInfo) {
	a.inner.Schedule(ctx, notify.ApprovalHold{
		QueryUID:      hold.QueryUID,
		ConnectionUID: hold.ConnectionUID,
		Username:      hold.Username,
		DatabaseName:  hold.DatabaseName,
		SQL:           hold.SQL,
		Pattern:       hold.Pattern,
		StartedAt:     hold.StartedAt,
	})
}

func (a escalatorAdapter) Resolved(ctx context.Context, queryUID uuid.UUID, status, byName, reason string) {
	a.inner.Resolved(ctx, queryUID, status, byName, reason)
}

// approvalDrain releases every parked statement on shutdown. Draining must
// explicitly abandon held queries rather than hang the shutdown — or, worse,
// silently let them through.
type approvalDrain struct {
	registry *approval.Registry
	logger   *slog.Logger
}

func (d approvalDrain) Shutdown(ctx context.Context) error {
	released := d.registry.ResolveAll(
		store.ApprovalAbandoned, "dbbat is shutting down; the statement was not executed", nil, "",
	)

	if len(released) > 0 {
		d.logger.WarnContext(ctx, "abandoned held queries on shutdown", slog.Int("count", len(released)))
	}

	return nil
}

// buildEventPlumbing constructs the API server together with the one broker,
// one approval registry and one Slack escalator the whole process shares. They
// have to be the same instances across the API and every proxy: a decision
// taken over REST must wake the session parked in the proxy, and both sides
// publish into the same stream.
func buildEventPlumbing(
	ctx context.Context, cfg *config.Config, dataStore *store.Store, logger *slog.Logger,
) (*api.Server, *approval.Registry, shared.ApprovalDeps) {
	broker := events.New()
	approvals := approval.NewRegistry()

	apiServer := api.NewServer(dataStore, cfg.EncryptionKey, logger, cfg)

	// Reuse the API server's Slack client rather than opening a second one.
	escalator := notify.NewApprovalEscalator(
		apiServer.Notifier(),
		cfg.Approval.SlackDelayDuration(),
		cfg.Approval.SlackSQL,
		logger,
	)

	apiServer.SetEventPlumbing(broker, approvals, escalator)

	if cfg.Approval.Enabled {
		logger.InfoContext(ctx, "approval holds enabled",
			slog.Duration("slack_delay", cfg.Approval.SlackDelayDuration()),
			slog.Bool("slack_sql", cfg.Approval.SlackSQL))
	}

	return apiServer, approvals, shared.ApprovalDeps{
		Enabled:   cfg.Approval.Enabled,
		Store:     dataStore,
		Registry:  approvals,
		Broker:    broker,
		Escalator: escalatorAdapter{escalator},
		Logger:    logger,
	}
}

// rowWriterCloser adapts the shared result-row writer to the shutdown list.
type rowWriterCloser struct {
	writer *shared.RowWriter
}

// Shutdown drains whatever rows are still queued and stops the writer.
func (c rowWriterCloser) Shutdown(ctx context.Context) error {
	c.writer.Close(ctx)

	return nil
}

// collectServers drops the nil entries (proxies whose listener is disabled)
// from a shutdown list. A typed-nil pointer in a shutdownable slice would
// panic on Shutdown, so the filter checks each concrete pointer.
func collectServers(
	drain approvalDrain,
	apiServer *api.Server,
	pgServer *postgresql.Server,
	oracleServer *oracle.Server,
	mysqlServer *mysql.Server,
	mongoServer *mongodb.Server,
	rowWriter *shared.RowWriter,
	sweeper *queryRetentionSweeper,
	heartbeat *instanceHeartbeat,
) []shutdownable {
	servers := []shutdownable{drain, apiServer, pgServer}

	if oracleServer != nil {
		servers = append(servers, oracleServer)
	}

	if mysqlServer != nil {
		servers = append(servers, mysqlServer)
	}

	if mongoServer != nil {
		servers = append(servers, mongoServer)
	}

	// After every proxy: the batched capture has to drain once no session can
	// queue another row, or the tail of the last queries is lost.
	if rowWriter != nil {
		servers = append(servers, rowWriterCloser{rowWriter})
	}

	if sweeper != nil {
		servers = append(servers, sweeper)
	}

	// Last on purpose: deregistering tells the other replicas our connections
	// are fair game, which must not happen while the proxies are still draining
	// live sessions.
	if heartbeat != nil {
		servers = append(servers, heartbeat)
	}

	return servers
}

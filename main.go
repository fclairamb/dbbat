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
	"github.com/fclairamb/dbbat/internal/proxy/mssql"
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
			auditCommand(flags),
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
		// Seals the audit and query chains. The store keeps only an HKDF
		// subkey of this; see docs/audit-chain.md.
		EncryptionKey: cfg.EncryptionKey,
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

	// One capture uploader for the whole process (nil when
	// DBB_DUMP_UPLOAD_URL is unset, which is the default and means captures
	// stay on local disk).
	dumpUploader, err := startDumpUploader(ctx, cfg, dataStore, logger)
	if err != nil {
		return err
	}

	apiServer, approvals, approvalDeps := buildEventPlumbing(ctx, cfg, dataStore, logger)
	apiServer.SetDumpStorage(dumpUploader)

	go func() {
		if err := apiServer.Start(cfg.ListenAPI); err != nil {
			logger.ErrorContext(context.Background(), "API server error", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	logger.InfoContext(ctx, "API server started", slog.String("addr", cfg.ListenAPI))

	// One result-row writer for the whole process (see shared.RowWriter).
	rowWriter := shared.NewRowWriter(dataStore, logger)

	// Create auth cache for proxy server (shared cache config with API)
	proxyAuthCache := cache.NewAuthCache(cache.AuthCacheConfig{
		Enabled:    cfg.AuthCache.Enabled,
		TTLSeconds: cfg.AuthCache.TTLSeconds,
		MaxSize:    cfg.AuthCache.MaxSize,
	})

	proxies := startProxies(ctx, cfg, dataStore, proxyAuthCache, approvalDeps, rowWriter, dumpUploader, logger)

	// One retention sweep for the whole process (nil when disabled, the default).
	sweeper := startQueryRetentionSweep(ctx, cfg, dataStore, logger)

	// Draining releases parked queries first, then stops the servers.
	servers := collectServers(approvalDrain{approvals, logger}, apiServer, proxies.postgres,
		proxies.oracle, proxies.mysql, proxies.mongo, proxies.mssql,
		rowWriter, sweeper, heartbeat, dumpUploader)

	return awaitShutdown(ctx, logger, servers...)
}

// proxySet holds the protocol listeners this process started. Every field
// except postgres is nil when its listen address is empty.
type proxySet struct {
	postgres *postgresql.Server
	oracle   *oracle.Server
	mysql    *mysql.Server
	mongo    *mongodb.Server
	mssql    *mssql.Server
}

// startProxies brings up every configured protocol listener and hands each one
// the process-wide capture uploader.
func startProxies(
	ctx context.Context,
	cfg *config.Config,
	dataStore *store.Store,
	authCache *cache.AuthCache,
	approvalDeps shared.ApprovalDeps,
	rowWriter *shared.RowWriter,
	dumpUploader *dump.Uploader,
	logger *slog.Logger,
) proxySet {
	set := proxySet{
		postgres: startPostgresProxy(ctx, cfg, dataStore, authCache, approvalDeps, rowWriter, logger),
		oracle:   startOracleProxy(ctx, cfg, dataStore, authCache, approvalDeps, rowWriter, logger),
		mysql:    startMySQLProxy(ctx, cfg, dataStore, authCache, approvalDeps, rowWriter, logger),
		mongo:    startMongoProxy(ctx, cfg, dataStore, authCache, approvalDeps, rowWriter, logger),
		mssql:    startMSSQLProxy(ctx, cfg, dataStore, authCache, approvalDeps, rowWriter, logger),
	}

	set.postgres.SetDumpUploader(dumpUploader)

	if set.oracle != nil {
		set.oracle.SetDumpUploader(dumpUploader)
	}

	if set.mysql != nil {
		set.mysql.SetDumpUploader(dumpUploader)
	}

	if set.mongo != nil {
		set.mongo.SetDumpUploader(dumpUploader)
	}

	if set.mssql != nil {
		set.mssql.SetDumpUploader(dumpUploader)
	}

	return set
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
// This is the startup pass only. The reclaim half also runs periodically on the
// heartbeat loop (instanceHeartbeat.reclaim), because an instance that dies
// while this deployment stays up would otherwise not be noticed by anyone: its
// registry row is fresh when its replacement starts, and stale only long after.
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
		// Worth info level: a large count means a previous run did not shut
		// down cleanly. Zero does not mean the opposite — a run that crashed
		// moments ago still looks alive in the registry, and its rows are
		// picked up by a later reclaim rather than here.
		logger.InfoContext(ctx, "Closed connections left open by a previous run",
			slog.Int64("connections", closed.Own),
			slog.String("instance_id", dataStore.InstanceID()),
			slog.String("run_id", dataStore.RunID()))
	}

	logReclaimedConnections(ctx, logger, closed.Reclaimed)

	// Housekeeping, after the reclaim so it still saw the rows it judged.
	pruneStaleInstances(ctx, dataStore, logger)
}

// logReclaimedConnections reports connections closed on behalf of an instance
// that is provably gone. Shared by the startup reconcile and the periodic
// reclaim on the heartbeat loop so both read identically in the logs: whenever
// it happens, a non-zero count means a process died without shutting down.
func logReclaimedConnections(ctx context.Context, logger *slog.Logger, reclaimed int64) {
	if reclaimed <= 0 {
		return
	}

	logger.InfoContext(ctx, "Reclaimed connections left open by instances that are gone",
		slog.Int64("connections", reclaimed),
		slog.Duration("stale_after", store.InstanceStaleAfter))
}

// pruneStaleInstances drops registry rows whose owner is past the grace period.
// Pure housekeeping — a stale row and a missing row mean the same thing to the
// reclaim — so a failure is a warning and the count is only worth debug level.
// Always call it after a reclaim, never before, so the reclaim still sees the
// rows it is judging.
func pruneStaleInstances(ctx context.Context, dataStore *store.Store, logger *slog.Logger) {
	pruned, err := dataStore.PruneStaleInstances(ctx)
	if err != nil {
		logger.WarnContext(ctx, "failed to prune stale instances", slog.Any("error", err))

		return
	}

	if pruned > 0 {
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

// startPostgresProxy builds and starts the PostgreSQL proxy. Unlike the other
// three it has no listener-disabled case: PostgreSQL is dbbat's default
// protocol and DBB_LISTEN_PG always has a value.
func startPostgresProxy(
	ctx context.Context,
	cfg *config.Config,
	dataStore *store.Store,
	authCache *cache.AuthCache,
	approvalDeps shared.ApprovalDeps,
	rowWriter *shared.RowWriter,
	logger *slog.Logger,
) *postgresql.Server {
	srv, err := postgresql.NewServer(dataStore, cfg.EncryptionKey, cfg.QueryStorage, cfg.Dump, authCache, cfg.PG, logger)
	if err != nil {
		logger.ErrorContext(ctx, "PostgreSQL proxy server init failed", slog.Any("error", err))
		os.Exit(1)
	}

	srv.SetApprovalDeps(approvalDeps)
	srv.SetRowWriter(rowWriter)

	go func() {
		if err := srv.Start(cfg.ListenPG); err != nil {
			logger.ErrorContext(context.Background(), "Proxy server error", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	logger.InfoContext(ctx, "Proxy server started",
		slog.String("addr", cfg.ListenPG),
		slog.Bool("tls", !cfg.PG.TLS.Disable))

	return srv
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

// startMSSQLProxy starts the SQL Server (TDS) proxy when a listen address is
// configured.
func startMSSQLProxy(
	ctx context.Context,
	cfg *config.Config,
	dataStore *store.Store,
	authCache *cache.AuthCache,
	approvalDeps shared.ApprovalDeps,
	rowWriter *shared.RowWriter,
	logger *slog.Logger,
) *mssql.Server {
	if cfg.ListenMSSQL == "" {
		return nil
	}

	srv, err := mssql.NewServer(dataStore, cfg.EncryptionKey, cfg.QueryStorage, cfg.Dump, authCache, cfg.MSSQL, logger)
	if err != nil {
		logger.ErrorContext(ctx, "SQL Server proxy init failed", slog.Any("error", err))
		os.Exit(1)
	}

	srv.SetApprovalDeps(approvalDeps)
	srv.SetRowWriter(rowWriter)

	go func() {
		if err := srv.Start(cfg.ListenMSSQL); err != nil {
			logger.ErrorContext(context.Background(), "SQL Server proxy error", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	logger.InfoContext(ctx, "SQL Server proxy started",
		slog.String("addr", cfg.ListenMSSQL),
		slog.Bool("tls", !cfg.MSSQL.TLS.Disable))

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

	// 5. Create the write and read-only grants (each an instance of a seeded
	// grant definition — a grant has no shape of its own).
	if err := seedTestGrants(ctx, dataStore, adminUser.UID, connectorUser.UID, viewerUser.UID, targetDB.UID); err != nil {
		return err
	}
	logger.InfoContext(ctx, "Created write and read-only grants on proxy_target")

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

// demoEpoch is the single instant every seeded demo row is dated from: the
// start of the current UTC day. Every seeded timestamp is expressed as an
// offset *before* it, so nothing a demo instance renders is ever in the future,
// whatever time of day the process was started at.
//
// Why a truncated-but-rolling epoch rather than a hardcoded constant
// (2026-01-15T09:00:00Z, as the spec sketched)? demo.dbbat.com is public and
// long-lived, and demo mode re-seeds from scratch on every start. A hardcoded
// epoch is byte-stable forever but ages with no forcing function to bump it —
// within a year the demo would greet visitors with a proxy "set up 18 months
// ago" whose newest query ran "14 months ago". Truncating to the UTC day keeps
// every date identical for the whole of a day (so a same-day showcase
// regeneration diffs cleanly, and a *running* demo instance never shifts a
// date under a visitor), while the story it tells stays plausible forever.
func demoEpoch() time.Time {
	return time.Now().UTC().Truncate(24 * time.Hour)
}

// demoGrantExpiry is when the seeded grants run out. It is an absolute instant
// like everything else here — not `time.Now().AddDate(...)` — but derived from
// the epoch, which keeps it both stable for the day and, at ten years out,
// comfortably clear of the real clock. A grant seeded with an expiry in the
// past is not a cosmetic problem: every demo connection would be refused.
func demoGrantExpiry(epoch time.Time) time.Time {
	return epoch.AddDate(10, 0, 0)
}

// backdate rewrites timestamp columns on an already-inserted row.
//
// The store's Create* helpers stamp created_at/updated_at with time.Now()
// themselves and take no creation time, so seeding at absolute dates means
// fixing them up right after the insert. Verified to round-trip: these are
// plain columns, not database defaults that would be re-applied.
func backdate(
	ctx context.Context,
	dataStore *store.Store,
	model any,
	uid uuid.UUID,
	at time.Time,
	columns ...string,
) error {
	query := dataStore.DB().NewUpdate().Model(model).Where("uid = ?", uid)
	for _, column := range columns {
		query = query.Set(column+" = ?", at)
	}

	if _, err := query.Exec(ctx); err != nil {
		return fmt.Errorf("failed to backdate %v: %w", columns, err)
	}

	return nil
}

// seedDemoUser creates one demo user and dates the row.
//
// The password is the username — that is the documented demo convention — and
// it is written a second time through UpdateUser so the row counts as
// "password already changed" and the account can log in without the
// first-login flow.
func seedDemoUser(
	ctx context.Context,
	dataStore *store.Store,
	username string,
	roles []string,
	createdAt time.Time,
) (*store.User, error) {
	passwordHash, err := crypto.HashPassword(username)
	if err != nil {
		return nil, fmt.Errorf("failed to hash %s password: %w", username, err)
	}

	user, err := dataStore.CreateUser(ctx, username, passwordHash, roles)
	if err != nil {
		return nil, fmt.Errorf("failed to create %s user: %w", username, err)
	}

	if err := dataStore.UpdateUser(ctx, user.UID, store.UserUpdate{PasswordHash: &passwordHash}); err != nil {
		return nil, fmt.Errorf("failed to mark %s password as changed: %w", username, err)
	}

	if err := backdate(ctx, dataStore, (*store.User)(nil), user.UID, createdAt,
		"created_at", "updated_at", "password_changed_at"); err != nil {
		return nil, err
	}

	return user, nil
}

func provisionDemoData(ctx context.Context, dataStore *store.Store, cfg *config.Config, logger *slog.Logger) error {
	logger.InfoContext(ctx, "Demo mode: provisioning demo data...")

	// Everything below is dated from this one instant — see demoEpoch().
	epoch := demoEpoch()

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
	// The admin row was inserted by EnsureDefaultAdmin at process start; date it
	// like the rest of the story — the account that set the proxy up.
	if err := backdate(ctx, dataStore, (*store.User)(nil), adminUser.UID,
		epoch.AddDate(0, 0, -30), "created_at", "updated_at", "password_changed_at"); err != nil {
		return err
	}
	logger.InfoContext(ctx, "Marked admin password as changed (username: admin, password: admin)")

	// 2. Create viewer user (viewer role only)
	viewerUser, err := seedDemoUser(ctx, dataStore,
		"viewer", []string{store.RoleViewer}, epoch.AddDate(0, 0, -28))
	if err != nil {
		return err
	}
	logger.InfoContext(ctx, "Created viewer user (username: viewer, password: viewer)")

	// 3. Create connector user (connector role only)
	connectorUser, err := seedDemoUser(ctx, dataStore,
		"connector", []string{store.RoleConnector}, epoch.AddDate(0, 0, -28).Add(3*time.Hour))
	if err != nil {
		return err
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
	if err := backdate(ctx, dataStore, (*store.Server)(nil), demoDB.UID,
		epoch.AddDate(0, 0, -27), "created_at", "updated_at"); err != nil {
		return err
	}
	logger.InfoContext(ctx, "Created demo_db database configuration")

	// 5. Define the two shapes the demo hands out, then grant the connector
	// full write access and the viewer read-only, both dated from the epoch.
	demoWriteDef, demoReadOnlyDef, err := seedDemoGrantDefinitions(ctx, dataStore, adminUser.UID)
	if err != nil {
		return err
	}

	if err := seedDemoGrant(ctx, dataStore, demoGrantSeed{
		userID:     connectorUser.UID,
		databaseID: demoDB.UID,
		grantedBy:  adminUser.UID,
		definition: demoWriteDef,
		grantedAt:  epoch.AddDate(0, 0, -26),
		expiresAt:  demoGrantExpiry(epoch),
	}); err != nil {
		return err
	}
	logger.InfoContext(ctx, "Created write grant for connector user on demo_db")

	// 6. Create read-only grant for viewer user
	if err := seedDemoGrant(ctx, dataStore, demoGrantSeed{
		userID:     viewerUser.UID,
		databaseID: demoDB.UID,
		grantedBy:  adminUser.UID,
		definition: demoReadOnlyDef,
		grantedAt:  epoch.AddDate(0, 0, -25),
		expiresAt:  demoGrantExpiry(epoch),
	}); err != nil {
		return err
	}
	logger.InfoContext(ctx, "Created read-only grant for viewer user on demo_db")

	// 7. Seed a spread of query history so the observability pages open on a
	// plausible timeline instead of an empty table.
	if err := seedDemoHistory(ctx, dataStore, epoch, demoDB.UID, map[string]uuid.UUID{
		"admin":     adminUser.UID,
		"viewer":    viewerUser.UID,
		"connector": connectorUser.UID,
	}); err != nil {
		return err
	}
	logger.InfoContext(ctx, "Seeded demo query history on demo_db")

	logger.InfoContext(ctx, "Demo data provisioning complete")
	return nil
}

// seedDemoGrantDefinitions creates the full-write and read-only shapes the
// demo data hands out.
func seedDemoGrantDefinitions(
	ctx context.Context,
	dataStore *store.Store,
	adminUID uuid.UUID,
) (*store.GrantDefinition, *store.GrantDefinition, error) {
	const thirtyDays = int64(30 * 24 * 3600)

	writeDef, err := dataStore.CreateGrantDefinition(ctx, &store.GrantDefinition{
		Name:            "Full write (demo)",
		Slug:            "demo-full-write",
		Description:     "Unrestricted access, seeded in demo mode.",
		DurationSeconds: thirtyDays,
		Controls:        []string{},
		CreatedBy:       adminUID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create demo write grant definition: %w", err)
	}

	readOnlyDef, err := dataStore.CreateGrantDefinition(ctx, &store.GrantDefinition{
		Name:            "Read only (demo)",
		Slug:            "demo-read-only",
		Description:     "Read-only access, seeded in demo mode.",
		DurationSeconds: thirtyDays,
		Controls:        []string{store.ControlReadOnly},
		CreatedBy:       adminUID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create demo read-only grant definition: %w", err)
	}

	return writeDef, readOnlyDef, nil
}

// demoGrantSeed describes one seeded demo grant.
type demoGrantSeed struct {
	userID     uuid.UUID
	databaseID uuid.UUID
	grantedBy  uuid.UUID
	definition *store.GrantDefinition
	grantedAt  time.Time
	expiresAt  time.Time
}

// seedGrantFromDefinition materializes a definition into a grant with an
// explicit window. BuildGrantFromDefinition derives the window from the
// definition's duration; seeded data wants absolute dates instead, so the
// expiry is overridden afterwards. The shape still comes from the definition
// — there is no other way to give a grant one.
func seedGrantFromDefinition(
	ctx context.Context,
	dataStore *store.Store,
	def *store.GrantDefinition,
	userID, databaseID, grantedBy uuid.UUID,
	startsAt, expiresAt time.Time,
) (*store.Grant, error) {
	grant := store.BuildGrantFromDefinition(def, userID, databaseID, grantedBy, startsAt)
	grant.ExpiresAt = expiresAt

	created, err := dataStore.CreateGrant(ctx, grant)
	if err != nil {
		return nil, fmt.Errorf("failed to create grant from definition %q: %w", def.Slug, err)
	}

	return created, nil
}

// seedDemoGrant creates a grant whose whole timeline — granted, starts,
// expires — is absolute. CreateGrant stamps created_at with time.Now(), so it
// is rewritten right after the insert.
func seedDemoGrant(ctx context.Context, dataStore *store.Store, seed demoGrantSeed) error {
	grant, err := seedGrantFromDefinition(ctx, dataStore, seed.definition,
		seed.userID, seed.databaseID, seed.grantedBy, seed.grantedAt, seed.expiresAt)
	if err != nil {
		return fmt.Errorf("failed to create demo grant: %w", err)
	}

	return backdate(ctx, dataStore, (*store.AccessGrant)(nil), grant.UID, seed.grantedAt, "created_at")
}

// demoSession is one seeded connection: who opened it, when (as an offset
// *before* the epoch), and what they ran on it.
type demoSession struct {
	user      string
	sourceIP  string
	openedAgo time.Duration
	// bytes is what the session is recorded as having transferred. Made up, but
	// in proportion to the rows its statements returned.
	bytes   int64
	queries []demoQuery
}

// demoQuery is one statement inside a demoSession, offset from the moment the
// session was opened.
type demoQuery struct {
	afterOpen  time.Duration
	sql        string
	durationMs float64
	rows       int64
}

// demoSessions is the history a demo instance starts life with: four sessions
// spread over the five days before the epoch, so the queries list shows a
// timeline ("3 days ago", "yesterday", "this morning") rather than a handful of
// rows from the same second.
//
// The SQL deliberately avoids the showcase's marker statement
// (`FROM customers ORDER BY mrr_eur`, see front/showcase/screenshots.spec.ts)
// so a seeded row can never be mistaken for the one produced by real traffic.
var demoSessions = []demoSession{
	{
		user:      "connector",
		sourceIP:  "10.42.7.19",
		openedAgo: 5 * 24 * time.Hour,
		bytes:     184_320,
		queries: []demoQuery{
			{afterOpen: 2 * time.Second, sql: "SELECT COUNT(*) FROM invoices WHERE issued_on >= DATE '2026-01-01'", durationMs: 4.118, rows: 1},
			{afterOpen: 47 * time.Second, sql: "UPDATE invoices SET status = 'settled' WHERE payment_reference = 'PR-88213'", durationMs: 12.905, rows: 1},
			{afterOpen: 3 * time.Minute, sql: "SELECT invoice_id, amount_eur, status FROM invoices WHERE status = 'overdue' ORDER BY amount_eur DESC LIMIT 50", durationMs: 8.442, rows: 50},
		},
	},
	{
		user:      "viewer",
		sourceIP:  "10.42.7.83",
		openedAgo: 3 * 24 * time.Hour,
		bytes:     51_200,
		queries: []demoQuery{
			{afterOpen: time.Second, sql: "SELECT region, SUM(amount_eur) AS total FROM invoices GROUP BY region ORDER BY total DESC", durationMs: 21.674, rows: 6},
			{afterOpen: 90 * time.Second, sql: "SELECT plan, COUNT(*) FROM subscriptions GROUP BY plan", durationMs: 3.201, rows: 4},
		},
	},
	{
		user:      "connector",
		sourceIP:  "10.42.7.19",
		openedAgo: 27 * time.Hour,
		bytes:     9_216,
		queries: []demoQuery{
			{afterOpen: 2 * time.Second, sql: "INSERT INTO shipments (order_ref, carrier, dispatched_at) VALUES ('OR-40218', 'meridian-freight', now())", durationMs: 6.773, rows: 1},
			{afterOpen: 11 * time.Second, sql: "SELECT order_ref, carrier, dispatched_at FROM shipments ORDER BY dispatched_at DESC LIMIT 20", durationMs: 2.958, rows: 20},
		},
	},
	{
		user:      "admin",
		sourceIP:  "10.42.9.4",
		openedAgo: 4 * time.Hour,
		bytes:     2_048,
		queries: []demoQuery{
			{afterOpen: 3 * time.Second, sql: "SELECT relname, n_live_tup FROM pg_stat_user_tables ORDER BY n_live_tup DESC LIMIT 10", durationMs: 1.884, rows: 10},
		},
	},
}

// errUnknownDemoUser guards demoSessions against naming a user the seeding
// never created.
var errUnknownDemoUser = errors.New("demo history references an unknown user")

// seedDemoHistory records demoSessions as closed connections carrying their
// statements, all dated from the epoch.
//
// It writes the connection's timestamps and counters itself rather than going
// through CreateConnection/CloseConnection: those stamp time.Now(), which is
// the whole thing this seeding is trying to avoid.
func seedDemoHistory(
	ctx context.Context,
	dataStore *store.Store,
	epoch time.Time,
	databaseID uuid.UUID,
	users map[string]uuid.UUID,
) error {
	for _, session := range demoSessions {
		userID, ok := users[session.user]
		if !ok {
			return fmt.Errorf("%w: %q", errUnknownDemoUser, session.user)
		}

		conn, err := dataStore.CreateConnection(ctx, userID, databaseID, session.sourceIP)
		if err != nil {
			return fmt.Errorf("failed to create demo connection: %w", err)
		}

		openedAt := epoch.Add(-session.openedAgo)
		lastActivityAt := openedAt

		for _, query := range session.queries {
			executedAt := openedAt.Add(query.afterOpen)
			if executedAt.After(lastActivityAt) {
				lastActivityAt = executedAt
			}

			durationMs := query.durationMs
			rowsAffected := query.rows
			if _, err := dataStore.CreateQuery(ctx, &store.Query{
				ConnectionID: conn.UID,
				SQLText:      query.sql,
				ExecutedAt:   executedAt,
				DurationMs:   &durationMs,
				RowsAffected: &rowsAffected,
			}); err != nil {
				return fmt.Errorf("failed to create demo query: %w", err)
			}
		}

		// Closed a minute after the last statement: an idle session left open
		// forever would show up as "active" on a demo nobody is connected to.
		disconnectedAt := lastActivityAt.Add(time.Minute)
		if _, err := dataStore.DB().NewUpdate().
			Model((*store.Connection)(nil)).
			Where("uid = ?", conn.UID).
			Set("connected_at = ?", openedAt).
			Set("last_activity_at = ?", lastActivityAt).
			Set("disconnected_at = ?", disconnectedAt).
			Set("queries = ?", len(session.queries)).
			Set("bytes_transferred = ?", session.bytes).
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to date demo connection: %w", err)
		}
	}

	return nil
}

// seedTestGrants creates the two unlimited seed shapes — full write and
// read-only — and hands each to its user. Definitions come first because a
// grant is an instance of one; there is no way to seed a grant otherwise.
func seedTestGrants(
	ctx context.Context,
	dataStore *store.Store,
	adminUID, connectorUID, viewerUID, databaseUID uuid.UUID,
) error {
	const tenYears = int64(10 * 365 * 24 * 3600)

	writeDef, err := dataStore.CreateGrantDefinition(ctx, &store.GrantDefinition{
		Name:            "Full write (seed)",
		Slug:            "seed-full-write",
		Description:     "Unrestricted access, seeded in test mode.",
		DurationSeconds: tenYears,
		Controls:        []string{},
		CreatedBy:       adminUID,
	})
	if err != nil {
		return fmt.Errorf("failed to create write grant definition: %w", err)
	}

	if _, err := seedGrantFromDefinition(ctx, dataStore, writeDef,
		connectorUID, databaseUID, adminUID, time.Now(), time.Now().AddDate(10, 0, 0)); err != nil {
		return fmt.Errorf("failed to create write grant for connector user: %w", err)
	}

	readOnlyDef, err := dataStore.CreateGrantDefinition(ctx, &store.GrantDefinition{
		Name:            "Read only (seed)",
		Slug:            "seed-read-only",
		Description:     "Read-only access, seeded in test mode.",
		DurationSeconds: tenYears,
		Controls:        []string{store.ControlReadOnly},
		CreatedBy:       adminUID,
	})
	if err != nil {
		return fmt.Errorf("failed to create read-only grant definition: %w", err)
	}

	if _, err := seedGrantFromDefinition(ctx, dataStore, readOnlyDef,
		viewerUID, databaseUID, adminUID, time.Now(), time.Now().AddDate(10, 0, 0)); err != nil {
		return fmt.Errorf("failed to create read-only grant for viewer user: %w", err)
	}

	return nil
}

// seedQuotaGrant creates a read-only grant bounded by a query count and a
// data-transfer quota, so the test-mode grants list has a grant with applied
// limits to render (alongside the unlimited seed grants).
func seedQuotaGrant(ctx context.Context, dataStore *store.Store, userID, databaseID uuid.UUID) error {
	maxQueries := int64(100)
	maxBytes := int64(1024 * 1024 * 1024) // 1 GB

	def, err := dataStore.CreateGrantDefinition(ctx, &store.GrantDefinition{
		Name:                "Read only with quotas (seed)",
		Slug:                "seed-read-only-quota",
		Description:         "Read-only access bounded by query and transfer quotas, seeded in test mode.",
		DurationSeconds:     int64(10 * 365 * 24 * time.Hour / time.Second),
		Controls:            []string{store.ControlReadOnly},
		MaxQueryCounts:      &maxQueries,
		MaxBytesTransferred: &maxBytes,
		CreatedBy:           userID,
	})
	if err != nil {
		return fmt.Errorf("failed to create quota grant definition: %w", err)
	}

	if _, err := seedGrantFromDefinition(ctx, dataStore, def,
		userID, databaseID, userID, time.Now(), time.Now().AddDate(10, 0, 0)); err != nil {
		return fmt.Errorf("failed to create quota grant: %w", err)
	}

	return nil
}

// seedSampleQuery records one historical query on a closed connection so the
// test-mode queries list has a clickable row and the query-detail breadcrumb
// has real SQL text (long enough to exercise the preview truncation) to render.
//
// The connection is stamped with whatever active grant seedQuotaGrant already
// created for this (user, database) pair — mirroring what CreateConnection's
// real call sites do at auth time — so the connection detail page's Grant
// section has real grant context to render in test mode, not just the "no
// grant on record" fallback.
func seedSampleQuery(ctx context.Context, dataStore *store.Store, userID, databaseID uuid.UUID) error {
	var opts []store.ConnectionOption
	if grant, err := dataStore.GetActiveGrant(ctx, userID, databaseID); err == nil {
		opts = append(opts, store.WithGrantUID(grant.UID))
	}

	conn, err := dataStore.CreateConnection(ctx, userID, databaseID, "127.0.0.1", opts...)
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

// auditCommand builds the `dbbat audit` command tree.
func auditCommand(flags *cliFlags) *cli.Command {
	return &cli.Command{
		Name:  "audit",
		Usage: "Tamper-evidence commands for the audit trail",
		Commands: []*cli.Command{
			{
				Name: "verify",
				Usage: "Walk the HMAC chain and report the first break " +
					"(exits non-zero when the chain does not verify)",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:  "queries",
						Usage: "verify the per-connection query chains instead of the audit log",
					},
					&cli.BoolFlag{
						Name:  "rows",
						Usage: "verify the per-query captured result row chains instead of the audit log",
					},
					&cli.StringFlag{
						Name:  "connection",
						Usage: "with --queries or --rows, verify only this connection uid",
					},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return runAuditVerify(ctx, flags, cmd)
				},
			},
		},
	}
}

var (
	errAuditChainBroken     = errors.New("audit chain verification failed")
	errAuditConnectionScope = errors.New("--connection only applies together with --queries or --rows")
	errAuditScopeConflict   = errors.New("--queries and --rows are different chains: pass one or the other")
)

func runAuditVerify(ctx context.Context, flags *cliFlags, cmd *cli.Command) error {
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

	if cmd.Bool("queries") && cmd.Bool("rows") {
		return errAuditScopeConflict
	}

	connectionArg := cmd.String("connection")
	if connectionArg != "" && !cmd.Bool("queries") && !cmd.Bool("rows") {
		return errAuditConnectionScope
	}

	var connectionUID *uuid.UUID

	if connectionArg != "" {
		parsed, err := uuid.Parse(connectionArg)
		if err != nil {
			return fmt.Errorf("invalid --connection uid %q: %w", connectionArg, err)
		}

		connectionUID = &parsed
	}

	dataStore, err := store.New(ctx, cfg.DSN, store.Options{EncryptionKey: cfg.EncryptionKey})
	if err != nil {
		return fmt.Errorf("failed to initialize store: %w", err)
	}

	defer dataStore.Close()

	if cmd.Bool("queries") {
		return verifyQueryChains(ctx, dataStore, logger, connectionUID)
	}

	if cmd.Bool("rows") {
		return verifyRowChains(ctx, dataStore, logger, connectionUID)
	}

	return verifyAuditChain(ctx, dataStore, logger)
}

func verifyAuditChain(ctx context.Context, dataStore *store.Store, logger *slog.Logger) error {
	result, err := dataStore.VerifyAuditChain(ctx)
	if err != nil {
		return fmt.Errorf("audit chain verification failed: %w", err)
	}

	if !result.OK() {
		logger.ErrorContext(ctx, "AUDIT CHAIN BROKEN",
			slog.String("break", result.Break.String()),
			slog.Int64("verified_before_break", result.Verified))

		return errAuditChainBroken
	}

	logger.InfoContext(ctx, "Audit chain verified",
		slog.Int64("entries", result.Verified),
		slog.Int64("head_seq", result.HeadSeq),
		slog.String("head_mac", result.HeadMACHex()),
		// Rows written before the chain anchor. Nothing sealed them, so
		// nothing can vouch for them; the count is reported rather than
		// quietly folded into "verified".
		slog.Int64("unverifiable_pre_anchor_entries", result.Unchained))

	return nil
}

func verifyQueryChains(
	ctx context.Context, dataStore *store.Store, logger *slog.Logger, connectionUID *uuid.UUID,
) error {
	result, err := dataStore.VerifyQueryChains(ctx, connectionUID)
	if err != nil {
		return fmt.Errorf("query chain verification failed: %w", err)
	}

	if !result.OK() {
		logger.ErrorContext(ctx, "QUERY CHAIN BROKEN",
			slog.String("break", result.Break.String()),
			slog.Int64("verified_before_break", result.Verified))

		return errAuditChainBroken
	}

	logger.InfoContext(ctx, "Query chains verified",
		slog.Int64("connections", result.Connections),
		slog.Int64("statements", result.Verified),
		// A chain missing its oldest statements is what
		// DBB_QUERY_STORAGE_RETENTION leaves behind on a long-lived session,
		// so it is counted rather than treated as tampering.
		slog.Int64("chains_with_retention_truncated_prefix", result.Truncated),
		// Sessions closed before 0.24, whose head stamp is a verbatim copy of
		// the last statement's MAC rather than a keyed seal. Their statements
		// verified; nothing keyed vouches for their *tail*, because that stamp
		// is writable by anyone who can write to the store. The number drains
		// as those sessions age out of retention.
		slog.Int64("sessions_with_legacy_forgeable_head_stamp", result.LegacyStamps))

	return nil
}

func verifyRowChains(
	ctx context.Context, dataStore *store.Store, logger *slog.Logger, connectionUID *uuid.UUID,
) error {
	result, err := dataStore.VerifyRowChains(ctx, connectionUID)
	if err != nil {
		return fmt.Errorf("captured row chain verification failed: %w", err)
	}

	if !result.OK() {
		logger.ErrorContext(ctx, "CAPTURED ROW CHAIN BROKEN",
			slog.String("break", result.Break.String()),
			slog.Int64("verified_before_break", result.Verified))

		return errAuditChainBroken
	}

	logger.InfoContext(ctx, "Captured row chains verified",
		slog.Int64("captures", result.Captures),
		slog.Int64("rows", result.Verified),
		// Rows captured before the row chain migration. Nothing sealed them,
		// so they are reported rather than folded into "verified".
		slog.Int64("unverifiable_pre_migration_rows", result.Unchained))

	return nil
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
	mssqlServer *mssql.Server,
	rowWriter *shared.RowWriter,
	sweeper *queryRetentionSweeper,
	heartbeat *instanceHeartbeat,
	dumpUploader *dump.Uploader,
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

	if mssqlServer != nil {
		servers = append(servers, mssqlServer)
	}

	// After every proxy: the batched capture has to drain once no session can
	// queue another row, or the tail of the last queries is lost.
	if rowWriter != nil {
		servers = append(servers, rowWriterCloser{rowWriter})
	}

	if sweeper != nil {
		servers = append(servers, sweeper)
	}

	// After the proxies, before the heartbeat: the queue can only be drained
	// once no session can add to it, and a capture still uploading is a good
	// reason not to hurry the rest of the shutdown.
	if dumpUploader != nil {
		servers = append(servers, dumpUploaderCloser{dumpUploader})
	}

	// Last on purpose: deregistering tells the other replicas our connections
	// are fair game, which must not happen while the proxies are still draining
	// live sessions.
	if heartbeat != nil {
		servers = append(servers, heartbeat)
	}

	return servers
}

// startDumpUploader opens the blob store finished captures are uploaded to and
// sweeps whatever the previous run left behind in the spool.
//
// The sweep has to happen here, before any proxy accepts: at that moment every
// capture in the spool is by definition finished — no session of this run has
// started one yet — so uploading all of them is safe. That is also the crash
// recovery path, since a session whose process died never reached its own
// upload.
//
// Returns (nil, nil) when uploading is not configured, which is the default:
// captures then stay on local disk exactly as before.
func startDumpUploader(
	ctx context.Context, cfg *config.Config, dataStore *store.Store, logger *slog.Logger,
) (*dump.Uploader, error) {
	uploader, err := dump.OpenUploader(ctx, dump.UploaderOptions{
		URL:        cfg.Dump.UploadURL,
		SpoolDir:   cfg.Dump.Dir,
		InstanceID: cfg.InstanceID,
		Recorder:   dataStore,
		Logger:     logger,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open capture storage: %w", err)
	}

	if uploader == nil {
		return nil, nil //nolint:nilnil // nil uploader is the documented "disabled" value
	}

	logger.InfoContext(ctx, "Session captures are uploaded to blob storage",
		slog.String("url", cfg.Dump.UploadURL),
		slog.String("spool", cfg.Dump.Dir))

	queued, err := uploader.SweepSpool(ctx)
	if err != nil {
		// Not fatal: an unreadable spool must not stop the proxy from serving.
		logger.WarnContext(ctx, "failed to sweep the capture spool", slog.Any("error", err))
	} else if queued > 0 {
		logger.InfoContext(ctx, "Queued captures left in the spool by a previous run",
			slog.Int("captures", queued))
	}

	return uploader, nil
}

// dumpUploaderCloser adapts the capture uploader to the shutdown sequence.
type dumpUploaderCloser struct {
	uploader *dump.Uploader
}

func (d dumpUploaderCloser) Shutdown(_ context.Context) error {
	return d.uploader.Close()
}

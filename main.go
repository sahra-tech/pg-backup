package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"pg-backup/internal/backup"
	"pg-backup/internal/config"
	"pg-backup/internal/health"
	"pg-backup/internal/logger"
	"pg-backup/internal/storage"

	"github.com/robfig/cron/v3"
)

// shutdownGrace bounds how long we wait for an interrupted backup to clean up.
const shutdownGrace = 30 * time.Second

func main() {
	// run returns the exit code so deferred cleanup still executes; calling
	// os.Exit directly would skip closing the log file.
	os.Exit(run())
}

func run() int {
	var (
		envFile = flag.String("env-file", "", "Optional KEY=value file to load before reading configuration")
		runOnce = flag.Bool("once", false, "Run backup once and exit")
		listDbs = flag.Bool("list", false, "List configured databases and exit")
	)
	flag.Parse()

	// Real environment variables always win over the file.
	if *envFile != "" {
		if err := config.LoadEnvFile(*envFile); err != nil {
			log.Printf("%v", err)
			return 1
		}
	}

	cfg, err := config.Load()
	if err != nil {
		log.Printf("%v", err)
		return 1
	}

	if *listDbs {
		return listDatabases(cfg)
	}

	// A logger is always returned; a file problem degrades to stderr-only
	// rather than stopping the backup.
	appLogger, logFileErr := logger.New(cfg.LogFile)
	if logFileErr != nil {
		appLogger.Warning("Continuing with stderr-only logging: %v", logFileErr)
	}
	defer appLogger.Close()

	storageProvider, err := newStorage(cfg)
	if err != nil {
		appLogger.Error("%v", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	backupService := backup.NewService(cfg, storageProvider, appLogger)

	if *runOnce {
		appLogger.Info("Running one-time backup")
		count, err := backupService.BackupAll(ctx)
		if err != nil {
			appLogger.Error("Backup failed: %v", err)
			return 1
		}
		appLogger.Info("Backup completed successfully for %d databases", count)
		return 0
	}

	return runScheduler(ctx, cfg, backupService, appLogger)
}

func listDatabases(cfg *config.Config) int {
	if len(cfg.Database.Databases) == 0 {
		fmt.Println("No databases configured; all non-template databases will be discovered at backup time.")
		return 0
	}
	fmt.Println("Configured databases:")
	for i, db := range cfg.Database.Databases {
		fmt.Printf("  %d. %s\n", i+1, db)
	}
	return 0
}

func newStorage(cfg *config.Config) (storage.Provider, error) {
	switch cfg.Storage.Type {
	case "local":
		return storage.NewLocal(cfg.Storage.Local.Path), nil
	case "s3":
		s3Provider, err := storage.NewS3(
			cfg.Storage.S3.Bucket,
			cfg.Storage.S3.Region,
			cfg.Storage.S3.Endpoint,
			cfg.Storage.S3.AccessKey,
			cfg.Storage.S3.SecretKey,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize S3 storage: %w", err)
		}
		return s3Provider, nil
	default:
		// config.validate already rejects other values; this is belt and braces.
		return nil, fmt.Errorf("invalid storage type: %s", cfg.Storage.Type)
	}
}

func runScheduler(ctx context.Context, cfg *config.Config, backupService *backup.Service, appLogger *logger.Logger) int {
	appLogger.Info("Starting pg-backup scheduler")

	healthService := health.NewService(appLogger, len(cfg.Database.Databases), cfg.TriggerToken)
	healthService.SetBackupService(backupService)
	healthService.SetBaseContext(ctx)

	c := cron.New()
	entryID, err := c.AddFunc(cfg.Schedule, func() {
		appLogger.Info("Starting scheduled backup")
		if !healthService.Run(ctx) {
			appLogger.Warning("Skipping scheduled backup: a backup is already running")
		}
	})
	if err != nil {
		// config.Load validates the expression, so reaching here is a bug --
		// but exiting non-zero beats falling off the end of main silently.
		appLogger.Error("Failed to schedule backup: %v", err)
		return 1
	}

	// Report the scheduler's real next fire time rather than guessing.
	healthService.SetNextRun(func() time.Time { return c.Entry(entryID).Next })

	// The health server is the process's liveness signal; if it cannot bind,
	// fail rather than run on with a dead port.
	serverErr := make(chan error, 1)
	go func() { serverErr <- healthService.Start(cfg.HealthCheckBind, cfg.HealthCheckPort) }()

	c.Start()
	appLogger.Info("Backup scheduler started with cron: %s", cfg.Schedule)

	if cfg.RunOnStart {
		appLogger.Info("Running initial backup")
		healthService.Run(ctx)
	}

	exitCode := 0
	select {
	case err := <-serverErr:
		if err != nil {
			appLogger.Error("Health check server failed: %v", err)
			exitCode = 1
		}
	case <-ctx.Done():
		appLogger.Info("Shutdown signal received, stopping scheduler")
	}

	// Stop scheduling new runs, then drain.
	<-c.Stop().Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := healthService.Shutdown(shutdownCtx); err != nil {
		appLogger.Warning("Health server shutdown: %v", err)
	}

	// Cancelling ctx already signalled pg_dump to stop; wait for the storage
	// providers to finish discarding their partial writes.
	if !healthService.Wait(shutdownCtx) {
		appLogger.Warning("Timed out waiting for in-flight backup to finish")
		if exitCode == 0 {
			exitCode = 1
		}
	}

	appLogger.Info("pg-backup stopped")
	return exitCode
}

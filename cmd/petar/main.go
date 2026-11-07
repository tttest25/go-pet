package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"petar/internal/api"
	"petar/internal/config"
	"petar/internal/db"
	"petar/internal/spool"
)

type componentResult struct {
	name string
	err  error
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		logger.Error("petar terminated with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.Default()
	signalCtx, stopSignals := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stopSignals()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	database, err := db.New(signalCtx, cfg.DBDSN)
	if err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}
	defer func() {
		database.Close()
		logger.Info("database closed")
	}()

	writer := spool.NewWriter(cfg.SpoolDir)
	worker := spool.NewWorker(
		database,
		writer,
		cfg.PollInterval,
		logger,
	)
	apiServer := api.NewServer(database, logger)
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog: slog.NewLogLogger(
			logger.Handler(),
			slog.LevelError,
		),
	}

	logger.Info(
		"petar initialized",
		"addr", cfg.HTTPAddr,
		"spool_dir", cfg.SpoolDir,
		"poll_interval", cfg.PollInterval,
	)

	workerCtx, stopWorker := context.WithCancel(context.Background())
	results := make(chan componentResult, 2)
	var components sync.WaitGroup
	components.Add(2)

	go func() {
		defer components.Done()

		err := httpServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		results <- componentResult{name: "http", err: err}
	}()

	go func() {
		defer components.Done()
		results <- componentResult{
			name: "worker",
			err:  worker.Run(workerCtx),
		}
	}()

	logger.Info("petar started", "addr", cfg.HTTPAddr)

	var runErr error
	select {
	case <-signalCtx.Done():
		logger.Info("shutdown signal received")
	case result := <-results:
		if result.err != nil {
			runErr = fmt.Errorf(
				"%s stopped: %w",
				result.name,
				result.err,
			)
		} else {
			runErr = fmt.Errorf("%s stopped unexpectedly", result.name)
		}
	}

	logger.Info("shutting down HTTP server")
	shutdownCtx, cancelShutdown := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	shutdownErr := httpServer.Shutdown(shutdownCtx)
	cancelShutdown()

	if shutdownErr != nil {
		runErr = errors.Join(
			runErr,
			fmt.Errorf("shutdown HTTP server: %w", shutdownErr),
		)
		if err := httpServer.Close(); err != nil {
			runErr = errors.Join(
				runErr,
				fmt.Errorf("close HTTP server: %w", err),
			)
		}
	}

	logger.Info("stopping spool worker")
	stopWorker()
	components.Wait()

	logger.Info("shutdown complete")
	return runErr
}

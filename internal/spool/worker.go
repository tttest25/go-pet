package spool

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"petar/internal/db"
)

type jobQueue interface {
	PickNextJob(ctx context.Context) (*db.Job, error)
	WaitForNotification(ctx context.Context) (string, error)
	ReconnectListener(ctx context.Context) error
}

type SpoolWorker struct {
	queue        jobQueue
	writer       *SpoolWriter
	pollInterval time.Duration
	logger       *slog.Logger
}

type notificationResult struct {
	err error
}

func NewWorker(
	database *db.DB,
	writer *SpoolWriter,
	pollInterval time.Duration,
	logger *slog.Logger,
) *SpoolWorker {
	return newWorker(database, writer, pollInterval, logger)
}

func newWorker(
	queue jobQueue,
	writer *SpoolWriter,
	pollInterval time.Duration,
	logger *slog.Logger,
) *SpoolWorker {
	if logger == nil {
		logger = slog.Default()
	}

	return &SpoolWorker{
		queue:        queue,
		writer:       writer,
		pollInterval: pollInterval,
		logger:       logger,
	}
}

func (w *SpoolWorker) Run(ctx context.Context) error {
	if w.pollInterval <= 0 {
		return fmt.Errorf("run spool worker: poll interval must be positive")
	}

	runCtx, cancel := context.WithCancel(ctx)
	var waitGroup sync.WaitGroup
	defer func() {
		cancel()
		waitGroup.Wait()
	}()

	if err := w.dispatchPending(runCtx); err != nil {
		if runCtx.Err() != nil {
			return nil
		}
		return err
	}

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	waitResults := make(chan notificationResult, 1)
	startWait := func() {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, err := w.queue.WaitForNotification(runCtx)
			waitResults <- notificationResult{err: err}
		}()
	}
	startWait()

	for {
		select {
		case <-runCtx.Done():
			return nil
		case <-ticker.C:
			if err := w.dispatchPending(runCtx); err != nil {
				if runCtx.Err() != nil {
					return nil
				}
				return err
			}
		case result := <-waitResults:
			if result.err != nil {
				if runCtx.Err() != nil {
					return nil
				}

				w.logger.Error(
					"spool listener connection lost; reconnecting",
					"error", result.err,
				)
				if err := w.reconnectListener(runCtx); err != nil {
					if runCtx.Err() != nil {
						return nil
					}
					return fmt.Errorf("reconnect spool listener: %w", err)
				}
			}

			if err := w.dispatchPending(runCtx); err != nil {
				if runCtx.Err() != nil {
					return nil
				}
				return err
			}
			if runCtx.Err() != nil {
				return nil
			}
			startWait()
		}
	}
}

func (w *SpoolWorker) dispatchPending(ctx context.Context) error {
	for {
		job, err := w.queue.PickNextJob(ctx)
		if err != nil {
			return fmt.Errorf("pick next spool job: %w", err)
		}
		if job == nil {
			return nil
		}

		if err := w.writer.WriteJob(ctx, job); err != nil {
			return fmt.Errorf("write spool job %s: %w", job.ID, err)
		}
	}
}

func (w *SpoolWorker) reconnectListener(ctx context.Context) error {
	for {
		if err := w.queue.ReconnectListener(ctx); err == nil {
			return nil
		} else {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.logger.Error("reconnect spool listener failed", "error", err)
		}

		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

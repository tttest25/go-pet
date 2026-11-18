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

	queueWake := make(chan struct{})
	startWait := func() {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if runCtx.Err() != nil {
				return
			}
			_, err := w.queue.WaitForNotification(runCtx)
			if runCtx.Err() != nil {
				return
			}
			if err != nil {
				w.logger.Error(
					"spool listener connection lost; reconnecting",
					"channel", "sm_jobs_channel",
					"error", err,
				)
				// Recover here, leaving the dispatcher free to service the ticker.
				if err := w.reconnectListener(runCtx); err != nil {
					return
				}
			}
			// Both a notification and a restored subscription require a drain.
			select {
			case queueWake <- struct{}{}:
			case <-runCtx.Done():
			}
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
		case <-queueWake:
			if runCtx.Err() != nil {
				return nil
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
		if err := ctx.Err(); err != nil {
			return err
		}
		err := w.queue.ReconnectListener(ctx)
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		w.logger.Error("reconnect spool listener failed", "error", err)

		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

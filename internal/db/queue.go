package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	listenJobsQuery      = "LISTEN sm_jobs_channel"
	createInputQuery     = `SELECT * FROM sm_create_input($1, $2)`
	pickNextJobQuery     = `SELECT * FROM sm_pick_next_job()`
	updateJobStatusQuery = `SELECT sm_update_job_status($1, $2, $3, $4)`
)

// ErrInvalidCallbackStatus indicates an invalid callback status.
var ErrInvalidCallbackStatus = errors.New("invalid callback status")

func mapQueueError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "P0002":
			return fmt.Errorf("%w: %w", pgx.ErrNoRows, err)
		case "22023":
			return fmt.Errorf("%w: %w", ErrInvalidCallbackStatus, err)
		}
	}
	return err
}

type CreatedInput struct {
	InputID   uuid.UUID
	ProcessID uuid.UUID
	Status    string
}

func (d *DB) CreateInput(ctx context.Context, source string, payload json.RawMessage) (CreatedInput, error) {
	var created CreatedInput
	err := d.pool.QueryRow(ctx, createInputQuery, source, payload).Scan(
		&created.InputID, &created.ProcessID, &created.Status,
	)
	if err != nil {
		return CreatedInput{}, fmt.Errorf("create input from source %q: %w", source, mapQueueError(err))
	}
	return created, nil
}

func (d *DB) PickNextJob(ctx context.Context) (*Job, error) {
	var job Job
	err := d.pool.QueryRow(ctx, pickNextJobQuery).Scan(
		&job.ID, &job.ProcessID, &job.ToolName, &job.Payload, &job.OldStatus, &job.Status,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pick next pending job: %w", mapQueueError(err))
	}
	return &job, nil
}

func (d *DB) UpdateJobStatus(ctx context.Context, jobID uuid.UUID, status Status, resultPath string, errorMsg string) error {
	_, err := d.pool.Exec(ctx, updateJobStatusQuery, jobID, status, resultPath, errorMsg)
	if err != nil {
		return fmt.Errorf("update job %s status to %q: %w", jobID, status, mapQueueError(err))
	}
	return nil
}

func (d *DB) WaitForNotification(ctx context.Context) (string, error) {
	d.listenerMu.Lock()
	defer d.listenerMu.Unlock()

	if d.listener == nil {
		return "", fmt.Errorf("wait for notification on sm_jobs_channel: listener is closed")
	}

	notification, err := d.listener.Conn().WaitForNotification(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("wait for notification on sm_jobs_channel: %w", ctxErr)
		}
		return "", fmt.Errorf("listener connection lost on sm_jobs_channel: %w", err)
	}

	return notification.Payload, nil
}

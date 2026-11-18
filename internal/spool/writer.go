package spool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"petar/internal/db"
)

type SpoolWriter struct {
	inDir string
}

type jobDocument struct {
	ID            string          `json:"id"`
	ProcessID     string          `json:"process_id"`
	ToolName      string          `json:"tool_name"`
	ActionVersion int32           `json:"action_version"`
	Payload       json.RawMessage `json:"payload"`
	OldStatus     db.Status       `json:"old_status"`
	Status        db.Status       `json:"status"`
}

func NewWriter(spoolDir string) *SpoolWriter {
	return &SpoolWriter{inDir: filepath.Join(spoolDir, "in")}
}

func (w *SpoolWriter) WriteJob(ctx context.Context, job *db.Job) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("write spool job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("write spool job: job is nil")
	}

	document := jobDocument{
		ID:            job.ID.String(),
		ProcessID:     job.ProcessID.String(),
		ToolName:      job.ToolName,
		ActionVersion: job.ActionVersion,
		Payload:       job.Payload,
		OldStatus:     job.OldStatus,
		Status:        job.Status,
	}
	content, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("marshal spool job %s: %w", job.ID, err)
	}

	tmpPath := filepath.Join(w.inDir, job.ID.String()+".tmp")
	jsonPath := filepath.Join(w.inDir, job.ID.String()+".json")
	file, err := os.OpenFile(
		tmpPath,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("open temporary spool file for job %s: %w", job.ID, err)
	}

	closed := false
	keepTemp := true
	defer func() {
		if !closed {
			_ = file.Close()
		}
		if keepTemp {
			_ = os.Remove(tmpPath)
		}
	}()

	written, err := file.Write(content)
	if err != nil {
		return fmt.Errorf("write temporary spool file for job %s: %w", job.ID, err)
	}
	if written != len(content) {
		return fmt.Errorf("write temporary spool file for job %s: %w", job.ID, io.ErrShortWrite)
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync temporary spool file for job %s: %w", job.ID, err)
	}
	if err := file.Close(); err != nil {
		closed = true
		return fmt.Errorf("close temporary spool file for job %s: %w", job.ID, err)
	}
	closed = true

	if err := os.Rename(tmpPath, jsonPath); err != nil {
		return fmt.Errorf("rename spool file for job %s: %w", job.ID, err)
	}
	keepTemp = false

	return nil
}

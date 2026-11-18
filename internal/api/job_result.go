package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"syscall"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"petar/internal/db"
)

const maxJobResultBytes = 1 << 20

var errJobResultTooLarge = errors.New("job result exceeds size limit")

func (s *Server) handleGetJobResult(w http.ResponseWriter, r *http.Request) {
	rawID := r.PathValue("id")
	jobID, err := uuid.Parse(rawID)
	// Parse accepts any two characters around a 36-byte UUID; Validate checks
	// that an optional wrapper consists of actual braces.
	if err != nil || uuid.Validate(rawID) != nil {
		s.writeError(w, http.StatusBadRequest, "invalid job ID")
		return
	}
	ctx, cancel := context.WithTimeoutCause(r.Context(), s.databaseTimeout, errDatabaseDeadline)
	defer cancel()
	status, err := s.database.GetJobStatus(ctx, jobID)
	if err != nil {
		if s.writeDatabaseTimeout(ctx, w) {
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeError(w, http.StatusNotFound, "job not found")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "failed to get job result")
		return
	}
	cancel()
	switch status {
	case db.StatusPending, db.StatusDispatched, db.StatusInProgress:
		s.writeJSON(w, http.StatusAccepted, statusResponse{Status: string(status)})
		return
	case db.StatusFailed:
		s.writeError(w, http.StatusConflict, "job_failed")
		return
	case db.StatusCompleted:
		// Only completed jobs may read the publication.
	default:
		s.writeError(w, http.StatusInternalServerError, "failed to get job result")
		return
	}

	document, err := readJobResult(r.Context(), s.spoolDir, jobID)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			s.writeError(w, http.StatusGone, "result_unavailable")
		case errors.Is(err, errJobResultTooLarge):
			s.writeError(w, http.StatusInternalServerError, "result_too_large")
		default:
			s.writeError(w, http.StatusInternalServerError, "failed to read job result")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if _, err := w.Write(document); err != nil {
		s.logger.ErrorContext(r.Context(), "write job result response", "job_id", jobID, "error", err)
	}
}

func readJobResult(ctx context.Context, spoolDir string, jobID uuid.UUID) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start reading job result: %w", err)
	}
	file, err := openJobResult(ctx, spoolDir, jobID)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	document, err := io.ReadAll(io.LimitReader(file, maxJobResultBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read public result: %w", err)
	}
	if len(document) > maxJobResultBytes {
		return nil, errJobResultTooLarge
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("finish reading job result: %w", err)
	}
	if !validJobResult(document) {
		return nil, errors.New("invalid public result envelope")
	}
	return document, nil
}

func validJobResult(document []byte) bool {
	if !utf8.Valid(document) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool, 3)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return false
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return false
		}
		switch key {
		case "type":
			var action string
			if json.Unmarshal(value, &action) != nil || action == "" {
				return false
			}
		case "version":
			if string(value) != "1" {
				return false
			}
		case "data":
			if value[0] != '{' {
				return false
			}
		default:
			return false
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || len(seen) != 3 {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func openJobResult(ctx context.Context, spoolDir string, jobID uuid.UUID) (*os.File, error) {
	const dirFlags = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_CLOEXEC
	root, err := syscall.Open(spoolDir, dirFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("open spool root: %w", err)
	}
	defer syscall.Close(root)

	// Pin each directory before opening its child. Never follow a symlink
	// below the configured spool root, even if renamed concurrently.
	out, err := syscall.Openat(root, "out", dirFlags|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open spool out: %w", err)
	}
	defer syscall.Close(out)
	job, err := syscall.Openat(out, jobID.String(), dirFlags|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open job result directory: %w", err)
	}
	defer syscall.Close(job)

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open public result: %w", err)
	}
	fd, err := syscall.Openat(job, "public.json",
		syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open public result: %w", err)
	}
	file := os.NewFile(uintptr(fd), "public.json")
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat public result: %w", err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("public result is not a regular file")
	}
	return file, nil
}

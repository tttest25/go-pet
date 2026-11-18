package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"petar/internal/db"
)

const jobResultFixture = `{"type":"future-action","version":1,"data":{"custom":["private-marker"]}}`

func TestGetJobResultStates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status db.Status
		code   int
		body   map[string]string
	}{
		{"invalid ID", "", 400, map[string]string{"error": "invalid job ID"}},
		{"invalid UUID wrapping", "", 400, map[string]string{"error": "invalid job ID"}},
		{"unknown job with orphan publication", "", 404, map[string]string{"error": "job not found"}},
		{"pending", db.StatusPending, 202, map[string]string{"status": "pending"}},
		{"dispatched", db.StatusDispatched, 202, map[string]string{"status": "dispatched"}},
		{"in_progress", db.StatusInProgress, 202, map[string]string{"status": "in_progress"}},
		{"failed", db.StatusFailed, 409, map[string]string{"error": "job_failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobID := uuid.New()
			routeID := jobID.String()
			switch tc.name {
			case "invalid ID":
				routeID = "invalid-id"
			case "invalid UUID wrapping":
				routeID = "x" + routeID + "x"
			}
			f := newFakeDatabase()
			calls := 0
			f.getJobStatus = func(_ context.Context, id uuid.UUID) (db.Status, error) {
				calls++
				if id != jobID {
					t.Fatalf("query ID = %s, want %s", id, jobID)
				}
				if tc.code == 404 {
					return "", fmt.Errorf("fixture: %w", pgx.ErrNoRows)
				}
				return tc.status, nil
			}
			server := newServer(f, apiTestLogger())
			server.spoolDir = t.TempDir()
			writeJobResultFixture(t, server.spoolDir, jobID, jobResultFixture)
			response := performRequest(server, http.MethodGet, "/api/v1/jobs/"+routeID+"/result", "")
			requireReadJSON(t, response, tc.code, tc.body)
			wantCalls := 1
			if tc.code == 400 {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("status lookup calls = %d, want %d", calls, wantCalls)
			}
		})
	}

	t.Run("real database missing row", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server, _ := newActionsIntegrationServer(ctx, t)
		jobID := uuid.New()
		writeJobResultFixture(t, server.spoolDir, jobID, jobResultFixture)
		response := performRequest(server, http.MethodGet, "/api/v1/jobs/"+jobID.String()+"/result", "")
		requireReadJSON(t, response, 404, map[string]string{"error": "job not found"})
	})

	t.Run("failed row with private diagnostic", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server, verification := newActionsIntegrationServer(ctx, t)
		_, jobID := createJobResultFixture(ctx, t, verification)
		if _, err := verification.Exec(ctx, `UPDATE public.sm_jobs SET status='failed',
			error_msg='private diagnostic from runner' WHERE id=$1`, jobID); err != nil {
			t.Fatal(err)
		}
		writeJobResultFixture(t, server.spoolDir, jobID, jobResultFixture)
		response := performRequest(server, http.MethodGet, "/api/v1/jobs/"+jobID.String()+"/result", "")
		requireReadJSON(t, response, 409, map[string]string{"error": "job_failed"})
	})
}

func TestGetJobResultFileErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string, id uuid.UUID)
		code  int
	}{
		{"missing job directory", func(t *testing.T, root string, _ uuid.UUID) {
			if err := os.Mkdir(filepath.Join(root, "out"), 0700); err != nil {
				t.Fatal(err)
			}
		}, 410},
		{"legacy files only", func(t *testing.T, root string, id uuid.UUID) {
			dir := filepath.Join(root, "out", id.String())
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"result.json", "stderr.log"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(jobResultFixture), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}, 410},
		{"directory instead of file", func(t *testing.T, root string, id uuid.UUID) {
			if err := os.MkdirAll(filepath.Join(root, "out", id.String(), "public.json"), 0700); err != nil {
				t.Fatal(err)
			}
		}, 500},
		{"FIFO instead of file", func(t *testing.T, root string, id uuid.UUID) {
			dir := filepath.Join(root, "out", id.String())
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(dir, "public.json"), 0600); err != nil {
				t.Fatal(err)
			}
		}, 500},
		{"public symlink to private artifact", func(t *testing.T, root string, id uuid.UUID) {
			dir := writeJobResultFixture(t, root, id, jobResultFixture)
			publicPath := filepath.Join(dir, "public.json")
			if err := os.Rename(publicPath, filepath.Join(dir, "result.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("result.json", publicPath); err != nil {
				t.Fatal(err)
			}
		}, 500},
		{"job directory symlink", func(t *testing.T, root string, id uuid.UUID) {
			otherDir := writeJobResultFixture(t, root, uuid.New(), jobResultFixture)
			if err := os.Symlink(otherDir, filepath.Join(root, "out", id.String())); err != nil {
				t.Fatal(err)
			}
		}, 500},
		{"out directory symlink", func(t *testing.T, root string, id uuid.UUID) {
			otherRoot := t.TempDir()
			writeJobResultFixture(t, otherRoot, id, jobResultFixture)
			if err := os.Symlink(filepath.Join(otherRoot, "out"), filepath.Join(root, "out")); err != nil {
				t.Fatal(err)
			}
		}, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, id := setupCompletedJobResult(t)
			tc.setup(t, server.spoolDir, id)
			response := performRequest(server, http.MethodGet, "/api/v1/jobs/"+id.String()+"/result", "")
			message := "failed to read job result"
			if tc.code == 410 {
				message = "result_unavailable"
			}
			requireReadJSON(t, response, tc.code, map[string]string{"error": message})
		})
	}

	for name, document := range map[string]string{
		"invalid JSON":       jobResultFixture[:len(jobResultFixture)-1],
		"invalid UTF-8":      strings.Replace(jobResultFixture, "private-marker", "\xff", 1),
		"second document":    jobResultFixture + "\n{}",
		"extra key":          `{"type":"echo-test","version":1,"data":{},"extra":"private-marker"}`,
		"duplicate key":      `{"type":"echo-test","version":1,"data":{},"\u0064ata":{"secret":"private-marker"}}`,
		"missing key":        `{"type":"echo-test","version":1}`,
		"wrong key case":     `{"Type":"echo-test","version":1,"data":{}}`,
		"wrong root type":    `[` + jobResultFixture + `]`,
		"wrong type type":    `{"type":42,"version":1,"data":{}}`,
		"empty type":         `{"type":"","version":1,"data":{}}`,
		"wrong version type": `{"type":"echo-test","version":"1","data":{}}`,
		"unknown version":    `{"type":"echo-test","version":2,"data":{}}`,
		"wrong data type":    `{"type":"echo-test","version":1,"data":[]}`,
		"null data":          `{"type":"echo-test","version":1,"data":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			server, id := setupCompletedJobResult(t)
			writeJobResultFixture(t, server.spoolDir, id, document)
			response := performRequest(server, http.MethodGet, "/api/v1/jobs/"+id.String()+"/result", "")
			requireReadJSON(t, response, 500, map[string]string{"error": "failed to read job result"})
		})
	}
}

func setupCompletedJobResult(t *testing.T) (*Server, uuid.UUID) {
	t.Helper()
	id := uuid.New()
	f := newFakeDatabase()
	f.getJobStatus = func(_ context.Context, got uuid.UUID) (db.Status, error) {
		if got != id {
			t.Fatalf("query ID = %s, want %s", got, id)
		}
		return db.StatusCompleted, nil
	}
	server := newServer(f, apiTestLogger())
	server.spoolDir = t.TempDir()
	return server, id
}

func TestGetJobResultSizeLimit(t *testing.T) {
	const limit = 1_048_576
	for _, size := range []int{limit, limit + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			server, id := setupCompletedJobResult(t)
			document := jobResultFixture + strings.Repeat(" ", size-len(jobResultFixture))
			writeJobResultFixture(t, server.spoolDir, id, document)
			response := performRequest(server, http.MethodGet, "/api/v1/jobs/"+id.String()+"/result", "")
			if size > limit {
				requireReadJSON(t, response, 500, map[string]string{"error": "result_too_large"})
				if strings.Contains(response.Body.String(), "private-marker") {
					t.Fatal("response contains a fragment of the oversized artifact")
				}
				return
			}
			requireJSONContentType(t, response)
			if response.Code != 200 || response.Body.Len() != limit || response.Body.String() != document {
				t.Fatalf("limit response: status=%d bytes=%d, want 200 and exact %d-byte document",
					response.Code, response.Body.Len(), limit)
			}
		})
	}
}

func TestGetJobResultDatabaseDeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"database budget", 504, "database request timed out"},
		{"ordinary error", 500, "failed to get job result"},
		{"parent canceled", 500, "failed to get job result"},
		{"earlier parent deadline", 500, "failed to get job result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const budget = 20 * time.Millisecond
			parent, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if tc.name == "parent canceled" {
				stop()
			}
			if tc.name == "earlier parent deadline" {
				ctx, cancel := context.WithDeadline(parent, time.Now().Add(-time.Second))
				defer cancel()
				parent = ctx
			}
			f := newFakeDatabase()
			f.getJobStatus = func(ctx context.Context, _ uuid.UUID) (db.Status, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > budget {
					t.Fatalf("lookup context did not receive the database budget (deadline %v)", deadline)
				}
				if tc.name == "earlier parent deadline" {
					want, _ := parent.Deadline()
					if !deadline.Equal(want) {
						t.Fatalf("lookup deadline = %v, want parent deadline %v", deadline, want)
					}
				}
				if tc.name == "ordinary error" {
					return "", errors.New("private database diagnostic")
				}
				<-ctx.Done()
				return "", fmt.Errorf("query interrupted: %w", ctx.Err())
			}
			server := newServer(f, apiTestLogger())
			server.databaseTimeout = budget
			request := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+uuid.NewString()+"/result", nil).WithContext(parent)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			requireReadJSON(t, response, tc.code, map[string]string{"error": tc.body})
		})
	}
	t.Run("successful lookup after budget still reads file", func(t *testing.T) {
		server, id := setupCompletedJobResult(t)
		writeJobResultFixture(t, server.spoolDir, id, jobResultFixture)
		server.databaseTimeout = 0
		server.database.(*fakeDatabase).getJobStatus = func(ctx context.Context, _ uuid.UUID) (db.Status, error) {
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatalf("lookup context should have expired: %v", ctx.Err())
			}
			return db.StatusCompleted, nil
		}
		response := performRequest(server, http.MethodGet, "/api/v1/jobs/"+id.String()+"/result", "")
		if response.Code != 200 || response.Body.String() != jobResultFixture {
			t.Fatalf("status = %d, body = %q; want 200 and public document", response.Code, response.Body)
		}
	})
}

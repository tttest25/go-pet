package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"petar/internal/db"
)

type waitingDatabase struct {
	*fakeDatabase
	wait func(context.Context, string) error
}

func (f *waitingDatabase) ListJobs(ctx context.Context, _ db.JobFilter) ([]db.JobDetails, error) {
	return nil, f.wait(ctx, "ListJobs")
}
func (f *waitingDatabase) GetProcess(ctx context.Context, _ uuid.UUID) (db.ProcessDetails, error) {
	return db.ProcessDetails{}, f.wait(ctx, "GetProcess")
}
func (f *waitingDatabase) CreateInput(ctx context.Context, _ string, _ json.RawMessage) (db.CreatedInput, error) {
	return db.CreatedInput{}, f.wait(ctx, "CreateInput")
}
func (f *waitingDatabase) UpdateJobStatus(ctx context.Context, _ uuid.UUID, _ db.Status, _, _ string) error {
	return f.wait(ctx, "UpdateJobStatus")
}
func (f *waitingDatabase) Ping(ctx context.Context) error { return f.wait(ctx, "Ping") }

func TestHTTPDatabaseTimeout(t *testing.T) {
	id := "019a0000-0000-7000-8000-000000000001"
	cases := []struct {
		method, path, body, call string
		status                   int
	}{
		{"GET", "/api/v1/jobs", "", "ListJobs", 504},
		{"GET", "/api/v1/processes/" + id, "", "GetProcess", 504},
		{"POST", "/api/v1/inputs", `{"source":"manual","payload":{"tool":"echo-test"}}`, "CreateInput", 504},
		{"POST", "/api/v1/jobs/" + id + "/status", `{"status":"in_progress"}`, "UpdateJobStatus", 504},
		{"GET", "/readyz", "", "Ping", 503},
	}
	for _, tc := range cases {
		t.Run(tc.call, func(t *testing.T) {
			called, expired := "", false
			f := &waitingDatabase{fakeDatabase: newFakeDatabase(), wait: func(ctx context.Context, method string) error {
				called = method
				if _, ok := ctx.Deadline(); !ok {
					t.Error("database context has no deadline")
					return errors.New("missing deadline")
				}
				<-ctx.Done()
				expired = errors.Is(ctx.Err(), context.DeadlineExceeded)
				return fmt.Errorf("database wait: %w", ctx.Err())
			}}
			s := newServer(f, apiTestLogger())
			s.databaseTimeout = 50 * time.Millisecond
			r := performRequest(s, tc.method, tc.path, tc.body)
			if called != tc.call || !expired {
				t.Errorf("called=%q expired=%v", called, expired)
			}
			if r.Code != tc.status {
				t.Errorf("status=%d want %d", r.Code, tc.status)
			}
			requireJSONContentType(t, r)
			var body errorResponse
			if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil || body.Error == "" {
				t.Fatalf("JSON error=%q decode=%v", r.Body.String(), err)
			}
		})
	}
	t.Run("parent deadline and values", func(t *testing.T) {
		type key struct{}
		parent, cancel := context.WithTimeout(context.WithValue(context.Background(), key{}, "value"), 50*time.Millisecond)
		defer cancel()
		want, _ := parent.Deadline()
		f := &waitingDatabase{wait: func(ctx context.Context, _ string) error {
			got, ok := ctx.Deadline()
			if !ok || !got.Equal(want) || ctx.Value(key{}) != "value" {
				t.Error("parent deadline or value lost")
			}
			<-ctx.Done()
			return fmt.Errorf("parent wait: %w", ctx.Err())
		}}
		s := newServer(f, apiTestLogger())
		r := httptest.NewRecorder()
		s.Handler().ServeHTTP(r, httptest.NewRequest("GET", "/api/v1/jobs", nil).WithContext(parent))
		if r.Code != 500 {
			t.Fatalf("parent timeout status=%d want 500, not server 504", r.Code)
		}
	})
	for _, mode := range []string{"client cancel", "network error", "late success"} {
		t.Run(mode, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &waitingDatabase{wait: func(ctx context.Context, _ string) error {
				switch mode {
				case "client cancel":
					cancel()
					<-ctx.Done()
					return fmt.Errorf("client wait: %w", ctx.Err())
				case "network error":
					return &net.OpError{Op: "read", Net: "tcp", Err: errors.New("network failure")}
				default:
					<-ctx.Done()
					return nil
				}
			}}
			s := newServer(f, apiTestLogger())
			s.databaseTimeout = 10 * time.Millisecond
			r := httptest.NewRecorder()
			s.Handler().ServeHTTP(r, httptest.NewRequest("GET", "/api/v1/jobs", nil).WithContext(parent))
			want := 500
			if mode == "late success" {
				want = 200
			}
			if r.Code != want {
				t.Fatalf("status=%d want %d", r.Code, want)
			}
		})
	}
}

func TestHTTPShutdownWithActiveRequest(t *testing.T) {
	entered := make(chan struct{})
	shuttingDown := make(chan struct{})
	f := &waitingDatabase{wait: func(ctx context.Context, _ string) error {
		close(entered)
		select {
		case <-shuttingDown:
		case <-time.After(3 * time.Second):
			return errors.New("shutdown did not start")
		}
		if _, ok := ctx.Deadline(); !ok {
			return errors.New("missing database deadline")
		}
		<-ctx.Done()
		return fmt.Errorf("active database wait: %w", ctx.Err())
	}}
	s := newServer(f, apiTestLogger())
	s.databaseTimeout = 100 * time.Millisecond
	server := &http.Server{Handler: s.Handler()}
	server.RegisterOnShutdown(func() { close(shuttingDown) })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	client := &http.Client{Timeout: 4 * time.Second}
	defer client.CloseIdleConnections()
	result := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://" + listener.Addr().String() + "/api/v1/jobs")
		if err == nil {
			defer resp.Body.Close()
			var body errorResponse
			err = json.NewDecoder(resp.Body).Decode(&body)
			if err == nil && (resp.StatusCode != 504 || body.Error == "" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json")) {
				err = fmt.Errorf("response status=%d body=%+v", resp.StatusCode, body)
			}
		}
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not enter database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("request did not finish")
	}
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("serving goroutine did not finish")
	}
}

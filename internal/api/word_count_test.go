package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"petar/internal/db"
	"petar/internal/spool"
)

func TestWordCountBackend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server, verification := newActionsIntegrationServer(ctx, t)
	database := server.database.(*db.DB)
	const base = "/api/v1"
	catalog := []actionContract{
		{"echo-test", 1, "Echo test"}, {"fabric", 1, "Fabric"},
		{"word-count", 1, "Word count"}, {"yt-dlp", 1, "yt-dlp"},
	}
	if got := readActions(t, server); !slices.Equal(got, catalog) {
		t.Fatalf("catalog = %+v, want %+v", got, catalog)
	}

	// The guarded integration harness serializes access to the dedicated DB.
	// Refuse to dispatch if any pending work from outside this test is present.
	var hasPending bool
	const pending = `SELECT EXISTS(SELECT FROM public.sm_jobs WHERE status='pending')`
	if err := verification.QueryRow(ctx, pending).Scan(&hasPending); err != nil || hasPending {
		t.Fatalf("need an empty pending queue: hasPending=%t error=%v", hasPending, err)
	}

	request := func(method, path, body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response,
			httptest.NewRequest(method, base+path, strings.NewReader(body)).WithContext(ctx))
		return response
	}
	response := request(http.MethodPost, "/actions", `{"action":"word-count","version":1,"data":{"text":"один два три"}}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body)
	}
	var created struct {
		InputID   uuid.UUID `json:"input_id"`
		ProcessID uuid.UUID `json:"process_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.InputID == uuid.Nil {
		t.Fatalf("invalid create response %s: %v", response.Body, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tag, err := verification.Exec(ctx, `DELETE FROM public.sm_inputs WHERE id=$1`, created.InputID)
		if err != nil || tag.RowsAffected() != 1 {
			t.Errorf("clean word-count fixture: %v, error=%v", tag, err)
		}
	})
	response = request(http.MethodGet, "/processes/"+created.ProcessID.String(), "")
	var process struct {
		Jobs []struct {
			ID uuid.UUID `json:"id"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &process); response.Code != http.StatusOK || err != nil ||
		len(process.Jobs) != 1 || process.Jobs[0].ID == uuid.Nil || process.Jobs[0].ID == created.InputID {
		t.Fatalf("expected process with one job: status=%d body=%s decode=%v", response.Code, response.Body, err)
	}
	jobID := process.Jobs[0].ID

	counts := func() (n [3]int) {
		t.Helper()
		if err := verification.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.sm_inputs),
			(SELECT count(*) FROM public.sm_processes), (SELECT count(*) FROM public.sm_jobs)`).
			Scan(&n[0], &n[1], &n[2]); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := counts()
	rejected := request(http.MethodPost, "/actions", `{"action":"word-count","version":1,"data":{"text":42}}`)
	if after := counts(); rejected.Code != http.StatusBadRequest || after != before {
		t.Fatalf("invalid text: status=%d body=%s, counts %v → %v", rejected.Code, rejected.Body, before, after)
	}
	if err := verification.QueryRow(ctx, `SELECT EXISTS(
		SELECT FROM public.sm_jobs WHERE status='pending' AND id<>$1)`, jobID).
		Scan(&hasPending); err != nil || hasPending {
		t.Fatalf("refusing dispatch of other jobs: hasPending=%t error=%v", hasPending, err)
	}

	job, err := database.PickNextJob(ctx)
	if err != nil || job == nil || job.ID != jobID || job.ActionVersion != 1 {
		t.Fatalf("dispatch job %s: got %+v, error=%v", jobID, job, err)
	}
	if err := os.Mkdir(filepath.Join(server.spoolDir, "in"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := spool.NewWriter(server.spoolDir).WriteJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	callback := httptest.NewServer(server.Handler())
	defer callback.Close()
	cmd := exec.CommandContext(ctx, "bash", "../../scripts/spool-runner.sh")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SPOOL_DIR=" + server.spoolDir, "API_BASE_URL=" + callback.URL + base}
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run spool job: %v\n%s", err, out)
	}
	response = request(http.MethodGet, "/jobs/"+jobID.String()+"/result", "")
	requireReadJSON(t, response, http.StatusOK, map[string]any{
		"type": "word-count", "version": 1, "data": map[string]int{"words": 3},
	})
}

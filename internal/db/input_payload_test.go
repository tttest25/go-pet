package db_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"petar/internal/db"
)

func TestCreateInputPayloadContract(t *testing.T) {
	env := newQueueTestEnv(t, "t17-db-"+uuid.NewString())
	ctx := queueTestContext(t)
	counts := func(t *testing.T) (result [3]int) {
		t.Helper()
		err := env.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM sm_inputs),
			(SELECT count(*) FROM sm_processes), (SELECT count(*) FROM sm_jobs)`).
			Scan(&result[0], &result[1], &result[2])
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	tests := []struct {
		name, payload, tool string // empty tool means rejection
	}{
		{"SQL null", "", ""},
		{"JSON null", `null`, ""},
		{"array", `[]`, ""},
		{"scalar", `"echo-test"`, ""},
		{"missing tool", `{}`, ""},
		{"null tool", `{"tool":null}`, ""},
		{"number tool", `{"tool":123}`, ""},
		{"unknown tool", `{"tool":"unknown"}`, ""},
		{"wrong case", `{"tool":"Echo-test"}`, ""},
		{"padded tool", `{"tool":" echo-test "}`, ""},
		{"default echo", `{"tool":"echo-test"}`, "echo-test"},
		{"preserved echo", `{"tool":"echo-test","message":"  привет  ","extra":1}`, "echo-test"},
		{"null message", `{"tool":"echo-test","message":null}`, ""},
		{"boolean message", `{"tool":"echo-test","message":false}`, ""},
		{"numeric message", `{"tool":"echo-test","message":8}`, ""},
		{"object message", `{"tool":"echo-test","message":{}}`, ""},
		{"empty message", `{"tool":"echo-test","message":""}`, ""},
		{"blank message", `{"tool":"echo-test","message":" \t\r\n"}`, ""},
		{"missing text", `{"tool":"fabric"}`, ""},
		{"null text", `{"tool":"fabric","text":null}`, ""},
		{"number text", `{"tool":"fabric","text":7}`, ""},
		{"empty text", `{"tool":"fabric","text":""}`, ""},
		{"blank text", `{"tool":"fabric","text":"\t\n "}`, ""},
		{"default pattern", `{"tool":"fabric","text":"  a\nb  "}`, "fabric"},
		{"named pattern", `{"tool":"fabric","text":"x","pattern":"A_b-2"}`, "fabric"},
		{"null pattern", `{"tool":"fabric","text":"x","pattern":null}`, ""},
		{"numeric pattern", `{"tool":"fabric","text":"x","pattern":8}`, ""},
		{"empty pattern", `{"tool":"fabric","text":"x","pattern":""}`, ""},
		{"non-ASCII pattern", `{"tool":"fabric","text":"x","pattern":"é"}`, ""},
		{"path pattern", `{"tool":"fabric","text":"x","pattern":"a/b.md"}`, ""},
		{"newline pattern", `{"tool":"fabric","text":"x","pattern":"a\n"}`, ""},
		{"whitespace pattern", `{"tool":"fabric","text":"x","pattern":" a "}`, ""},
		{"web URL", `{"tool":"yt-dlp","url":"HTTPS://example.org/a?q=1#t"}`, "yt-dlp"},
		{"local host", `{"tool":"yt-dlp","url":"http://localhost:8080/x"}`, "yt-dlp"},
		{"IPv4 host", `{"tool":"yt-dlp","url":"http://127.0.0.1/x"}`, "yt-dlp"},
		{"IPv6 host", `{"tool":"yt-dlp","url":"http://[::1]:8080/"}`, "yt-dlp"},
		{"missing URL", `{"tool":"yt-dlp"}`, ""},
		{"null URL", `{"tool":"yt-dlp","url":null}`, ""},
		{"numeric URL", `{"tool":"yt-dlp","url":7}`, ""},
		{"relative URL", `{"tool":"yt-dlp","url":"/path"}`, ""},
		{"scheme-relative URL", `{"tool":"yt-dlp","url":"//host/a"}`, ""},
		{"FTP URL", `{"tool":"yt-dlp","url":"ftp://host/a"}`, ""},
		{"empty URL host", `{"tool":"yt-dlp","url":"https:///a"}`, ""},
		{"query as host", `{"tool":"yt-dlp","url":"https://?q"}`, ""},
		{"port as host", `{"tool":"yt-dlp","url":"https://:443/a"}`, ""},
		{"userinfo as host", `{"tool":"yt-dlp","url":"https://user@/a"}`, ""},
		{"unclosed IPv6", `{"tool":"yt-dlp","url":"http://[::1/a"}`, ""},
		{"raw whitespace", `{"tool":"yt-dlp","url":"https://host/a b"}`, ""},
		{"raw control", `{"tool":"yt-dlp","url":"https://host/a\tb"}`, ""},
	}
	for _, length := range []int{128, 129} {
		name, tool := "128-byte pattern", "fabric"
		if length == 129 {
			name, tool = "129-byte pattern", ""
		}
		tests = append(tests, struct{ name, payload, tool string }{
			name, `{"tool":"fabric","text":"x","pattern":"` + strings.Repeat("a", length) + `"}`, tool,
		})
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := counts(t)
			payload := json.RawMessage(tc.payload)
			if tc.payload == "" {
				payload = nil
			}
			created, err := env.database.CreateInput(ctx, env.source, payload)
			if tc.tool == "" {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "22023" {
					t.Errorf("CreateInput error = %v, want SQLSTATE 22023", err)
				}
				if !errors.Is(err, db.ErrInvalidInput) || errors.Is(err, db.ErrInvalidCallbackStatus) {
					t.Errorf("CreateInput error = %v, want only the input sentinel", err)
				}
				if got := counts(t); got != before {
					t.Errorf("input/process/job counts changed after rejection: %v → %v", before, got)
				}
				return
			}
			if err != nil || created.InputID == uuid.Nil || created.ProcessID == uuid.Nil || created.Status != "new" {
				t.Fatalf("CreateInput = %+v, %v", created, err)
			}
			var matching int
			err = env.pool.QueryRow(ctx, `SELECT count(*) FROM sm_inputs i
				JOIN sm_processes p ON p.input_id = i.id JOIN sm_jobs j ON j.process_id = p.id
				WHERE i.id=$1 AND p.id=$2 AND i.payload=$3::jsonb AND j.payload=i.payload
					AND j.tool_name=$4 AND j.status='pending'`, created.InputID, created.ProcessID, payload, tc.tool).Scan(&matching)
			if err != nil || matching != 1 {
				t.Fatalf("matching chains = %d, %v; want one with unchanged payload", matching, err)
			}
			if got := counts(t); got != [3]int{before[0] + 1, before[1] + 1, before[2] + 1} {
				t.Fatalf("expected one new input/process/job: %v → %v", before, got)
			}
		})
	}
}

func TestPayloadValidationLeavesCallbackSentinelUnchanged(t *testing.T) {
	env := newQueueTestEnv(t, "t17-callback-"+uuid.NewString())
	ctx := queueTestContext(t)
	err := env.database.UpdateJobStatus(ctx, uuid.New(), db.Status("unknown"), "", "")
	var pgErr *pgconn.PgError
	if !errors.Is(err, db.ErrInvalidCallbackStatus) || errors.Is(err, db.ErrInvalidInput) ||
		!errors.As(err, &pgErr) || pgErr.Code != "22023" {
		t.Fatalf("callback error = %v, want callback sentinel and SQLSTATE 22023", err)
	}
}

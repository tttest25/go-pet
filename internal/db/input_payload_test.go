package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"petar/internal/db"
)

func TestCreateActionPayloadContract(t *testing.T) {
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
		name, action, data string
		valid              bool
	}{
		{"SQL null", "echo-test", "", false},
		{"JSON null", "echo-test", `null`, false},
		{"array", "echo-test", `[]`, false},
		{"scalar", "echo-test", `"echo-test"`, false},
		{"empty action", "", `{}`, false},
		{"null tool collision", "echo-test", `{"tool":null}`, false},
		{"number tool collision", "echo-test", `{"tool":123}`, false},
		{"unknown action", "unknown", `{}`, false},
		{"wrong case", "Echo-test", `{}`, false},
		{"padded action", " echo-test ", `{}`, false},
		{"default echo", "echo-test", `{}`, true},
		{"preserved echo", "echo-test", `{"message":"  привет  ","extra":9007199254740993}`, true},
		{"null message", "echo-test", `{"message":null}`, false},
		{"boolean message", "echo-test", `{"message":false}`, false},
		{"numeric message", "echo-test", `{"message":8}`, false},
		{"object message", "echo-test", `{"message":{}}`, false},
		{"empty message", "echo-test", `{"message":""}`, false},
		{"blank message", "echo-test", `{"message":" \t\r\n"}`, false},
		{"missing text", "fabric", `{}`, false},
		{"null text", "fabric", `{"text":null}`, false},
		{"number text", "fabric", `{"text":7}`, false},
		{"empty text", "fabric", `{"text":""}`, false},
		{"blank text", "fabric", `{"text":"\t\n "}`, false},
		{"default pattern", "fabric", `{"text":"  a\nb  "}`, true},
		{"named pattern", "fabric", `{"text":"x","pattern":"A_b-2"}`, true},
		{"null pattern", "fabric", `{"text":"x","pattern":null}`, false},
		{"numeric pattern", "fabric", `{"text":"x","pattern":8}`, false},
		{"empty pattern", "fabric", `{"text":"x","pattern":""}`, false},
		{"non-ASCII pattern", "fabric", `{"text":"x","pattern":"é"}`, false},
		{"path pattern", "fabric", `{"text":"x","pattern":"a/b.md"}`, false},
		{"newline pattern", "fabric", `{"text":"x","pattern":"a\n"}`, false},
		{"whitespace pattern", "fabric", `{"text":"x","pattern":" a "}`, false},
		{"128-byte pattern", "fabric", `{"text":"x","pattern":"` + strings.Repeat("a", 128) + `"}`, true},
		{"129-byte pattern", "fabric", `{"text":"x","pattern":"` + strings.Repeat("a", 129) + `"}`, false},
		{"web URL", "yt-dlp", `{"url":"HTTPS://example.org/a?q=1#t"}`, true},
		{"local host", "yt-dlp", `{"url":"http://localhost:8080/x"}`, true},
		{"IPv4 host", "yt-dlp", `{"url":"http://127.0.0.1/x"}`, true},
		{"IPv6 host", "yt-dlp", `{"url":"http://[::1]:8080/"}`, true},
		{"missing URL", "yt-dlp", `{}`, false},
		{"null URL", "yt-dlp", `{"url":null}`, false},
		{"numeric URL", "yt-dlp", `{"url":7}`, false},
		{"relative URL", "yt-dlp", `{"url":"/path"}`, false},
		{"scheme-relative URL", "yt-dlp", `{"url":"//host/a"}`, false},
		{"FTP URL", "yt-dlp", `{"url":"ftp://host/a"}`, false},
		{"empty URL host", "yt-dlp", `{"url":"https:///a"}`, false},
		{"query as host", "yt-dlp", `{"url":"https://?q"}`, false},
		{"port as host", "yt-dlp", `{"url":"https://:443/a"}`, false},
		{"userinfo as host", "yt-dlp", `{"url":"https://user@/a"}`, false},
		{"unclosed IPv6", "yt-dlp", `{"url":"http://[::1/a"}`, false},
		{"raw whitespace", "yt-dlp", `{"url":"https://host/a b"}`, false},
		{"raw control", "yt-dlp", `{"url":"https://host/a\tb"}`, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := counts(t)
			var data json.RawMessage
			if tc.data != "" {
				data = json.RawMessage(tc.data)
			}
			created, err := env.database.CreateAction(ctx, tc.action, 1, data)
			if created.InputID != uuid.Nil {
				t.Cleanup(func() {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if _, err := env.pool.Exec(cleanupCtx, `DELETE FROM sm_inputs WHERE id=$1`, created.InputID); err != nil {
						t.Errorf("clean up action input %s: %v", created.InputID, err)
					}
				})
			}
			if !tc.valid {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "22023" {
					t.Errorf("CreateAction error = %v, want SQLSTATE 22023", err)
				}
				if !errors.Is(err, db.ErrInvalidInput) || errors.Is(err, db.ErrInvalidCallbackStatus) {
					t.Errorf("CreateAction error = %v, want only the input sentinel", err)
				}
				if got := counts(t); got != before {
					t.Errorf("input/process/job counts changed after rejection: %v → %v", before, got)
				}
				return
			}
			if err != nil || created.InputID == uuid.Nil || created.ProcessID == uuid.Nil || created.Status != "new" {
				t.Fatalf("CreateAction = %+v, %v", created, err)
			}
			var matching int
			err = env.pool.QueryRow(ctx, `SELECT count(*) FROM sm_inputs i
				JOIN sm_processes p ON p.input_id = i.id JOIN sm_jobs j ON j.process_id = p.id
				WHERE i.id=$1 AND p.id=$2 AND i.source='api'
					AND i.payload=($3::jsonb || jsonb_build_object('tool', $4::text))
					AND j.payload=i.payload AND j.tool_name=$4 AND j.status='pending'
					AND j.action_version=1`, created.InputID, created.ProcessID, data, tc.action).Scan(&matching)
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

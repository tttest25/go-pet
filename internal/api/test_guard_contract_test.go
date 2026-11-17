package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTestDSNContract(t *testing.T) {
	for _, name := range []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGSERVICE", "PGSERVICEFILE", "PGOPTIONS"} {
		t.Setenv(name, "")
	}
	t.Setenv("DB_DSN", "postgres://petar_user:private-secret@127.0.0.1:5432/petar_db?sslmode=disable")
	for _, tc := range []struct {
		name, dsn string
		ok        bool
	}{
		{"missing", "", false},
		{"production", "postgres://petar_user:private-secret@127.0.0.1:5432/petar_db?sslmode=disable", false},
		{"unknown", "postgres://petar_user:private-secret@127.0.0.1:5432/another_test?sslmode=disable", false},
		{"malformed", "://private-secret", false},
		{"url", "postgres://petar_user:private-secret@127.0.0.1:5432/petar_test?sslmode=disable", true},
		{"keyword", "host=127.0.0.1 port=5432 user=petar_user password=private-secret dbname=petar_test sslmode=disable", true},
		{"override", "postgres://petar_user:private-secret@127.0.0.1:5432/petar_test?sslmode=disable&dbname=petar_db", false},
		{"encoded", "postgres://petar_user:private-secret@127.0.0.1:5432/petar%5fdb?sslmode=disable", false},
		{"remote", "postgres://petar_user:private-secret@example.com:5432/petar_test?sslmode=disable", false},
		{"options", "host=127.0.0.1 port=5432 user=petar_user dbname=petar_test options='-c search_path=evil' sslmode=disable", false},
		{"fallback", "host=127.0.0.1,localhost port=5432 user=petar_user dbname=petar_test sslmode=disable", false},
		{"port", "postgres://petar_user:private-secret@127.0.0.1:5433/petar_test?sslmode=disable", false},
		{"user", "postgres://postgres:private-secret@127.0.0.1:5432/petar_test?sslmode=disable", false},
		{"socket", "host=/var/run/postgresql port=5432 user=petar_user dbname=petar_test sslmode=disable", false},
		{"search-path", "host=127.0.0.1 port=5432 user=petar_user dbname=petar_test search_path=evil sslmode=disable", false},
		{"service", "service=private-secret", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PETAR_TEST_DB_DSN", tc.dsn)
			_, err := testDSN(context.Background())
			if (err == nil) != tc.ok {
				t.Fatalf("validation success = %v, want %v", err == nil, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), "private-secret") {
				t.Fatal("validation exposed credentials")
			}
		})
	}
}

var guardIntegrationTests = []string{"TestServerIntegration"}

func TestDatabaseErrorsPreserveCauseWithoutCredentials(t *testing.T) {
	cause := errors.New("driver error for postgres://petar_user:private-secret@127.0.0.1/petar_test")
	err := fmt.Errorf("guard failed: %w", testDatabaseError("read marker", cause))
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "read marker") {
		t.Fatal("database error lost cause or operation context")
	}
	if strings.Contains(err.Error(), "private-secret") || strings.Contains(err.Error(), "postgres://") {
		t.Fatal("database error exposed connection credentials")
	}
}

func TestTestDSNRejectsEnvironmentOverrides(t *testing.T) {
	t.Setenv("PETAR_TEST_DB_DSN", "postgres://petar_user:private-secret@127.0.0.1:5432/petar_test?sslmode=disable")
	for _, name := range []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGSERVICE", "PGSERVICEFILE", "PGOPTIONS"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "private-secret")
			_, err := testDSN(context.Background())
			if err == nil {
				t.Fatal("ambiguous environment accepted")
			}
			if strings.Contains(err.Error(), "private-secret") {
				t.Fatal("environment override leaked")
			}
		})
	}
}

func TestInheritedLockRejectsBogusFD(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "not-the-lock")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, fd := range []string{"not-a-number", "999999", strconv.Itoa(int(file.Fd()))} {
		t.Run(fd, func(t *testing.T) {
			t.Setenv("PETAR_TEST_LOCK_FD", fd)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			lock, err := acquireTestLock(ctx)
			if err == nil {
				lock.Close()
				t.Fatal("unverified inherited FD accepted")
			}
		})
	}
}

func TestGuardDirectBinaryRefusesUnsafeDSN(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "docker-called")
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nprintf called > \"$PETAR_GUARD_DOCKER_LOG\"\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, dsn := range []string{"", "postgres://petar_user:private-secret@127.0.0.1:5432/petar_db?sslmode=disable"} {
		for _, entry := range guardIntegrationTests {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+entry+"$", "-test.count=1")
			for _, item := range os.Environ() {
				if !strings.HasPrefix(item, "PETAR_TEST_") && !strings.HasPrefix(item, "PETAR_GUARD_") && !strings.HasPrefix(item, "DB_DSN=") && !strings.HasPrefix(item, "PATH=") {
					cmd.Env = append(cmd.Env, item)
				}
			}
			cmd.Env = append(cmd.Env, "PETAR_TEST_DB_DSN="+dsn,
				"DB_DSN=postgres://petar_user:private-secret@127.0.0.1:5432/petar_db?sslmode=disable",
				"PATH="+dir, "PETAR_GUARD_DOCKER_LOG="+log)
			output, err := cmd.CombinedOutput()
			cancel()
			if err == nil {
				t.Fatal("direct test binary accepted unsafe test DSN")
			}
			if !strings.Contains(string(output), "test database guard:") || strings.Contains(string(output), "panic:") {
				t.Fatalf("%s failed outside expected guard: %s", entry, output)
			}
			if ctx.Err() == context.DeadlineExceeded {
				t.Fatal("guard did not fail before connection/lock wait")
			}
			if strings.Contains(string(output), "private-secret") {
				t.Fatal("guard leaked credentials")
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("guard executed docker before refusing unsafe DSN")
			}
		}
	}
}

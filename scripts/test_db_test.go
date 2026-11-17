package scripts_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestLifecycleWrapper(t *testing.T) {
	for _, command := range []string{"prepare", "check", "reset", "gate"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "log")
			// Fake the compiler, not Docker or a database: no live I/O is possible.
			fake := `#!/bin/bash
set -eu
echo "$*" >> "$FAKE_LOG"
while [[ $# -gt 0 ]]; do
  if [[ $1 == -o ]]; then
    shift
    cat > "$1" <<'EOF'
#!/bin/bash
echo "bridge:$PETAR_TEST_DB_COMMAND:$*" >> "$FAKE_LOG"
exit "${FAKE_EXIT:-0}"
EOF
    chmod +x "$1"
    exit 0
  fi
  shift
done
exit 91
`
			if err := os.WriteFile(filepath.Join(dir, "go"), []byte(fake), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			args := []string{"./test-db.sh", command}
			if command == "gate" {
				args = []string{"./quality-gate.sh"}
			}
			if command == "reset" {
				args = append(args, "--confirm", "petar_test")
			}
			cmd := exec.CommandContext(ctx, "bash", args...)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FAKE_LOG="+log)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("wrapper: %v: %s", err, out)
			}
			b, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), "test -c -o ") || !strings.Contains(string(b), "bridge:"+command+":-test.run=^TestPetarTestDBCommand$") {
				t.Fatalf("unexpected compiler/bridge calls: %s", b)
			}
			failed := exec.CommandContext(ctx, "bash", args...)
			failed.Env = append(cmd.Env, "FAKE_EXIT=19")
			if out, err := failed.CombinedOutput(); err == nil || strings.Contains(string(out), "QUALITY GATE PASSED") {
				t.Fatalf("bridge failure hidden: %v: %s", err, out)
			}
		})
	}
}

var bridgeBuild struct {
	sync.Once
	path   string
	err    error
	output []byte
}

var _ func(context.Context, *testing.T) string = testBridge

func TestBridgeCleanupReportsFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.test")
	for _, name := range []string{path, filepath.Join(dir, "unexpected")} {
		if err := os.WriteFile(name, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanupBridge(context.Background(), path); err == nil {
		t.Fatal("nonempty bridge directory cleanup reported success")
	}
}

func testBridge(ctx context.Context, t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	bridgeBuild.Do(func() {
		dir, err := os.MkdirTemp("/tmp/opencode", "petar-script-contract-")
		if err != nil {
			bridgeBuild.err = fmt.Errorf("create bridge directory: %w", err)
			return
		}
		bridgeBuild.path = filepath.Join(dir, "db.test")
		cmd := exec.CommandContext(ctx, "go", "test", "-c", "-o", bridgeBuild.path, "./internal/db")
		cmd.Dir = ".."
		output, err := cmd.CombinedOutput()
		bridgeBuild.output = output
		if err != nil {
			bridgeBuild.err = fmt.Errorf("build bridge subprocess: %w", err)
		}
	})
	if bridgeBuild.err != nil {
		t.Fatalf("compile safe bridge: %v: %s", bridgeBuild.err, bridgeBuild.output)
	}
	return bridgeBuild.path
}

func TestMain(m *testing.M) {
	code := m.Run()
	if bridgeBuild.path != "" {
		if err := cleanupBridge(context.Background(), bridgeBuild.path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	os.Exit(code)
}

func cleanupBridge(_ context.Context, path string) error {
	// Cleanup must also execute after the build context has been canceled.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove test bridge binary: %w", err)
	}
	if err := os.Remove(filepath.Dir(path)); err != nil {
		return fmt.Errorf("remove test bridge directory: %w", err)
	}
	return nil
}

func bridgeEnvironment() []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !strings.HasPrefix(key, "PETAR_TEST_") && !strings.HasPrefix(key, "PG") {
			env = append(env, item)
		}
	}
	return env
}

func TestLifecycleBridgeRefusesUnsafeTargetsBeforeDocker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	binary := testBridge(ctx, t)
	for _, dsn := range []string{
		"",
		"postgres://petar_user:private-secret@127.0.0.1:5432/petar_db?sslmode=disable",
		"postgres://petar_user:private-secret@127.0.0.1:5432/unknown?sslmode=disable",
		"://private-secret",
		"postgres://petar_user:private-secret@127.0.0.1:5432/petar_test?sslmode=disable&dbname=petar_db",
		"service=private-secret",
	} {
		for _, command := range []string{"prepare", "check", "reset", "gate", "unknown"} {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			cmd := exec.CommandContext(ctx, binary, "-test.run=^TestPetarTestDBCommand$")
			cmd.Env = append(bridgeEnvironment(), "PETAR_TEST_DB_COMMAND="+command, "PETAR_TEST_DB_DSN="+dsn, "PETAR_TEST_DB_CONFIRM=petar_test",
				"DB_DSN=postgres://petar_user:private-secret@127.0.0.1:5432/petar_db?sslmode=disable",
				"PATH="+t.TempDir())
			out, err := cmd.CombinedOutput()
			cancel()
			if err == nil {
				t.Fatalf("unsafe target accepted command=%s", command)
			}
			if ctx.Err() == context.DeadlineExceeded {
				t.Fatal("unsafe target did not fail before I/O")
			}
			if strings.Contains(string(out), "private-secret") {
				t.Fatal("bridge leaked credentials")
			}
			if strings.Contains(string(out), "Docker") || strings.Contains(string(out), "PostgreSQL is unavailable") {
				t.Fatalf("unsafe target reached I/O: %s", out)
			}
		}
	}
}

func TestLifecycleSafeStateMachineAndLockSubprocesses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, testBridge(ctx, t), "-test.run=^TestLifecycle", "-test.count=1", "-test.v")
	// Keep a verified gate descriptor when the harness itself runs under gate:
	// Go's compiler does not need it, but the compiled test's lock checks do.
	cmd.Env = bridgeEnvironment()
	if socket := os.Getenv("PETAR_TEST_LOCK_SOCKET"); socket != "" {
		cmd.Env = append(cmd.Env, "PETAR_TEST_LOCK_SOCKET="+socket, "PETAR_TEST_LOCK_TOKEN="+os.Getenv("PETAR_TEST_LOCK_TOKEN"))
	}
	if raw := os.Getenv("PETAR_TEST_LOCK_FD"); raw != "" {
		// Do not assume fd 3 is usable after the compiler. Duplicate the current
		// inherited descriptor through ExtraFiles for this explicit child.
		n, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatal(err)
		}
		dup, err := syscall.Dup(n)
		if err != nil {
			t.Fatal(err)
		}
		f := os.NewFile(uintptr(dup), "inherited-test-lock")
		defer func() {
			if err := f.Close(); err != nil {
				t.Errorf("close inherited test lock: %v", err)
			}
		}()
		cmd.ExtraFiles = []*os.File{f}
		cmd.Env = append(cmd.Env, "PETAR_TEST_LOCK_FD=3")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("safe lifecycle fakes: %v: %s", err, out)
	}
}

func TestLifecycleResetRequiresConfirmation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "./test-db.sh", "reset")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("unconfirmed reset succeeded: %s", out)
	}
}

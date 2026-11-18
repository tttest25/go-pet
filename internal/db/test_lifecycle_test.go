package db_test

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The command test is inert in ordinary go test, and its environment is removed
// from every gate child so lifecycle cannot recursively execute inside the suite.
func TestPetarTestDBCommand(t *testing.T) {
	command := os.Getenv("PETAR_TEST_DB_COMMAND")
	if command == "" {
		return
	}
	switch command {
	case "prepare", "check", "reset", "gate":
	default:
		t.Fatal("test database command is not allowed")
	}
	if command == "reset" && os.Getenv("PETAR_TEST_DB_CONFIRM") != "petar_test" {
		t.Fatal("reset requires --confirm petar_test")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := runTestDBCommand(ctx, command); err != nil {
		t.Fatal(err)
	}
}

func runTestDBCommand(ctx context.Context, command string) error {
	dsn, err := testDSN(ctx)
	if err != nil {
		return err
	}
	lock, err := acquireTestLock(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	cfg, err := testConnectionConfig(ctx, dsn)
	if err != nil {
		return err
	}
	cfg.Database = "postgres"
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return testDatabaseError("connect lifecycle administration", err)
	}
	defer admin.Close(context.Background())
	if err := checkTestIdentity(ctx, admin, "postgres"); err != nil {
		return err
	}
	if err := checkTestDocker(ctx, admin); err != nil {
		return err
	}
	store := &pgTestLifecycle{admin: admin, dsn: dsn}
	if err := runTestLifecycle(ctx, command, store); err != nil {
		return err
	}
	if command != "gate" {
		return nil
	}
	for _, args := range [][]string{
		{"vet", "./..."},
		{"test", "-p=1", "-count=1", "-v", "-race", "-timeout", "30s", "./..."},
	} {
		if err := runTestGateChild(ctx, lock, args); err != nil {
			return err
		}
	}
	fmt.Println("QUALITY GATE PASSED")
	return nil
}

func testProjectRoot() string { return "/opt/go-pet" }

func runTestGateChild(ctx context.Context, lock *os.File, args []string) (resultErr error) {
	relay, err := startTestLockRelay(ctx, lock)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, relay.close(ctx)) }()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = testProjectRoot()
	cmd.Env = testChildEnvironment()
	cmd.Env = append(cmd.Env, "PETAR_TEST_LOCK_SOCKET="+relay.socket, "PETAR_TEST_LOCK_TOKEN="+relay.token)
	// ExtraFiles preserves ownership in the immediate launcher. Descendants
	// authenticate to the relay instead of relying on go's incidental FD policy.
	cmd.ExtraFiles = []*os.File{lock}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Kill the entire process group on cancellation, not just the go launcher.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("quality gate %s failed: %w", args[0], err)
	}
	return nil
}

func testChildEnvironment() []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if key != "PETAR_TEST_DB_COMMAND" && key != "PETAR_TEST_DB_CONFIRM" && key != "PETAR_TEST_LOCK_FD" && key != "PETAR_TEST_LOCK_SOCKET" && key != "PETAR_TEST_LOCK_TOKEN" {
			env = append(env, item)
		}
	}
	return env
}

type testLockRelay struct {
	socket, token string
	listener      *net.UnixListener
	cancel        context.CancelFunc
	done          chan struct{}
	workers       sync.WaitGroup
	errorsMu      sync.Mutex
	failures      []error
}

func startTestLockRelay(ctx context.Context, lock *os.File) (*testLockRelay, error) {
	// The socket is an authenticated transport, never evidence of lock ownership.
	// Each client independently verifies the received descriptor with flock.
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("test lock relay: generate token: %w", err)
	}
	dir, err := os.MkdirTemp("/opt/go-pet/postgres", ".petar-test-lock-relay-")
	if err != nil {
		return nil, fmt.Errorf("test lock relay: create private directory: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, errors.Join(fmt.Errorf("test lock relay: set directory permissions: %w", err), removeTestRelayPath(ctx, dir))
	}
	socket := filepath.Join(dir, "lock.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("test lock relay: listen: %w", err), removeTestRelayPath(ctx, dir))
	}
	if err := os.Chmod(socket, 0600); err != nil {
		return nil, errors.Join(fmt.Errorf("test lock relay: set socket permissions: %w", err), closeTestRelayListener(ctx, listener), removeTestRelayPath(ctx, socket), removeTestRelayPath(ctx, dir))
	}
	childCtx, cancel := context.WithCancel(ctx)
	r := &testLockRelay{socket: socket, token: hex.EncodeToString(random), listener: listener, cancel: cancel, done: make(chan struct{})}
	listenerClosed := make(chan struct{})
	stop := context.AfterFunc(childCtx, func() {
		defer close(listenerClosed)
		r.recordError(childCtx, closeTestRelayListener(childCtx, listener))
	})
	go func() {
		defer close(r.done)
		defer func() {
			if !stop() {
				<-listenerClosed
			}
		}()
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					r.recordError(childCtx, fmt.Errorf("test lock relay: accept client: %w", err))
				}
				return
			}
			r.workers.Add(1)
			go func() {
				defer r.workers.Done()
				defer func() {
					if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
						r.recordError(childCtx, fmt.Errorf("test lock relay: close client: %w", err))
					}
				}()
				clientClosed := make(chan struct{})
				stop := context.AfterFunc(childCtx, func() {
					defer close(clientClosed)
					if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
						r.recordError(childCtx, fmt.Errorf("test lock relay: cancel client: %w", err))
					}
				})
				defer func() {
					if !stop() {
						<-clientClosed
					}
				}()
				r.recordError(childCtx, r.serveClient(childCtx, conn, lock))
			}()
		}
	}()
	return r, nil
}

func (r *testLockRelay) serveClient(ctx context.Context, conn *net.UnixConn, lock *os.File) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("test lock relay: serve client context: %w", err)
	}
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("test lock relay: set client deadline: %w", err)
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("test lock relay: access client descriptor: %w", err)
	}
	var credentials *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return fmt.Errorf("test lock relay: inspect client descriptor: %w", err)
	}
	if credErr != nil {
		return fmt.Errorf("test lock relay: read peer credentials: %w", credErr)
	}
	if credentials == nil || credentials.Uid != uint32(os.Getuid()) {
		return nil
	} // Unauthorized peers receive no descriptor.
	request := make([]byte, 64)
	if _, err := io.ReadFull(conn, request); err != nil {
		return fmt.Errorf("test lock relay: read authentication: %w", err)
	}
	if subtle.ConstantTimeCompare(request, []byte(r.token)) != 1 {
		return nil
	}
	rights := syscall.UnixRights(int(lock.Fd()))
	n, controlN, err := conn.WriteMsgUnix([]byte{'L'}, rights, nil)
	if err != nil {
		return fmt.Errorf("test lock relay: send descriptor: %w", err)
	}
	if n != 1 || controlN != len(rights) {
		return fmt.Errorf("test lock relay: send descriptor: %w", io.ErrShortWrite)
	}
	return nil
}

func (r *testLockRelay) recordError(ctx context.Context, err error) {
	if err == nil || (ctx.Err() != nil && (errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))) {
		return
	}
	r.errorsMu.Lock()
	defer r.errorsMu.Unlock()
	r.failures = append(r.failures, err)
}

func removeTestRelayPath(ctx context.Context, path string) error {
	// Cleanup must run even after cancellation.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("test lock relay: remove %s: %w", path, err)
	}
	return nil
}

func closeTestRelayListener(ctx context.Context, listener *net.UnixListener) error {
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("test lock relay: close listener: %w", err)
	}
	return nil
}

func (r *testLockRelay) close(ctx context.Context) error {
	r.cancel()
	closeErr := closeTestRelayListener(ctx, r.listener)
	<-r.done
	r.workers.Wait()
	cleanupErr := errors.Join(removeTestRelayPath(ctx, r.socket), removeTestRelayPath(ctx, filepath.Dir(r.socket)))
	r.errorsMu.Lock()
	defer r.errorsMu.Unlock()
	return errors.Join(append(r.failures, closeErr, cleanupErr)...)
}

// State-machine seam permits safe failure/concurrency tests without a server.
// The concrete implementation below contains all SQL mutations.
type testLifecycleStore interface {
	marker(context.Context) (string, bool, error)
	check(context.Context) error
	create(context.Context) error
	migrate(context.Context, string) error
	mark(context.Context, string) error
	active(context.Context) (bool, error)
	drop(context.Context) error
}

var testMigrationNames = []string{
	"000001_init_extensions.up.sql",
	"000002_create_state_machine.up.sql",
	"000003_db_state_functions.up.sql",
	"000004_validate_tool_payload.up.sql",
	"000005_action_catalog.up.sql",
	"000006_universal_json_input.up.sql",
}

func TestLifecycleMigrationOrder(t *testing.T) {
	want := []string{
		"000001_init_extensions.up.sql",
		"000002_create_state_machine.up.sql",
		"000003_db_state_functions.up.sql",
		"000004_validate_tool_payload.up.sql",
		"000005_action_catalog.up.sql",
		"000006_universal_json_input.up.sql",
	}
	if !slices.Equal(testMigrationNames, want) {
		t.Fatalf("migration order = %v, want %v", testMigrationNames, want)
	}
}

func runTestLifecycle(ctx context.Context, command string, store testLifecycleStore) error {
	marker, exists, err := store.marker(ctx)
	if err != nil {
		return err
	}
	switch command {
	case "check", "gate":
		if !exists || marker != testReadyMarker {
			return errors.New("test lifecycle: ready marker missing; explicit prepare/reset required")
		}
		return store.check(ctx)
	case "prepare":
		if exists {
			if marker != testReadyMarker {
				return errors.New("test lifecycle: existing database is unowned or preparing; no automatic adoption/repair")
			}
			return store.check(ctx)
		}
	case "reset":
		if !exists || (marker != testReadyMarker && marker != testPreparingMarker) {
			return errors.New("test lifecycle: reset requires an existing lifecycle-owned database")
		}
		active, err := store.active(ctx)
		if err != nil {
			return err
		}
		if active {
			return errors.New("test lifecycle: reset refused while connections are active")
		}
		// Re-read immediately before DROP; all cooperating clients hold the lock.
		again, present, err := store.marker(ctx)
		if err != nil {
			return err
		}
		if !present || again != marker {
			return errors.New("test lifecycle: marker changed before reset")
		}
		if err := store.drop(ctx); err != nil {
			return err
		}
	default:
		return errors.New("test lifecycle: command not allowed")
	}
	if err := store.create(ctx); err != nil {
		return err
	}
	if err := store.mark(ctx, testPreparingMarker); err != nil {
		return err
	}
	for _, name := range testMigrationNames {
		if err := store.migrate(ctx, name); err != nil {
			return err
		}
	}
	if err := store.check(ctx); err != nil {
		return err
	}
	return store.mark(ctx, testReadyMarker)
}

type pgTestLifecycle struct {
	admin *pgx.Conn
	dsn   string
}

func (s *pgTestLifecycle) marker(ctx context.Context) (string, bool, error) {
	var marker string
	err := s.admin.QueryRow(ctx, `SELECT coalesce(shobj_description(oid,'pg_database'),'') FROM pg_database WHERE datname='petar_test'`).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, testDatabaseError("read lifecycle marker", err)
	}
	return marker, true, nil
}
func (s *pgTestLifecycle) create(ctx context.Context) error {
	_, err := s.admin.Exec(ctx, `CREATE DATABASE "petar_test" OWNER "petar_user"`)
	if err != nil {
		return testDatabaseError("create petar_test", err)
	}
	return nil
}
func (s *pgTestLifecycle) mark(ctx context.Context, marker string) error {
	// Only the two fixed marker constants are accepted, never user SQL.
	if marker != testReadyMarker && marker != testPreparingMarker {
		return errors.New("test lifecycle: invalid marker")
	}
	_, err := s.admin.Exec(ctx, `COMMENT ON DATABASE "petar_test" IS '`+marker+`'`)
	if err != nil {
		return testDatabaseError("update lifecycle marker", err)
	}
	return nil
}
func (s *pgTestLifecycle) connect(ctx context.Context) (*pgx.Conn, error) {
	cfg, err := testConnectionConfig(ctx, s.dsn)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, testDatabaseError("connect to petar_test", err)
	}
	if err := checkTestIdentity(ctx, conn, "petar_test"); err != nil {
		conn.Close(ctx)
		return nil, err
	}
	return conn, nil
}
func (s *pgTestLifecycle) check(ctx context.Context) error {
	conn, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return checkTestSchema(ctx, conn)
}
func (s *pgTestLifecycle) migrate(ctx context.Context, name string) error {
	allowed := false
	for _, expected := range testMigrationNames {
		if name == expected {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("test lifecycle: migration not allowed")
	}
	sql, err := os.ReadFile(filepath.Join(testProjectRoot(), "postgres", "migrations", name))
	if err != nil {
		return fmt.Errorf("test lifecycle: read migration %s: %w", name, err)
	}
	conn, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	// Simple protocol handles each complete existing migration as a script;
	// pgx returns the first SQL error, equivalent to psql ON_ERROR_STOP.
	if _, err := conn.Exec(ctx, string(sql), pgx.QueryExecModeSimpleProtocol); err != nil {
		return fmt.Errorf("test lifecycle: apply migration %s: %w", name, err)
	}
	return nil
}
func (s *pgTestLifecycle) active(ctx context.Context) (bool, error) {
	var active bool
	err := s.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname='petar_test')`).Scan(&active)
	if err != nil {
		return false, testDatabaseError("check active connections", err)
	}
	return active, nil
}
func (s *pgTestLifecycle) drop(ctx context.Context) error {
	// Plain DROP intentionally refuses new sessions racing the read-only check.
	_, err := s.admin.Exec(ctx, `DROP DATABASE "petar_test"`)
	if err != nil {
		return testDatabaseError("drop refused (database may have active connections)", err)
	}
	return nil
}

type fakeTestLifecycle struct {
	value   string
	exists  bool
	busy    bool
	fail    string
	changed bool
	events  []string
}

func (s *fakeTestLifecycle) event(name string) error {
	s.events = append(s.events, name)
	if name == s.fail {
		return errors.New("injected lifecycle failure")
	}
	return nil
}
func (s *fakeTestLifecycle) marker(context.Context) (string, bool, error) {
	err := s.event("marker")
	value := s.value
	if s.changed && len(s.events) > 2 {
		value = "unowned"
	}
	return value, s.exists, err
}
func (s *fakeTestLifecycle) check(context.Context) error { return s.event("check") }
func (s *fakeTestLifecycle) create(context.Context) error {
	if err := s.event("create"); err != nil {
		return err
	}
	s.exists = true
	return nil
}
func (s *fakeTestLifecycle) migrate(_ context.Context, name string) error { return s.event(name) }
func (s *fakeTestLifecycle) mark(_ context.Context, value string) error {
	if err := s.event(value); err != nil {
		return err
	}
	s.value = value
	return nil
}
func (s *fakeTestLifecycle) active(context.Context) (bool, error) { return s.busy, s.event("active") }
func (s *fakeTestLifecycle) drop(context.Context) error {
	if err := s.event("drop"); err != nil {
		return err
	}
	s.exists = false
	return nil
}

func TestLifecycleFreshPrepareAndRepeat(t *testing.T) {
	store := &fakeTestLifecycle{}
	if err := runTestLifecycle(context.Background(), "prepare", store); err != nil {
		t.Fatal(err)
	}
	expected := []string{"marker", "create", testPreparingMarker}
	expected = append(expected, testMigrationNames...)
	expected = append(expected, "check", testReadyMarker)
	if strings.Join(store.events, "\n") != strings.Join(expected, "\n") {
		t.Fatalf("events=%v want %v", store.events, expected)
	}
	store.events = nil
	if err := runTestLifecycle(context.Background(), "prepare", store); err != nil {
		t.Fatal(err)
	}
	if strings.Join(store.events, ",") != "marker,check" {
		t.Fatalf("repeat prepare mutated: %v", store.events)
	}
}

func TestLifecycleFailureNeverMarksReady(t *testing.T) {
	stages := []string{"marker", "create", testPreparingMarker}
	stages = append(stages, testMigrationNames...)
	stages = append(stages, "check", testReadyMarker)
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			store := &fakeTestLifecycle{fail: stage}
			if err := runTestLifecycle(context.Background(), "prepare", store); err == nil {
				t.Fatal("injected failure hidden")
			}
			if store.value == testReadyMarker {
				t.Fatalf("failed prepare marked ready: %v", store.events)
			}
			for _, event := range store.events {
				if event == "drop" {
					t.Fatal("failed prepare silently dropped database")
				}
			}
		})
	}
}

func TestLifecycleUnownedAndPreparingRefused(t *testing.T) {
	for _, marker := range []string{"", testPreparingMarker, "petar:test-db:lifecycle=v2;state=ready"} {
		for _, command := range []string{"prepare", "check", "gate"} {
			t.Run(command+"/"+marker, func(t *testing.T) {
				store := &fakeTestLifecycle{exists: true, value: marker}
				if err := runTestLifecycle(context.Background(), command, store); err == nil {
					t.Fatal("unready database accepted")
				}
				if strings.Join(store.events, ",") != "marker" {
					t.Fatalf("unready target mutated: %v", store.events)
				}
			})
		}
	}
}

func TestLifecycleResetFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store fakeTestLifecycle
	}{
		{"absent", fakeTestLifecycle{}},
		{"unowned", fakeTestLifecycle{exists: true}},
		{"unknown-marker", fakeTestLifecycle{exists: true, value: "foreign"}},
		{"active", fakeTestLifecycle{exists: true, value: testReadyMarker, busy: true}},
		{"marker-race", fakeTestLifecycle{exists: true, value: testReadyMarker, changed: true}},
		{"activity-failure", fakeTestLifecycle{exists: true, value: testReadyMarker, fail: "active"}},
		{"plain-drop-failure", fakeTestLifecycle{exists: true, value: testReadyMarker, fail: "drop"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runTestLifecycle(context.Background(), "reset", &tc.store); err == nil {
				t.Fatal("unsafe reset succeeded")
			}
			for _, event := range tc.store.events {
				if event == "create" || strings.HasSuffix(event, ".sql") || event == testPreparingMarker {
					t.Fatalf("unsafe reset mutated: %v", tc.store.events)
				}
				if event == "drop" && tc.store.fail != "drop" {
					t.Fatalf("unsafe drop: %v", tc.store.events)
				}
			}
		})
	}
	for _, marker := range []string{testReadyMarker, testPreparingMarker} {
		store := &fakeTestLifecycle{exists: true, value: marker}
		if err := runTestLifecycle(context.Background(), "reset", store); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(strings.Join(store.events, ","), "marker,active,marker,drop,create,") {
			t.Fatalf("reset ordering: %v", store.events)
		}
	}
}

func TestLifecycleCheckRejectsSchemaFailure(t *testing.T) {
	store := &fakeTestLifecycle{exists: true, value: testReadyMarker, fail: "check"}
	if err := runTestLifecycle(context.Background(), "check", store); err == nil {
		t.Fatal("missing schema accepted")
	}
	if strings.Join(store.events, ",") != "marker,check" {
		t.Fatalf("check mutated database: %v", store.events)
	}
}

func TestLifecycleLockChild(t *testing.T) {
	if os.Getenv("PETAR_LIFECYCLE_LOCK_CHILD") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	lock, err := acquireTestLock(ctx)
	if os.Getenv("PETAR_LIFECYCLE_LOCK_EXPECT") == "blocked" {
		if err == nil {
			lock.Close()
			t.Fatal("second open-file description bypassed lock")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want bounded wait: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleLockInheritanceAndContention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	lock, err := acquireTestLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	run := func(inherit bool) {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleLockChild$", "-test.count=1")
		cmd.Env = append(testChildEnvironment(), "PETAR_LIFECYCLE_LOCK_CHILD=1")
		if inherit {
			cmd.Env = append(cmd.Env, "PETAR_TEST_LOCK_FD=3")
			cmd.ExtraFiles = []*os.File{lock}
		} else {
			cmd.Env = append(cmd.Env, "PETAR_LIFECYCLE_LOCK_EXPECT=blocked")
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("lock child inherit=%v: %v: %s", inherit, err, out)
		}
	}
	run(true)
	// Closing the child's duplicate must not unlock the parent's description.
	run(false)
	if _, inherited := os.LookupEnv("PETAR_TEST_LOCK_FD"); !inherited && os.Getenv("PETAR_TEST_LOCK_SOCKET") == "" {
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
		released, err := acquireTestLock(ctx)
		if err != nil {
			t.Fatalf("lock not released after close: %v", err)
		}
		released.Close()
	}
}

func TestLifecycleGateChildSanitizesEnvironment(t *testing.T) {
	t.Setenv("PETAR_TEST_DB_COMMAND", "reset")
	t.Setenv("PETAR_TEST_DB_CONFIRM", "petar_test")
	t.Setenv("PETAR_TEST_LOCK_FD", "99")
	for _, item := range testChildEnvironment() {
		key, _, _ := strings.Cut(item, "=")
		if key == "PETAR_TEST_DB_COMMAND" || key == "PETAR_TEST_DB_CONFIRM" || key == "PETAR_TEST_LOCK_FD" || key == "PETAR_TEST_LOCK_SOCKET" || key == "PETAR_TEST_LOCK_TOKEN" {
			t.Fatal("lifecycle command leaked into child")
		}
	}
}

func TestLifecycleDockerReadOnlyIdentityAndApp(t *testing.T) {
	for _, tc := range []struct {
		name, app, identity string
		fail                bool
	}{
		{"normal", "petar_db", "123456", false},
		{"app-test", "petar_test", "123456", true},
		{"wrong-cluster", "petar_db", "different", true},
		{"malformed-app", "malformed", "123456", true},
		{"docker-failure", "failure", "123456", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "docker.log")
			script := `#!/bin/bash
set -eu
echo "$*" >> "$FAKE_DOCKER_LOG"
case "$*" in
  'inspect petar_postgres') echo '[{"Name":"/petar_postgres","State":{"Running":true}}]' ;;
  exec*) echo "$FAKE_CLUSTER" ;;
  container*) echo petar_app ;;
  'inspect petar_app')
    if [[ $FAKE_APP == failure ]]; then exit 7; fi
    if [[ $FAKE_APP == malformed ]]; then echo '{}'; exit; fi
    echo '[{"Name":"/petar_app","State":{"Running":true},"Config":{"Env":["DB_DSN=postgres://petar_user:private-secret@postgres:5432/'"$FAKE_APP"'?sslmode=disable"]}}]'
    ;;
  *) exit 9 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			t.Setenv("FAKE_DOCKER_LOG", log)
			t.Setenv("FAKE_CLUSTER", tc.identity)
			t.Setenv("FAKE_APP", tc.app)
			err := checkTestDockerConfiguration(context.Background(), "123456")
			if (err != nil) != tc.fail {
				t.Fatalf("inspection error=%v want fail=%v", err, tc.fail)
			}
			if err != nil && strings.Contains(err.Error(), "private-secret") {
				t.Fatal("app credentials leaked")
			}
			if tc.name == "docker-failure" {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("Docker subprocess cause discarded: %v", err)
				}
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				if !strings.HasPrefix(line, "inspect ") && !strings.HasPrefix(line, "container ls ") && !strings.HasPrefix(line, "exec petar_postgres psql ") {
					t.Fatalf("non-read-only Docker operation: %s", line)
				}
				if strings.Contains(line, "DROP") || strings.Contains(line, "CREATE") || strings.Contains(line, "ALTER") {
					t.Fatalf("Docker SQL mutation: %s", line)
				}
			}
		})
	}
}

func TestLifecycleGateChildFailureAndLock(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "child.log")
	script := `#!/bin/bash
set -eu
[[ -z ${PETAR_TEST_DB_COMMAND+x} && -z ${PETAR_TEST_DB_CONFIRM+x} ]]
[[ -z ${PETAR_TEST_LOCK_FD+x} && -n $PETAR_TEST_LOCK_SOCKET && -n $PETAR_TEST_LOCK_TOKEN && -e /proc/self/fd/3 ]]
echo "$*" >> "$FAKE_GATE_LOG"
exit "${FAKE_GATE_EXIT:-0}"
`
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_GATE_LOG", log)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lock, err := acquireTestLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	t.Setenv("PETAR_TEST_DB_COMMAND", "reset")
	t.Setenv("PETAR_TEST_DB_CONFIRM", "petar_test")
	for _, stage := range []string{"vet", "test"} {
		t.Setenv("FAKE_GATE_EXIT", "0")
		if err := runTestGateChild(ctx, lock, []string{stage, "./..."}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("FAKE_GATE_EXIT", "17")
		if err := runTestGateChild(ctx, lock, []string{stage, "./..."}); err == nil {
			t.Fatal("child failure hidden")
		}
	}
}

func TestLifecycleGateCancellationKillsLockHoldingDescendants(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "ready")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/bash
set -eu
# The descendant inherits FD 3; killing only the launcher leaks the lock.
sleep 60 &
printf R > "$FAKE_GATE_READY"
wait
`
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_GATE_READY", fifo)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lock, err := acquireTestLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ready := make(chan error, 1)
	go func() {
		f, err := os.Open(fifo)
		if err != nil {
			ready <- err
			return
		}
		defer f.Close()
		var b [1]byte
		_, err = f.Read(b[:])
		ready <- err
	}()
	done := make(chan error, 1)
	go func() { done <- runTestGateChild(ctx, lock, []string{"test", "./..."}) }()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("fake child did not reach readiness barrier")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation hidden")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gate descendants survived cancellation")
	}
	if os.Getenv("PETAR_TEST_LOCK_FD") == "" && os.Getenv("PETAR_TEST_LOCK_SOCKET") == "" {
		lock.Close()
		checkCtx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		released, err := acquireTestLock(checkCtx)
		if err != nil {
			t.Fatalf("descendant retained lock after cancellation: %v", err)
		}
		released.Close()
	}
}

func TestLifecycleRealGoRunnerPreservesLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lock, err := acquireTestLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	t.Setenv("PETAR_LIFECYCLE_LOCK_CHILD", "1")
	if err := runTestGateChild(ctx, lock, []string{"test", "-p=1", "-count=1", "-run=^TestLifecycleLockChild$", "./internal/db"}); err != nil {
		t.Fatalf("real go test did not preserve gate lock ownership: %v", err)
	}
}

func TestLifecycleMissingRelayFailsClosed(t *testing.T) {
	t.Setenv("PETAR_TEST_LOCK_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	t.Setenv("PETAR_TEST_LOCK_TOKEN", "not-a-real-token")
	t.Setenv("PETAR_TEST_LOCK_FD", "")
	os.Unsetenv("PETAR_TEST_LOCK_FD")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lock, err := acquireTestLock(ctx)
	if err == nil {
		lock.Close()
		t.Fatal("missing relay fell back to standalone lock acquisition")
	}
	if !strings.Contains(err.Error(), "relay") {
		t.Fatalf("missing relay did not fail closed at relay boundary: %v", err)
	}
}

func unsetLifecycleEnvironment(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func setLifecycleRelayEnvironment(t *testing.T, relay *testLockRelay) {
	t.Helper()
	unsetLifecycleEnvironment(t, "PETAR_TEST_LOCK_FD")
	t.Setenv("PETAR_TEST_LOCK_SOCKET", relay.socket)
	t.Setenv("PETAR_TEST_LOCK_TOKEN", relay.token)
}

func TestLifecycleRelayAuthenticationAndDescriptorValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lock, err := acquireTestLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	unlocked, err := os.OpenFile(testLockPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer unlocked.Close()
	foreign, err := os.CreateTemp(t.TempDir(), "foreign")
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	for _, tc := range []struct {
		name       string
		file       *os.File
		wrongToken bool
		ok         bool
	}{
		{"valid", lock, false, true},
		{"wrong-token", lock, true, false},
		{"foreign-descriptor", foreign, false, false},
		{"unlocked-canonical-description", unlocked, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			relay, err := startTestLockRelay(ctx, tc.file)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := relay.close(ctx); err != nil {
					t.Errorf("close test relay: %v", err)
				}
			}()
			setLifecycleRelayEnvironment(t, relay)
			if tc.wrongToken {
				t.Setenv("PETAR_TEST_LOCK_TOKEN", strings.Repeat("z", 64))
			}
			got, err := acquireTestLock(ctx)
			if got != nil {
				got.Close()
			}
			if (err == nil) != tc.ok {
				t.Fatalf("descriptor accepted=%v want %v: %v", err == nil, tc.ok, err)
			}
		})
	}
}

func TestLifecycleRelayCleanupAndRetainedDescriptor(t *testing.T) {
	inherited := os.Getenv("PETAR_TEST_LOCK_FD") != "" || os.Getenv("PETAR_TEST_LOCK_SOCKET") != "" || testGuardState.lock != nil
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lock, err := acquireTestLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	relay, err := startTestLockRelay(ctx, lock)
	if err != nil {
		t.Fatal(err)
	}
	setLifecycleRelayEnvironment(t, relay)
	dirInfo, err := os.Stat(filepath.Dir(relay.socket))
	if err != nil {
		t.Fatalf("stat relay directory: %v", err)
	}
	if dirInfo.Mode().Perm() != 0700 {
		t.Fatal("relay directory not private")
	}
	socketInfo, err := os.Stat(relay.socket)
	if err != nil {
		t.Fatalf("stat relay socket: %v", err)
	}
	if socketInfo.Mode().Perm() != 0600 {
		t.Fatal("relay socket not private")
	}
	received, err := acquireTestLock(ctx)
	if err != nil {
		t.Fatal(errors.Join(err, relay.close(ctx)))
	}
	defer received.Close()
	if err := relay.close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(relay.socket)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("relay directory survived cleanup: %v", err)
	}
	if got, err := acquireTestLock(ctx); err == nil {
		got.Close()
		t.Fatal("closed relay silently fell back to standalone lock")
	}
	for _, key := range []string{"PETAR_TEST_LOCK_FD", "PETAR_TEST_LOCK_SOCKET", "PETAR_TEST_LOCK_TOKEN"} {
		unsetLifecycleEnvironment(t, key)
	}
	lock.Close()
	if testGuardState.lock == nil {
		blockCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
		defer stop()
		if got, err := acquireTestLock(blockCtx); err == nil {
			got.Close()
			t.Fatal("relay cleanup released client's lock prematurely")
		}
	}
	received.Close()
	if !inherited {
		got, err := acquireTestLock(ctx)
		if err != nil {
			t.Fatalf("received descriptor leaked after close: %v", err)
		}
		got.Close()
	}
}

func TestLifecycleRelayCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lock, err := acquireTestLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	relay, err := startTestLockRelay(ctx, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := relay.close(ctx); err != nil {
			t.Errorf("close canceled test relay: %v", err)
		}
	}()
	conn, err := net.Dial("unix", relay.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// A connected client that never authenticates must not block shutdown.
	cancel()
	done := make(chan struct{})
	go func() {
		relay.cancel()
		relay.recordError(ctx, closeTestRelayListener(ctx, relay.listener))
		<-relay.done
		relay.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay workers survived cancellation")
	}
}

func TestLifecycleLazyBinaryLockReuse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lock, err := acquireTestLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	original := testGuardState.lock
	testGuardState.lock = lock
	defer func() { testGuardState.lock = original }()
	for _, key := range []string{"PETAR_TEST_LOCK_FD", "PETAR_TEST_LOCK_SOCKET", "PETAR_TEST_LOCK_TOKEN"} {
		unsetLifecycleEnvironment(t, key)
	}
	duplicate, err := acquireTestLock(ctx)
	if err != nil {
		t.Fatalf("same-binary lazy lock self-deadlocked: %v", err)
	}
	if duplicate.Fd() == lock.Fd() {
		t.Fatal("lazy helper returned owner's descriptor rather than a duplicate")
	}
	duplicate.Close()
	if _, err := lock.Stat(); err != nil {
		t.Fatal("duplicate cleanup closed lazy lock owner")
	}
}

func TestLifecycleLockErrorsPreserveCauses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	t.Run("closed-inherited-descriptor", func(t *testing.T) {
		t.Setenv("PETAR_TEST_LOCK_FD", "999999")
		lock, err := acquireTestLock(ctx)
		if lock != nil {
			lock.Close()
		}
		if !errors.Is(err, syscall.EBADF) {
			t.Fatalf("inherited fstat cause discarded: %v", err)
		}
	})
	t.Run("missing-relay-socket", func(t *testing.T) {
		dir, err := os.MkdirTemp("/opt/go-pet/postgres", ".petar-test-lock-relay-cause-")
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := removeTestRelayPath(ctx, dir); err != nil {
				t.Error(err)
			}
		}()
		unsetLifecycleEnvironment(t, "PETAR_TEST_LOCK_FD")
		t.Setenv("PETAR_TEST_LOCK_SOCKET", filepath.Join(dir, "missing.sock"))
		t.Setenv("PETAR_TEST_LOCK_TOKEN", strings.Repeat("a", 64))
		lock, err := acquireTestLock(ctx)
		if lock != nil {
			lock.Close()
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("relay lstat cause discarded: %v", err)
		}
	})
}

package spool

// This file is deliberately self-contained: api and spool use the same test-only
// guard, without introducing a production package or changing application config.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testLockPath = "/opt/go-pet/postgres/.petar-test-db.lock"
const testReadyMarker = "petar:test-db:lifecycle=v2;state=ready"
const testPreparingMarker = "petar:test-db:lifecycle=v2;state=preparing"

// Driver errors may contain connection strings. Preserve their causal chain for
// errors.Is/As without exposing credentials through Error or ordinary %w logs.
type testRedactedCause struct{ cause error }

func (e testRedactedCause) Error() string { return "driver error (details redacted)" }
func (e testRedactedCause) Unwrap() error { return e.cause }

func testDatabaseError(operation string, err error) error {
	return fmt.Errorf("test database %s: %w", operation, testRedactedCause{cause: err})
}

var testGuardState struct {
	sync.Once
	lock *os.File
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if testGuardState.lock != nil {
		if err := testGuardState.lock.Close(); err != nil && code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func requireTestDSN(ctx context.Context, t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dsn, err := testDSN(ctx)
	if err != nil {
		t.Fatalf("test database guard: %v", err)
	}
	testGuardState.Do(func() { testGuardState.lock, testGuardState.err = acquireTestLock(ctx) })
	if testGuardState.err != nil {
		t.Fatalf("test database guard: %v", testGuardState.err)
	}
	if err := checkTestDatabase(ctx, dsn); err != nil {
		t.Fatalf("test database guard: %v", err)
	}
	return dsn
}

func testDSN(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("test DSN context: %w", err)
	}
	dsn := os.Getenv("PETAR_TEST_DB_DSN")
	if strings.TrimSpace(dsn) == "" {
		return "", errors.New("PETAR_TEST_DB_DSN is required; no DB_DSN fallback")
	}
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") && value != "" {
			return "", errors.New("test DSN: PostgreSQL environment overrides are not allowed")
		}
	}
	allowed := map[string]bool{"host": true, "port": true, "dbname": true, "user": true, "password": true, "sslmode": true, "connect_timeout": true}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", testDatabaseError("parse DSN URL", err)
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return "", testDatabaseError("parse DSN query", err)
		}
		for key, values := range q {
			if !allowed[key] || len(values) != 1 {
				return "", errors.New("test DSN: unsupported or ambiguous parameter")
			}
		}
		if u.Fragment != "" {
			return "", errors.New("test DSN: fragments are not allowed")
		}
	} else {
		// Driver parsing below remains authoritative. This conservative lexical
		// allowlist also rejects service settings consumed (and hidden) by pgconn.
		keys := regexp.MustCompile(`(?:^|\s)([A-Za-z_][A-Za-z_0-9]*)\s*=`).FindAllStringSubmatch(dsn, -1)
		seen := map[string]bool{}
		for _, match := range keys {
			if !allowed[match[1]] || seen[match[1]] {
				return "", errors.New("test DSN: unsupported or repeated parameter")
			}
			seen[match[1]] = true
		}
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return "", testDatabaseError("parse DSN configuration", err)
	}
	c := cfg.ConnConfig
	if c.Host != "127.0.0.1" || c.Port != 5432 || c.Database != "petar_test" || c.User != "petar_user" {
		return "", errors.New("test DSN: only petar_user at 127.0.0.1:5432/petar_test is allowed")
	}
	if len(c.Fallbacks) != 0 || len(c.RuntimeParams) != 0 || c.TLSConfig != nil {
		return "", errors.New("test DSN: require sslmode=disable and no fallback hosts or runtime options")
	}
	return dsn, nil
}

func acquireTestLock(ctx context.Context) (result *os.File, resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	fd, err := syscall.Open(testLockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("test lock: open canonical lock file: %w", err)
	}
	canonical := os.NewFile(uintptr(fd), testLockPath)
	defer func() {
		if result != canonical {
			if err := canonical.Close(); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("test lock: close canonical descriptor: %w", err))
			}
		}
		if resultErr != nil && result != nil {
			if err := result.Close(); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("test lock: close failed result: %w", err))
			}
			result = nil
		}
	}()
	info, err := canonical.Stat()
	if err != nil {
		return nil, fmt.Errorf("test lock: stat canonical descriptor: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("test lock: not a regular file")
	}
	raw, inherited := os.LookupEnv("PETAR_TEST_LOCK_FD")
	socket, relay := os.LookupEnv("PETAR_TEST_LOCK_SOCKET")
	if !inherited && (relay || os.Getenv("PETAR_TEST_LOCK_TOKEN") != "") {
		received, err := receiveTestLock(ctx, socket)
		if err != nil {
			return nil, err
		}
		defer func() {
			if err := received.Close(); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("test lock: close received descriptor: %w", err))
			}
		}()
		raw, inherited = strconv.Itoa(int(received.Fd())), true
	}
	// Later test-only lifecycle/lock probes in the same direct test binary must
	// share its lazy fixture lock, not wait on an independent flock it owns.
	// Explicit inherited/relay errors above still fail closed.
	if !inherited && testGuardState.lock != nil {
		raw, inherited = strconv.Itoa(int(testGuardState.lock.Fd())), true
	}
	if inherited {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, testDatabaseError("parse inherited lock descriptor", err)
		}
		if n < 3 {
			return nil, errors.New("test lock: invalid inherited descriptor")
		}
		var stat syscall.Stat_t
		if err := syscall.Fstat(n, &stat); err != nil {
			return nil, fmt.Errorf("test lock: stat inherited descriptor: %w", err)
		}
		want := info.Sys().(*syscall.Stat_t)
		if stat.Dev != want.Dev || stat.Ino != want.Ino {
			return nil, errors.New("test lock: inherited descriptor is not canonical")
		}
		// A second open-file description must be blocked before inherited flock
		// succeeds. This rejects a merely open (but unlocked) canonical descriptor.
		if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
				return nil, fmt.Errorf("test lock: release ownership probe: %w", err)
			}
			return nil, errors.New("test lock: inherited descriptor was not locked")
		} else if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("test lock: verify ownership: %w", err)
		}
		if err := syscall.Flock(n, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return nil, fmt.Errorf("test lock: verify inherited exclusive lock: %w", err)
		}
		dup, err := syscall.Dup(n)
		if err != nil {
			return nil, fmt.Errorf("test lock: duplicate inherited descriptor: %w", err)
		}
		syscall.CloseOnExec(dup)
		return os.NewFile(uintptr(dup), testLockPath), nil
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("test lock: %w", err)
		}
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return canonical, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("test lock: acquire: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("test lock: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func receiveTestLock(ctx context.Context, socket string) (result *os.File, resultErr error) {
	token := os.Getenv("PETAR_TEST_LOCK_TOKEN")
	if len(token) != 64 || !strings.HasPrefix(socket, "/opt/go-pet/postgres/") {
		return nil, errors.New("test lock relay: invalid endpoint configuration")
	}
	directory, err := os.Lstat(filepath.Dir(socket))
	if err != nil {
		return nil, fmt.Errorf("test lock relay: stat endpoint directory: %w", err)
	}
	if !directory.IsDir() || directory.Mode().Perm() != 0700 || directory.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return nil, errors.New("test lock relay: endpoint directory is not private")
	}
	info, err := os.Lstat(socket)
	if err != nil {
		return nil, fmt.Errorf("test lock relay: stat endpoint: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return nil, errors.New("test lock relay: endpoint is unavailable or unsafe")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("test lock relay: connect: %w", err)
	}
	defer func() {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("test lock relay: close connection: %w", err))
		}
		if resultErr != nil && result != nil {
			if err := result.Close(); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("test lock relay: close failed descriptor: %w", err))
			}
			result = nil
		}
	}()
	cancelDone := make(chan struct{})
	var cancelCloseErr error
	stop := context.AfterFunc(ctx, func() { defer close(cancelDone); cancelCloseErr = conn.Close() })
	defer func() {
		if !stop() {
			<-cancelDone
			if cancelCloseErr != nil && !errors.Is(cancelCloseErr, net.ErrClosed) {
				resultErr = errors.Join(resultErr, fmt.Errorf("test lock relay: cancel connection: %w", cancelCloseErr))
			}
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("test lock relay: set deadline: %w", err)
	}
	if n, err := conn.Write([]byte(token)); err != nil {
		return nil, fmt.Errorf("test lock relay: write authentication: %w", err)
	} else if n != len(token) {
		return nil, fmt.Errorf("test lock relay: write authentication: %w", io.ErrShortWrite)
	}
	unix := conn.(*net.UnixConn)
	data := make([]byte, 1)
	control := make([]byte, syscall.CmsgSpace(4)*4)
	n, oobn, flags, _, err := unix.ReadMsgUnix(data, control)
	// Parse and close every received FD even on malformed/truncated messages.
	var fds []int
	messages, parseErr := syscall.ParseSocketControlMessage(control[:oobn])
	for _, message := range messages {
		rights, e := syscall.ParseUnixRights(&message)
		if e != nil {
			parseErr = errors.Join(parseErr, fmt.Errorf("test lock relay: parse descriptor rights: %w", e))
			continue
		}
		for _, fd := range rights {
			syscall.CloseOnExec(fd)
		}
		fds = append(fds, rights...)
	}
	if err != nil || parseErr != nil || flags&syscall.MSG_CTRUNC != 0 || n != 1 || data[0] != 'L' || len(fds) != 1 {
		failure := errors.New("test lock relay: authenticated descriptor transfer failed")
		if err != nil {
			failure = fmt.Errorf("test lock relay: read descriptor message: %w", err)
		}
		if parseErr != nil {
			failure = errors.Join(failure, fmt.Errorf("test lock relay: parse control message: %w", parseErr))
		}
		for _, fd := range fds {
			if err := syscall.Close(fd); err != nil {
				failure = errors.Join(failure, fmt.Errorf("test lock relay: close rejected descriptor: %w", err))
			}
		}
		return nil, failure
	}
	return os.NewFile(uintptr(fds[0]), testLockPath), nil
}

func testConnectionConfig(ctx context.Context, dsn string) (*pgx.ConnConfig, error) {
	validated, err := testDSN(ctx)
	if err != nil {
		return nil, err
	}
	if dsn != validated {
		return nil, errors.New("test connection: DSN differs from validated target")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, testDatabaseError("parse connection configuration", err)
	}
	cfg.ConnectTimeout = 5 * time.Second
	return cfg, nil
}

func checkTestDatabase(ctx context.Context, dsn string) error {
	cfg, err := testConnectionConfig(ctx, dsn)
	if err != nil {
		return err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return testDatabaseError("unavailable; run scripts/test-db.sh prepare explicitly", err)
	}
	defer conn.Close(context.Background())
	if err := checkTestIdentity(ctx, conn, "petar_test"); err != nil {
		return err
	}
	if err := checkTestDocker(ctx, conn); err != nil {
		return err
	}
	var ready bool
	if err := conn.QueryRow(ctx, `SELECT coalesce(shobj_description(oid,'pg_database'),'')=$1 FROM pg_database WHERE datname=current_database()`, testReadyMarker).Scan(&ready); err != nil {
		return testDatabaseError("read ready marker", err)
	}
	if !ready {
		return errors.New("test database: lifecycle ready marker missing; explicit prepare/reset required")
	}
	return checkTestSchema(ctx, conn)
}

func checkTestIdentity(ctx context.Context, conn *pgx.Conn, database string) error {
	var name, user string
	var version int
	if err := conn.QueryRow(ctx, `SELECT current_database(),current_user,current_setting('server_version_num')::int`).Scan(&name, &user, &version); err != nil {
		return testDatabaseError("query identity", err)
	}
	if name != database || user != "petar_user" || version < 180000 || version >= 190000 {
		return errors.New("test database: unexpected database, user, or PostgreSQL version")
	}
	return nil
}

func checkTestSchema(ctx context.Context, conn *pgx.Conn) error {
	var valid bool
	err := conn.QueryRow(ctx, `SELECT
	(SELECT count(*)=6 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind='r' AND c.relname IN ('sm_inputs','sm_processes','sm_jobs','sm_job_logs','sm_outputs','sm_actions'))
	AND (SELECT count(*)=3 FROM pg_extension WHERE extname IN ('vector','file_fdw','pg_trgm'))
	AND EXISTS (SELECT FROM public.sm_actions WHERE action='word-count' AND version=1)
	AND EXISTS (SELECT FROM pg_constraint WHERE conrelid=to_regclass('public.sm_jobs')
	  AND conname='sm_jobs_tool_name_check' AND contype='c' AND convalidated
	  AND strpos(pg_get_constraintdef(oid), $$'word-count'$$)>0)
	AND to_regprocedure('public.sm_create_input(text,jsonb)') IS NULL
	AND to_regprocedure('public.sm_create_action(text,integer,jsonb)') IS NOT NULL
	AND EXISTS (SELECT FROM information_schema.columns
	  WHERE table_schema='public' AND table_name='sm_jobs' AND column_name='action_version'
	    AND data_type='integer' AND is_nullable='NO' AND column_default='1')
	AND to_regprocedure('public.sm_validate_tool_payload(jsonb)') IS NOT NULL
	AND to_regprocedure('public.sm_pick_next_job()') IS NOT NULL
	AND to_regprocedure('public.sm_pick_next_job_with_version()') IS NOT NULL
	AND to_regprocedure('public.sm_update_job_status(uuid,text,text,text)') IS NOT NULL
	AND (SELECT count(*)=3 FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace
	  WHERE n.nspname='public' AND NOT t.tgisinternal AND t.tgenabled IN ('O','A')
	  AND ((c.relname='sm_inputs' AND t.tgname='trg_sm_inputs_after_insert')
	    OR (c.relname='sm_jobs' AND t.tgname IN ('trg_sm_jobs_notify','trg_sm_jobs_status_audit'))))`).Scan(&valid)
	if err != nil {
		return testDatabaseError("query required schema objects", err)
	}
	if !valid {
		return errors.New("test database: required 000008 schema objects missing or legacy input still present")
	}
	return nil
}

// Read-only Docker checks bind the local TCP server to the expected container
// using PostgreSQL's cluster identifier, rather than trusting a container name.
func checkTestDocker(ctx context.Context, admin *pgx.Conn) error {
	var identifier string
	if err := admin.QueryRow(ctx, `SELECT system_identifier::text FROM pg_control_system()`).Scan(&identifier); err != nil {
		return testDatabaseError("verify local cluster identity", err)
	}
	return checkTestDockerConfiguration(ctx, identifier)
}

func checkTestDockerConfiguration(ctx context.Context, identifier string) error {
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		childCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(childCtx, "docker", args...).Output()
		if err != nil {
			return nil, testDatabaseError("required Docker read-only inspection", err)
		}
		return out, nil
	}
	out, err := run(ctx, "inspect", "petar_postgres")
	if err != nil {
		return err
	}
	var containers []struct {
		Name  string
		State struct{ Running bool }
	}
	if err := json.Unmarshal(out, &containers); err != nil {
		return testDatabaseError("decode PostgreSQL container inspection", err)
	}
	if len(containers) != 1 || containers[0].Name != "/petar_postgres" || !containers[0].State.Running {
		return errors.New("test lifecycle: petar_postgres is not running")
	}
	out, err = run(ctx, "exec", "petar_postgres", "psql", "-X", "-A", "-t", "-U", "petar_user", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-c", "SELECT system_identifier FROM pg_control_system()")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != identifier || identifier == "" {
		return errors.New("test lifecycle: local server does not match petar_postgres")
	}
	out, err = run(ctx, "container", "ls", "--all", "--filter", "name=^/petar_app$", "--format", "{{.Names}}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil
	}
	if strings.TrimSpace(string(out)) != "petar_app" {
		return errors.New("test lifecycle: ambiguous app identity")
	}
	out, err = run(ctx, "inspect", "petar_app")
	if err != nil {
		return err
	}
	var apps []struct {
		Name   string
		State  struct{ Running bool }
		Config struct{ Env []string }
	}
	if err := json.Unmarshal(out, &apps); err != nil {
		return testDatabaseError("decode app container inspection", err)
	}
	if len(apps) != 1 || apps[0].Name != "/petar_app" {
		return errors.New("test lifecycle: app inspection malformed")
	}
	if !apps[0].State.Running {
		return nil
	}
	var appDSN string
	for _, item := range apps[0].Config.Env {
		key, value, _ := strings.Cut(item, "=")
		if key == "DB_DSN" {
			if appDSN != "" {
				return errors.New("test lifecycle: ambiguous app configuration")
			}
			appDSN = value
		}
		if strings.HasPrefix(key, "PG") && value != "" {
			return errors.New("test lifecycle: app PostgreSQL overrides are ambiguous")
		}
	}
	if appDSN == "" {
		return errors.New("test lifecycle: running app has no explicit DB_DSN")
	}
	cfg, err := pgx.ParseConfig(appDSN)
	if err != nil {
		return testDatabaseError("parse running app configuration", err)
	}
	if cfg.Database == "petar_test" {
		return errors.New("test lifecycle: running app targets petar_test")
	}
	if cfg.Database != "petar_db" || len(cfg.Fallbacks) > 0 || len(cfg.RuntimeParams) > 0 {
		return errors.New("test lifecycle: running app database configuration is ambiguous")
	}
	return nil
}

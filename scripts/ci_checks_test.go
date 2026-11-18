package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCIChecksStopsOnPrepareFailure(t *testing.T) {
	calls, output, err := runCIChecks(t, "17", "0")
	if calls != "prepare "+ciDSN+"\n" || err == nil {
		t.Fatalf("want one prepare call and nonzero exit; calls=%q error=%v\n%s", calls, err, output)
	}
}

const ciDSN = "postgres://ci@127.0.0.1/petar_test"

func TestCIChecksRunsGateBeforeBuild(t *testing.T) {
	calls, output, err := runCIChecks(t, "0", "0")
	want := "prepare " + ciDSN + "\ncheck " + ciDSN + "\ngate " + ciDSN + " -count=1\ngo build ./...\n"
	if calls != want || err != nil {
		t.Fatalf("want calls %q; got %q error=%v\n%s", want, calls, err, output)
	}
}

func TestCIChecksStopsOnGateFailure(t *testing.T) {
	calls, output, err := runCIChecks(t, "0", "19")
	want := "prepare " + ciDSN + "\ncheck " + ciDSN + "\ngate " + ciDSN + " -count=1\n"
	if calls != want || err == nil {
		t.Fatalf("want calls %q and nonzero exit; got %q error=%v\n%s", want, calls, err, output)
	}
}

func runCIChecks(t *testing.T, prepareExit, gateExit string) (string, string, error) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"scripts", "bin"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	entrypoint, err := os.ReadFile("ci-checks.sh")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	files := map[string]string{
		"scripts/test-db.sh":      "#!/bin/bash\nprintf '%s %s\\n' \"$1\" \"$PETAR_TEST_DB_DSN\" >> \"$CALLS\"\n[[ $1 != prepare ]] || exit \"$PREPARE_EXIT\"\n",
		"scripts/quality-gate.sh": "#!/bin/bash\nprintf 'gate %s %s\\n' \"$PETAR_TEST_DB_DSN\" \"$GOFLAGS\" >> \"$CALLS\"\nexit \"$GATE_EXIT\"\n",
		"bin/go":                  "#!/bin/bash\nprintf 'go %s\\n' \"$*\" >> \"$CALLS\"\n",
	}
	if err == nil {
		files["scripts/ci-checks.sh"] = string(entrypoint)
	}
	for path, data := range files {
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(root, "calls")
	cmd := exec.Command("bash", filepath.Join(root, "scripts", "ci-checks.sh"))
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"),
		"CALLS="+logPath, "PREPARE_EXIT="+prepareExit, "GATE_EXIT="+gateExit,
		"PETAR_TEST_DB_DSN="+ciDSN, "GOFLAGS=-mod=readonly")
	output, runErr := cmd.CombinedOutput()
	calls, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(calls), strings.TrimSpace(string(output)), runErr
}

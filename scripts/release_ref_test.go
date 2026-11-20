package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseRefResolvesAnnotatedTagAndRejectsMovedTag(t *testing.T) {
	root, commit := releaseTree(t, nil)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("update-ref", "HEAD", commit)
	git("remote", "add", "origin", ".")
	git("-c", "user.name=Release Test", "-c", "user.email=release@example.invalid",
		"tag", "-a", "v0.1.0", "-m", "candidate")
	tag := git("rev-parse", "refs/tags/v0.1.0")
	git("update-ref", "refs/tags/v0.2.0", commit)
	script, err := filepath.Abs("check-release-ref.sh")
	if err != nil {
		t.Fatal(err)
	}
	check := func(event, name, sha string) (string, error) {
		cmd := exec.Command("bash", script)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GITHUB_EVENT_NAME="+event, "GITHUB_REF_TYPE=tag",
			"GITHUB_REF_NAME="+name, "GITHUB_SHA="+sha)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	for _, tc := range []struct {
		name, sha string
	}{
		{"v0.1.0", tag},
		{"v0.1.0", commit},
		{"v0.2.0", commit},
	} {
		if out, err := check("push", tc.name, tc.sha); err != nil || out != commit {
			t.Fatalf("tag did not resolve to commit: %q: %v", out, err)
		}
	}
	if out, err := check("pull_request", "v0.1.0", tag); err == nil {
		t.Fatalf("non-push event accepted: %q", out)
	}
	if out, err := check("push", "v0.1.0-rc.1", tag); err == nil {
		t.Fatalf("unexpected tag accepted: %q", out)
	}
	other := git("-c", "user.name=Release Test", "-c", "user.email=release@example.invalid",
		"commit-tree", git("write-tree"), "-p", commit, "-m", "different commit")
	git("update-ref", "refs/tags/v0.1.0", other)
	if out, err := check("push", "v0.1.0", tag); err == nil || !strings.Contains(out, "does not match") {
		t.Fatalf("moved tag was not rejected: %q: %v", out, err)
	}
}

package scripts_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type releaseFile struct {
	mode string
	body string
}

func releaseTree(t *testing.T, edit func(map[string]releaseFile)) (root, sha string) {
	t.Helper()
	root = t.TempDir()
	files := map[string]releaseFile{
		"scripts/spool-runner.sh":                  {"100755", "#!/bin/sh\n"},
		"postgres/bootstrap.sh":                    {"100755", "#!/bin/sh\n"},
		"postgres/postgresql.conf":                 {"100644", "port = 5432\n"},
		"postgres/migrations/000001_test.up.sql":   {"100644", "SELECT 1;\n"},
		"postgres/migrations/000001_test.down.sql": {"100644", "SELECT 2;\n"},
		"postgres/data/do-not-package":             {"100644", "not for release"},
	}
	if edit != nil {
		edit(files)
	}
	git := func(input string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("", "init", "-q")
	for name, file := range files {
		id := git(file.body, "hash-object", "-w", "--stdin")
		git("", "update-index", "--add", "--cacheinfo", file.mode+","+id+","+name)
	}
	tree := git("", "write-tree")
	sha = git("Release test\n", "-c", "user.name=Release Test", "-c", "user.email=release@example.invalid",
		"commit-tree", tree)
	return root, sha
}

func runBundle(t *testing.T, repo string, args ...string) (string, error) {
	t.Helper()
	script, err := filepath.Abs("release-bundle.py")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", append([]string{script}, args...)...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestReleaseBundleUsesDeterministicGitBytes(t *testing.T) {
	repo, sha := releaseTree(t, nil)
	if err := os.Mkdir(filepath.Join(repo, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scripts/spool-runner.sh"), []byte("uncommitted"), 0600); err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(t.TempDir(), "a.tgz"), filepath.Join(t.TempDir(), "b.tgz")
	for _, out := range []string{a, b} {
		if msg, err := runBundle(t, repo, "pack", sha, out); err != nil {
			t.Fatalf("pack: %s: %v", msg, err)
		}
		if msg, err := runBundle(t, repo, "verify", sha, out); err != nil {
			t.Fatalf("verify: %s: %v", msg, err)
		}
	}
	read := func(path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	data := read(a)
	if !bytes.Equal(data, read(b)) {
		t.Fatal("same SHA produced different bytes")
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	found := map[string]string{}
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		found[h.Name] = fmt.Sprintf("%o:%s", h.Mode, body)
	}
	if len(found) != 5 || found["scripts/spool-runner.sh"] != "755:#!/bin/sh\n" ||
		found["postgres/bootstrap.sh"] != "755:#!/bin/sh\n" ||
		found["postgres/postgresql.conf"] != "644:port = 5432\n" ||
		found["postgres/migrations/000001_test.up.sql"] != "644:SELECT 1;\n" ||
		found["postgres/migrations/000001_test.down.sql"] != "644:SELECT 2;\n" {
		t.Fatalf("unexpected archive: %#v", found)
	}
}

func TestReleaseBundleRejectsBadTree(t *testing.T) {
	for _, tc := range []struct {
		name, diagnostic string
		edit             func(map[string]releaseFile)
	}{
		{"missing pair", "unpaired", func(m map[string]releaseFile) {
			delete(m, "postgres/migrations/000001_test.down.sql")
		}},
		{"extra migration file", "unexpected migration", func(m map[string]releaseFile) {
			m["postgres/migrations/README.md"] = releaseFile{"100644", "extra"}
		}},
		{"wrong mode", "mode", func(m map[string]releaseFile) {
			m["scripts/spool-runner.sh"] = releaseFile{"100644", "#!/bin/sh\n"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, sha := releaseTree(t, tc.edit)
			path := filepath.Join(t.TempDir(), "bad.tgz")
			msg, err := runBundle(t, repo, "pack", sha, path)
			if err == nil || !strings.Contains(msg, tc.diagnostic) {
				t.Fatalf("expected %q rejection: %s: %v", tc.diagnostic, msg, err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid tree generated archive: %v", err)
			}
		})
	}
}

func TestReleaseBundleRejectsArchiveWithExtraMemberAndCorruption(t *testing.T) {
	repo, sha := releaseTree(t, nil)
	dir := t.TempDir()
	path := filepath.Join(dir, "release.tgz")
	if msg, err := runBundle(t, repo, "pack", sha, path); err != nil {
		t.Fatalf("pack: %s: %v", msg, err)
	}
	checksum, err := exec.Command("sha256sum", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), checksum, 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	input, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var output bytes.Buffer
	compressed := gzip.NewWriter(&output)
	archive := tar.NewWriter(compressed)
	reader := tar.NewReader(input)
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := archive.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(archive, reader); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.WriteHeader(&tar.Header{Name: "extra", Mode: 0644, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, output.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if msg, err := runBundle(t, repo, "verify", sha, path); err == nil || !strings.Contains(msg, "unexpected archive member") {
		t.Fatalf("expected extra member rejection: %s: %v", msg, err)
	}
	data[len(data)/2] ^= 1
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("sha256sum", "-c", filepath.Join(dir, "SHA256SUMS")).Run(); err == nil {
		t.Fatal("corrupted archive passed SHA-256 check")
	}
}

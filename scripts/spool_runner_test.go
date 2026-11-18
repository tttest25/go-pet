package scripts_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const runnerID = "019a0000-0000-7000-8000-000000000001"

type runnerFixture struct {
	t                *testing.T
	root, bin, spool string
	env              []string
}

func newRunnerFixture(t *testing.T) *runnerFixture {
	t.Helper()
	f := &runnerFixture{t: t, root: t.TempDir()}
	f.bin, f.spool = filepath.Join(f.root, "bin"), filepath.Join(f.root, "spool")
	for _, dir := range []string{f.bin, filepath.Join(f.spool, "in")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"bash", "jq", "basename", "tail", "mkdir", "mv", "rm", "flock", "timeout", "sleep", "cat"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(f.bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	f.env = []string{"PATH=" + f.bin, "SPOOL_DIR=" + f.spool, "FIXTURE=" + f.root, "API_BASE_URL=http://fixture.invalid/api/v1"}
	f.program("curl", fakeRunnerCurl)
	return f
}

const fakeRunnerCurl = `
shopt -s nullglob
posts=("$FIXTURE"/post-*.json)
n=${#posts[@]}
printf '%s\n' "$@" > "$FIXTURE/curl-$n"
sink=
format=
while (($#)); do
  case "$1" in
    -d) printf '%s' "$2" > "$FIXTURE/post-$n.json"; shift;;
    -o) sink=$2; shift;;
    -w) format=$2; shift;;
    http://*) printf '%s' "$1" > "$FIXTURE/url-$n";;
  esac
  shift
done
code=204
rc=0
if [[ $n == "${FAULT_POST:-none}" ]]; then
  code=$FAULT_CODE
  rc=$FAULT_EXIT
  if [[ -n $sink ]]; then printf response-secret > "$sink"; else printf response-secret; fi
  printf transport-secret >&2
fi
printf '%b' "${format//'%{http_code}'/$code}"
exit "$rc"
`

func (f *runnerFixture) write(path, contents string, mode os.FileMode) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *runnerFixture) program(name, body string) {
	f.write(filepath.Join(f.bin, name), "#!/usr/bin/env bash\nset -eu\n"+body, 0700)
}

func (f *runnerFixture) job(id, tool, payload string) {
	f.write(filepath.Join(f.spool, "in", id+".json"), `{"id":"`+id+`","tool_name":"`+tool+`","payload":`+payload+`}`, 0600)
}

func (f *runnerFixture) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, filepath.Join(f.bin, "bash"), append([]string{"./spool-runner.sh"}, args...)...)
	cmd.Env = f.env
	cmd.WaitDelay = time.Second
	return cmd
}

func (f *runnerFixture) run() (string, error) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := f.command(ctx).CombinedOutput()
	return string(out), errors.Join(ctx.Err(), err)
}

func (f *runnerFixture) read(path string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, path))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *runnerFixture) posts() []map[string]string {
	f.t.Helper()
	files, err := filepath.Glob(filepath.Join(f.root, "post-*.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	posts := make([]map[string]string, len(files))
	for i, path := range files {
		if err := json.Unmarshal([]byte(f.read(filepath.Base(path))), &posts[i]); err != nil {
			f.t.Fatal(err)
		}
	}
	return posts
}

func (f *runnerFixture) absent(path string) {
	f.t.Helper()
	if _, err := os.Stat(filepath.Join(f.root, path)); !os.IsNotExist(err) {
		f.t.Errorf("%s: expected absent, stat error = %v", path, err)
	}
}

func TestRunnerMissingCLI(t *testing.T) {
	t.Run("yt-dlp", func(t *testing.T) {
		checkRunnerMissingCLI(t, "yt-dlp", `{"tool":"yt-dlp","url":"https://example.org/v"}`)
	})
	t.Run("fabric", func(t *testing.T) {
		checkRunnerMissingCLI(t, "fabric", `{"tool":"fabric","text":"hello"}`)
	})
}

func checkRunnerMissingCLI(t *testing.T, tool, payload string) {
	f := newRunnerFixture(t)
	f.job(runnerID, tool, payload)
	out, err := f.run()
	if err != nil {
		t.Fatalf("confirmed failure should exit normally: %v\n%s", err, out)
	}
	posts := f.posts()
	if len(posts) != 2 || posts[0]["status"] != "in_progress" || posts[1]["status"] != "failed" {
		t.Fatalf("want in_progress → failed; got %v", posts)
	}
	if !strings.Contains(posts[1]["error_msg"], tool) || !strings.Contains(posts[1]["error_msg"], "not found") {
		t.Errorf("missing CLI not diagnosed: %v", posts[1])
	}
	f.absent("spool/out/" + runnerID + "/result.json")
	f.absent("spool/processing/" + runnerID + ".json")
}

func TestRunnerCallback(t *testing.T) {
	for _, stage := range []string{"in_progress", "completed", "failed"} {
		for _, fault := range []struct{ name, code, exit string }{
			{"confirmed", "204", "0"},
			{"200", "200", "0"},
			{"redirect", "302", "0"},
			{"500", "500", "0"},
			{"transport", "000", "28"},
			{"204-with-transport-error", "204", "18"},
		} {
			t.Run(stage+"/"+fault.name, func(t *testing.T) {
				f := newRunnerFixture(t)
				f.job(runnerID, "yt-dlp", `{"tool":"yt-dlp","url":"https://example.org/v"}`)
				toolExit, index := "0", "1"
				if stage == "failed" {
					toolExit = "9"
				}
				if stage == "in_progress" {
					index = "0"
				}
				f.program("yt-dlp", `printf 'run\n' >> "$FIXTURE/runs"; printf '{"ok":true}'; exit `+toolExit)
				f.env = append(f.env, "FAULT_POST="+index, "FAULT_CODE="+fault.code, "FAULT_EXIT="+fault.exit)

				out, err := f.run()
				confirmed := fault.name == "confirmed"
				if confirmed && err != nil || !confirmed && err == nil || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("callback %s at %s: exit = %v\n%s", fault.name, stage, err, out)
				}
				if !confirmed && (!strings.Contains(out, runnerID) || !strings.Contains(out, stage) || !strings.Contains(out, "check the database")) {
					t.Errorf("missing reconciliation diagnostic: %s", out)
				}
				if strings.Contains(out, "response-secret") || strings.Contains(out, "transport-secret") {
					t.Errorf("curl output leaked: %s", out)
				}
				posts := f.posts()
				want := []string{"in_progress"}
				ran := confirmed || stage != "in_progress"
				if ran {
					terminal := "completed"
					if stage == "failed" {
						terminal = "failed"
					}
					want = append(want, terminal)
				}
				if len(posts) != len(want) {
					t.Fatalf("posts = %v, want statuses %v", posts, want)
				}
				for i, status := range want {
					if posts[i]["status"] != status {
						t.Errorf("POST %d = %v, want %s", i, posts[i], status)
					}
					checkRunnerCurl(t, f, i)
				}
				if confirmed {
					f.absent("spool/processing/" + runnerID + ".json")
				} else {
					f.read("spool/processing/" + runnerID + ".json")
					if out, err := f.run(); err != nil || len(f.posts()) != len(want) {
						t.Fatalf("ordinary rerun should not retry callbacks: %v\n%s", err, out)
					}
				}
				if ran {
					if runs := f.read("runs"); runs != "run\n" {
						t.Errorf("CLI runs = %q", runs)
					}
					if result := f.read("spool/out/" + runnerID + "/result.json"); result != `{"ok":true}` {
						t.Errorf("result not preserved: %q", result)
					}
				} else {
					f.absent("runs")
					f.absent("spool/out/" + runnerID)
				}
			})
		}
	}
}

func checkRunnerCurl(t *testing.T, f *runnerFixture, i int) {
	t.Helper()
	index := strconv.Itoa(i)
	if url := f.read("url-" + index); url != "http://fixture.invalid/api/v1/jobs/"+runnerID+"/status" {
		t.Errorf("callback URL = %q", url)
	}
	args := strings.Split(strings.TrimSuffix(f.read("curl-"+index), "\n"), "\n")
	bounds := map[string]bool{"--connect-timeout": false, "--max-time": false}
	for j, arg := range args {
		switch arg {
		case "--connect-timeout", "--max-time":
			if j+1 < len(args) {
				n, err := strconv.Atoi(args[j+1])
				bounds[arg] = err == nil && n > 0
			}
		case "-L", "--location", "--location-trusted", "--retry", "--retry-all-errors":
			t.Errorf("unexpected retry/redirect option %s", arg)
		}
	}
	for flag, ok := range bounds {
		if !ok {
			t.Errorf("missing finite positive %s: %v", flag, args)
		}
	}
}

func TestRunnerToolExecution(t *testing.T) {
	const url = "https://example.org/v?x=url-secret&y=2"
	const text = "-n \"text-secret\"\nsecond line\n\n"
	for _, tc := range []struct {
		name, tool, payload, stdin, artifact, stdout string
		args                                         []string
		fails                                        bool
	}{
		{"echo-default", "echo-test", `{"tool":"echo-test"}`, "", "result.json", "default echo", nil, false},
		{"echo-message", "echo-test", `{"tool":"echo-test","message":"hello\n\n"}`, "", "result.json", "hello\n\n", nil, false},
		{"yt-dlp", "yt-dlp", `{"tool":"yt-dlp","url":"` + url + `"}`, "", "result.json", `{"captured":true}`, []string{"--dump-json", "--", url}, false},
		{"fabric", "fabric", `{"tool":"fabric","text":` + strconv.Quote(text) + `,"pattern":"custom-1"}`, text, "output.md", "# stdout\n", []string{"--pattern", "custom-1"}, false},
		{"fabric-default", "fabric", `{"tool":"fabric","text":` + strconv.Quote(text) + `}`, text, "output.md", "# stdout\n", []string{"--pattern", "summarize"}, false},
		{"fabric-nonzero", "fabric", `{"tool":"fabric","text":` + strconv.Quote(text) + `}`, text, "output.md", "# partial\n", []string{"--pattern", "summarize"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunnerFixture(t)
			f.job(runnerID, tc.tool, tc.payload)
			resultDir := "spool/out/" + runnerID + "/"
			if tc.args != nil {
				body := `printf '%s\0' "$@" > "$FIXTURE/argv"` + "\n"
				if tc.stdin != "" {
					body += `cat > "$FIXTURE/stdin"` + "\n"
				}
				body += `printf '%s' "$FAKE_STDOUT"; printf 'stderr-secret\n' >&2` + "\n"
				if tc.fails {
					body += "exit 7\n"
				}
				f.program(tc.tool, body)
				f.env = append(f.env, "FAKE_STDOUT="+tc.stdout)
			}

			out, err := f.run()
			if err != nil {
				t.Fatalf("runner exit = %v\n%s", err, out)
			}
			posts := f.posts()
			if len(posts) != 2 || posts[0]["status"] != "in_progress" {
				t.Fatalf("unexpected posts: %v", posts)
			}
			if tc.fails {
				if posts[1]["status"] != "failed" || !strings.Contains(posts[1]["error_msg"], "code 7") {
					t.Errorf("expected brief CLI failure, got %v", posts[1])
				}
				f.absent(resultDir + "result.json")
			} else if posts[1]["status"] != "completed" || posts[1]["result_path"] != "/spool/out/"+runnerID+"/result.json" {
				t.Errorf("expected completed with legacy result path, got %v", posts[1])
			}
			for _, secret := range []string{"text-secret", "url-secret", "stderr-secret"} {
				if strings.Contains(out, secret) || strings.Contains(posts[1]["error_msg"], secret) {
					t.Errorf("secret %q leaked into callback/logger", secret)
				}
			}
			if tc.args == nil {
				var result map[string]string
				if err := json.Unmarshal([]byte(f.read(resultDir+"result.json")), &result); err != nil || result["echo"] != tc.stdout || result["status"] != "ok" {
					t.Errorf("unexpected echo result: %v (%v)", result, err)
				}
			} else {
				if got := strings.Split(strings.TrimSuffix(f.read("argv"), "\x00"), "\x00"); !slices.Equal(got, tc.args) {
					t.Errorf("argv = %q, want %q", got, tc.args)
				}
				if tc.stdin != "" {
					if got := f.read("stdin"); got != tc.stdin {
						t.Errorf("stdin = %q, want %q", got, tc.stdin)
					}
				}
				if got := f.read(resultDir + tc.artifact); got != tc.stdout {
					t.Errorf("stdout = %q, want %q", got, tc.stdout)
				}
				if got := f.read(resultDir + "stderr.log"); got != "stderr-secret\n" {
					t.Errorf("stderr = %q", got)
				}
				if tc.tool == "fabric" && !tc.fails {
					var result map[string]string
					if err := json.Unmarshal([]byte(f.read(resultDir+"result.json")), &result); err != nil ||
						result["pattern"] != tc.args[1] || result["output_path"] != filepath.Join(f.root, resultDir, "output.md") {
						t.Errorf("unexpected Fabric result: %v (%v)", result, err)
					}
				}
			}
			f.absent("spool/processing/" + runnerID + ".json")
		})
	}
}

func TestRunnerToolTimeout(t *testing.T) {
	for _, ignoreTERM := range []bool{false, true} {
		t.Run("ignore-TERM-"+strconv.FormatBool(ignoreTERM), func(t *testing.T) {
			f := newRunnerFixture(t)
			f.job(runnerID, "yt-dlp", `{"tool":"yt-dlp","url":"https://example.org/v"}`)
			f.env = append(f.env, "RUNNER_TOOL_TIMEOUT=1")
			body := ""
			if ignoreTERM {
				body = "trap '' TERM\n"
			}
			f.program("yt-dlp", body+`
printf '%s' "$$" > "$FIXTURE/tool-pid"
printf 'partial stdout\n'
printf 'partial stderr\n' >&2
exec sleep 60
`)
			out, err := f.run()
			if errors.Is(err, context.DeadlineExceeded) {
				if b, readErr := os.ReadFile(filepath.Join(f.root, "tool-pid")); readErr == nil {
					if pid, convErr := strconv.Atoi(string(b)); convErr == nil {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			}
			if err != nil {
				t.Fatalf("expected confirmed timeout failure before external deadline: %v\n%s", err, out)
			}
			posts := f.posts()
			if len(posts) != 2 || posts[0]["status"] != "in_progress" || posts[1]["status"] != "failed" ||
				!strings.Contains(posts[1]["error_msg"], "timed out") {
				t.Fatalf("unexpected timeout callbacks: %v", posts)
			}
			dir := "spool/out/" + runnerID + "/"
			if f.read(dir+"result.json") != "partial stdout\n" || f.read(dir+"stderr.log") != "partial stderr\n" {
				t.Error("partial output was not preserved")
			}
			f.absent("spool/processing/" + runnerID + ".json")
		})
	}
	for _, invalid := range []string{"0", "", "nope", "-1", "999999999999999999999999"} {
		t.Run("invalid-"+invalid, func(t *testing.T) {
			f := newRunnerFixture(t)
			f.job(runnerID, "echo-test", `{"tool":"echo-test"}`)
			f.env = append(f.env, "RUNNER_TOOL_TIMEOUT="+invalid)
			out, err := f.run()
			if err == nil || !strings.Contains(out, "RUNNER_TOOL_TIMEOUT") || len(f.posts()) != 0 {
				t.Fatalf("invalid timeout should fail before capture: exit = %v, log = %s", err, out)
			}
			f.read("spool/in/" + runnerID + ".json")
		})
	}
	t.Run("missing-utility", func(t *testing.T) {
		f := newRunnerFixture(t)
		if err := os.Remove(filepath.Join(f.bin, "timeout")); err != nil {
			t.Fatal(err)
		}
		f.job(runnerID, "echo-test", `{"tool":"echo-test"}`)
		out, err := f.run()
		if err == nil || !strings.Contains(out, "timeout") || len(f.posts()) != 0 {
			t.Fatalf("missing utility should fail before capture: exit = %v, log = %s", err, out)
		}
		f.read("spool/in/" + runnerID + ".json")
	})
}

func TestRunnerInvalidJob(t *testing.T) {
	for _, tc := range []struct{ name, document string }{
		{"malformed", `{"id":`},
		{"multi-document", `{"id":"` + runnerID + `"} {"id":"` + runnerID + `"}`},
		{"missing-id", `{"tool_name":"yt-dlp"}`},
		{"id-type", `{"id":42}`},
		{"noncanonical-id", `{"id":"019a0000000070008000000000000001"}`},
		{"id-newline", `{"id":"` + runnerID + `\n"}`},
		{"id-path", `{"id":"../escaped","tool_name":"echo-test","payload":{"tool":"echo-test"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunnerFixture(t)
			f.program("yt-dlp", `printf 'run' > "$FIXTURE/runs"`)
			f.write(filepath.Join(f.spool, "in", "original.json"), tc.document, 0600)
			out, err := f.run()
			if err == nil || errors.Is(err, context.DeadlineExceeded) || len(f.posts()) != 0 {
				t.Fatalf("invalid envelope: exit = %v, callbacks = %v\n%s", err, f.posts(), out)
			}
			if got := f.read("spool/processing/original.json"); got != tc.document {
				t.Errorf("retained document = %q, want %q", got, tc.document)
			}
			f.absent("spool/in/original.json")
			f.absent("runs")
			f.absent("spool/escaped")
			entries, err := os.ReadDir(filepath.Join(f.spool, "out"))
			if err != nil || len(entries) != 0 {
				t.Errorf("invalid envelope created output: %v (%v)", entries, err)
			}
		})
	}
	for _, tc := range []struct{ name, tool, payload string }{
		{"unsupported-tool", "other-secret", `{"tool":"other-secret"}`},
		{"tool-mismatch", "echo-test", `{"tool":"fabric","text":"abc"}`},
		{"tool-missing", "echo-test", `{}`},
		{"tool-wrong-type", "echo-test", `{"tool":false}`},
		{"payload-type", "echo-test", `[]`},
		{"message-number", "echo-test", `{"tool":"echo-test","message":123}`},
		{"message-null", "echo-test", `{"tool":"echo-test","message":null}`},
		{"message-blank", "echo-test", `{"tool":"echo-test","message":" \t\n"}`},
		{"text-absent", "fabric", `{"tool":"fabric"}`},
		{"text-null", "fabric", `{"tool":"fabric","text":null}`},
		{"text-boolean", "fabric", `{"tool":"fabric","text":true}`},
		{"text-blank", "fabric", `{"tool":"fabric","text":" \t\n"}`},
		{"pattern-null", "fabric", `{"tool":"fabric","text":"hello","pattern":null}`},
		{"pattern-number", "fabric", `{"tool":"fabric","text":"hello","pattern":42}`},
		{"pattern-empty", "fabric", `{"tool":"fabric","text":"hello","pattern":""}`},
		{"pattern-newline", "fabric", `{"tool":"fabric","text":"hello","pattern":"valid\n"}`},
		{"pattern-path", "fabric", `{"tool":"fabric","text":"hello","pattern":"../other-secret"}`},
		{"pattern-long", "fabric", `{"tool":"fabric","text":"hello","pattern":"` + strings.Repeat("x", 129) + `"}`},
		{"url-missing", "yt-dlp", `{"tool":"yt-dlp"}`},
		{"url-null", "yt-dlp", `{"tool":"yt-dlp","url":null}`},
		{"url-type", "yt-dlp", `{"tool":"yt-dlp","url":true}`},
		{"url-option", "yt-dlp", `{"tool":"yt-dlp","url":"--other-secret"}`},
		{"url-empty-host", "yt-dlp", `{"tool":"yt-dlp","url":"https:///other-secret"}`},
		{"url-port-only", "yt-dlp", `{"tool":"yt-dlp","url":"https://:443/path"}`},
		{"url-user-only", "yt-dlp", `{"tool":"yt-dlp","url":"https://user@/path"}`},
		{"url-space", "yt-dlp", `{"tool":"yt-dlp","url":"https://ex ample.org/other-secret"}`},
		{"url-trailing-newline", "yt-dlp", `{"tool":"yt-dlp","url":"https://example.org/v\n"}`},
		{"url-relative", "yt-dlp", `{"tool":"yt-dlp","url":"//example.org/v"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunnerFixture(t)
			const v4 = "557f79b1-1594-4399-9d6d-bdf9fbbc14dd"
			f.job(v4, tc.tool, tc.payload)
			for _, tool := range []string{"yt-dlp", "fabric"} {
				f.program(tool, `printf 'run' > "$FIXTURE/runs"`)
			}
			out, err := f.run()
			if err != nil {
				t.Fatalf("confirmed invalid payload should exit normally: %v\n%s", err, out)
			}
			posts := f.posts()
			if len(posts) != 2 || posts[0]["status"] != "in_progress" || posts[1]["status"] != "failed" || posts[1]["error_msg"] == "" {
				t.Fatalf("want in_progress → failed for invalid payload, got %v", posts)
			}
			if strings.Contains(out+posts[1]["error_msg"], "other-secret") {
				t.Errorf("untrusted payload leaked into diagnostic: %s %v", out, posts[1])
			}
			f.absent("runs")
			f.absent("spool/out/" + v4)
			f.absent("spool/processing/" + v4 + ".json")
		})
	}
}

func TestRunnerSpoolLock(t *testing.T) {
	t.Run("competing-process", func(t *testing.T) {
		f := newRunnerFixture(t)
		f.job(runnerID, "yt-dlp", `{"tool":"yt-dlp","url":"https://example.org/v"}`)
		f.program("yt-dlp", `
printf x >&3
IFS= read -r -u 4
printf '{}'
`)
		readyR, readyW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer readyR.Close()
		defer readyW.Close()
		releaseR, releaseW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer releaseR.Close()
		defer releaseW.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		owner := f.command(ctx, "--loop")
		owner.ExtraFiles = []*os.File{readyW, releaseR}
		if err := owner.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			cancel()
			releaseW.Close()
			if owner.ProcessState == nil {
				_ = owner.Wait()
			}
		}()
		readyW.Close()
		releaseR.Close()
		if err := readyR.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1)
		if _, err := io.ReadFull(readyR, buf); err != nil || string(buf) != "x" {
			t.Fatalf("owner did not reach CLI: %v", err)
		}

		const nextID = "019a0000-0000-7000-8000-000000000002"
		f.job(nextID, "echo-test", `{"tool":"echo-test"}`)
		out, err := f.run()
		if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(out, "spool lock") {
			t.Fatalf("contender should fail promptly on lock: exit = %v\n%s", err, out)
		}
		f.read("spool/in/" + nextID + ".json")
		if posts := f.posts(); len(posts) != 1 || posts[0]["status"] != "in_progress" {
			t.Fatalf("contender posted a callback: %v", posts)
		}
		cancel()
		if err := owner.Wait(); err == nil {
			t.Fatal("expected owner to stop on cancellation")
		}
		releaseW.Close()
		out, err = f.run()
		if err != nil {
			t.Fatalf("runner after owner exit: %v\n%s", err, out)
		}
		f.absent("spool/in/" + nextID + ".json")
		f.read("spool/out/" + nextID + "/result.json")
		f.read("spool/processing/" + runnerID + ".json")
	})

	t.Run("retained-file-collision", func(t *testing.T) {
		f := newRunnerFixture(t)
		f.job(runnerID, "echo-test", `{"tool":"echo-test"}`)
		dir := filepath.Join(f.spool, "processing")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		f.write(filepath.Join(dir, runnerID+".json"), "original retained job", 0600)
		out, err := f.run()
		if err == nil || len(f.posts()) != 0 {
			t.Fatalf("expected refusal to overwrite retained job: %v\n%s", err, out)
		}
		if got := f.read("spool/processing/" + runnerID + ".json"); got != "original retained job" {
			t.Errorf("retained job was overwritten: %q", got)
		}
		f.read("spool/in/" + runnerID + ".json")
	})
}

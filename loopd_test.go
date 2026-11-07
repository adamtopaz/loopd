package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeEngine writes a shell script that stands in for claude or codex.
func fakeEngine(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engine")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testWorker(t *testing.T, bin string) Worker {
	return Worker{
		Name: "w", Engine: "claude", Bin: bin, Dir: t.TempDir(), Prompt: "/supervisor",
		Every: Duration{time.Hour}, Timeout: Duration{time.Minute},
	}
}

func TestCommand(t *testing.T) {
	claude := command(Worker{Engine: "claude", Prompt: "/supervisor", Args: []string{"--allowedTools", "Read"}})
	want := []string{"claude", "-p", "/supervisor", "--output-format", "stream-json", "--verbose", "--allowedTools", "Read"}
	if !reflect.DeepEqual(claude, want) {
		t.Errorf("claude: got %q, want %q", claude, want)
	}
	codex := command(Worker{Engine: "codex", Bin: "/opt/codex", Prompt: "Use the researcher skill.", Args: []string{"-s", "workspace-write"}})
	want = []string{"/opt/codex", "exec", "--json", "-s", "workspace-write", "--", "Use the researcher skill."}
	if !reflect.DeepEqual(codex, want) {
		t.Errorf("codex: got %q, want %q", codex, want)
	}
}

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	config, err := loadConfig(writeConfig(t, `{"workers": [{"name": "supervisor", "engine": "claude", "dir": "`+dir+`", "prompt": "/supervisor", "every": "20m"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	worker := config.Workers[0]
	if worker.Every.Duration != 20*time.Minute || worker.Timeout.Duration != time.Hour {
		t.Errorf("every %s, timeout %s", worker.Every.Duration, worker.Timeout.Duration)
	}
	if home, _ := os.UserHomeDir(); config.Logs != filepath.Join(home, ".loopd/logs") {
		t.Errorf("logs %q", config.Logs)
	}

	bad := map[string]string{
		"unknown field":  `{"workers": [{"name": "a", "engine": "claude", "dir": "` + dir + `", "prompt": "p", "every": "1m", "evry": "1m"}]}`,
		"duplicate name": `{"workers": [{"name": "a", "engine": "claude", "dir": "` + dir + `", "prompt": "p", "every": "1m"}, {"name": "a", "engine": "codex", "dir": "` + dir + `", "prompt": "p", "every": "1m"}]}`,
		"bad engine":     `{"workers": [{"name": "a", "engine": "gpt", "dir": "` + dir + `", "prompt": "p", "every": "1m"}]}`,
		"bad name":       `{"workers": [{"name": "a/b", "engine": "claude", "dir": "` + dir + `", "prompt": "p", "every": "1m"}]}`,
		"missing dir":    `{"workers": [{"name": "a", "engine": "claude", "dir": "` + dir + `/nope", "prompt": "p", "every": "1m"}]}`,
		"no every":       `{"workers": [{"name": "a", "engine": "claude", "dir": "` + dir + `", "prompt": "p"}]}`,
		"bad duration":   `{"workers": [{"name": "a", "engine": "claude", "dir": "` + dir + `", "prompt": "p", "every": 60}]}`,
		"no workers":     `{"workers": []}`,
	}
	for name, text := range bad {
		if _, err := loadConfig(writeConfig(t, text)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestPassLogsAndSummary(t *testing.T) {
	bin := fakeEngine(t, `echo '{"type":"system","subtype":"init"}'
echo "worker=$LOOPD_WORKER extra=$EXTRA dir=$(pwd) args=$*" >&2
echo '{"type":"result","result":"Reviewed #29.\nMore detail.","total_cost_usd":1.234}'`)
	worker := testWorker(t, bin)
	worker.Env = map[string]string{"EXTRA": "yes"}
	logPath := filepath.Join(t.TempDir(), "w", "pass.jsonl")
	r := runPass(context.Background(), worker, logPath)
	if r.err != nil || r.exit != 0 || r.stopped != "" {
		t.Fatalf("result %+v", r)
	}
	if r.summary != "$1.23: Reviewed #29." {
		t.Errorf("summary %q", r.summary)
	}
	stderr, _ := os.ReadFile(strings.TrimSuffix(logPath, ".jsonl") + ".stderr")
	for _, want := range []string{"worker=w", "extra=yes", "dir=" + worker.Dir, "args=-p /supervisor --output-format stream-json --verbose"} {
		if !strings.Contains(string(stderr), want) {
			t.Errorf("stderr %q lacks %q", stderr, want)
		}
	}
}

func TestEmptyStderrIsRemovedAndExitCodeKept(t *testing.T) {
	worker := testWorker(t, fakeEngine(t, `exit 3`))
	logPath := filepath.Join(t.TempDir(), "pass.jsonl")
	r := runPass(context.Background(), worker, logPath)
	if r.exit != 3 || r.err != nil {
		t.Errorf("result %+v", r)
	}
	if _, err := os.Stat(strings.TrimSuffix(logPath, ".jsonl") + ".stderr"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("empty stderr file kept")
	}
}

func TestMissingBinary(t *testing.T) {
	worker := testWorker(t, filepath.Join(t.TempDir(), "nope"))
	if r := runPass(context.Background(), worker, filepath.Join(t.TempDir(), "pass.jsonl")); r.err == nil {
		t.Errorf("no error for a missing binary")
	}
}

func TestCodexSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pass.jsonl")
	os.WriteFile(path, []byte(`{"type":"thread.started"}
{"type":"item.completed","item":{"type":"agent_message","text":"first"}}
not json
{"type":"item.completed","item":{"type":"agent_message","text":"Opened PR #40.\nDetails."}}
{"type":"turn.completed","usage":{"input_tokens":1200,"cached_input_tokens":800,"output_tokens":300}}
`), 0o644)
	if got := summarize(path); got != "1200 tokens in, 300 out: Opened PR #40." {
		t.Errorf("summary %q", got)
	}
}

// waitGone waits for a process to disappear.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
	}
	t.Errorf("process %d still running", pid)
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if data, err := os.ReadFile(path); err == nil && strings.HasSuffix(string(data), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil {
				return pid
			}
		}
	}
	t.Fatalf("no pid in %s", path)
	return 0
}

func TestTimeoutStopsTheWholePass(t *testing.T) {
	grace = 500 * time.Millisecond
	pidFile := filepath.Join(t.TempDir(), "pid")
	// The script ignores SIGTERM, so loopd has to fall back to SIGKILL.
	worker := testWorker(t, fakeEngine(t, `trap '' TERM
sleep 1000 &
echo $! > "$PIDFILE"
sleep 1000`))
	worker.Env = map[string]string{"PIDFILE": pidFile}
	worker.Timeout = Duration{300 * time.Millisecond}
	start := time.Now()
	r := runPass(context.Background(), worker, filepath.Join(t.TempDir(), "pass.jsonl"))
	if r.stopped != "timed out" {
		t.Errorf("result %+v", r)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %s", elapsed)
	}
	waitGone(t, readPid(t, pidFile))
}

func TestLeftoverBackgroundProcessesAreKilled(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	worker := testWorker(t, fakeEngine(t, `sleep 1000 > /dev/null 2>&1 &
echo $! > "$PIDFILE"`))
	worker.Env = map[string]string{"PIDFILE": pidFile}
	if r := runPass(context.Background(), worker, filepath.Join(t.TempDir(), "pass.jsonl")); r.exit != 0 || r.err != nil {
		t.Fatalf("result %+v", r)
	}
	waitGone(t, readPid(t, pidFile))
}

type lines struct {
	sync.Mutex
	text []string
}

func (l *lines) logf(format string, args ...any) {
	l.Lock()
	defer l.Unlock()
	l.text = append(l.text, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (l *lines) count(prefix string) int {
	l.Lock()
	defer l.Unlock()
	n := 0
	for _, line := range l.text {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

func TestLoopRepeatsUntilStopped(t *testing.T) {
	grace = 500 * time.Millisecond
	worker := testWorker(t, fakeEngine(t, `echo '{"type":"result","result":"ok","total_cost_usd":0}'`))
	worker.Every = Duration{50 * time.Millisecond}
	var log lines
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		loop(ctx, worker, t.TempDir(), log.logf)
		close(finished)
	}()
	for deadline := time.Now().Add(3 * time.Second); log.count("w: pass finished") < 3; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("passes: %q", log.text)
		}
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("loop didn't stop")
	}
}

func TestStopDuringPass(t *testing.T) {
	grace = 500 * time.Millisecond
	worker := testWorker(t, fakeEngine(t, `sleep 1000`))
	var log lines
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		loop(ctx, worker, t.TempDir(), log.logf)
		close(finished)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("loop didn't stop")
	}
	if log.count("w: pass stopped") != 1 || log.count("w: next pass") != 0 {
		t.Errorf("log %q", log.text)
	}
}

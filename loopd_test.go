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
	claude = command(Worker{Engine: "claude", Prompt: "/researcher", Model: "claude-opus-5-5", Effort: "xhigh", Args: []string{"--debug"}})
	want = []string{"claude", "-p", "/researcher", "--output-format", "stream-json", "--verbose", "--model", "claude-opus-5-5", "--effort", "xhigh", "--debug"}
	if !reflect.DeepEqual(claude, want) {
		t.Errorf("claude with model: got %q, want %q", claude, want)
	}
	codex = command(Worker{Engine: "codex", Prompt: "go", Model: "gpt-5.5", Effort: "xhigh"})
	want = []string{"codex", "exec", "--json", "--model", "gpt-5.5", "-c", `model_reasoning_effort="xhigh"`, "--", "go"}
	if !reflect.DeepEqual(codex, want) {
		t.Errorf("codex with model: got %q, want %q", codex, want)
	}
}

func TestParseConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	dir := t.TempDir()
	config, err := parseConfig([]byte(`{"workers": [{"name": "supervisor", "engine": "claude", "dir": "` + dir + `", "prompt": "/supervisor", "every": "20m"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	worker := config.Workers[0]
	if worker.Every.Duration != 20*time.Minute || worker.Timeout.Duration != time.Hour {
		t.Errorf("every %s, timeout %s", worker.Every.Duration, worker.Timeout.Duration)
	}
	if config.Logs != "/state/loopd/logs" {
		t.Errorf("logs %q", config.Logs)
	}
	if config, err := parseConfig([]byte(`{"workers": []}`)); err != nil || len(config.Workers) != 0 {
		t.Errorf("an empty worker list should be allowed: %v", err)
	}

	bad := map[string]string{
		"unknown field":  `{"workers": [{"name": "a", "engine": "claude", "dir": "` + dir + `", "prompt": "p", "every": "1m", "evry": "1m"}]}`,
		"duplicate name": `{"workers": [{"name": "a", "engine": "claude", "dir": "` + dir + `", "prompt": "p", "every": "1m"}, {"name": "a", "engine": "codex", "dir": "` + dir + `", "prompt": "p", "every": "1m"}]}`,
		"bad engine":     `{"workers": [{"name": "a", "engine": "gpt", "dir": "` + dir + `", "prompt": "p", "every": "1m"}]}`,
		"bad name":       `{"workers": [{"name": "a/b", "engine": "claude", "dir": "` + dir + `", "prompt": "p", "every": "1m"}]}`,
		"missing dir":    `{"workers": [{"name": "a", "engine": "claude", "dir": "` + dir + `/nope", "prompt": "p", "every": "1m"}]}`,
		"no every":       `{"workers": [{"name": "a", "engine": "claude", "dir": "` + dir + `", "prompt": "p"}]}`,
		"bad duration":   `{"workers": [{"name": "a", "engine": "claude", "dir": "` + dir + `", "prompt": "p", "every": 60}]}`,
		"not json":       `{"workers": [`,
	}
	for name, text := range bad {
		if _, err := parseConfig([]byte(text)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestDefaultPaths(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
	if path, err := defaultConfigPath(); err != nil || path != "/xdg/config/loopd/config.json" {
		t.Errorf("config path %q, %v", path, err)
	}
	t.Setenv("XDG_STATE_HOME", "")
	if home, _ := os.UserHomeDir(); defaultLogs() != filepath.Join(home, ".local/state/loopd/logs") {
		t.Errorf("logs %q", defaultLogs())
	}
}

func TestPassLogsAndSummary(t *testing.T) {
	bin := fakeEngine(t, `echo '{"type":"system","subtype":"init","model":"claude-opus-5-5"}'
echo "worker=$LOOPD_WORKER extra=$EXTRA dir=$(pwd) args=$*" >&2
echo '{"type":"result","result":"Reviewed #29.\nMore detail.","total_cost_usd":1.234}'`)
	worker := testWorker(t, bin)
	worker.Env = map[string]string{"EXTRA": "yes"}
	logPath := filepath.Join(t.TempDir(), "w", "pass.jsonl")
	r := runPass(context.Background(), worker, logPath)
	if r.err != nil || r.exit != 0 || r.stopped != "" {
		t.Fatalf("result %+v", r)
	}
	if r.summary != "claude-opus-5-5 $1.23: Reviewed #29." {
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

func TestWorkRepeatsUntilStopped(t *testing.T) {
	grace = 500 * time.Millisecond
	worker := testWorker(t, fakeEngine(t, `echo '{"type":"result","result":"ok","total_cost_usd":0}'`))
	worker.Every = Duration{50 * time.Millisecond}
	var log lines
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		work(ctx, &spec{worker, t.TempDir()}, make(chan *spec, 1), log.logf)
		close(finished)
	}()
	log.waitFor(t, "w: pass finished", 3)
	cancel()
	waitClosed(t, finished)
}

func TestStopDuringPass(t *testing.T) {
	grace = 500 * time.Millisecond
	worker := testWorker(t, fakeEngine(t, `sleep 1000`))
	var log lines
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		work(ctx, &spec{worker, t.TempDir()}, make(chan *spec, 1), log.logf)
		close(finished)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	waitClosed(t, finished)
	if log.count("w: pass stopped") != 1 || log.count("w: next pass") != 0 {
		t.Errorf("log %q", log.text)
	}
}

func (l *lines) waitFor(t *testing.T, prefix string, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); l.count(prefix) < n; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			l.Lock()
			defer l.Unlock()
			t.Fatalf("wanted %d lines starting %q, log %q", n, prefix, l.text)
		}
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("didn't stop")
	}
}

// configFile writes a config whose workers all use the given fake engine.
type configFile struct {
	t    *testing.T
	path string
	dir  string
	bin  string
	logs string
}

func newConfigFile(t *testing.T, bin string) *configFile {
	dir := t.TempDir()
	return &configFile{t, filepath.Join(dir, "config.json"), dir, bin, filepath.Join(dir, "logs")}
}

// write sets the workers, given as name and every pairs, or writes raw text if given one string.
func (c *configFile) write(workers ...string) {
	c.t.Helper()
	text := workers[0]
	if len(workers) != 1 {
		var entries []string
		for i := 0; i < len(workers); i += 2 {
			entries = append(entries, fmt.Sprintf(`{"name": %q, "engine": "claude", "bin": %q, "dir": %q, "prompt": "p", "every": %q}`,
				workers[i], c.bin, c.dir, workers[i+1]))
		}
		text = fmt.Sprintf(`{"logs": %q, "workers": [%s]}`, c.logs, strings.Join(entries, ", "))
	}
	if err := os.WriteFile(c.path, []byte(text), 0o644); err != nil {
		c.t.Fatal(err)
	}
}

func startRun(t *testing.T, path string, log *lines) (cancel func(), finished chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	finished = make(chan struct{})
	go func() {
		if err := run(ctx, path, log.logf); err != nil {
			t.Error(err)
		}
		close(finished)
	}()
	return cancel, finished
}

func TestReload(t *testing.T) {
	grace, pollInterval = 500*time.Millisecond, 20*time.Millisecond
	config := newConfigFile(t, fakeEngine(t, `echo '{"type":"result","result":"ok","total_cost_usd":0}'`))
	config.write("a", "1h")
	var log lines
	cancel, finished := startRun(t, config.path, &log)
	log.waitFor(t, "a: next pass in 1h", 1)

	// Shortening every applies to the wait already under way, and a new worker starts.
	config.write("a", "50ms", "b", "1h")
	log.waitFor(t, "a: config changed", 1)
	log.waitFor(t, "a: pass finished", 3)
	log.waitFor(t, "b: pass finished", 1)

	// A broken config is ignored.
	config.write(`{"workers": [`)
	log.waitFor(t, "loopd: "+config.path+": unexpected EOF; keeping the previous config", 1)
	before := log.count("a: pass finished")
	log.waitFor(t, "a: pass finished", before+2)

	// Removing a worker stops it, and the others carry on.
	config.write("b", "1h", "c", "1h")
	log.waitFor(t, "a: removed from the config", 1)
	log.waitFor(t, "c: pass finished", 1)
	stopped := log.count("a: pass started")
	time.Sleep(200 * time.Millisecond)
	if log.count("a: pass started") != stopped || log.count("b: pass started") != 1 {
		t.Errorf("log %q", log.text)
	}
	cancel()
	waitClosed(t, finished)
}

func TestRemovedDuringPassFinishesIt(t *testing.T) {
	grace, pollInterval = 500*time.Millisecond, 20*time.Millisecond
	config := newConfigFile(t, fakeEngine(t, `sleep 0.5; echo '{"type":"result","result":"done","total_cost_usd":0}'`))
	config.write("a", "1h")
	var log lines
	cancel, finished := startRun(t, config.path, &log)
	log.waitFor(t, "a: pass started", 1)
	config.write(`{"workers": []}`)
	log.waitFor(t, "a: removed from the config", 1)
	if log.count("a: pass finished") != 1 || log.count("a: next pass") != 0 {
		t.Errorf("log %q", log.text)
	}

	// Added back once its loop has stopped, it gets a new loop.
	config.write("a", "1h")
	log.waitFor(t, "a: pass started", 2)
	cancel()
	waitClosed(t, finished)
}

func TestRunNeedsAValidConfigToStart(t *testing.T) {
	var log lines
	if err := run(context.Background(), filepath.Join(t.TempDir(), "missing.json"), log.logf); err == nil {
		t.Error("no error for a missing config")
	}
	config := newConfigFile(t, "claude")
	config.write(`{"workers": [`)
	if err := run(context.Background(), config.path, log.logf); err == nil {
		t.Error("no error for a broken config")
	}
}

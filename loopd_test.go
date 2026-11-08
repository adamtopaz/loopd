package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	r := runPass(context.Background(), worker, logPath, nil)
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
	r := runPass(context.Background(), worker, logPath, nil)
	if r.exit != 3 || r.err != nil {
		t.Errorf("result %+v", r)
	}
	if _, err := os.Stat(strings.TrimSuffix(logPath, ".jsonl") + ".stderr"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("empty stderr file kept")
	}
}

func TestMissingBinary(t *testing.T) {
	worker := testWorker(t, filepath.Join(t.TempDir(), "nope"))
	if r := runPass(context.Background(), worker, filepath.Join(t.TempDir(), "pass.jsonl"), nil); r.err == nil {
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
	r := runPass(context.Background(), worker, filepath.Join(t.TempDir(), "pass.jsonl"), nil)
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
	if r := runPass(context.Background(), worker, filepath.Join(t.TempDir(), "pass.jsonl"), nil); r.exit != 0 || r.err != nil {
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

func startWork(t *testing.T, worker Worker, log *lines) (*runner, func(), chan struct{}) {
	r := newRunner(&spec{worker, t.TempDir()}, newRegistry(), "")
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		r.work(ctx, log.logf)
		close(finished)
	}()
	return r, cancel, finished
}

func TestWorkRepeatsUntilStopped(t *testing.T) {
	grace = 500 * time.Millisecond
	worker := testWorker(t, fakeEngine(t, `echo '{"type":"result","result":"ok","total_cost_usd":0}'`))
	worker.Every = Duration{50 * time.Millisecond}
	var log lines
	_, cancel, finished := startWork(t, worker, &log)
	log.waitFor(t, "w: pass finished", 3)
	cancel()
	waitClosed(t, finished)
}

func TestStopDuringPass(t *testing.T) {
	grace = 500 * time.Millisecond
	worker := testWorker(t, fakeEngine(t, `sleep 1000`))
	var log lines
	_, cancel, finished := startWork(t, worker, &log)
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

// shortSocket is a socket path short enough for the kernel's limit on Unix socket names.
func shortSocket(t *testing.T) string {
	dir, err := os.MkdirTemp("/tmp", "ld")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

type testDaemon struct {
	t        *testing.T
	socket   string
	cancel   func()
	finished chan struct{}
}

func startDaemon(t *testing.T, path string, log *lines) *testDaemon {
	d := &testDaemon{t: t, socket: shortSocket(t), finished: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	go func() {
		if err := run(ctx, path, d.socket, log.logf); err != nil {
			t.Error(err)
		}
		close(d.finished)
	}()
	log.waitFor(t, "loopd: config", 1)
	return d
}

func (d *testDaemon) stop() {
	d.cancel()
	waitClosed(d.t, d.finished)
}

func (d *testDaemon) reload() ([]string, error) {
	var reply reloadReply
	err := call(d.socket, http.MethodPost, "/reload", &reply)
	return reply.Changes, err
}

func (d *testDaemon) control(worker, action string) error {
	return call(d.socket, http.MethodPost, "/workers/"+worker+"/"+action, nil)
}

func (d *testDaemon) status() daemonStatus {
	d.t.Helper()
	var status daemonStatus
	if err := call(d.socket, http.MethodGet, "/status", &status); err != nil {
		d.t.Fatal(err)
	}
	return status
}

func (d *testDaemon) worker(name string) workerStatus {
	d.t.Helper()
	for _, worker := range d.status().Workers {
		if worker.Name == name {
			return worker
		}
	}
	d.t.Fatalf("no worker %s in status", name)
	return workerStatus{}
}

// waitState waits for a worker to reach a state.
func (d *testDaemon) waitState(name, state string) workerStatus {
	d.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if worker := d.worker(name); worker.State == state {
			return worker
		} else if time.Now().After(deadline) {
			d.t.Fatalf("%s is %s, not %s", name, worker.State, state)
		}
	}
}

const quickPass = `echo '{"type":"result","result":"ok","total_cost_usd":0}'`

func TestReloadOnlyOnRequest(t *testing.T) {
	grace = 500 * time.Millisecond
	config := newConfigFile(t, fakeEngine(t, quickPass))
	config.write("a", "1h")
	var log lines
	d := startDaemon(t, config.path, &log)
	defer d.stop()
	log.waitFor(t, "a: next pass in 1h", 1)

	// Editing the file alone changes nothing.
	config.write("a", "50ms", "b", "1h")
	time.Sleep(200 * time.Millisecond)
	if log.count("a: config changed") != 0 || log.count("b: pass started") != 0 {
		t.Fatalf("the config changed without a reload: %q", log.text)
	}

	// Shortening every applies to the wait already under way, and a new worker starts.
	changes, err := d.reload()
	if err != nil || strings.Join(changes, ", ") != "changed a, added b" {
		t.Fatalf("reload: %q, %v", changes, err)
	}
	log.waitFor(t, "a: pass finished", 3)
	log.waitFor(t, "b: pass finished", 1)

	// A broken config is refused, with the reason, and the old one stays in force.
	config.write(`{"workers": [`)
	if _, err := d.reload(); err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("broken config: %v", err)
	}
	before := log.count("a: pass finished")
	log.waitFor(t, "a: pass finished", before+2)

	// Reloading an unchanged file reports no changes.
	config.write("a", "50ms", "b", "1h")
	if changes, err := d.reload(); err != nil || len(changes) != 0 {
		t.Errorf("unchanged reload: %q, %v", changes, err)
	}

	// Removing a worker stops it, and the others carry on.
	config.write("b", "1h", "c", "1h")
	if changes, err := d.reload(); err != nil || strings.Join(changes, ", ") != "added c, removed a" {
		t.Errorf("reload: %q, %v", changes, err)
	}
	log.waitFor(t, "a: removed from the config", 1)
	log.waitFor(t, "c: pass finished", 1)
	stopped := log.count("a: pass started")
	time.Sleep(200 * time.Millisecond)
	if log.count("a: pass started") != stopped || log.count("b: pass started") != 1 {
		t.Errorf("log %q", log.text)
	}
	names := []string{}
	for _, worker := range d.status().Workers {
		names = append(names, worker.Name)
	}
	if strings.Join(names, " ") != "b c" {
		t.Errorf("status lists %q", names)
	}
}

func TestRemovedDuringPassFinishesIt(t *testing.T) {
	grace = 500 * time.Millisecond
	config := newConfigFile(t, fakeEngine(t, `sleep 0.5; echo '{"type":"result","result":"done","total_cost_usd":0}'`))
	config.write("a", "1h")
	var log lines
	d := startDaemon(t, config.path, &log)
	defer d.stop()
	log.waitFor(t, "a: pass started", 1)
	config.write(`{"workers": []}`)
	if _, err := d.reload(); err != nil {
		t.Fatal(err)
	}
	if worker := d.worker("a"); !worker.Removed || worker.State != "running" {
		t.Errorf("status during the last pass: %+v", worker)
	}
	log.waitFor(t, "a: removed from the config", 1)
	if log.count("a: pass finished") != 1 || log.count("a: next pass") != 0 {
		t.Errorf("log %q", log.text)
	}

	// Added back once its loop has stopped, it gets a new loop.
	config.write("a", "1h")
	if _, err := d.reload(); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, "a: pass started", 2)
}

func TestControls(t *testing.T) {
	grace = 500 * time.Millisecond
	config := newConfigFile(t, fakeEngine(t, quickPass))
	config.write("a", "1h")
	var log lines
	d := startDaemon(t, config.path, &log)
	defer d.stop()

	waiting := d.waitState("a", "waiting")
	if waiting.Passes != 1 || waiting.Next == nil || time.Until(*waiting.Next) < 59*time.Minute || waiting.Last == nil ||
		!strings.HasPrefix(waiting.Last.Outcome, "finished in") || waiting.Last.Summary != "$0.00: ok" {
		t.Errorf("status after the first pass: %+v", waiting)
	}

	// wake skips the wait.
	if err := d.control("a", "wake"); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, "a: pass finished", 2)

	// pause stops the loop, and wake still runs one pass while it is paused.
	if err := d.control("a", "pause"); err != nil {
		t.Fatal(err)
	}
	d.waitState("a", "paused")
	if err := d.control("a", "pause"); err == nil {
		t.Error("pausing twice gave no error")
	}
	if err := d.control("a", "wake"); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, "a: pass finished", 3)
	if worker := d.waitState("a", "paused"); worker.Passes != 3 {
		t.Errorf("passes %d", worker.Passes)
	}

	// resume goes back to waiting, counting from the end of the last pass.
	if err := d.control("a", "resume"); err != nil {
		t.Fatal(err)
	}
	d.waitState("a", "waiting")
	if err := d.control("a", "resume"); err == nil {
		t.Error("resuming a worker that isn't paused gave no error")
	}

	// Errors name the problem.
	if err := d.control("a", "cancel"); err == nil || err.Error() != "a: no pass is running" {
		t.Errorf("cancel while waiting: %v", err)
	}
	if err := d.control("nobody", "wake"); err == nil || err.Error() != "nobody: no such worker" {
		t.Errorf("unknown worker: %v", err)
	}
	if err := d.control("a", "explode"); err == nil {
		t.Error("unknown action gave no error")
	}

	var out strings.Builder
	printStatus(&out, d.status(), time.Now())
	for _, want := range []string{"\na: waiting, next pass in 1h", "claude, every 1h0m0s, timeout 1h0m0s, passes so far: 3", "    $0.00: ok"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status output %q lacks %q", out.String(), want)
		}
	}
}

func TestCancelRunningPass(t *testing.T) {
	grace = 500 * time.Millisecond
	config := newConfigFile(t, fakeEngine(t, `sleep 1000`))
	config.write("a", "1h")
	var log lines
	d := startDaemon(t, config.path, &log)
	defer d.stop()
	d.waitState("a", "running")
	if err := d.control("a", "wake"); err == nil {
		t.Error("wake during a pass gave no error")
	}
	if err := d.control("a", "pause"); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	printStatus(&out, d.status(), time.Now())
	if !strings.Contains(out.String(), "a: running a pass for") || !strings.Contains(out.String(), ", then pausing") {
		t.Errorf("status output %q", out.String())
	}
	if err := d.control("a", "cancel"); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, "a: pass cancelled after", 1)
	worker := d.waitState("a", "paused")
	time.Sleep(100 * time.Millisecond)
	if n := log.count("a: paused"); n != 1 {
		t.Errorf("logged paused %d times", n)
	}
	if worker.Last == nil || !strings.HasPrefix(worker.Last.Outcome, "cancelled after") {
		t.Errorf("status after cancel: %+v", worker)
	}
}

func TestOneDaemonPerSocket(t *testing.T) {
	config := newConfigFile(t, fakeEngine(t, quickPass))
	config.write("a", "1h")
	var log lines
	d := startDaemon(t, config.path, &log)
	defer d.stop()
	err := run(context.Background(), config.path, d.socket, log.logf)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("second daemon: %v", err)
	}
	if info, err := os.Stat(d.socket); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("socket: %v, %v", info, err)
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	config := newConfigFile(t, fakeEngine(t, quickPass))
	config.write("a", "1h")
	socket := shortSocket(t)
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var log lines
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		if err := run(ctx, config.path, socket, log.logf); err != nil {
			t.Error(err)
		}
		close(finished)
	}()
	log.waitFor(t, "a: pass finished", 1)
	cancel()
	waitClosed(t, finished)
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket left behind after shutdown: %v", err)
	}
}

func TestClientWithoutDaemon(t *testing.T) {
	err := call(shortSocket(t), http.MethodGet, "/status", nil)
	if err == nil || !strings.Contains(err.Error(), "is `loopd run` running?") {
		t.Errorf("error %v", err)
	}
}

func TestSocketPath(t *testing.T) {
	t.Setenv("LOOPD_SOCKET", "")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if path := socketPath(); path != "/run/user/1000/loopd/loopd.sock" {
		t.Errorf("socket path %q", path)
	}
	t.Setenv("LOOPD_SOCKET", "/elsewhere/s")
	if path := socketPath(); path != "/elsewhere/s" {
		t.Errorf("socket path %q", path)
	}
}

func TestRunNeedsAValidConfigToStart(t *testing.T) {
	var log lines
	if err := run(context.Background(), filepath.Join(t.TempDir(), "missing.json"), shortSocket(t), log.logf); err == nil {
		t.Error("no error for a missing config")
	}
	config := newConfigFile(t, "claude")
	config.write(`{"workers": [`)
	if err := run(context.Background(), config.path, shortSocket(t), log.logf); err == nil {
		t.Error("no error for a broken config")
	}
}

func TestCopies(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"kb-r-1", "kb-r-2", "kb-r-3", "kb-s"} {
		os.Mkdir(filepath.Join(dir, sub), 0o755)
	}
	config, err := parseConfig([]byte(`{"workers": [
		{"name": "r", "copies": 3, "engine": "claude", "dir": "` + dir + `/kb-{name}", "prompt": "/researcher {name}", "every": "5m",
		 "args": ["--tag", "copy-{copy}"], "env": {"ME": "{name}"}},
		{"name": "s", "engine": "claude", "dir": "` + dir + `/kb-s", "prompt": "/supervisor {name} {copy}", "every": "5m"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, worker := range config.Workers {
		got = append(got, fmt.Sprintf("%s|%s|%s|%v|%s|%d", worker.Name, filepath.Base(worker.Dir), worker.Prompt, worker.Args, worker.Env["ME"], worker.Copies))
	}
	want := []string{
		"r-1|kb-r-1|/researcher r-1|[--tag copy-1]|r-1|0",
		"r-2|kb-r-2|/researcher r-2|[--tag copy-2]|r-2|0",
		"r-3|kb-r-3|/researcher r-3|[--tag copy-3]|r-3|0",
		"s|kb-s|/supervisor s 1|[]||0",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("instances:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	bad := map[string]string{
		"clash":      `{"workers": [{"name": "r", "copies": 2, "engine": "claude", "dir": "` + dir + `", "prompt": "p", "every": "1m"}, {"name": "r-1", "engine": "claude", "dir": "` + dir + `", "prompt": "p", "every": "1m"}]}`,
		"too many":   `{"workers": [{"name": "r", "copies": 101, "engine": "claude", "dir": "` + dir + `", "prompt": "p", "every": "1m"}]}`,
		"negative":   `{"workers": [{"name": "r", "copies": -1, "engine": "claude", "dir": "` + dir + `", "prompt": "p", "every": "1m"}]}`,
		"no own dir": `{"workers": [{"name": "r", "copies": 4, "engine": "claude", "dir": "` + dir + `/kb-{name}", "prompt": "p", "every": "1m"}]}`,
	}
	for name, text := range bad {
		if _, err := parseConfig([]byte(text)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}

	// Raising copies adds copies without touching the others.
	two, _ := parseConfig([]byte(`{"workers": [{"name": "r", "copies": 2, "engine": "claude", "dir": "` + dir + `/kb-{name}", "prompt": "p", "every": "5m"}]}`))
	three, _ := parseConfig([]byte(`{"workers": [{"name": "r", "copies": 3, "engine": "claude", "dir": "` + dir + `/kb-{name}", "prompt": "p", "every": "5m"}]}`))
	if changes := diff(two, three); strings.Join(changes, ", ") != "added r-3" {
		t.Errorf("changes %q", changes)
	}
}

func TestRegistry(t *testing.T) {
	g := newRegistry()
	a, b := g.begin("a"), g.begin("b")
	if _, err := g.claim(a, "pr-1", "reviewing"); err != nil {
		t.Fatal(err)
	}
	if c, err := g.claim(a, "pr-1", ""); err != nil || c.Note != "reviewing" {
		t.Errorf("claiming again from the same pass: %+v, %v", c, err)
	}
	if _, err := g.claim(b, "pr-1", ""); err == nil || !strings.HasPrefix(err.Error(), "pr-1 is held by a, claimed ") || !strings.HasSuffix(err.Error(), ": reviewing") {
		t.Errorf("claiming a held key: %v", err)
	}
	if err := g.release(b, "pr-1"); err == nil || err.Error() != "pr-1 is held by a, not by this pass" {
		t.Errorf("releasing another pass's claim: %v", err)
	}
	if _, err := g.claim("forged", "pr-2", ""); !errors.Is(err, errNotInPass) {
		t.Errorf("claim from no pass: %v", err)
	}
	if _, err := g.claim(a, "", ""); err == nil {
		t.Error("an empty key was accepted")
	}
	if _, err := g.claim(a, "two\nlines", ""); err == nil {
		t.Error("a key with a newline was accepted")
	}
	g.claim(a, "issue-3", "")
	if keys := len(g.list(a)); keys != 2 || len(g.list("")) != 2 || len(g.list(b)) != 0 {
		t.Errorf("list: %d, %d, %d", keys, len(g.list("")), len(g.list(b)))
	}
	if err := g.release(a, "issue-3"); err != nil {
		t.Error(err)
	}
	if err := g.release(a, "issue-3"); err == nil || err.Error() != "issue-3 isn't claimed" {
		t.Errorf("releasing twice: %v", err)
	}
	// Ending a pass releases its claims, and its token stops working.
	g.end(a)
	if _, err := g.claim(b, "pr-1", ""); err != nil {
		t.Errorf("claim after the holder ended: %v", err)
	}
	if _, err := g.claim(a, "pr-9", ""); !errors.Is(err, errNotInPass) {
		t.Errorf("claim from an ended pass: %v", err)
	}
}

// passEnv reads the environment a fake engine recorded for its pass.
func passEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		data, err := os.ReadFile(path)
		if err != nil || !strings.HasSuffix(string(data), "end\n") {
			continue
		}
		env := map[string]string{}
		for _, line := range strings.Split(string(data), "\n") {
			if key, value, ok := strings.Cut(line, "="); ok {
				env[key] = value
			}
		}
		return env
	}
	t.Fatalf("no environment in %s", path)
	return nil
}

func TestClaimsBelongToPasses(t *testing.T) {
	grace = 500 * time.Millisecond
	dir := t.TempDir()
	bin := fakeEngine(t, `out="`+dir+`/$LOOPD_WORKER.env"
{ echo "LOOPD_PASS=$LOOPD_PASS"; echo "LOOPD_SOCKET=$LOOPD_SOCKET"; echo "PATH=$PATH"; echo end; } > "$out.tmp" && mv "$out.tmp" "$out"
sleep 1000`)
	config := newConfigFile(t, bin)
	config.write("a", "1h", "b", "1h")
	var log lines
	d := startDaemon(t, config.path, &log)
	defer d.stop()
	a, b := passEnv(t, filepath.Join(dir, "a.env")), passEnv(t, filepath.Join(dir, "b.env"))

	exe, _ := os.Executable()
	if a["LOOPD_SOCKET"] != d.socket || a["LOOPD_PASS"] == "" || a["LOOPD_PASS"] == b["LOOPD_PASS"] ||
		!strings.HasPrefix(a["PATH"], filepath.Dir(exe)+":") {
		t.Fatalf("pass environment %q", a)
	}

	if err := callAs(d.socket, a["LOOPD_PASS"], http.MethodPost, "/claim?key=pr-1&note=reviewing", nil); err != nil {
		t.Fatal(err)
	}
	err := callAs(d.socket, b["LOOPD_PASS"], http.MethodPost, "/claim?key=pr-1", nil)
	if err == nil || !strings.Contains(err.Error(), "pr-1 is held by a") {
		t.Errorf("second claim: %v", err)
	}
	if worker := d.worker("a"); len(worker.Claims) != 1 || worker.Claims[0].Key != "pr-1" || worker.Claims[0].Note != "reviewing" {
		t.Errorf("status claims %+v", worker.Claims)
	}
	var claims []claim
	if err := call(d.socket, http.MethodGet, "/claims", &claims); err != nil || len(claims) != 1 || claims[0].Worker != "a" {
		t.Errorf("claims %+v, %v", claims, err)
	}
	var out strings.Builder
	printStatus(&out, d.status(), time.Now())
	if !strings.Contains(out.String(), "  claims: pr-1\n") {
		t.Errorf("status output %q", out.String())
	}

	// When a's pass ends, its claim goes with it.
	if err := d.control("a", "cancel"); err != nil {
		t.Fatal(err)
	}
	d.waitState("a", "waiting")
	if err := callAs(d.socket, b["LOOPD_PASS"], http.MethodPost, "/claim?key=pr-1", nil); err != nil {
		t.Errorf("claim after a's pass ended: %v", err)
	}
	if err := callAs(d.socket, a["LOOPD_PASS"], http.MethodPost, "/claim?key=pr-2", nil); err == nil {
		t.Error("an ended pass's token still works")
	}
}

func TestClaimOutsideAPass(t *testing.T) {
	t.Setenv("LOOPD_PASS", "")
	var out strings.Builder
	if err := claimKey(&out, "pr-1", ""); err == nil || !strings.Contains(err.Error(), "LOOPD_PASS") {
		t.Errorf("error %v", err)
	}
}

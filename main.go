// Command loopd runs coding agents, Claude Code or Codex, over and over. Each pass is a fresh
// non-interactive session: `claude -p` or `codex exec`.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Config is loopd's JSON config file.
type Config struct {
	Logs    string   `json:"logs"`
	Workers []Worker `json:"workers"`
}

// Worker is one agent that loopd runs pass after pass.
type Worker struct {
	Name    string            `json:"name"`
	Engine  string            `json:"engine"`
	Bin     string            `json:"bin"`
	Dir     string            `json:"dir"`
	Prompt  string            `json:"prompt"`
	Model   string            `json:"model"`
	Effort  string            `json:"effort"`
	Every   Duration          `json:"every"`
	Timeout Duration          `json:"timeout"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// Duration reads strings like "20m" or "1h30m" from JSON.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("durations are strings such as \"20m\"")
	}
	parsed, err := time.ParseDuration(text)
	d.Duration = parsed
	return err
}

var (
	// grace is how long a pass gets to exit after SIGTERM before loopd kills it.
	grace = 10 * time.Second
	// pollInterval is how often loopd checks its config file for changes.
	pollInterval = 5 * time.Second
)

var validName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// defaultConfigPath is $XDG_CONFIG_HOME/loopd/config.json, or ~/.config/loopd/config.json.
func defaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "loopd", "config.json"), nil
}

// defaultLogs is $XDG_STATE_HOME/loopd/logs, or ~/.local/state/loopd/logs.
func defaultLogs() string {
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "loopd", "logs")
	}
	return expand("~/.local/state/loopd/logs")
}

func parseConfig(data []byte) (*Config, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return nil, err
	}
	if config.Logs == "" {
		config.Logs = defaultLogs()
	}
	config.Logs = expand(config.Logs)
	seen := map[string]bool{}
	for i := range config.Workers {
		worker := &config.Workers[i]
		if !validName.MatchString(worker.Name) {
			return nil, fmt.Errorf("worker %d: name %q must be letters, digits, '.', '_' or '-'", i+1, worker.Name)
		}
		if seen[worker.Name] {
			return nil, fmt.Errorf("worker %s: name used twice", worker.Name)
		}
		seen[worker.Name] = true
		if worker.Engine != "claude" && worker.Engine != "codex" {
			return nil, fmt.Errorf("worker %s: engine must be \"claude\" or \"codex\"", worker.Name)
		}
		worker.Dir = expand(worker.Dir)
		if info, err := os.Stat(worker.Dir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("worker %s: dir %q is not a directory", worker.Name, worker.Dir)
		}
		if worker.Prompt == "" {
			return nil, fmt.Errorf("worker %s: prompt is empty", worker.Name)
		}
		if worker.Every.Duration <= 0 {
			return nil, fmt.Errorf("worker %s: every must be positive", worker.Name)
		}
		if worker.Timeout.Duration <= 0 {
			worker.Timeout.Duration = time.Hour
		}
		worker.Bin = expand(worker.Bin)
	}
	return &config, nil
}

func expand(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

// command is the argv for one pass. Claude's prompt goes straight after -p, so that an option
// in args that takes several values can't swallow it.
func command(worker Worker) []string {
	bin := worker.Bin
	if bin == "" {
		bin = worker.Engine
	}
	if worker.Engine == "claude" {
		argv := []string{bin, "-p", worker.Prompt, "--output-format", "stream-json", "--verbose"}
		if worker.Model != "" {
			argv = append(argv, "--model", worker.Model)
		}
		if worker.Effort != "" {
			argv = append(argv, "--effort", worker.Effort)
		}
		return append(argv, worker.Args...)
	}
	argv := []string{bin, "exec", "--json"}
	if worker.Model != "" {
		argv = append(argv, "--model", worker.Model)
	}
	if worker.Effort != "" {
		argv = append(argv, "-c", fmt.Sprintf("model_reasoning_effort=%q", worker.Effort))
	}
	argv = append(argv, worker.Args...)
	return append(argv, "--", worker.Prompt)
}

type result struct {
	exit     int
	duration time.Duration
	stopped  string // "timed out" or "stopped" when loopd ended the pass
	err      error  // the pass couldn't start
	summary  string
}

// runPass runs one pass, writing its stdout to logPath and its stderr next to it.
func runPass(ctx context.Context, worker Worker, logPath string) result {
	start := time.Now()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return result{err: err}
	}
	stdout, err := os.Create(logPath)
	if err != nil {
		return result{err: err}
	}
	defer stdout.Close()
	stderrPath := strings.TrimSuffix(logPath, ".jsonl") + ".stderr"
	stderr, err := os.Create(stderrPath)
	if err != nil {
		return result{err: err}
	}
	defer removeIfEmpty(stderrPath)
	defer stderr.Close()

	argv := command(worker)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = worker.Dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), "LOOPD_WORKER="+worker.Name)
	for key, value := range worker.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	// The pass gets its own process group, so that loopd can stop everything it started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return result{err: err}
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(worker.Timeout.Duration)
	defer timer.Stop()
	var r result
	var waitErr error
	select {
	case waitErr = <-done:
	case <-timer.C:
		r.stopped = "timed out"
		waitErr = stop(pid, done)
	case <-ctx.Done():
		r.stopped = "stopped"
		waitErr = stop(pid, done)
	}
	// Kill anything the agent left running in the background.
	syscall.Kill(-pid, syscall.SIGKILL)

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		r.exit = exitErr.ExitCode()
	} else if waitErr != nil {
		r.err = waitErr
	}
	r.duration = time.Since(start)
	r.summary = summarize(logPath)
	return r
}

// stop sends SIGTERM to a pass's process group, then SIGKILL if it hasn't exited after grace.
func stop(pid int, done <-chan error) error {
	syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		syscall.Kill(-pid, syscall.SIGKILL)
		return <-done
	}
}

func removeIfEmpty(path string) {
	if info, err := os.Stat(path); err == nil && info.Size() == 0 {
		os.Remove(path)
	}
}

// event holds the fields loopd reads from Claude's stream-json and Codex's JSONL output.
type event struct {
	Type    string  `json:"type"`
	Subtype string  `json:"subtype"`
	Model   string  `json:"model"`
	Result  string  `json:"result"`
	Cost    float64 `json:"total_cost_usd"`
	Item    struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Usage struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
	} `json:"usage"`
}

// summarize returns the model Claude reports, the cost or token use of a pass, and the first line
// of its final message.
func summarize(logPath string) string {
	file, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer file.Close()
	var model, text, usage string
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		var e event
		if json.Unmarshal(line, &e) == nil {
			switch {
			case e.Type == "system" && e.Subtype == "init" && model == "": // Claude
				model = e.Model
			case e.Type == "result": // Claude
				text, usage = e.Result, fmt.Sprintf("$%.2f", e.Cost)
			case e.Type == "item.completed" && e.Item.Type == "agent_message": // Codex
				text = e.Item.Text
			case e.Type == "turn.completed": // Codex
				usage = fmt.Sprintf("%d tokens in, %d out", e.Usage.Input, e.Usage.Output)
			}
		}
		if err != nil {
			break
		}
	}
	text, _, _ = strings.Cut(strings.TrimSpace(text), "\n")
	if runes := []rune(text); len(runes) > 200 {
		text = string(runes[:200]) + "…"
	}
	if model != "" {
		usage = strings.TrimSpace(model + " " + usage)
	}
	switch {
	case usage == "":
		return text
	case text == "":
		return usage
	}
	return usage + ": " + text
}

func describe(worker Worker, r result) string {
	switch {
	case r.err != nil:
		return fmt.Sprintf("%s: pass failed: %v", worker.Name, r.err)
	case r.stopped == "timed out":
		return fmt.Sprintf("%s: pass timed out after %s", worker.Name, worker.Timeout.Duration)
	case r.stopped != "":
		return fmt.Sprintf("%s: pass stopped after %s", worker.Name, r.duration.Round(time.Second))
	}
	line := fmt.Sprintf("%s: pass finished in %s with exit %d", worker.Name, r.duration.Round(time.Second), r.exit)
	if r.summary != "" {
		line += "; " + r.summary
	}
	return line
}

// spec is everything a worker's loop needs from the config.
type spec struct {
	worker Worker
	logs   string
}

// work runs a worker's passes until ctx ends or it receives nil on updates. A new spec on
// updates applies from the next pass, and the wait before it counts from the end of the last pass.
func work(ctx context.Context, s *spec, updates <-chan *spec, logf func(string, ...any)) {
	// apply takes a new spec, and reports false if the worker has been removed.
	apply := func(next *spec) bool {
		if next == nil {
			logf("%s: removed from the config", s.worker.Name)
			return false
		}
		s = next
		logf("%s: config changed", s.worker.Name)
		return true
	}
	for {
		logPath := filepath.Join(s.logs, s.worker.Name, time.Now().UTC().Format("20060102T150405.000Z")+".jsonl")
		logf("%s: pass started, log %s", s.worker.Name, logPath)
		logf("%s", describe(s.worker, runPass(ctx, s.worker, logPath)))
		if ctx.Err() != nil {
			return
		}
		end := time.Now()
		select {
		case next := <-updates: // a change made during the pass
			if !apply(next) {
				return
			}
		default:
		}
	wait:
		for {
			wait := max(time.Until(end.Add(s.worker.Every.Duration)), 0)
			logf("%s: next pass in %s", s.worker.Name, wait.Round(time.Second))
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case next := <-updates:
				timer.Stop()
				if !apply(next) {
					return
				}
			case <-timer.C:
				break wait
			}
		}
	}
}

// runner is a worker's running loop, as the manager in run sees it.
type runner struct {
	updates chan *spec // holds at most the newest spec
	done    chan struct{}
	sent    *spec
}

func (r *runner) send(s *spec) {
	select {
	case <-r.updates:
	default:
	}
	r.updates <- s
	r.sent = s
}

// run starts a loop for each worker in the config at path, then checks the file every
// pollInterval and brings the loops in line with it, until ctx ends.
func run(ctx context.Context, path string, logf func(string, ...any)) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	config, err := parseConfig(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	runners := map[string]*runner{}
	var wg sync.WaitGroup
	reconcile := func() {
		wanted := map[string]*spec{}
		for _, worker := range config.Workers {
			wanted[worker.Name] = &spec{worker, config.Logs}
		}
		for name, r := range runners {
			select {
			case <-r.done:
				delete(runners, name)
				continue
			default:
			}
			if wanted[name] == nil && r.sent != nil {
				r.send(nil)
			}
		}
		// A worker removed and added back before its loop has stopped keeps the same loop. One
		// whose loop has already stopped gets a new loop, so that its passes never overlap.
		for name, s := range wanted {
			if r := runners[name]; r == nil {
				r = &runner{updates: make(chan *spec, 1), done: make(chan struct{}), sent: s}
				runners[name] = r
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer close(r.done)
					work(ctx, s, r.updates, logf)
				}()
			} else if !reflect.DeepEqual(r.sent, s) {
				r.send(s)
			}
		}
	}
	reconcile()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var lastErr string
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case <-ticker.C:
		}
		next, err := os.ReadFile(path)
		if err != nil {
			if err.Error() != lastErr {
				logf("loopd: %v; keeping the previous config", err)
				lastErr = err.Error()
			}
		} else if lastErr = ""; !bytes.Equal(next, data) {
			data = next
			if parsed, err := parseConfig(next); err != nil {
				logf("loopd: %s: %v; keeping the previous config", path, err)
			} else {
				config = parsed
				logf("loopd: reloaded %s", path)
			}
		}
		reconcile()
	}
}

func newLogger(out io.Writer) func(string, ...any) {
	flags := log.LstdFlags
	if os.Getenv("JOURNAL_STREAM") != "" {
		flags = 0 // journald adds its own timestamps
	}
	return log.New(out, "", flags).Printf
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 || len(args) > 2 || args[0] != "run" {
		fmt.Fprintln(os.Stderr, "usage: loopd run [config.json]")
		os.Exit(2)
	}
	var path string
	if len(args) == 2 {
		path = args[1]
	} else {
		var err error
		if path, err = defaultConfigPath(); err != nil {
			fmt.Fprintln(os.Stderr, "loopd:", err)
			os.Exit(1)
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logf := newLogger(os.Stdout)
	logf("loopd: using %s", path)
	if err := run(ctx, path, logf); err != nil {
		fmt.Fprintln(os.Stderr, "loopd:", err)
		os.Exit(1)
	}
	logf("loopd: stopped")
}

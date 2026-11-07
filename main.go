// Command loopd runs coding agents, Claude Code or Codex, over and over. Each pass is a fresh
// non-interactive session: `claude -p` or `codex exec`.
package main

import (
	"bufio"
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
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Config is the JSON file given to `loopd run`.
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

// grace is how long a pass gets to exit after SIGTERM before loopd kills it.
var grace = 10 * time.Second

var validName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func loadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if config.Logs == "" {
		config.Logs = "~/.loopd/logs"
	}
	config.Logs = expand(config.Logs)
	if len(config.Workers) == 0 {
		return nil, errors.New("no workers configured")
	}
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
		return append([]string{bin, "-p", worker.Prompt, "--output-format", "stream-json", "--verbose"}, worker.Args...)
	}
	argv := append([]string{bin, "exec", "--json"}, worker.Args...)
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

	r.exit = 0
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
	Type   string  `json:"type"`
	Result string  `json:"result"`
	Cost   float64 `json:"total_cost_usd"`
	Item   struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Usage struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
	} `json:"usage"`
}

// summarize returns the cost or token use of a pass and the first line of its final message.
func summarize(logPath string) string {
	file, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer file.Close()
	var text, usage string
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		var e event
		if json.Unmarshal(line, &e) == nil {
			switch {
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

// loop runs a worker's passes until ctx ends, waiting worker.Every after each pass.
func loop(ctx context.Context, worker Worker, logs string, logf func(string, ...any)) {
	for {
		logPath := filepath.Join(logs, worker.Name, time.Now().UTC().Format("20060102T150405.000Z")+".jsonl")
		logf("%s: pass started, log %s", worker.Name, logPath)
		logf("%s", describe(worker, runPass(ctx, worker, logPath)))
		if ctx.Err() != nil {
			return
		}
		logf("%s: next pass in %s", worker.Name, worker.Every.Duration)
		select {
		case <-ctx.Done():
			return
		case <-time.After(worker.Every.Duration):
		}
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
	if len(os.Args) != 3 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: loopd run <config.json>")
		os.Exit(2)
	}
	config, err := loadConfig(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "loopd:", err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logf := newLogger(os.Stdout)
	var wg sync.WaitGroup
	for _, worker := range config.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop(ctx, worker, config.Logs, logf)
		}()
	}
	wg.Wait()
	logf("loopd: stopped")
}

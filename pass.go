package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// grace is how long a pass gets to exit after SIGTERM before loopd kills it.
var grace = 10 * time.Second

// errCancelled is the cause given when `loopd cancel` stops a pass.
var errCancelled = errors.New("cancelled")

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
	stopped  string // "timed out", "cancelled" or "stopped" when loopd ended the pass
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
		if errors.Is(context.Cause(ctx), errCancelled) {
			r.stopped = "cancelled"
		}
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

// outcome says how a pass ended.
func outcome(worker Worker, r result) string {
	switch {
	case r.err != nil:
		return fmt.Sprintf("failed: %v", r.err)
	case r.stopped == "timed out":
		return fmt.Sprintf("timed out after %s", worker.Timeout.Duration)
	case r.stopped != "":
		return fmt.Sprintf("%s after %s", r.stopped, r.duration.Round(time.Second))
	}
	return fmt.Sprintf("finished in %s with exit %d", r.duration.Round(time.Second), r.exit)
}

func describe(worker Worker, r result) string {
	line := worker.Name + ": pass " + outcome(worker, r)
	if r.err == nil && r.stopped == "" && r.summary != "" {
		line += "; " + r.summary
	}
	return line
}

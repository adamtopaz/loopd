package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

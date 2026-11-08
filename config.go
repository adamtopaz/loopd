package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config is loopd's JSON config file.
type Config struct {
	Logs    string   `json:"logs"`
	Workers []Worker `json:"workers"`
}

// Worker is one agent that loopd runs pass after pass, or several copies of one.
type Worker struct {
	Name    string            `json:"name"`
	Copies  int               `json:"copies"`
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
	for i, worker := range config.Workers {
		if !validName.MatchString(worker.Name) {
			return nil, fmt.Errorf("worker %d: name %q must be letters, digits, '.', '_' or '-'", i+1, worker.Name)
		}
		if worker.Copies < 0 || worker.Copies > 100 {
			return nil, fmt.Errorf("worker %s: copies must be between 1 and 100", worker.Name)
		}
	}
	config.Workers = instances(config.Workers)
	seen := map[string]bool{}
	for i := range config.Workers {
		worker := &config.Workers[i]
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

// instances turns each worker with copies above 1 into copies named name-1, name-2 and so on,
// and fills in {name} and {copy} in each one's dir, prompt, args and env values.
func instances(workers []Worker) []Worker {
	var out []Worker
	for _, worker := range workers {
		copies := max(worker.Copies, 1)
		for i := 1; i <= copies; i++ {
			instance := worker
			instance.Copies = 0
			if copies > 1 {
				instance.Name = fmt.Sprintf("%s-%d", worker.Name, i)
			}
			fill := strings.NewReplacer("{name}", instance.Name, "{copy}", strconv.Itoa(i))
			instance.Dir = fill.Replace(worker.Dir)
			instance.Prompt = fill.Replace(worker.Prompt)
			instance.Args = nil
			for _, arg := range worker.Args {
				instance.Args = append(instance.Args, fill.Replace(arg))
			}
			if worker.Env != nil {
				instance.Env = map[string]string{}
				for key, value := range worker.Env {
					instance.Env[key] = fill.Replace(value)
				}
			}
			out = append(out, instance)
		}
	}
	return out
}

func expand(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

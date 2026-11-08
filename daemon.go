package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// spec is everything a worker's loop needs from the config.
type spec struct {
	worker Worker
	logs   string
}

// passStatus is what status reports about a finished pass.
type passStatus struct {
	Started time.Time `json:"started"`
	Outcome string    `json:"outcome"`
	Summary string    `json:"summary,omitempty"`
	Log     string    `json:"log"`
}

// runner is one worker's loop and the controls on it.
type runner struct {
	updates chan *spec    // holds at most the newest spec; nil means the worker was removed
	kick    chan struct{} // tells a waiting loop to look at its controls again
	done    chan struct{}
	sent    *spec // the spec last sent to the loop; only the daemon uses it, under its lock

	mu         sync.Mutex
	spec       *spec
	state      string // "running", "waiting" or "paused"
	since      time.Time
	next       time.Time
	paused     bool
	wakeNow    bool
	cancelPass context.CancelCauseFunc
	log        string
	passes     int
	last       *passStatus
}

func newRunner(s *spec) *runner {
	return &runner{
		updates: make(chan *spec, 1),
		kick:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		sent:    s,
		spec:    s,
	}
}

func (r *runner) send(s *spec) {
	select {
	case <-r.updates:
	default:
	}
	r.updates <- s
	r.sent = s
}

func (r *runner) poke() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

func (r *runner) locked(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

// work runs passes until ctx ends or the worker is removed. A new spec applies from the next
// pass, and the wait before it counts from the end of the last pass.
func (r *runner) work(ctx context.Context, logf func(string, ...any)) {
	s := r.spec
	apply := func(next *spec) bool {
		if next == nil {
			logf("%s: removed from the config", s.worker.Name)
			return false
		}
		s = next
		r.locked(func() { r.spec = next })
		logf("%s: config changed", s.worker.Name)
		return true
	}
	for ctx.Err() == nil {
		started := time.Now()
		logPath := filepath.Join(s.logs, s.worker.Name, started.UTC().Format("20060102T150405.000Z")+".jsonl")
		passCtx, cancel := context.WithCancelCause(ctx)
		r.locked(func() { r.state, r.since, r.log, r.cancelPass = "running", started, logPath, cancel })
		logf("%s: pass started, log %s", s.worker.Name, logPath)
		res := runPass(passCtx, s.worker, logPath)
		cancel(nil)
		logf("%s", describe(s.worker, res))
		r.locked(func() {
			r.cancelPass = nil
			r.passes++
			r.last = &passStatus{Started: started, Outcome: outcome(s.worker, res), Summary: res.summary, Log: logPath}
		})
		if ctx.Err() != nil {
			return
		}
		end := time.Now()
		select {
		case next := <-r.updates: // a change made during the pass
			if !apply(next) {
				return
			}
		default:
		}
		select {
		case <-r.kick: // a control used during the pass, which the wait below takes into account
		default:
		}
	wait:
		for {
			r.mu.Lock()
			if r.wakeNow {
				r.wakeNow = false
				r.mu.Unlock()
				break wait
			}
			paused, next := r.paused, end.Add(s.worker.Every.Duration)
			r.state, r.since, r.next = "waiting", end, next
			if paused {
				r.state = "paused"
			}
			r.mu.Unlock()
			var timer *time.Timer
			var fire <-chan time.Time
			if paused {
				logf("%s: paused", s.worker.Name)
			} else {
				wait := max(time.Until(next), 0)
				logf("%s: next pass in %s", s.worker.Name, wait.Round(time.Second))
				timer = time.NewTimer(wait)
				fire = timer.C
			}
			select {
			case <-ctx.Done():
				stopTimer(timer)
				return
			case next := <-r.updates:
				stopTimer(timer)
				if !apply(next) {
					return
				}
			case <-r.kick:
				stopTimer(timer)
			case <-fire:
				break wait
			}
		}
	}
}

func stopTimer(timer *time.Timer) {
	if timer != nil {
		timer.Stop()
	}
}

var errNoWorker = errors.New("no such worker")

// control applies `loopd pause`, `resume`, `wake` or `cancel` to the worker.
func (r *runner) control(action string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch action {
	case "pause":
		if r.paused {
			return errors.New("already paused")
		}
		r.paused = true
	case "resume":
		if !r.paused {
			return errors.New("not paused")
		}
		r.paused = false
	case "wake":
		if r.state == "running" {
			return errors.New("a pass is already running")
		}
		r.wakeNow = true
	case "cancel":
		if r.cancelPass == nil {
			return errors.New("no pass is running")
		}
		r.cancelPass(errCancelled)
		return nil
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	r.poke()
	return nil
}

// workerStatus is what `loopd status` reports about a worker.
type workerStatus struct {
	Name    string      `json:"name"`
	Engine  string      `json:"engine"`
	Model   string      `json:"model,omitempty"`
	Effort  string      `json:"effort,omitempty"`
	Every   string      `json:"every"`
	Timeout string      `json:"timeout"`
	State   string      `json:"state"`
	Since   time.Time   `json:"since"`
	Next    *time.Time  `json:"next,omitempty"`
	Paused  bool        `json:"paused"`
	Removed bool        `json:"removed,omitempty"`
	Log     string      `json:"log,omitempty"`
	Passes  int         `json:"passes"`
	Last    *passStatus `json:"last,omitempty"`
}

func (r *runner) status() workerStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	worker := r.spec.worker
	status := workerStatus{
		Name: worker.Name, Engine: worker.Engine, Model: worker.Model, Effort: worker.Effort,
		Every: worker.Every.String(), Timeout: worker.Timeout.String(),
		State: r.state, Since: r.since, Paused: r.paused, Log: r.log, Passes: r.passes, Last: r.last,
	}
	if r.state == "waiting" {
		next := r.next
		status.Next = &next
	}
	return status
}

// daemonStatus is the reply to `loopd status`.
type daemonStatus struct {
	Version string         `json:"version"`
	Config  string         `json:"config"`
	Started time.Time      `json:"started"`
	Workers []workerStatus `json:"workers"`
}

type daemon struct {
	ctx     context.Context
	path    string
	logf    func(string, ...any)
	started time.Time
	wg      sync.WaitGroup

	mu      sync.Mutex
	config  *Config
	runners map[string]*runner
}

func (d *daemon) load() (*Config, error) {
	data, err := os.ReadFile(d.path)
	if err != nil {
		return nil, err
	}
	config, err := parseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.path, err)
	}
	return config, nil
}

// reload applies the config file and returns what changed. If the file is invalid, the
// previous config stays in force.
func (d *daemon) reload() ([]string, error) {
	config, err := d.load()
	if err != nil {
		d.logf("loopd: reload failed, keeping the previous config: %v", err)
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	changes := diff(d.config, config)
	d.config = config
	d.reconcile()
	if len(changes) == 0 {
		d.logf("loopd: reloaded %s, no changes", d.path)
	} else {
		d.logf("loopd: reloaded %s: %s", d.path, strings.Join(changes, ", "))
	}
	return changes, nil
}

func diff(before, after *Config) []string {
	var changes []string
	if before.Logs != after.Logs {
		changes = append(changes, "logs now in "+after.Logs)
	}
	old := map[string]Worker{}
	for _, worker := range before.Workers {
		old[worker.Name] = worker
	}
	kept := map[string]bool{}
	for _, worker := range after.Workers {
		kept[worker.Name] = true
		if previous, ok := old[worker.Name]; !ok {
			changes = append(changes, "added "+worker.Name)
		} else if !reflect.DeepEqual(previous, worker) {
			changes = append(changes, "changed "+worker.Name)
		}
	}
	for _, worker := range before.Workers {
		if !kept[worker.Name] {
			changes = append(changes, "removed "+worker.Name)
		}
	}
	return changes
}

// reconcile brings the worker loops in line with d.config. The caller holds d.mu.
func (d *daemon) reconcile() {
	wanted := map[string]*spec{}
	for _, worker := range d.config.Workers {
		wanted[worker.Name] = &spec{worker, d.config.Logs}
	}
	for name, r := range d.runners {
		select {
		case <-r.done:
			delete(d.runners, name)
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
		if r := d.runners[name]; r == nil {
			d.start(s)
		} else if !reflect.DeepEqual(r.sent, s) {
			r.send(s)
		}
	}
}

// start begins a loop for a worker. The caller holds d.mu.
func (d *daemon) start(s *spec) {
	if d.ctx.Err() != nil {
		return
	}
	r := newRunner(s)
	d.runners[s.worker.Name] = r
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		r.work(d.ctx, d.logf)
		close(r.done)
		// The worker may have been added back while its last pass ran.
		d.mu.Lock()
		defer d.mu.Unlock()
		d.reconcile()
	}()
}

func (d *daemon) status() daemonStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	status := daemonStatus{Version: version(), Config: d.path, Started: d.started, Workers: []workerStatus{}}
	// Workers in config order, then removed ones still finishing a pass.
	var names []string
	listed := map[string]bool{}
	for _, worker := range d.config.Workers {
		names = append(names, worker.Name)
		listed[worker.Name] = true
	}
	var removed []string
	for name := range d.runners {
		if !listed[name] {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	for _, name := range append(names, removed...) {
		r := d.runners[name]
		if r == nil {
			continue
		}
		select {
		case <-r.done:
			continue
		default:
		}
		worker := r.status()
		worker.Removed = r.sent == nil
		status.Workers = append(status.Workers, worker)
	}
	return status
}

func (d *daemon) control(name, action string) error {
	d.mu.Lock()
	r := d.runners[name]
	d.mu.Unlock()
	if r == nil {
		return errNoWorker
	}
	return r.control(action)
}

type errorReply struct {
	Error string `json:"error"`
}

type reloadReply struct {
	Changes []string `json:"changes"`
}

func reply(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}

func (d *daemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, req *http.Request) {
		reply(w, http.StatusOK, d.status())
	})
	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, req *http.Request) {
		changes, err := d.reload()
		if err != nil {
			reply(w, http.StatusBadRequest, errorReply{err.Error()})
			return
		}
		reply(w, http.StatusOK, reloadReply{Changes: append([]string{}, changes...)})
	})
	mux.HandleFunc("POST /workers/{name}/{action}", func(w http.ResponseWriter, req *http.Request) {
		name, action := req.PathValue("name"), req.PathValue("action")
		switch err := d.control(name, action); {
		case errors.Is(err, errNoWorker):
			reply(w, http.StatusNotFound, errorReply{fmt.Sprintf("%s: %v", name, err)})
		case err != nil:
			reply(w, http.StatusConflict, errorReply{fmt.Sprintf("%s: %v", name, err)})
		default:
			d.logf("%s: %s requested", name, action)
			reply(w, http.StatusOK, struct{}{})
		}
	})
	return mux
}

// socketPath is $LOOPD_SOCKET, or loopd/loopd.sock in $XDG_RUNTIME_DIR, or in a per-user
// directory under the temp directory if that isn't set.
func socketPath() string {
	if path := os.Getenv("LOOPD_SOCKET"); path != "" {
		return path
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "loopd", "loopd.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("loopd-%d", os.Getuid()), "loopd.sock")
}

// listen opens the control socket, which only this user can use. It refuses to replace the
// socket of a loopd that is still running.
func listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if conn, err := net.Dial("unix", path); err == nil {
		conn.Close()
		return nil, fmt.Errorf("another loopd is already running on %s", path)
	}
	os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}

// run is the daemon: it starts a loop for each worker in the config at path and serves the
// control socket until ctx ends. SIGHUP reloads the config, like `loopd reload`.
func run(ctx context.Context, path, socket string, logf func(string, ...any)) error {
	d := &daemon{ctx: ctx, path: path, logf: logf, started: time.Now(), runners: map[string]*runner{}}
	config, err := d.load()
	if err != nil {
		return err
	}
	listener, err := listen(socket)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: d.handler()}
	go server.Serve(listener)
	logf("loopd: config %s, control socket %s", path, socket)
	d.mu.Lock()
	d.config = config
	d.reconcile()
	d.mu.Unlock()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case <-hup:
			d.reload()
		case <-ctx.Done():
			server.Close()
			d.wg.Wait()
			return nil
		}
	}
}

// version is the module version, which names the git revision for a build from a checkout or
// from `go install` at a commit.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	v := "(devel)"
	for _, setting := range info.Settings {
		switch {
		case setting.Key == "vcs.revision" && len(setting.Value) >= 7:
			v += " " + setting.Value[:7]
		case setting.Key == "vcs.modified" && setting.Value == "true":
			v += "+dirty"
		}
	}
	return v
}

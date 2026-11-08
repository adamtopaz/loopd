// Command loopd runs coding agents, Claude Code or Codex, over and over. Each pass is a fresh
// non-interactive session: `claude -p` or `codex exec`. `loopd run` is the daemon, and the other
// subcommands control it through its socket.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const usage = `usage:
  loopd run [config.json]   run the daemon (config defaults to $XDG_CONFIG_HOME/loopd/config.json)
  loopd status [--json]     show the daemon and its workers
  loopd reload              re-read the config and apply it
  loopd pause <worker>      stop after the pass under way, if any
  loopd resume <worker>     undo pause
  loopd wake <worker>       start a pass now
  loopd cancel <worker>     stop the pass under way
  loopd claim <key> [note]  claim a key for the pass under way (only inside a pass)
  loopd release <key>       release a key this pass claimed
  loopd claims              list the claims that passes hold`

// confirmations are printed after a control command succeeds.
var confirmations = map[string]string{
	"pause":  "paused; a pass under way will finish",
	"resume": "resumed",
	"wake":   "starting a pass",
	"cancel": "stopping the pass under way",
}

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) >= 1 && args[0] == "run" && len(args) <= 2:
		err = runDaemon(args[1:])
	case len(args) == 1 && args[0] == "status":
		err = showStatus(os.Stdout, false)
	case len(args) == 2 && args[0] == "status" && args[1] == "--json":
		err = showStatus(os.Stdout, true)
	case len(args) == 1 && args[0] == "reload":
		err = reloadConfig(os.Stdout)
	case len(args) == 2 && confirmations[args[0]] != "":
		err = call(socketPath(), http.MethodPost, "/workers/"+url.PathEscape(args[1])+"/"+args[0], nil)
		if err == nil {
			fmt.Printf("%s: %s\n", args[1], confirmations[args[0]])
		}
	case len(args) >= 2 && args[0] == "claim":
		err = claimKey(os.Stdout, args[1], strings.Join(args[2:], " "))
	case len(args) == 2 && args[0] == "release":
		err = releaseKey(os.Stdout, args[1])
	case len(args) == 1 && args[0] == "claims":
		err = showClaims(os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "loopd:", err)
		os.Exit(1)
	}
}

func runDaemon(args []string) error {
	path := ""
	if len(args) == 1 {
		path = args[0]
	} else {
		var err error
		if path, err = defaultConfigPath(); err != nil {
			return err
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logf := newLogger(os.Stdout)
	if err := run(ctx, path, socketPath(), logf); err != nil {
		return err
	}
	logf("loopd: stopped")
	return nil
}

func newLogger(out io.Writer) func(string, ...any) {
	flags := log.LstdFlags
	if os.Getenv("JOURNAL_STREAM") != "" {
		flags = 0 // journald adds its own timestamps
	}
	return log.New(out, "", flags).Printf
}

// call sends one request to the daemon and decodes its JSON reply into out.
func call(socket, method, path string, out any) error {
	return callAs(socket, "", method, path, out)
}

// callAs is call on behalf of the pass with the given token.
func callAs(socket, pass, method, path string, out any) error {
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}},
	}
	request, err := http.NewRequest(method, "http://loopd"+path, nil)
	if err != nil {
		return err
	}
	if pass != "" {
		request.Header.Set(passHeader, pass)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("can't reach the daemon at %s; is `loopd run` running? (%v)", socket, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		var e errorReply
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("the daemon replied %s", response.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// passToken is the LOOPD_PASS token that loopd gives each pass.
func passToken() (string, error) {
	token := os.Getenv("LOOPD_PASS")
	if token == "" {
		return "", errors.New("claims work only inside a pass that loopd runs, where LOOPD_PASS is set")
	}
	return token, nil
}

func claimKey(out io.Writer, key, note string) error {
	token, err := passToken()
	if err != nil {
		return err
	}
	query := url.Values{"key": {key}}
	if note != "" {
		query.Set("note", note)
	}
	if err := callAs(socketPath(), token, http.MethodPost, "/claim?"+query.Encode(), nil); err != nil {
		return err
	}
	fmt.Fprintln(out, "claimed", key)
	return nil
}

func releaseKey(out io.Writer, key string) error {
	token, err := passToken()
	if err != nil {
		return err
	}
	if err := callAs(socketPath(), token, http.MethodPost, "/release?"+url.Values{"key": {key}}.Encode(), nil); err != nil {
		return err
	}
	fmt.Fprintln(out, "released", key)
	return nil
}

func showClaims(out io.Writer) error {
	var claims []claim
	if err := call(socketPath(), http.MethodGet, "/claims", &claims); err != nil {
		return err
	}
	if len(claims) == 0 {
		fmt.Fprintln(out, "no claims")
	}
	for _, c := range claims {
		line := fmt.Sprintf("%s: %s, for %s", c.Key, c.Worker, time.Since(c.Since).Round(time.Second))
		if c.Note != "" {
			line += ": " + c.Note
		}
		fmt.Fprintln(out, line)
	}
	return nil
}

func reloadConfig(out io.Writer) error {
	var reply reloadReply
	if err := call(socketPath(), http.MethodPost, "/reload", &reply); err != nil {
		return err
	}
	if len(reply.Changes) == 0 {
		fmt.Fprintln(out, "reloaded, no changes")
		return nil
	}
	fmt.Fprintln(out, "reloaded:", strings.Join(reply.Changes, ", "))
	return nil
}

func showStatus(out io.Writer, asJSON bool) error {
	var status daemonStatus
	if err := call(socketPath(), http.MethodGet, "/status", &status); err != nil {
		return err
	}
	if asJSON {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(status)
	}
	printStatus(out, status, time.Now())
	return nil
}

func printStatus(out io.Writer, status daemonStatus, now time.Time) {
	fmt.Fprintf(out, "loopd %s, up %s, config %s\n", status.Version, now.Sub(status.Started).Round(time.Second), status.Config)
	if len(status.Workers) == 0 {
		fmt.Fprintln(out, "no workers")
	}
	for _, worker := range status.Workers {
		fmt.Fprintf(out, "\n%s: %s\n", worker.Name, stateLine(worker, now))
		settings := worker.Engine
		for _, value := range []string{worker.Model, worker.Effort} {
			if value != "" {
				settings += " " + value
			}
		}
		fmt.Fprintf(out, "  %s, every %s, timeout %s, passes so far: %d\n", settings, worker.Every, worker.Timeout, worker.Passes)
		if last := worker.Last; last != nil {
			fmt.Fprintf(out, "  last pass: started %s, %s\n", last.Started.Local().Format("15:04:05"), last.Outcome)
			if last.Summary != "" {
				fmt.Fprintf(out, "    %s\n", last.Summary)
			}
		}
		if worker.Log != "" {
			fmt.Fprintf(out, "  log: %s\n", worker.Log)
		}
		if len(worker.Claims) > 0 {
			var keys []string
			for _, c := range worker.Claims {
				keys = append(keys, c.Key)
			}
			fmt.Fprintf(out, "  claims: %s\n", strings.Join(keys, ", "))
		}
	}
}

func stateLine(worker workerStatus, now time.Time) string {
	var line string
	switch worker.State {
	case "running":
		line = fmt.Sprintf("running a pass for %s", now.Sub(worker.Since).Round(time.Second))
		if worker.Paused {
			line += ", then pausing"
		}
	case "waiting":
		line = "waiting"
		if worker.Next != nil {
			line += fmt.Sprintf(", next pass in %s", max(worker.Next.Sub(now), 0).Round(time.Second))
		}
	default:
		line = worker.State
	}
	if worker.Removed {
		line += "; removed from the config, so it stops after this pass"
	}
	return line
}

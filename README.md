# loopd

loopd runs coding agents over and over. Each worker runs one pass, waits, and runs the next. A pass
is a fresh non-interactive session of Claude Code (`claude -p`) or Codex (`codex exec`), so
anything a worker needs to carry from one pass to the next has to live outside the session, for
example in a git repo.

## Install

```bash
CGO_ENABLED=0 GOBIN=~/.local/bin go install github.com/adamtopaz/loopd@latest
```

Or build from a clone:

```bash
nix develop        # or any Go 1.22+
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o loopd .
```

The result is a single static binary.

## Run

`loopd run` is the daemon. The other subcommands talk to it through its control socket.

```text
loopd run [config.json]   run the daemon (config defaults to $XDG_CONFIG_HOME/loopd/config.json)
loopd status [--json]     show the daemon and its workers
loopd reload              re-read the config and apply it
loopd pause <worker>      stop after the pass under way, if any
loopd resume <worker>     undo pause
loopd wake <worker>       start a pass now
loopd cancel <worker>     stop the pass under way
loopd claim <key> [note]  claim a key for the pass under way (only inside a pass)
loopd release <key>       release a key this pass claimed
loopd claims              list the claims that passes hold
```

The daemon stays in the foreground and logs one line per event to stdout. SIGINT or SIGTERM stops
it: it sends SIGTERM to any running pass, waits up to 10 seconds, then kills it. To run it as a
service, see [`loopd.service`](loopd.service).

The control socket is `$XDG_RUNTIME_DIR/loopd/loopd.sock`, or `$LOOPD_SOCKET` if that is set.
Only the user running loopd can open it, which includes any agent loopd runs as that user. Its
API is plain HTTP with JSON, so `curl --unix-socket <socket> http://loopd/status` works too.

`status` shows each worker's state (running a pass, waiting for the next, or paused), its settings,
how its last pass ended with the model Claude reports, cost and final message, and its current
log. Pausing lasts until loopd restarts. To stop a worker for good, remove it from the config.

## Config

See [`example.json`](example.json). loopd reads the file when it starts and on `loopd reload`, or
SIGHUP, which `systemctl --user reload loopd` sends. A reload applies changes without a restart:

- A new worker starts at once.
- A changed worker uses its new settings from its next pass. A pass already running carries on as
  it was. If the worker is waiting, its wait is recounted from the end of its last pass, so a
  shorter `every` takes effect straight away.
- A removed worker stops. If it is in the middle of a pass, it finishes that pass first.
- If the file is invalid, `loopd reload` says why and the previous config stays in force.

loopd only refuses to start if the config is missing or invalid. An empty `workers` list is
valid, and leaves loopd idle until workers are added.

| Field | Meaning |
|---|---|
| `logs` | Directory for pass logs. Default `$XDG_STATE_HOME/loopd/logs`, usually `~/.local/state/loopd/logs`. |
| `workers[].name` | Letters, digits, `.`, `_` and `-`. Must be unique. |
| `workers[].copies` | Run this many copies, named `<name>-1`, `<name>-2` and so on. Default 1. |
| `workers[].engine` | `claude` or `codex`. |
| `workers[].bin` | The engine's binary. Default: `claude` or `codex` on `PATH`. |
| `workers[].dir` | Working directory for the pass. |
| `workers[].prompt` | The prompt for each pass, such as `/supervisor` to run a Claude Code skill. |
| `workers[].model` | Model, such as `claude-opus-5-5`. Default: the engine's own setting. |
| `workers[].effort` | Reasoning effort, such as `xhigh`. Default: the engine's own setting. |
| `workers[].every` | Wait after a pass ends before the next starts, such as `"20m"`. |
| `workers[].timeout` | Longest a pass may run before loopd kills it. Default `"1h"`. |
| `workers[].args` | Extra command-line options for the engine. |
| `workers[].env` | Extra environment variables for the pass. |

A pass runs:

- `claude -p <prompt> --output-format stream-json --verbose [--model <model>] [--effort <effort>] <args>`
- `codex exec --json [--model <model>] [-c model_reasoning_effort="<effort>"] <args> -- <prompt>`

with `LOOPD_WORKER` set to the worker's name. Each pass runs in its own process group, and loopd
kills the group when the pass ends, so nothing the agent started keeps running. Its stdout goes to
`<logs>/<worker>/<time>.jsonl` and its stderr, if any, to `<time>.stderr`. After each pass loopd
logs the exit status, the model Claude reports, the cost or token use, and the first line of the
agent's final message.

## Several agents at once

Workers run side by side. To run several copies of one agent, give it `copies`. In `dir`,
`prompt`, `args` and `env` values, `{name}` becomes each copy's name and `{copy}` its number:

```json
{"name": "researcher", "copies": 2, "engine": "claude", "dir": "~/kb-{name}",
 "prompt": "/researcher {name}", "every": "5m"}
```

This runs `researcher-1` in `~/kb-researcher-1` and `researcher-2` in `~/kb-researcher-2`. Copies
that change files usually need a directory each, such as their own git clone.

Agents running at once coordinate through **claims**. Inside a pass, `loopd claim pr-31 reviewing`
claims the key `pr-31` for that pass, or fails with exit status 1 and says which worker holds it,
since when and why. A key is any line of text up to 200 characters, so a team can agree on names
such as `pr-31`, `issue-30` or `entry:hyperbolic-curve`. A pass can `loopd release` a key early.
Otherwise loopd releases all of a pass's claims when the pass ends, however it ends, so a claim
never outlives the agent that made it. `loopd claims` and `loopd status` show who holds what.

To make this work, each pass gets `LOOPD_PASS`, a token naming the pass, and `LOOPD_SOCKET`, and
the directory holding loopd comes first on its `PATH`. A Claude Code agent needs permission to
run the commands, such as `Bash(loopd claim *)`, `Bash(loopd release *)` and `Bash(loopd claims)`
in a skill's `allowed-tools`.

## Notes

- **Claude Code.** A skill's `allowed-tools` apply in print mode. A command that needs approval is
  refused rather than waiting for someone, and the pass carries on. Claude Code also refuses
  commands that expand a variable, such as `echo $LOOPD_WORKER`, so give Claude agents what they
  need to know in the prompt.
- **Codex.** Codex finds skills in `.agents/skills/`. Its sandbox defaults to read-only; set
  another with `args`, such as `["--sandbox", "workspace-write"]`. Note that `codex -p` selects a
  config profile; it is not print mode.

## License

Copyright 2026 Adam Topaz. Licensed under the [Apache License, Version 2.0](LICENSE).

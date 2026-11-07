# loopd

loopd runs coding agents over and over. Each worker runs one pass, waits, and runs the next. A pass
is a fresh non-interactive session of Claude Code (`claude -p`) or Codex (`codex exec`), so
anything a worker needs to carry from one pass to the next has to live outside the session, for
example in a git repo.

## Install

```bash
CGO_ENABLED=0 go install github.com/adamtopaz/loopd@latest
```

Or build from a clone:

```bash
nix develop        # or any Go 1.22+
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o loopd .
```

The result is a single static binary.

## Run

```bash
loopd run config.json
```

loopd stays in the foreground and logs one line per event to stdout. SIGINT or SIGTERM stops it:
it sends SIGTERM to any running pass, waits up to 10 seconds, then kills it. To run it as a
service, see [`loopd.service`](loopd.service).

## Config

See [`example.json`](example.json).

| Field | Meaning |
|---|---|
| `logs` | Directory for pass logs. Default `~/.loopd/logs`. |
| `workers[].name` | Letters, digits, `.`, `_` and `-`. Must be unique. |
| `workers[].engine` | `claude` or `codex`. |
| `workers[].bin` | The engine's binary. Default: `claude` or `codex` on `PATH`. |
| `workers[].dir` | Working directory for the pass. |
| `workers[].prompt` | The prompt for each pass, such as `/supervisor` to run a Claude Code skill. |
| `workers[].every` | Wait after a pass ends before the next starts, such as `"20m"`. |
| `workers[].timeout` | Longest a pass may run before loopd kills it. Default `"1h"`. |
| `workers[].args` | Extra command-line options for the engine. |
| `workers[].env` | Extra environment variables for the pass. |

A pass runs:

- `claude -p <prompt> --output-format stream-json --verbose <args>`
- `codex exec --json <args> -- <prompt>`

with `LOOPD_WORKER` set to the worker's name. Each pass runs in its own process group, and loopd
kills the group when the pass ends, so nothing the agent started keeps running. Its stdout goes to
`<logs>/<worker>/<time>.jsonl` and its stderr, if any, to `<time>.stderr`. After each pass loopd
logs the exit status, and the cost or token use and the first line of the agent's final message.

## Notes

- **Claude Code.** A skill's `allowed-tools` apply in print mode. A command that needs approval is
  refused rather than waiting for someone, and the pass carries on. Claude Code also refuses
  commands that expand a variable, such as `echo $LOOPD_WORKER`, so give Claude agents what they
  need to know in the prompt.
- **Codex.** Codex finds skills in `.agents/skills/`. Its sandbox defaults to read-only; set
  another with `args`, such as `["--sandbox", "workspace-write"]`. Note that `codex -p` selects a
  config profile; it is not print mode.

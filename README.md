# roostlabs/runner

The Roost Runner: the process that runs on a developer's own VPS, dials out to
Cloud, and executes tasks in disposable Docker sandboxes.

This is the open-source half of Roost, and it is open for a reason. The Runner is
the only component that ever touches your code or your credentials, so being able
to read it is the evidence that nothing leaks upward.

## Status

Early, but a task now runs end to end and an agent decides what it does. What is
missing is the last step: the work is left in the checkout rather than committed
and opened as a pull request.

Working today:

- outbound WebSocket channel to Cloud, with handshake, keepalive and
  backoff-with-jitter reconnect
- config loading that refuses a credentials file other accounts can read
- repositories cloned once and kept, with a fresh `git worktree` per task
- commands executed in a disposable container under CPU, memory and pid limits
- command output streamed with credential values masked
- append-only SQLite event journal with per-task sequence numbers
- `task_history`, `task_trace` and `config` queries answered from that journal
- one task at a time, cancellable, surviving a dropped channel
- an agent that reads the ticket, explores the repository, edits files and runs
  commands, through a session that cannot reach past the task's checkout
- every model call priced and journalled as an `llm_call` event, and a budget
  that stops the task rather than only reporting the overspend

Next: committing what the agent left and opening the pull request under a
service account with no merge rights.

## Build and run

```bash
go build ./cmd/runner
./runner -config /etc/roost/config.json
```

The config is JSON and must be mode `0600`:

```json
{
  "cloudUrl": "wss://api.example.com/channel",
  "token": "<from the dashboard>",
  "dataDir": "/var/lib/roost",
  "creds": {
    "git": "...",
    "taskManager": "...",
    "llm": "..."
  },
  "sandbox": {
    "image": "golang:1.26",
    "commands": [["go", "build", "./..."], ["go", "test", "./..."]],
    "network": "bridge"
  },
  "agent": {
    "model": "claude-opus-5",
    "effort": "xhigh",
    "maxSteps": 40,
    "budgetUsd": 5
  }
}
```

`sandbox.image` has no default: which image a task needs is a property of the
repository, not of the Runner.

`creds.llm` is the switch. With it set, an agent decides what the task does.
Without it the Runner falls back to running `sandbox.commands`, which is a real
mode rather than a stub: a repository whose build and test sequence is fixed
does not need a model to rediscover it every time. Those commands are argv
lists, not shell lines.

`agent.budgetUsd` caps a task when Cloud sends no budget of its own. Since what
a call will cost is not knowable before making it, the check is made on what has
already been spent, so a task can overshoot by one call and no more. Zero leaves
it uncapped, which is worth deciding deliberately.

`ROOST_CONFIG` overrides the config path, `ROOST_DATA_DIR` the data directory.

## What the design is protecting

**The Runner dials out.** Cloud never connects inward, so the VPS needs no open
ports and sits behind whatever NAT or firewall it likes. One connection carries
every stream.

**Credentials never go up.** They live in the config file, are injected into a
sandbox as environment variables for the length of one task, and are reported to
Cloud as booleans. `config.Creds.Values` exists so command output can be scrubbed
of them before it is streamed. Cloud can push a credential *down* in Managed
mode; this build refuses that, because Local mode is the default.

**The VPS owns the history.** Every event is written to the local SQLite journal
before it is streamed. A dropped channel therefore costs nothing: tasks keep
running, events keep accumulating, and on reconnect Cloud reports the last
sequence number it holds per task so the Runner can replay the rest. The channel
is observation and control, not life support.

**One task at a time.** The target machine is 1 CPU and 2 GB, so concurrency is 1
by default. Containers drop all capabilities, cannot gain new ones, get a
read-only root with the working copy as the only writable mount, and have swap
disabled so the memory limit is real rather than advisory.

**A clean baseline per task.** The clone persists, but no task works in it: each
gets a `git worktree` branched from the remote's head, so nothing inherits what
the last attempt left behind.

### The open risk

The sandbox has network access by default, because an agent has to reach the LLM
API and the git remote. That is also the route by which generated code could send
a repository somewhere it should not go. Narrowing it to an allowlist is open
work. `"network": "none"` is available today for tasks that need nothing external.

## Layout

| Path | What it does |
|---|---|
| `cmd/runner` | binary: flags, logging, message dispatch |
| `internal/channel` | outbound WebSocket, handshake, keepalive, reconnect |
| `internal/config` | local config and credentials, with permission checks |
| `internal/eventstore` | append-only SQLite journal and replay |
| `internal/repo` | persistent clones and per-task worktrees |
| `internal/sandbox` | disposable containers under limits |
| `internal/redact` | masks credential values in streamed output |
| `internal/llm` | the Anthropic Messages API, and what a call cost |
| `internal/agent` | decides a task's work; a model, or a fixed command list |
| `internal/executor` | runs a task, reports it throughout, and holds the budget |

The wire contract lives in [roostlabs/protocol](https://github.com/roostlabs/protocol).

## Dependencies

Two, both cgo-free, so the Runner stays a single static binary that an installer
can drop onto a box:

- `github.com/coder/websocket`
- `modernc.org/sqlite`

The Anthropic client is not among them. `internal/llm` speaks the HTTP API
directly, because the Runner uses a narrow slice of it — one endpoint, tool use,
and the token counts budgets are built from — and that slice is smaller than the
dependency would be.

## Tests

```bash
go test -race ./...
```

The channel tests run against a stub Cloud and the repo tests against a local
git repository, so neither needs a network. The sandbox tests that need a real
container are skipped when no Docker daemon answers; the rest of the suite still
covers the command line those containers would be started with, including the
assertion that no credential value ever appears in it.

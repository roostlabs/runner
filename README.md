# roostlabs/runner

The Roost Runner: the process that runs on a developer's own VPS, dials out to
Cloud, and executes tasks in disposable Docker sandboxes.

This is the open-source half of Roost, and it is open for a reason. The Runner is
the only component that ever touches your code or your credentials, so being able
to read it is the evidence that nothing leaks upward.

## Status

Early. The channel, the configuration and the event journal work; **sandbox
execution does not exist yet**, so a `task.run` is refused honestly rather than
silently dropped.

Working today:

- outbound WebSocket channel to Cloud, with handshake, keepalive and
  backoff-with-jitter reconnect
- config loading that refuses a credentials file other accounts can read
- append-only SQLite event journal with per-task sequence numbers
- `task_history`, `task_trace` and `config` queries answered from that journal

Next: Docker sandboxes with CPU and memory limits, `git worktree` baselines off
a persistent clone, the agent loop, and opening the pull request.

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
  }
}
```

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
by default and sandboxes carry explicit CPU and memory limits.

## Layout

| Path | What it does |
|---|---|
| `cmd/runner` | binary: flags, logging, message dispatch |
| `internal/channel` | outbound WebSocket, handshake, keepalive, reconnect |
| `internal/config` | local config and credentials, with permission checks |
| `internal/eventstore` | append-only SQLite journal and replay |

The wire contract lives in [roostlabs/protocol](https://github.com/roostlabs/protocol).

## Dependencies

Two, both cgo-free, so the Runner stays a single static binary that an installer
can drop onto a box:

- `github.com/coder/websocket`
- `modernc.org/sqlite`

## Tests

```bash
go test -race ./...
```

The channel tests run against a stub Cloud, so the handshake, the reconnect and
the rejection paths are exercised without a server.

# roostlabs/runner

The Roost Runner: the process that runs on a developer's own VPS, dials out to
Cloud, and executes tasks in disposable Docker sandboxes.

This is the open-source half of Roost, and it is open for a reason. The Runner is
the only component that ever touches your code or your credentials, so being able
to read it is the evidence that nothing leaks upward.

## Status

A ticket goes in and a pull request comes out. Early, but whole.

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
- an agent that repeats the same call and gets the same answer is warned once,
  then stopped, before the step limit or the budget would have caught it
- the work committed to the task's branch and opened as a pull request, under a
  service account that cannot merge it
- the ticket read from Jira or Linear when Cloud sends only its id, moved to
  an in-progress state when the task starts, and told where its pull request
  is when the task ends
- tickets picked up on their own: the Runner polls the tracker for a ready
  state and starts a task per ticket, so a ticket filed at night is a pull
  request in the morning without anyone opening the dashboard

- host and sandbox metrics — CPU, memory, disk, load, and each running
  container's share — streamed every five seconds, but only while a dashboard
  is subscribed

- alerts to Telegram and to a webhook when a task finishes, fails, stops at
  its budget or waits for approval, sent from the VPS so the bot token stays
  there with the other credentials
- optionally, a hold before anything leaves the server: with `agent.approvePr`
  on, the commit is made and the task waits for the developer to approve the
  pull request from the dashboard; declined work stays on its branch on the VPS

## Install

On a Linux box with Docker, from the dashboard's one-liner:

```bash
curl -fsSL https://raw.githubusercontent.com/roostlabs/runner/main/install.sh \
  | sh -s -- --token=<from the dashboard> --cloud-url=wss://<cloud>/channel
```

It checks for Docker, creates a `roost` service account, installs the binary,
writes `/etc/roost/config.json` at mode `0600` owned by that account, and starts
a systemd unit. `--dry-run` prints every step and changes nothing; `--help`
lists the rest.

The binary comes from the [latest release](https://github.com/roostlabs/runner/releases/latest),
Linux amd64 or arm64, and its checksum is verified before anything is
installed; `--version=<tag>` pins a release. `--binary=<path>` installs a
build of your own instead.

Two things it deliberately will not do. It refuses a `ws://` URL to a remote
host, because the token would cross the network in clear text. And it never
overwrites an existing config without `--force-config`, because that file holds
every credential on the box — a one-liner pasted twice must not wipe them.

A token passed as an argument is visible in the process list for as long as the
installer runs. On a machine where that matters, use `--token-file=<path>` or
the `ROOST_TOKEN` environment variable.

The systemd unit runs as the service account with `NoNewPrivileges`,
`ProtectSystem=strict`, `ProtectHome`, and the data directory as its only
writable path. It requires `docker.service`, because a task with no container
has nowhere to run.

macOS and Windows are not supported by the installer, and whether they ever
should be is still open. Build and run the binary directly there.

### With Docker Compose

For a box where compose is how services are run, `docker-compose.yml` runs the
same binary from `ghcr.io/roostlabs/runner` (amd64 and arm64, published with
every release) with git and the docker CLI beside it:

```bash
git clone https://github.com/roostlabs/runner && cd runner
cp config.example.json config/config.json && chmod 600 config/config.json   # then edit
echo "DOCKER_GID=$(stat -c %g /var/run/docker.sock)" > .env
sudo install -d -o 1000 -g 1000 /var/lib/roost
docker compose up -d
```

The container does not run Docker. It talks to the host daemon over the
mounted socket, so every sandbox is a sibling container on the host, started by
a Runner that happens to live in a container too. Two consequences follow, and
the compose file is built around them. The data directory is mounted at the
same path inside as on the host, because the Runner hands checkout paths to
`docker run -v` and the host daemon resolves them; set `ROOST_DATA_DIR` in
`.env` to move it, and leave `dataDir` out of the config. And the Runner runs
as uid 1000, not root, so `DOCKER_GID` has to be the group that owns the socket
on your host; it differs between distributions, which is why it is read rather
than written.

The Runner's own root filesystem is read-only, with `no-new-privileges`; the
config directory is mounted read-write because Managed mode writes credentials
into it. `make image` builds the same image locally, and `build:` in the
compose file swaps it in.

## Build and run

```bash
make build                 # ./roost-runner, static, version stamped
./roost-runner -config /etc/roost/config.json
make check                 # gofmt, vet, race tests
make dist                  # release tarballs with checksums, linux/amd64 and arm64
make lint-install          # shellcheck install.sh, in a container
```

The config is JSON and must be mode `0600`:

```json
{
  "cloudUrl": "wss://api.example.com/channel",
  "token": "<from the dashboard>",
  "dataDir": "/var/lib/roost",
  "creds": {
    "mode": "local",
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
    "budgetUsd": 5,
    "approvePr": false
  },
  "git": {
    "authorName": "Roost",
    "authorEmail": "roost@example.com",
    "forge": "gitlab",
    "apiBase": "https://git.example.com"
  },
  "tracker": {
    "kind": "jira",
    "baseUrl": "https://acme.atlassian.net",
    "user": "roost@example.com",
    "projects": {
      "APP": "https://git.example.com/acme/app.git",
      "WEB": "https://git.example.com/acme/web.git"
    },
    "pollIntervalSec": 60,
    "states": {
      "ready": "Ready for agent",
      "inProgress": "In Progress",
      "inReview": "In Review"
    }
  },
  "alerts": {
    "events": ["failed", "budget", "awaiting_approval", "done"],
    "telegram": { "token": "<from @BotFather>", "chatId": "<your chat>" },
    "webhook": { "url": "https://hooks.example.com/roost", "secret": "<shared>" }
  }
}
```

`sandbox.image` has no default: which image a task needs is a property of the
repository, not of the Runner.

`creds.mode` is `local` or `managed`, and an empty value means `local`.

In Local mode — the default and the recommendation — the values above are yours
to set on this machine. Cloud is told which slots are filled and nothing else,
and a credential arriving down the channel is refused rather than written.

In Managed mode you can paste a credential into the dashboard and it is relayed
down to this config instead. It buys you not needing shell access to rotate a
token; it costs you the value transiting Cloud on the way here, which Local mode
never does. That is why the switch is in this file rather than in the dashboard:
the transit is your decision, and Cloud granting itself the permission would not
be a permission. Changing the mode needs a restart. A credential change does not:
it is written to this file at `0600` and applied to the next task. A task already
running keeps the credentials it started with, and blocks the change until it
finishes — otherwise it would push as one identity and open a pull request as
another. An empty value clears a slot, which is how a revoked token is retired.

`creds.llm` is the switch. With it set, an agent decides what the task does.
Without it the Runner falls back to running `sandbox.commands`, which is a real
mode rather than a stub: a repository whose build and test sequence is fixed
does not need a model to rediscover it every time. Those commands are argv
lists, not shell lines.

`git.forge` is `github` or `gitlab`. Leave it empty for github.com and
gitlab.com, which are recognised by hostname. A self-hosted forge has to say
which one it is: guessing from a hostname would push a branch and then fail to
open anything on it, which is a worse failure than refusing before the push.

`git.apiBase` overrides the API root. For GitHub Enterprise it includes the
path — `https://git.example.com/api/v3` — and for a self-hosted GitLab it does
not, because GitLab's own paths already start with `/api/v4`.

`tracker` connects the task manager, using `creds.taskManager` as the
credential. `tracker.kind` is `jira` or `linear`. Jira needs `baseUrl` (the
site) and `user` (the email the API token belongs to, because Jira Cloud
authenticates a token as `email:token`); Linear needs neither. `project` is the
Jira project key or the Linear team key.

With a tracker configured, a task whose ticket names that tracker as its
provider is handled end to end: when Cloud sends only the ticket's id, its title
and description are read from the tracker before anything else happens; the
ticket is moved to `states.inProgress` when the task starts; and when a pull
request is open, a comment with its link is left and the ticket moves to
`states.inReview`. A task that ends without a pull request — nothing to change,
or a failure — leaves a comment saying so. Both states are optional and named
as you see them in the tracker, not by id. A ticket typed into the dashboard
by hand carries the provider `manual` and is nobody's to update.

`states.ready` turns polling on. Every `pollIntervalSec` (default 60) the
Runner lists each project's tickets in that state and starts a task for the
first one it has not tried recently, against that project's repository.
`tracker.projects` maps a Jira project key or Linear team key to a git remote
url; `tracker.project` with `tracker.repo` is the one-project shorthand and
counts as one more entry. One tracker, one token, several repositories on the
same VPS, still one task at a time. A project the tracker cannot list is
skipped for that round and the others are still asked. Polling needs
`states.inProgress`: moving the ticket is what stops it being picked up again
on the next round, so a config with `ready` and no `inProgress` is refused. A
ticket that stays ready — the tracker refused the transition, the repository
was unreachable — is retried after ten minutes rather than every minute. The
VPS has no open port, so this is the Runner's own trigger: the tracker cannot
call in, the Runner asks.

`agent.budgetUsd` caps a task when Cloud sends no budget of its own. Since what
a call will cost is not knowable before making it, the check is made on what has
already been spent, so a task can overshoot by one call and no more. Zero leaves
it uncapped, which is worth deciding deliberately.

`agent.maxSteps` (default 40) bounds the conversation. Before that limit is
reached, a loop that repeats itself is caught earlier: a tool call that comes
back with the same result it already produced earns the model a warning in the
result, and the third identical pass fails the task as looping. The same
command with a different output counts as progress; `write_file` is never
counted, since rewriting a file is how a model retries.

`agent.approvePr` adds a human step before the only moment the Runner itself
acts outside the server. The agent's work is committed on the task branch,
then the task is reported as `awaiting_approval` with the agent's title and
summary, and nothing is pushed until the dashboard answers. Approved, it goes
on to the pull request; declined, the task fails with that reason, the commit
stays in the clone on the VPS, and the ticket is told. It is off by default
because the service account cannot merge, so a pull request is already a review
step; this is for the developer who wants to read the change before it exists
anywhere but their own machine. A waiting task holds the single execution slot,
so it gives up after `agent.approvalTimeoutSec` (a day when unset).

`alerts` is how the Runner reaches the developer who is not watching the
dashboard: a task that opened a pull request, failed, stopped at its budget,
or is waiting to be approved. Two sinks, both optional. `telegram` posts to one
chat through a bot made with @BotFather (message the bot once, then read your
chat id from its `getUpdates`). `webhook` posts the event as JSON to one URL,
with the same text a chat would show under `text`, and signs the body with
`X-Roost-Signature: sha256=<hmac>` when a `secret` is set, so the receiver can
tell the Runner's posts from anyone else's; the URL must be `https` unless it
is loopback. `events` narrows the set. The alerts are sent from the VPS, not
from Cloud, because a bot token is a credential and credentials live on the
VPS; the token and the secret are added to the redaction filter like the
rest. Delivery is best effort: a sink that is down costs a log line, never a
task. A cancelled task sends nothing, since the developer did that themselves.

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
the last attempt left behind. Git hooks are disabled throughout, because a
repository's hooks are files in the tree being checked out and git would run
them on the host, outside the container everything else is confined to.

**A pull request, never a merge.** The work lands on a branch under `roost/` and
is opened for review by a service account. That account needs to push and to
open pull requests; it should not be able to merge one. Nothing in the Runner
can, either — on GitLab it explicitly declines to remove the source branch,
because that branch is the task's output.

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
| `internal/metrics` | reads host usage from /proc and container usage from docker |
| `internal/forge` | opens the pull request the work becomes, on GitHub or GitLab |
| `internal/tracker` | reads, moves and comments on the ticket, in Jira or Linear |
| `internal/alert` | tells the developer, over Telegram or a webhook, when a task needs them |
| `internal/agent` | decides a task's work; a model, or a fixed command list |
| `internal/executor` | runs a task, reports it throughout, and holds the budget |
| `install.sh` | the one-liner installer: prerequisites, service account, systemd unit |
| `packaging` | tests that run the installer in a throwaway container |

The wire contract lives in [roostlabs/protocol](https://github.com/roostlabs/protocol).

## License

Apache-2.0. See `LICENSE`.

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

The installer is tested by running it: a throwaway Debian container, a
cross-compiled binary, and `docker` and `systemctl` stubbed out, asserting the
file modes, the ownership, the unit's contents, and that a second run leaves an
existing config alone. Those tests are skipped when no Docker daemon answers.

The same checks run in GitHub Actions on every push and pull request
(`.github/workflows/ci.yml`), and a pushed tag `v*` builds the release tarballs
and publishes them with their checksums (`.github/workflows/release.yml`), so a
release is a tag and nothing more.

The channel tests run against a stub Cloud and the repo tests against a local
git repository, so neither needs a network. The sandbox tests that need a real
container are skipped when no Docker daemon answers; the rest of the suite still
covers the command line those containers would be started with, including the
assertion that no credential value ever appears in it.

package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/agent"
	"github.com/roostlabs/runner/internal/eventstore"
	"github.com/roostlabs/runner/internal/redact"
	"github.com/roostlabs/runner/internal/repo"
	"github.com/roostlabs/runner/internal/sandbox"
)

// recorder captures everything a task reported.
type recorder struct {
	mu      sync.Mutex
	events  []protocol.TaskEvent
	seqs    []uint64
	states  []protocol.TaskState
	results []protocol.TaskResult
	fail    bool // report failures, to prove they do not stop a task
}

func (r *recorder) TaskEvent(_ context.Context, _ string, seq uint64, ev protocol.TaskEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	r.seqs = append(r.seqs, seq)
	if r.fail {
		return errors.New("cloud is unreachable")
	}
	return nil
}

func (r *recorder) TaskState(_ context.Context, _ string, st protocol.TaskState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, st)
	if r.fail {
		return errors.New("cloud is unreachable")
	}
	return nil
}

func (r *recorder) TaskResult(_ context.Context, _ string, res protocol.TaskResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, res)
	if r.fail {
		return errors.New("cloud is unreachable")
	}
	return nil
}

func (r *recorder) kinds() []protocol.EventKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]protocol.EventKind, len(r.events))
	for i, ev := range r.events {
		out[i] = ev.Event
	}
	return out
}

func (r *recorder) statuses() []protocol.TaskStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]protocol.TaskStatus, len(r.states))
	for i, st := range r.states {
		out[i] = st.State
	}
	return out
}

func (r *recorder) output() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var sb strings.Builder
	for _, ev := range r.events {
		if ev.Event != protocol.EventCmdOutput {
			continue
		}
		var p protocol.CmdOutputPayload
		if err := json.Unmarshal(ev.Payload, &p); err == nil {
			sb.WriteString(p.Chunk)
		}
	}
	return sb.String()
}

// origin builds a repository to clone from, so these tests need git but no
// network and no Docker.
func origin(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("origin\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")
	return dir
}

type fixture struct {
	ex    *Executor
	store *eventstore.Store
	task  protocol.TaskRun
}

func newFixture(t *testing.T, cfg Config) fixture {
	t.Helper()

	store, err := eventstore.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("eventstore.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	if cfg.Repos == nil {
		cfg.Repos = repo.New(t.TempDir(), "")
	}
	cfg.Store = store
	if cfg.Agent == nil {
		cfg.Agent = agent.FixedFromArgv([][]string{{"true"}})
	}
	if cfg.Sandbox.Image == "" {
		cfg.Sandbox.Image = "alpine:3"
	}
	if cfg.Run == nil {
		cfg.Run = func(context.Context, sandbox.Spec, []string, io.Writer, io.Writer) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 0}, nil
		}
	}

	return fixture{
		ex:    New(cfg),
		store: store,
		task:  protocol.TaskRun{TaskID: "T-1", Repo: origin(t)},
	}
}

func TestRunReportsAFullTrace(t *testing.T) {
	f := newFixture(t, Config{
		Agent: agent.FixedFromArgv([][]string{{"go", "build", "./..."}, {"go", "test", "./..."}}),
		Run: func(_ context.Context, _ sandbox.Spec, argv []string, stdout, _ io.Writer) (sandbox.Result, error) {
			stdout.Write([]byte("ran " + argv[0] + "\n"))
			return sandbox.Result{ExitCode: 0, Duration: time.Millisecond}, nil
		},
	})
	rec := &recorder{}

	if err := f.ex.Run(context.Background(), f.task, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Two stages, then for each of the two commands: what the agent said it
	// was doing, and the command's own start/output/exit.
	wantKinds := []protocol.EventKind{
		protocol.EventStage,
		protocol.EventStage,
		protocol.EventAgentStep,
		protocol.EventCmdStart, protocol.EventCmdOutput, protocol.EventCmdExit,
		protocol.EventAgentStep,
		protocol.EventCmdStart, protocol.EventCmdOutput, protocol.EventCmdExit,
	}
	if got := f.kindsMatch(rec, wantKinds); !got {
		t.Errorf("event kinds = %v, want %v", rec.kinds(), wantKinds)
	}
	if got, want := rec.statuses(), []protocol.TaskStatus{
		protocol.TaskPreparing, protocol.TaskRunning, protocol.TaskDone,
	}; !equalStatuses(got, want) {
		t.Errorf("states = %v, want %v", got, want)
	}
	if len(rec.results) != 1 {
		t.Errorf("got %d results, want exactly 1", len(rec.results))
	}
}

func (f fixture) kindsMatch(rec *recorder, want []protocol.EventKind) bool {
	got := rec.kinds()
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equalStatuses(a, b []protocol.TaskStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Every event has to be in the journal before it is streamed, or a dropped
// channel would lose history the VPS is supposed to own.
func TestRunJournalsEveryEventItStreams(t *testing.T) {
	f := newFixture(t, Config{})
	rec := &recorder{}

	if err := f.ex.Run(context.Background(), f.task, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stored, err := f.store.Since(context.Background(), "T-1", 0, 0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(stored) != len(rec.events) {
		t.Fatalf("journalled %d events but streamed %d", len(stored), len(rec.events))
	}
	for i, ev := range stored {
		if ev.Seq != rec.seqs[i] {
			t.Errorf("event %d journalled as seq %d but streamed as %d", i, ev.Seq, rec.seqs[i])
		}
	}
}

// The channel is observation, not life support: a task runs to completion with
// Cloud unreachable, and the journal keeps everything for the replay.
func TestRunSurvivesAnUnreachableCloud(t *testing.T) {
	f := newFixture(t, Config{})
	rec := &recorder{fail: true}

	if err := f.ex.Run(context.Background(), f.task, rec); err != nil {
		t.Fatalf("Run failed because reporting failed: %v", err)
	}
	stored, err := f.store.Since(context.Background(), "T-1", 0, 0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(stored) == 0 {
		t.Error("nothing was journalled")
	}
}

// Command output is where a token most plausibly escapes.
func TestRunMasksCredentialsInOutput(t *testing.T) {
	const secret = "ghp_realtokenvalue"

	f := newFixture(t, Config{
		Filter: redact.New(secret),
		Env:    []string{"ROOST_GIT_TOKEN=" + secret},
		Run: func(_ context.Context, _ sandbox.Spec, _ []string, stdout, _ io.Writer) (sandbox.Result, error) {
			// Split across writes, as a real stream would be.
			stdout.Write([]byte("cloning with ghp_re"))
			stdout.Write([]byte("altokenvalue now\n"))
			return sandbox.Result{ExitCode: 0}, nil
		},
	})
	rec := &recorder{}

	if err := f.ex.Run(context.Background(), f.task, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	streamed := rec.output()
	if strings.Contains(streamed, secret) {
		t.Errorf("the token reached the wire: %q", streamed)
	}
	if !strings.Contains(streamed, redact.Mask) {
		t.Errorf("output was not masked at all: %q", streamed)
	}

	// The journal is on the VPS, but there is no reason for it to hold the
	// token either.
	stored, _ := f.store.Since(context.Background(), "T-1", 0, 0)
	for _, ev := range stored {
		if strings.Contains(string(ev.Payload), secret) {
			t.Errorf("the token was journalled: %s", ev.Payload)
		}
	}
}

func TestRunStopsAtTheFirstFailingStep(t *testing.T) {
	var ran int
	f := newFixture(t, Config{
		Agent: agent.FixedFromArgv([][]string{{"first"}, {"second"}, {"third"}}),
		Run: func(_ context.Context, _ sandbox.Spec, argv []string, _, _ io.Writer) (sandbox.Result, error) {
			ran++
			if argv[0] == "second" {
				return sandbox.Result{ExitCode: 2}, nil
			}
			return sandbox.Result{ExitCode: 0}, nil
		},
	})
	rec := &recorder{}

	err := f.ex.Run(context.Background(), f.task, rec)
	if err == nil {
		t.Fatal("Run returned nil for a failing step")
	}
	if ran != 2 {
		t.Errorf("ran %d steps, want it to stop after the failing one", ran)
	}
	if got := rec.statuses(); got[len(got)-1] != protocol.TaskFailed {
		t.Errorf("final state = %q, want %q", got[len(got)-1], protocol.TaskFailed)
	}
}

// Concurrency is 1: the target machine cannot host two sandboxes.
func TestRunRefusesASecondTask(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	f := newFixture(t, Config{
		Run: func(context.Context, sandbox.Spec, []string, io.Writer, io.Writer) (sandbox.Result, error) {
			close(started)
			<-release
			return sandbox.Result{ExitCode: 0}, nil
		},
	})

	done := make(chan error, 1)
	go func() { done <- f.ex.Run(context.Background(), f.task, &recorder{}) }()
	<-started

	if id, busy := f.ex.Active(); !busy || id != "T-1" {
		t.Errorf("Active() = %q, %v; want T-1, true", id, busy)
	}
	second := protocol.TaskRun{TaskID: "T-2", Repo: f.task.Repo}
	if err := f.ex.Run(context.Background(), second, &recorder{}); !errors.Is(err, ErrBusy) {
		t.Errorf("second Run error = %v, want %v", err, ErrBusy)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, busy := f.ex.Active(); busy {
		t.Error("the slot was still held after the task finished")
	}
}

func TestCancelStopsTheRunningTask(t *testing.T) {
	started := make(chan struct{})

	f := newFixture(t, Config{
		Run: func(ctx context.Context, _ sandbox.Spec, _ []string, _, _ io.Writer) (sandbox.Result, error) {
			close(started)
			<-ctx.Done()
			return sandbox.Result{ExitCode: -1}, ctx.Err()
		},
	})
	rec := &recorder{}

	done := make(chan error, 1)
	go func() { done <- f.ex.Run(context.Background(), f.task, rec) }()
	<-started

	if !f.ex.Cancel("T-1") {
		t.Fatal("Cancel reported that T-1 was not running")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run error = %v, want context.Canceled", err)
	}
	if got := rec.statuses(); got[len(got)-1] != protocol.TaskCancelled {
		t.Errorf("final state = %q, want %q", got[len(got)-1], protocol.TaskCancelled)
	}
}

func TestCancelIgnoresAnotherTask(t *testing.T) {
	f := newFixture(t, Config{})
	if f.ex.Cancel("T-nope") {
		t.Error("Cancel reported success for a task that is not running")
	}
}

// A cancelled task must still tidy up, or every cancel leaks a checkout.
func TestCancelledTaskStillRemovesItsWorktree(t *testing.T) {
	started := make(chan struct{})
	paths := make(chan string, 1)

	f := newFixture(t, Config{
		Run: func(ctx context.Context, spec sandbox.Spec, _ []string, _, _ io.Writer) (sandbox.Result, error) {
			paths <- spec.HostPath
			close(started)
			<-ctx.Done()
			return sandbox.Result{}, ctx.Err()
		},
	})

	done := make(chan error, 1)
	go func() { done <- f.ex.Run(context.Background(), f.task, &recorder{}) }()
	<-started

	f.ex.Cancel("T-1")
	<-done

	path := <-paths
	if _, err := os.Stat(path); err == nil {
		t.Errorf("worktree %s survived the cancellation", path)
	}
}

func TestRunRejectsATaskWithNoRepo(t *testing.T) {
	f := newFixture(t, Config{})
	err := f.ex.Run(context.Background(), protocol.TaskRun{TaskID: "T-1"}, &recorder{})
	if err == nil {
		t.Error("Run accepted a task with no repo")
	}
}

func TestRunRejectsATaskWithNoID(t *testing.T) {
	f := newFixture(t, Config{})
	if err := f.ex.Run(context.Background(), protocol.TaskRun{Repo: "x"}, &recorder{}); err == nil {
		t.Error("Run accepted a task with no id")
	}
}

func TestRunRequiresAnImage(t *testing.T) {
	f := newFixture(t, Config{Sandbox: sandbox.Spec{}})
	// newFixture fills in a default image, so clear it through a fresh executor.
	ex := New(Config{
		Repos: repo.New(t.TempDir(), ""),
		Store: f.store,
		Agent: agent.FixedFromArgv([][]string{{"true"}}),
		Run: func(context.Context, sandbox.Spec, []string, io.Writer, io.Writer) (sandbox.Result, error) {
			return sandbox.Result{}, nil
		},
	})

	err := ex.Run(context.Background(), f.task, &recorder{})
	if err == nil || !strings.Contains(err.Error(), "image") {
		t.Errorf("Run error = %v, want it to name the missing image", err)
	}
}

func TestKeepWorktreeLeavesTheCheckout(t *testing.T) {
	paths := make(chan string, 1)
	f := newFixture(t, Config{
		KeepWorktree: true,
		Run: func(_ context.Context, spec sandbox.Spec, _ []string, _, _ io.Writer) (sandbox.Result, error) {
			paths <- spec.HostPath
			return sandbox.Result{ExitCode: 0}, nil
		},
	})

	if err := f.ex.Run(context.Background(), f.task, &recorder{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	path := <-paths
	if _, err := os.Stat(path); err != nil {
		t.Errorf("worktree %s was removed despite KeepWorktree: %v", path, err)
	}
}

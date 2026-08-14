// Package executor runs a task from ticket to finished trace.
//
// It is the piece that holds the Runner's promises together: one task at a time,
// a clean baseline for each, execution only inside a container, every event
// journalled before it is streamed, and no credential in the output.
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/agent"
	"github.com/roostlabs/runner/internal/eventstore"
	"github.com/roostlabs/runner/internal/redact"
	"github.com/roostlabs/runner/internal/repo"
	"github.com/roostlabs/runner/internal/sandbox"
)

// ErrBusy means a task is already running. Concurrency is 1 by default because
// the target machine is one core and two gigabytes.
var ErrBusy = errors.New("executor: a task is already running")

// cleanupTimeout bounds tearing a worktree down after the task ended.
const cleanupTimeout = time.Minute

// Reporter is where a running task is announced. The channel implements it.
//
// Every method may fail without the task failing: Cloud being unreachable is
// not a reason to stop working, since the journal will replay on reconnect.
type Reporter interface {
	TaskEvent(ctx context.Context, taskID string, seq uint64, ev protocol.TaskEvent) error
	TaskState(ctx context.Context, taskID string, st protocol.TaskState) error
	TaskResult(ctx context.Context, taskID string, res protocol.TaskResult) error
}

// RunFunc executes one command in a sandbox. It exists as a field so tests can
// drive the executor without a Docker daemon.
type RunFunc func(ctx context.Context, spec sandbox.Spec, argv []string, stdout, stderr io.Writer) (sandbox.Result, error)

// Config assembles an Executor.
type Config struct {
	Repos *repo.Manager
	Store *eventstore.Store
	Agent agent.Agent
	// Filter masks credential values in command output. Nil masks nothing,
	// which is correct only when there are no credentials.
	Filter *redact.Filter
	// Sandbox is the template for every container: image and limits. HostPath
	// and Env are filled in per task.
	Sandbox sandbox.Spec
	// Env are the credentials to expose to the sandbox, as KEY=VALUE.
	Env []string
	// KeepWorktree leaves the checkout on disk after the task, for debugging.
	KeepWorktree bool
	// Run defaults to sandbox.Run.
	Run    RunFunc
	Logger *slog.Logger
}

// Executor runs tasks.
type Executor struct {
	cfg Config
	log *slog.Logger

	mu     sync.Mutex
	active *active
}

type active struct {
	id     string
	cancel context.CancelFunc
}

// New returns an Executor.
func New(cfg Config) *Executor {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Run == nil {
		cfg.Run = sandbox.Run
	}
	return &Executor{cfg: cfg, log: cfg.Logger}
}

// Active reports the running task, if any.
func (e *Executor) Active() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active == nil {
		return "", false
	}
	return e.active.id, true
}

// Cancel stops taskID if it is the one running, reporting whether it was.
func (e *Executor) Cancel(taskID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active == nil || e.active.id != taskID {
		return false
	}
	e.active.cancel()
	return true
}

// Run carries out a task and reports it throughout.
//
// It returns ErrBusy when another task holds the single execution slot, and
// otherwise returns whatever made the task fail. The failure is already
// reported by the time it returns; the error is for the caller's own logging.
func (e *Executor) Run(ctx context.Context, task protocol.TaskRun, rep Reporter) error {
	if task.TaskID == "" {
		return errors.New("executor: task has no id")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	e.mu.Lock()
	if e.active != nil {
		running := e.active.id
		e.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrBusy, running)
	}
	e.active = &active{id: task.TaskID, cancel: cancel}
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		e.active = nil
		e.mu.Unlock()
	}()

	start := time.Now()
	err := e.execute(ctx, task, rep)
	duration := time.Since(start)

	// Reporting outlives the task's own context: a cancelled task still has to
	// say that it was cancelled.
	reportCtx, cancelReport := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancelReport()

	switch {
	case err == nil:
		e.state(reportCtx, task.TaskID, protocol.TaskDone, "", rep)
	case errors.Is(err, context.Canceled):
		e.state(reportCtx, task.TaskID, protocol.TaskCancelled, "cancelled", rep)
	default:
		e.emit(reportCtx, task.TaskID, protocol.EventError, protocol.EventErrorPayload{Msg: err.Error()}, rep)
		e.state(reportCtx, task.TaskID, protocol.TaskFailed, err.Error(), rep)
	}

	if repErr := rep.TaskResult(reportCtx, task.TaskID, protocol.TaskResult{
		DurationMs: duration.Milliseconds(),
	}); repErr != nil {
		e.log.Debug("could not report the result", "taskId", task.TaskID, "err", repErr)
	}
	return err
}

func (e *Executor) execute(ctx context.Context, task protocol.TaskRun, rep Reporter) error {
	if task.Repo == "" {
		return errors.New("executor: task has no repo")
	}

	e.state(ctx, task.TaskID, protocol.TaskPreparing, "", rep)
	e.emit(ctx, task.TaskID, protocol.EventStage, protocol.StagePayload{Name: "prepare-repo"}, rep)

	prepared, err := e.cfg.Repos.Prepare(ctx, task.Repo, "")
	if err != nil {
		return err
	}
	worktree, err := prepared.Worktree(ctx, task.TaskID)
	if err != nil {
		return err
	}
	if !e.cfg.KeepWorktree {
		defer func() {
			// Detached from ctx on purpose: a cancelled task still has to clean
			// up after itself, or a cancel would leak a checkout every time.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			if err := worktree.Remove(cleanupCtx); err != nil {
				e.log.Warn("could not remove the worktree",
					"taskId", task.TaskID, "path", worktree.Path, "err", err)
			}
		}()
	}

	steps, err := e.cfg.Agent.Plan(ctx, task, worktree.Path)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return errors.New("executor: the agent produced no steps")
	}

	spec := e.cfg.Sandbox
	spec.HostPath = worktree.Path
	spec.Env = e.cfg.Env
	if spec.Image == "" {
		return errors.New("executor: no sandbox image configured; set sandbox.image in the runner config")
	}
	if task.TimeoutMs > 0 {
		spec.Timeout = time.Duration(task.TimeoutMs) * time.Millisecond
	}

	e.state(ctx, task.TaskID, protocol.TaskRunning, "", rep)
	e.emit(ctx, task.TaskID, protocol.EventStage, protocol.StagePayload{Name: "execute"}, rep)

	for i, step := range steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.runStep(ctx, task.TaskID, i, step, spec, rep); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) runStep(ctx context.Context, taskID string, index int, step agent.Step, spec sandbox.Spec, rep Reporter) error {
	cmdID := fmt.Sprintf("c%d", index+1)
	e.emit(ctx, taskID, protocol.EventCmdStart, protocol.CmdStartPayload{
		CmdID: cmdID,
		Argv:  step.Argv,
		Dir:   spec.WorkDir,
	}, rep)

	stdout := e.output(ctx, taskID, cmdID, "stdout", rep)
	stderr := e.output(ctx, taskID, cmdID, "stderr", rep)

	result, runErr := e.cfg.Run(ctx, spec, step.Argv, stdout, stderr)

	// The redaction writers hold back a tail so a credential cannot be split
	// across two chunks; without these flushes that tail is simply lost.
	stdout.Flush()
	stderr.Flush()

	e.emit(ctx, taskID, protocol.EventCmdExit, protocol.CmdExitPayload{
		CmdID:      cmdID,
		Code:       result.ExitCode,
		DurationMs: result.Duration.Milliseconds(),
	}, rep)

	if runErr != nil {
		return runErr
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("executor: %s exited with code %d", step.Name, result.ExitCode)
	}
	return nil
}

// output returns a writer that masks credentials and turns what survives into
// cmd_output events.
func (e *Executor) output(ctx context.Context, taskID, cmdID, stream string, rep Reporter) *outputWriter {
	sink := &chunkSink{ex: e, ctx: ctx, taskID: taskID, cmdID: cmdID, stream: stream, rep: rep}
	return &outputWriter{masked: e.cfg.Filter.Writer(sink)}
}

type outputWriter struct {
	masked *redact.StreamWriter
}

func (w *outputWriter) Write(p []byte) (int, error) { return w.masked.Write(p) }

// Flush releases the tail the redaction writer was holding.
func (w *outputWriter) Flush() { w.masked.Flush() }

// chunkSink turns masked output into events.
type chunkSink struct {
	ex            *Executor
	ctx           context.Context
	taskID, cmdID string
	stream        string
	rep           Reporter
}

// Write never fails the command. Output that cannot be journalled or streamed is
// a reporting problem, and killing a task over one would make the channel life
// support, which it is not.
func (s *chunkSink) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.ex.emit(s.ctx, s.taskID, protocol.EventCmdOutput, protocol.CmdOutputPayload{
		CmdID:  s.cmdID,
		Stream: s.stream,
		Chunk:  string(p),
	}, s.rep)
	return len(p), nil
}

// emit journals an event and then streams it.
//
// The order is the invariant the history model rests on. The journal write is
// detached from the task's context so the reason a cancelled task stopped is
// still recorded.
func (e *Executor) emit(ctx context.Context, taskID string, kind protocol.EventKind, payload any, rep Reporter) {
	raw, err := json.Marshal(payload)
	if err != nil {
		e.log.Error("could not encode an event", "taskId", taskID, "kind", kind, "err", err)
		return
	}

	journalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	seq, err := e.cfg.Store.Append(journalCtx, taskID, kind, raw, time.Now().UnixMilli())
	if err != nil {
		e.log.Error("could not journal an event", "taskId", taskID, "kind", kind, "err", err)
		return
	}
	if err := rep.TaskEvent(ctx, taskID, seq, protocol.TaskEvent{Event: kind, Payload: raw}); err != nil {
		e.log.Debug("could not stream an event", "taskId", taskID, "seq", seq, "err", err)
	}
}

func (e *Executor) state(ctx context.Context, taskID string, status protocol.TaskStatus, reason string, rep Reporter) {
	if err := rep.TaskState(ctx, taskID, protocol.TaskState{State: status, Reason: reason}); err != nil {
		e.log.Debug("could not report a state change", "taskId", taskID, "state", status, "err", err)
	}
}

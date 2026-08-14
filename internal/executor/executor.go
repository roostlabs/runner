// Package executor runs a task from ticket to finished trace.
//
// It is the piece that holds the Runner's promises together: one task at a time,
// a clean baseline for each, execution only inside a container, every event
// journalled before it is streamed, no credential in the output, and every
// model call priced and charged against the task's budget.
//
// The agent is handed a Session rather than any of these components, so the
// promises are not something an agent has to cooperate with.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/agent"
	"github.com/roostlabs/runner/internal/eventstore"
	"github.com/roostlabs/runner/internal/forge"
	"github.com/roostlabs/runner/internal/llm"
	"github.com/roostlabs/runner/internal/redact"
	"github.com/roostlabs/runner/internal/repo"
	"github.com/roostlabs/runner/internal/sandbox"
)

// ErrBusy means a task is already running. Concurrency is 1 by default because
// the target machine is one core and two gigabytes.
var ErrBusy = errors.New("executor: a task is already running")

// cleanupTimeout bounds tearing a worktree down after the task ended.
const cleanupTimeout = time.Minute

// maxCapture is how much of a command's output is kept to hand back to the
// agent. Everything is still streamed and journalled; this only bounds what one
// tool result costs to send to the model.
const maxCapture = 32 << 10

// maxFileRead bounds one read_file. A file larger than this is not something to
// put in a prompt whole.
const maxFileRead = 256 << 10

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

// PRFunc opens a pull request. It is a field for the same reason RunFunc is.
type PRFunc func(ctx context.Context, req forge.Request) (forge.PR, error)

// Config assembles an Executor.
type Config struct {
	Repos *repo.Manager
	Store *eventstore.Store
	Agent agent.Agent
	// LLM answers the agent's model calls. Nil is valid for an agent that does
	// not use one, and any call from an agent that does is an error.
	LLM *llm.Client
	// BudgetUSD caps what one task may spend when the task itself names no
	// budget. Zero means uncapped.
	BudgetUSD float64
	// Forge opens the pull request a finished task becomes. Nil means a task
	// that changed something has nowhere to put it, which is reported as an
	// error rather than left as a commit nobody will see.
	Forge PRFunc
	// Author is who the Runner's commits are attributed to. It is a service
	// account: a commit claiming to be the developer's would put their name on
	// work they have not read.
	Author repo.Author
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

	budget := task.BudgetUSD
	if budget <= 0 {
		budget = e.cfg.BudgetUSD
	}
	sess := &session{ex: e, task: task, rep: rep, budget: budget}

	start := time.Now()
	err := e.execute(ctx, task, rep, sess)
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

	cost, tokens, prURL := sess.totals()
	if repErr := rep.TaskResult(reportCtx, task.TaskID, protocol.TaskResult{
		PRURL:      prURL,
		CostUSD:    cost,
		Tokens:     tokens,
		DurationMs: duration.Milliseconds(),
	}); repErr != nil {
		e.log.Debug("could not report the result", "taskId", task.TaskID, "err", repErr)
	}
	return err
}

func (e *Executor) execute(ctx context.Context, task protocol.TaskRun, rep Reporter, sess *session) error {
	if task.Repo == "" {
		return errors.New("executor: task has no repo")
	}
	if e.cfg.Agent == nil {
		return errors.New("executor: no agent configured")
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

	spec := e.cfg.Sandbox
	spec.HostPath = worktree.Path
	spec.Env = e.cfg.Env
	if spec.Image == "" {
		return errors.New("executor: no sandbox image configured; set sandbox.image in the runner config")
	}
	if task.TimeoutMs > 0 {
		spec.Timeout = time.Duration(task.TimeoutMs) * time.Millisecond
	}

	sess.spec = spec
	sess.workDir = worktree.Path

	e.state(ctx, task.TaskID, protocol.TaskRunning, "", rep)
	e.emit(ctx, task.TaskID, protocol.EventStage, protocol.StagePayload{Name: "execute"}, rep)

	result, err := e.cfg.Agent.Run(ctx, task, sess)
	if err != nil {
		return err
	}
	e.log.Info("agent finished", "taskId", task.TaskID, "title", result.Title)
	return e.publish(ctx, task, prepared, worktree, result, sess, rep)
}

// publish turns what the agent left in the checkout into a pull request.
//
// Nothing is merged and nothing reaches the default branch. What the developer
// asked for is a change waiting for review in the morning, and that is the
// whole of what this does.
func (e *Executor) publish(
	ctx context.Context,
	task protocol.TaskRun,
	prepared *repo.Repo,
	worktree *repo.Worktree,
	result agent.Result,
	sess *session,
	rep Reporter,
) error {
	e.emit(ctx, task.TaskID, protocol.EventStage, protocol.StagePayload{Name: "commit"}, rep)

	changed, err := worktree.Commit(ctx, commitMessage(task, result), e.cfg.Author)
	if err != nil {
		return err
	}
	if !changed {
		// An agent that read the ticket and found nothing to change has done
		// its job. Forcing a commit out of that would open a pull request with
		// nothing in it.
		e.emit(ctx, task.TaskID, protocol.EventStage, protocol.StagePayload{Name: "no-changes"}, rep)
		e.log.Info("the agent changed nothing", "taskId", task.TaskID)
		return nil
	}
	if e.cfg.Forge == nil {
		return errors.New("executor: the agent made changes but no forge is configured to open a pull request")
	}

	e.emit(ctx, task.TaskID, protocol.EventStage, protocol.StagePayload{Name: "open-pull-request"}, rep)
	if err := worktree.Push(ctx); err != nil {
		return err
	}

	pr, err := e.cfg.Forge(ctx, forge.Request{
		RemoteURL: task.Repo,
		Base:      prepared.DefaultBranch,
		Head:      worktree.Branch,
		Title:     result.Title,
		Body:      result.Summary,
	})
	if err != nil {
		return err
	}

	sess.setPR(pr.URL)
	e.emit(ctx, task.TaskID, protocol.EventPR, protocol.PRPayload{
		URL:    pr.URL,
		Branch: worktree.Branch,
	}, rep)
	e.log.Info("opened a pull request",
		"taskId", task.TaskID, "url", pr.URL, "branch", worktree.Branch, "existed", pr.Existed)
	return nil
}

// subjectLimit is where a commit subject stops being a subject.
const subjectLimit = 72

// commitMessage is what the task leaves in the developer's history.
//
// It carries the agent's own title and summary and a reference back to the
// ticket, and nothing about what produced it. The commit is read as part of the
// repository, not as a record of the tool that wrote it.
func commitMessage(task protocol.TaskRun, result agent.Result) string {
	subject, _, _ := strings.Cut(strings.TrimSpace(result.Title), "\n")
	subject = strings.TrimSpace(subject)
	if subject == "" {
		subject = "apply the changes for " + task.TaskID
	}
	if len(subject) > subjectLimit {
		subject = strings.TrimSpace(subject[:subjectLimit])
	}

	var b strings.Builder
	b.WriteString(subject)
	if summary := strings.TrimSpace(result.Summary); summary != "" {
		b.WriteString("\n\n")
		b.WriteString(summary)
	}
	if ref := ticketRef(task.Ticket); ref != "" {
		b.WriteString("\n\nRefs: ")
		b.WriteString(ref)
	}
	return b.String()
}

func ticketRef(ticket protocol.Ticket) string {
	if ticket.URL != "" {
		return ticket.URL
	}
	return ticket.ID
}

// session is the executor's side of agent.Session: the only surface an agent
// gets, and the one every guarantee is enforced on.
type session struct {
	ex      *Executor
	task    protocol.TaskRun
	rep     Reporter
	spec    sandbox.Spec
	workDir string
	budget  float64

	mu     sync.Mutex
	cmds   int
	steps  int
	cost   float64
	tokens protocol.Tokens
	prURL  string
}

func (s *session) WorkDir() string { return s.workDir }

func (s *session) setPR(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prURL = url
}

func (s *session) totals() (float64, protocol.Tokens, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cost, s.tokens, s.prURL
}

// Exec runs one command in a container and reports it as it goes.
func (s *session) Exec(ctx context.Context, argv []string) (agent.Exec, error) {
	if len(argv) == 0 {
		return agent.Exec{}, errors.New("executor: no command")
	}
	if err := ctx.Err(); err != nil {
		return agent.Exec{}, err
	}

	s.mu.Lock()
	s.cmds++
	cmdID := fmt.Sprintf("c%d", s.cmds)
	s.mu.Unlock()

	s.ex.emit(ctx, s.task.TaskID, protocol.EventCmdStart, protocol.CmdStartPayload{
		CmdID: cmdID,
		Argv:  argv,
		Dir:   s.spec.WorkDir,
	}, s.rep)

	// One buffer for both streams: the agent needs to read what happened in the
	// order it happened, which is how a shell shows it.
	captured := &capture{limit: maxCapture}
	stdout := s.ex.output(ctx, s.task.TaskID, cmdID, "stdout", s.rep, captured)
	stderr := s.ex.output(ctx, s.task.TaskID, cmdID, "stderr", s.rep, captured)

	result, runErr := s.ex.cfg.Run(ctx, s.spec, argv, stdout, stderr)

	// The redaction writers hold back a tail so a credential cannot be split
	// across two chunks; without these flushes that tail is simply lost.
	stdout.Flush()
	stderr.Flush()

	s.ex.emit(ctx, s.task.TaskID, protocol.EventCmdExit, protocol.CmdExitPayload{
		CmdID:      cmdID,
		Code:       result.ExitCode,
		DurationMs: result.Duration.Milliseconds(),
	}, s.rep)

	if runErr != nil {
		return agent.Exec{}, runErr
	}
	text, truncated := captured.text()
	return agent.Exec{
		ExitCode:   result.ExitCode,
		Output:     text,
		Truncated:  truncated,
		DurationMs: result.Duration.Milliseconds(),
	}, nil
}

// ReadFile returns a file from the checkout, masked, because a model provider is
// as much somewhere a credential should not go as Cloud is.
func (s *session) ReadFile(name string) (string, error) {
	full, err := s.path(name)
	if err != nil {
		return "", err
	}
	f, err := os.Open(full)
	if err != nil {
		// The host path is deliberately not repeated back: the agent works in
		// repository-relative terms and does not need the VPS's layout.
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s does not exist", name)
		}
		return "", fmt.Errorf("cannot read %s", name)
	}
	defer f.Close()

	raw, err := io.ReadAll(io.LimitReader(f, maxFileRead))
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", name, err)
	}
	return string(s.ex.cfg.Filter.Bytes(raw)), nil
}

// WriteFile replaces a file in the checkout.
func (s *session) WriteFile(name, content string) error {
	full, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("cannot create the directory for %s: %w", name, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return fmt.Errorf("cannot write %s: %w", name, err)
	}
	return nil
}

// path resolves a repository-relative path, refusing anything that leaves the
// checkout. The agent's reach is the worktree and nothing else on the VPS.
func (s *session) path(name string) (string, error) {
	if name == "" {
		return "", errors.New("path is empty")
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if !filepath.IsLocal(clean) {
		return "", fmt.Errorf("path %q is outside the repository checkout", name)
	}
	return filepath.Join(s.workDir, clean), nil
}

// Complete calls the model, journals the call, and holds the budget.
//
// The check is made before the call rather than after, so a task stops instead
// of paying for an answer it will not use. That lets a task overshoot its
// budget by at most one call, which is the cheaper of the two mistakes.
func (s *session) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if s.ex.cfg.LLM == nil {
		return llm.Response{}, errors.New("executor: no llm configured; set creds.llm in the runner config")
	}

	s.mu.Lock()
	spent := s.cost
	s.mu.Unlock()
	if s.budget > 0 && spent >= s.budget {
		return llm.Response{}, fmt.Errorf("%w: spent $%.4f of $%.4f", agent.ErrBudget, spent, s.budget)
	}

	resp, err := s.ex.cfg.LLM.Complete(ctx, req)
	if err != nil {
		return llm.Response{}, err
	}

	tokens := protocol.Tokens{
		In:  resp.Usage.Input + resp.Usage.CacheRead + resp.Usage.CacheWrite,
		Out: resp.Usage.Output,
	}
	s.mu.Lock()
	s.cost += resp.CostUSD
	s.tokens.In += tokens.In
	s.tokens.Out += tokens.Out
	s.mu.Unlock()

	s.ex.emit(ctx, s.task.TaskID, protocol.EventLLMCall, protocol.LLMCallPayload{
		Model:      resp.Model,
		Tokens:     tokens,
		CostUSD:    resp.CostUSD,
		DurationMs: resp.Duration.Milliseconds(),
	}, s.rep)
	return resp, nil
}

// Step records what the agent said it is doing. The text is masked: it is the
// model's own words, and the model has been reading the repository.
func (s *session) Step(ctx context.Context, text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	s.steps++
	stepID := fmt.Sprintf("s%d", s.steps)
	s.mu.Unlock()

	s.ex.emit(ctx, s.task.TaskID, protocol.EventAgentStep, protocol.AgentStepPayload{
		StepID: stepID,
		Text:   s.ex.cfg.Filter.String(text),
	}, s.rep)
}

// capture keeps the first limit bytes of a command's output for the agent.
//
// The start is kept rather than the end because it holds the first error, which
// is usually the one that caused the rest.
type capture struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

// Write never fails: it is one half of a MultiWriter whose other half is the
// event stream, and dropping output must not stop a command.
func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.truncated = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

func (c *capture) text() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String(), c.truncated
}

// output returns a writer that masks credentials and turns what survives into
// cmd_output events, keeping a bounded copy for the agent.
func (e *Executor) output(ctx context.Context, taskID, cmdID, stream string, rep Reporter, captured *capture) *outputWriter {
	sink := &chunkSink{ex: e, ctx: ctx, taskID: taskID, cmdID: cmdID, stream: stream, rep: rep}
	// Masking happens first, so what the agent reads is what Cloud reads.
	return &outputWriter{masked: e.cfg.Filter.Writer(io.MultiWriter(sink, captured))}
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

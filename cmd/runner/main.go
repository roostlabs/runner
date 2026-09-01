// Command runner is the Roost task runner. It runs on the developer's own VPS,
// dials out to Cloud, and executes tasks in disposable Docker sandboxes.
//
// A ticket goes in and a pull request comes out: the repository is cloned once
// and kept, each task gets a clean worktree, an agent works in a container
// under limits, every event is journalled before it is streamed, credentials
// are masked out of everything that leaves the box, spend is metered against a
// budget, and the result is a branch opened for review by a service account
// that cannot merge it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/agent"
	"github.com/roostlabs/runner/internal/channel"
	"github.com/roostlabs/runner/internal/config"
	"github.com/roostlabs/runner/internal/eventstore"
	"github.com/roostlabs/runner/internal/executor"
	"github.com/roostlabs/runner/internal/forge"
	"github.com/roostlabs/runner/internal/llm"
	"github.com/roostlabs/runner/internal/redact"
	"github.com/roostlabs/runner/internal/repo"
	"github.com/roostlabs/runner/internal/sandbox"
)

// Version is the Runner build, set with -ldflags "-X main.Version=...".
var Version = "dev"

// errOffline means there is no channel to report on. A task keeps running
// regardless; the journal will replay once Cloud is reachable again.
var errOffline = errors.New("runner: not connected to cloud")

func main() {
	if err := run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return // shut down on a signal, which is not a failure
		}
		fmt.Fprintf(os.Stderr, "runner: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", config.DefaultPath(), "path to the runner config file")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn or error")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		return nil
	}

	level, err := parseLevel(*logLevel)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	store, err := eventstore.Open(filepath.Join(cfg.DataDir, "events.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	docker := sandbox.ServerVersion(ctx)
	if docker == "" {
		log.Warn("no docker daemon detected; tasks will fail until one is running")
	}

	creds, err := buildCreds(cfg)
	if err != nil {
		return err
	}
	if creds.Forge == nil {
		log.Warn("no git credential configured; a task that changes anything will have nowhere to open a pull request")
	}

	repos := repo.New(filepath.Join(cfg.DataDir, "repos"), cfg.Creds.Git)
	svc := &service{
		ctx:        ctx,
		configPath: *configPath,
		store:      store,
		repos:      repos,
		log:        log,
	}
	svc.cfg.Store(&cfg)
	svc.creds.Store(&creds)

	svc.exec = executor.New(executor.Config{
		Repos: repos,
		Store: store,
		// Resolved per task rather than captured here: Managed mode replaces
		// credentials while the Runner is running.
		Creds:        svc.currentCreds,
		BudgetUSD:    cfg.Agent.BudgetUSD,
		Author:       author(cfg.Git),
		Sandbox:      sandboxSpec(cfg.Sandbox),
		KeepWorktree: cfg.Sandbox.KeepWorktree,
		Run:          sandbox.Run,
		Logger:       log,
	})
	svc.running = svc.exec.Active

	log.Info("runner starting",
		"version", Version, "cloud", cfg.CloudURL, "dataDir", cfg.DataDir,
		"creds", cfg.Creds, "docker", docker, "image", cfg.Sandbox.Image,
		"agent", agentKind(creds.LLM), "model", modelName(creds.LLM),
		"budgetUsd", cfg.Agent.BudgetUSD)

	return channel.Run(ctx, channel.Options{
		URL:           cfg.CloudURL,
		Token:         cfg.Token,
		RunnerVersion: Version,
		Host: protocol.HostInfo{
			OS:     runtime.GOOS,
			Arch:   runtime.GOARCH,
			Docker: docker,
		},
		OnConnect: svc.onConnect,
		Logger:    log,
	}, svc.handle)
}

// buildCreds turns the configured credentials into what a task runs with.
//
// It is one function rather than five call sites so that Managed mode has
// exactly one way to rebuild them, and so that a value the Runner cannot use —
// a forge it does not know, a model id it cannot price — is refused while it is
// still only a message, before anything is written to disk.
func buildCreds(cfg config.Config) (executor.Creds, error) {
	brain, model, err := newAgent(cfg)
	if err != nil {
		return executor.Creds{}, err
	}
	openPR, err := newForge(cfg)
	if err != nil {
		return executor.Creds{}, err
	}
	return executor.Creds{
		Agent: brain,
		LLM:   model,
		Forge: openPR,
		// The filter can only mask values it was told about, which is exactly
		// the set the sandbox is given.
		Filter: redact.New(cfg.Creds.Values()...),
		Env:    cfg.Creds.Env(),
	}, nil
}

// newAgent picks what decides a task's work.
//
// An LLM key is what makes the Runner an agent rather than a build server, so
// its presence is the switch. Without one the configured commands still run,
// which is a real mode: a repository whose build and test sequence is fixed
// does not need a model to rediscover it every time.
func newAgent(cfg config.Config) (agent.Agent, *llm.Client, error) {
	if cfg.Creds.LLM == "" {
		return agent.FixedFromArgv(cfg.Sandbox.Commands), nil, nil
	}
	client, err := llm.New(llm.Options{
		APIKey:    cfg.Creds.LLM,
		BaseURL:   cfg.Agent.BaseURL,
		Model:     cfg.Agent.Model,
		Effort:    cfg.Agent.Effort,
		MaxTokens: cfg.Agent.MaxTokens,
	})
	if err != nil {
		return nil, nil, err
	}
	return agent.Model{MaxSteps: cfg.Agent.MaxSteps}, client, nil
}

// newForge builds the pull-request opener, or reports that there is none.
//
// The git credential is what makes one possible: without it the Runner cannot
// push a branch, let alone open anything on top of it.
func newForge(cfg config.Config) (executor.PRFunc, error) {
	if cfg.Creds.Git == "" {
		return nil, nil
	}
	client, err := forge.New(forge.Options{
		Token:   cfg.Creds.Git,
		Kind:    forge.Kind(cfg.Git.Forge),
		APIBase: cfg.Git.APIBase,
	})
	if err != nil {
		return nil, err
	}
	return client.Open, nil
}

func author(cfg config.Git) repo.Author {
	a := repo.DefaultAuthor
	if cfg.AuthorName != "" {
		a.Name = cfg.AuthorName
	}
	if cfg.AuthorEmail != "" {
		a.Email = cfg.AuthorEmail
	}
	return a
}

func agentKind(model *llm.Client) string {
	if model == nil {
		return "fixed"
	}
	return "model"
}

func modelName(model *llm.Client) string {
	if model == nil {
		return ""
	}
	return model.Model()
}

// sandboxSpec maps the config onto a container spec. WritableRoot is negated
// here so that a config which says nothing produces a read-only container.
func sandboxSpec(cfg config.Sandbox) sandbox.Spec {
	spec := sandbox.Spec{
		Image:        cfg.Image,
		Env:          nil, // filled in per task by the executor
		User:         sandbox.CurrentUser(),
		CPUs:         cfg.CPUs,
		MemoryMB:     cfg.MemoryMB,
		PidsLimit:    cfg.PidsLimit,
		Network:      cfg.Network,
		ReadOnlyRoot: !cfg.WritableRoot,
	}
	if cfg.TimeoutMs > 0 {
		spec.Timeout = time.Duration(cfg.TimeoutMs) * time.Millisecond
	}
	return spec
}

// service holds what handling a message needs.
type service struct {
	// ctx is the process lifetime, not a connection's. Tasks are started from
	// it so that a dropped channel does not kill work in progress.
	ctx        context.Context
	configPath string
	store      *eventstore.Store
	repos      *repo.Manager
	exec       *executor.Executor
	log        *slog.Logger

	// cfg and creds are replaced together when Managed mode accepts a
	// credential, so both are read through a pointer swap rather than copied
	// into the components that use them.
	cfg   atomic.Pointer[config.Config]
	creds atomic.Pointer[executor.Creds]
	// credMu serialises credential updates. Without it two of them could each
	// build a new config from the same old one and the second would undo the
	// first.
	credMu sync.Mutex

	// running reports the task holding the execution slot. It is a field rather
	// than a call into exec so that a test can drive setCred without a Docker
	// daemon behind it.
	running func() (string, bool)

	// conn is the current connection, replaced on every reconnect. A task that
	// outlives a connection reports onto whatever is current when it writes.
	conn atomic.Pointer[channel.Conn]
}

// config is the configuration as it stands now, which Managed mode can change.
func (s *service) config() config.Config {
	return *s.cfg.Load()
}

// currentCreds is what the Executor calls at the start of each task.
func (s *service) currentCreds() executor.Creds {
	return *s.creds.Load()
}

// setCred applies a credential Cloud pushed down the channel.
//
// Three things have to be true before a value is kept, and all three are the
// point of the feature rather than incidental to it: the developer turned
// Managed mode on in the config on their own machine, no task is running that
// would see the change halfway through, and the Runner can actually build a
// working credential set out of it. Only then is anything written, and the file
// it is written to is the same 0600 config a Local-mode Runner reads.
//
// The value never appears in a log line, an error message or an event. What
// Cloud learns back is the flag: a fresh cred.status, which the dashboard reads
// as the slot turning from ✗ to ✓.
func (s *service) setCred(key protocol.CredKey, value string) error {
	s.credMu.Lock()
	defer s.credMu.Unlock()

	cfg := s.config()
	if !cfg.Creds.Managed() {
		return fmt.Errorf("managed mode is off; set creds.mode to %q in %s to allow it",
			protocol.CredModeManaged, s.configPath)
	}
	if id, busy := s.running(); busy {
		return fmt.Errorf("task %s is running; a credential change would land halfway through it", id)
	}

	next := cfg
	updated, err := next.Creds.With(key, value)
	if err != nil {
		return err
	}
	next.Creds = updated

	creds, err := buildCreds(next)
	if err != nil {
		return err
	}
	if err := config.Save(s.configPath, next); err != nil {
		return err
	}

	s.cfg.Store(&next)
	s.creds.Store(&creds)
	s.repos.SetToken(next.Creds.Git)

	// Which slot changed, and whether it now holds anything. Never the value.
	s.log.Info("credential updated from the dashboard",
		"key", key, "set", value != "", "creds", next.Creds)
	return nil
}

// onConnect sends the state Cloud needs as soon as the channel is up, on every
// connection including reconnects, since Cloud keeps minimal state of its own.
func (s *service) onConnect(ctx context.Context, c *channel.Conn) error {
	s.conn.Store(c)

	if err := c.SendMessage(ctx, protocol.TypeCredStatus, s.config().Creds.Status()); err != nil {
		return err
	}
	if err := c.SendMessage(ctx, protocol.TypeStatus, s.status()); err != nil {
		return err
	}
	return c.SendMessage(ctx, protocol.TypeRepoStatus, s.repos.Status(ctx))
}

func (s *service) status() protocol.Status {
	state := protocol.RunnerIdle
	var activeTasks []string
	if id, busy := s.exec.Active(); busy {
		state = protocol.RunnerBusy
		activeTasks = []string{id}
	}
	return protocol.Status{State: state, ActiveTasks: activeTasks}
}

// handle dispatches one inbound message. It returns an error only when the
// connection itself is no longer usable; anything the Runner cannot satisfy is
// answered with a protocol error instead.
//
// Nothing here may block: this runs on the channel's read loop, so a handler
// that waits stalls every other message. Work goes to a goroutine.
func (s *service) handle(ctx context.Context, c *channel.Conn, env protocol.Envelope) error {
	switch env.Type {
	case protocol.TypeTaskRun:
		var task protocol.TaskRun
		if err := env.Decode(&task); err != nil {
			return s.replyError(ctx, c, env.ID, protocol.ErrInternal, err.Error())
		}
		s.startTask(task, env.ID)
		return nil

	case protocol.TypeTaskCancel:
		var cancel protocol.TaskCancel
		if err := env.Decode(&cancel); err != nil {
			return s.replyError(ctx, c, env.ID, protocol.ErrInternal, err.Error())
		}
		if !s.exec.Cancel(cancel.TaskID) {
			return s.replyError(ctx, c, env.ID, protocol.ErrTaskNotFound, "no such task is running")
		}
		s.log.Info("cancelling task", "taskId", cancel.TaskID, "reason", cancel.Reason)
		return nil

	case protocol.TypeRepoPrepare:
		var prepare protocol.RepoPrepare
		if err := env.Decode(&prepare); err != nil {
			return s.replyError(ctx, c, env.ID, protocol.ErrInternal, err.Error())
		}
		go s.prepareRepo(prepare)
		return nil

	case protocol.TypeTaskApprove:
		return s.replyError(ctx, c, env.ID, protocol.ErrTaskNotFound, "no task is awaiting approval")

	case protocol.TypeCredSet:
		// Local mode is the default, so the message is not even decoded until
		// the mode says it may be: a value nobody asked for should not be read
		// out of the frame at all.
		if !s.config().Creds.Managed() {
			s.log.Warn("refusing cred.set: managed mode is off")
			return s.replyError(ctx, c, env.ID, protocol.ErrCredRefused,
				"managed mode is off on this runner; credentials are set on the VPS")
		}
		var cred protocol.CredSet
		if err := env.Decode(&cred); err != nil {
			// The error is the decoder's, so it describes the frame's shape
			// rather than its contents.
			return s.replyError(ctx, c, env.ID, protocol.ErrCredRefused, "malformed cred.set")
		}
		ref := env.ID
		go func() {
			if err := s.setCred(cred.Key, cred.Value); err != nil {
				s.log.Warn("refusing cred.set", "key", cred.Key, "err", err)
				s.sendError(ref, protocol.ErrCredRefused, err.Error())
				return
			}
			s.announceCreds()
		}()
		return nil

	case protocol.TypeQuery:
		var q protocol.Query
		if err := env.Decode(&q); err != nil {
			return s.replyError(ctx, c, env.ID, protocol.ErrInternal, err.Error())
		}
		return s.answerQuery(ctx, c, q)

	case protocol.TypeSubscribe, protocol.TypeUnsubscribe:
		var sub protocol.Subscribe
		if err := env.Decode(&sub); err != nil {
			return s.replyError(ctx, c, env.ID, protocol.ErrInternal, err.Error())
		}
		s.log.Info("stream subscription ignored; metrics are not implemented",
			"type", env.Type, "stream", sub.Stream)
		return nil

	case protocol.TypeChat:
		var chat protocol.Chat
		if err := env.Decode(&chat); err != nil {
			return nil
		}
		s.log.Info("chat from the dashboard", "taskId", chat.TaskID, "text", chat.Text)
		return nil

	case protocol.TypeError:
		var e protocol.Error
		if err := env.Decode(&e); err == nil {
			s.log.Error("cloud reported an error", "code", e.Code, "ref", e.Ref, "msg", e.Msg)
		}
		return nil

	default:
		// Unknown types are dropped, never rejected: that is what lets a newer
		// Cloud talk to a Runner nobody has upgraded yet.
		s.log.Debug("ignoring unknown message type", "type", env.Type)
		return nil
	}
}

// startTask runs a task on its own goroutine, tied to the process rather than to
// the connection or the message that asked for it.
func (s *service) startTask(task protocol.TaskRun, ref string) {
	s.log.Info("task requested",
		"taskId", task.TaskID, "repo", task.Repo,
		"ticket", task.Ticket.ID, "budgetUsd", task.BudgetUSD)

	go func() {
		err := s.exec.Run(s.ctx, task, s)
		s.announceStatus()

		switch {
		case err == nil:
			s.log.Info("task finished", "taskId", task.TaskID)
		case errors.Is(err, executor.ErrBusy):
			// Concurrency is 1, so this is a queueing signal rather than a
			// failure. Cloud decides whether to retry.
			s.log.Info("refusing task: already busy", "taskId", task.TaskID)
			s.sendError(ref, protocol.ErrBusy, err.Error())
		case errors.Is(err, agent.ErrBudget):
			// The task stopped because it ran out of money, which Cloud shows
			// differently from a task that broke.
			s.log.Warn("task stopped on budget", "taskId", task.TaskID, "err", err)
			s.sendError(ref, protocol.ErrBudgetExceeded, err.Error())
		default:
			s.log.Warn("task failed", "taskId", task.TaskID, "err", err)
		}
	}()

	s.announceStatus()
}

func (s *service) prepareRepo(prepare protocol.RepoPrepare) {
	s.log.Info("preparing repo", "url", prepare.URL, "branch", prepare.Branch)
	if _, err := s.repos.Prepare(s.ctx, prepare.URL, prepare.Branch); err != nil {
		s.log.Warn("could not prepare repo", "url", prepare.URL, "err", err)
		s.sendError("", protocol.ErrInternal, err.Error())
		return
	}
	if c := s.conn.Load(); c != nil {
		if err := c.SendMessage(s.ctx, protocol.TypeRepoStatus, s.repos.Status(s.ctx)); err != nil {
			s.log.Debug("could not report repo status", "err", err)
		}
	}
}

// The Reporter implementation. Each call resolves the current connection, so a
// task that spans a reconnect keeps reporting onto the live one.

func (s *service) TaskEvent(ctx context.Context, taskID string, seq uint64, ev protocol.TaskEvent) error {
	c := s.conn.Load()
	if c == nil {
		return errOffline
	}
	return c.SendTaskMessage(ctx, protocol.TypeTaskEvent, taskID, seq, ev)
}

func (s *service) TaskState(ctx context.Context, taskID string, st protocol.TaskState) error {
	c := s.conn.Load()
	if c == nil {
		return errOffline
	}
	return c.SendTaskMessage(ctx, protocol.TypeTaskState, taskID, 0, st)
}

func (s *service) TaskResult(ctx context.Context, taskID string, res protocol.TaskResult) error {
	c := s.conn.Load()
	if c == nil {
		return errOffline
	}
	return c.SendTaskMessage(ctx, protocol.TypeTaskResult, taskID, 0, res)
}

// announceCreds reports the credential flags, which is the only acknowledgement
// a cred.set gets and the only thing Cloud is entitled to know about the values.
func (s *service) announceCreds() {
	c := s.conn.Load()
	if c == nil {
		return
	}
	if err := c.SendMessage(s.ctx, protocol.TypeCredStatus, s.config().Creds.Status()); err != nil {
		s.log.Debug("could not report credential status", "err", err)
	}
}

func (s *service) announceStatus() {
	c := s.conn.Load()
	if c == nil {
		return
	}
	if err := c.SendMessage(s.ctx, protocol.TypeStatus, s.status()); err != nil {
		s.log.Debug("could not report status", "err", err)
	}
}

func (s *service) sendError(ref string, code protocol.ErrorCode, msg string) {
	c := s.conn.Load()
	if c == nil {
		return
	}
	if err := c.SendMessage(s.ctx, protocol.TypeError, protocol.Error{Code: code, Ref: ref, Msg: msg}); err != nil {
		s.log.Debug("could not report an error", "err", err)
	}
}

// traceParams is the payload of a task_trace query.
type traceParams struct {
	TaskID   string `json:"taskId"`
	AfterSeq uint64 `json:"afterSeq"`
	Limit    int    `json:"limit"`
}

// configView is the config as Cloud is allowed to see it: endpoint, paths and
// limits, credentials as flags, and no token.
type configView struct {
	CloudURL      string              `json:"cloudUrl"`
	DataDir       string              `json:"dataDir"`
	RunnerVersion string              `json:"runnerVersion"`
	Image         string              `json:"image"`
	Network       string              `json:"network"`
	Creds         protocol.CredStatus `json:"creds"`
}

// answerQuery serves a one-shot request from the local journal. Finished tasks
// are read this way rather than pushed, which is what keeps history on the VPS.
func (s *service) answerQuery(ctx context.Context, c *channel.Conn, q protocol.Query) error {
	data, err := s.query(ctx, q)
	if err != nil {
		s.log.Warn("query failed", "queryId", q.QueryID, "what", q.What, "err", err)
		return c.SendMessage(ctx, protocol.TypeQueryResult, protocol.QueryResult{
			QueryID: q.QueryID,
			OK:      false,
			Error:   err.Error(),
		})
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode %s result: %w", q.What, err)
	}
	return c.SendMessage(ctx, protocol.TypeQueryResult, protocol.QueryResult{
		QueryID: q.QueryID,
		OK:      true,
		Data:    raw,
	})
}

func (s *service) query(ctx context.Context, q protocol.Query) (any, error) {
	switch q.What {
	case protocol.QueryTaskHistory:
		return s.store.Tasks(ctx)

	case protocol.QueryTaskTrace:
		var p traceParams
		if len(q.Params) > 0 {
			if err := json.Unmarshal(q.Params, &p); err != nil {
				return nil, fmt.Errorf("bad params: %w", err)
			}
		}
		if p.TaskID == "" {
			return nil, errors.New("taskId is required")
		}
		return s.store.Since(ctx, p.TaskID, p.AfterSeq, p.Limit)

	case protocol.QueryConfig:
		cfg := s.config()
		network := cfg.Sandbox.Network
		if network == "" {
			network = sandbox.NetworkBridge
		}
		return configView{
			CloudURL:      cfg.CloudURL,
			DataDir:       cfg.DataDir,
			RunnerVersion: Version,
			Image:         cfg.Sandbox.Image,
			Network:       network,
			Creds:         cfg.Creds.Status(),
		}, nil

	case protocol.QueryDiskUsage:
		return nil, errors.New("disk usage is not implemented in this build")

	default:
		return nil, fmt.Errorf("unknown query %q", q.What)
	}
}

func (s *service) replyError(ctx context.Context, c *channel.Conn, ref string, code protocol.ErrorCode, msg string) error {
	return c.SendMessage(ctx, protocol.TypeError, protocol.Error{Code: code, Ref: ref, Msg: msg})
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", s)
	}
}

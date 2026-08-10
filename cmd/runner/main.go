// Command runner is the Roost task runner. It runs on the developer's own VPS,
// dials out to Cloud, and executes tasks in disposable Docker sandboxes.
//
// Sandbox execution is not built yet. This binary establishes and holds the
// channel, records task events in the local journal, and answers history
// queries from it; a task.run is refused honestly rather than silently dropped.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/channel"
	"github.com/roostlabs/runner/internal/config"
	"github.com/roostlabs/runner/internal/eventstore"
)

// Version is the Runner build, set with -ldflags "-X main.Version=...".
var Version = "dev"

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

	docker := dockerVersion(ctx)
	if docker == "" {
		log.Warn("no docker daemon detected; sandboxes cannot start")
	}

	svc := &service{cfg: cfg, store: store, log: log}
	log.Info("runner starting",
		"version", Version, "cloud", cfg.CloudURL, "dataDir", cfg.DataDir,
		"creds", cfg.Creds, "docker", docker)

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

// service holds what handling a message needs.
type service struct {
	cfg   config.Config
	store *eventstore.Store
	log   *slog.Logger
}

// onConnect sends the state Cloud needs as soon as the channel is up, on every
// connection including reconnects, since Cloud keeps minimal state of its own.
func (s *service) onConnect(ctx context.Context, c *channel.Conn) error {
	if err := c.SendMessage(ctx, protocol.TypeCredStatus, s.cfg.Creds.Status()); err != nil {
		return err
	}
	return c.SendMessage(ctx, protocol.TypeStatus, protocol.Status{
		State:       protocol.RunnerIdle,
		ActiveTasks: nil,
		QueuedTasks: 0,
	})
}

// handle dispatches one inbound message. It returns an error only when the
// connection itself is no longer usable; a message the Runner cannot satisfy is
// answered with a protocol error instead.
func (s *service) handle(ctx context.Context, c *channel.Conn, env protocol.Envelope) error {
	switch env.Type {
	case protocol.TypeTaskRun:
		return s.handleTaskRun(ctx, c, env)

	case protocol.TypeTaskCancel:
		var cancel protocol.TaskCancel
		if err := env.Decode(&cancel); err != nil {
			return s.replyError(ctx, c, env.ID, protocol.ErrInternal, err.Error())
		}
		s.log.Info("cancel for a task that is not running", "taskId", cancel.TaskID)
		return s.replyError(ctx, c, env.ID, protocol.ErrTaskNotFound, "no such task is running")

	case protocol.TypeTaskApprove:
		return s.replyError(ctx, c, env.ID, protocol.ErrTaskNotFound, "no task is awaiting approval")

	case protocol.TypeRepoPrepare:
		return s.replyError(ctx, c, env.ID, protocol.ErrInternal, "repo preparation is not implemented in this build")

	case protocol.TypeCredSet:
		// Local mode is the default and this build has no Managed mode, so a
		// credential arriving from Cloud is refused rather than written. The
		// payload is deliberately not decoded or logged.
		s.log.Warn("refusing cred.set: managed mode is not enabled")
		return s.replyError(ctx, c, env.ID, protocol.ErrInternal, "managed mode is not enabled on this runner")

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

// handleTaskRun records the request and fails it, because there is no sandbox to
// run it in yet. Recording first means the dashboard shows a real trace rather
// than a task that vanished.
func (s *service) handleTaskRun(ctx context.Context, c *channel.Conn, env protocol.Envelope) error {
	var task protocol.TaskRun
	if err := env.Decode(&task); err != nil {
		return s.replyError(ctx, c, env.ID, protocol.ErrInternal, err.Error())
	}
	s.log.Info("task requested",
		"taskId", task.TaskID, "repo", task.Repo,
		"ticket", task.Ticket.ID, "budgetUsd", task.BudgetUSD)

	const reason = "sandbox execution is not implemented in this runner build"
	if err := s.emit(ctx, c, task.TaskID, protocol.EventError,
		protocol.EventErrorPayload{Msg: reason}); err != nil {
		return err
	}
	return c.SendMessage(ctx, protocol.TypeTaskState, protocol.TaskState{
		State:  protocol.TaskFailed,
		Reason: reason,
	})
}

// emit journals an event and then streams it.
//
// The order is the invariant the whole history model rests on: the VPS is the
// source of truth, so an event that is not stored must not be considered sent.
func (s *service) emit(ctx context.Context, c *channel.Conn, taskID string, kind protocol.EventKind, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", kind, err)
	}
	seq, err := s.store.Append(ctx, taskID, kind, raw, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return c.SendTaskMessage(ctx, protocol.TypeTaskEvent, taskID, seq, protocol.TaskEvent{
		Event:   kind,
		Payload: raw,
	})
}

// traceParams is the payload of a task_trace query.
type traceParams struct {
	TaskID   string `json:"taskId"`
	AfterSeq uint64 `json:"afterSeq"`
	Limit    int    `json:"limit"`
}

// configView is the config as Cloud is allowed to see it: endpoint and paths,
// credentials as flags, and no token.
type configView struct {
	CloudURL      string              `json:"cloudUrl"`
	DataDir       string              `json:"dataDir"`
	RunnerVersion string              `json:"runnerVersion"`
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
		return configView{
			CloudURL:      s.cfg.CloudURL,
			DataDir:       s.cfg.DataDir,
			RunnerVersion: Version,
			Creds:         s.cfg.Creds.Status(),
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

// dockerVersion reports the Docker server version, or an empty string when no
// daemon answers. Docker is a prerequisite, but a missing one is a warning at
// startup rather than a refusal to run: the channel still has value.
func dockerVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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

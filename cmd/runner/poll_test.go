package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/config"
	"github.com/roostlabs/runner/internal/executor"
	"github.com/roostlabs/runner/internal/repo"
	"github.com/roostlabs/runner/internal/tracker"
)

// listOnly is a tracker that answers List and nothing else, which is all the
// poller asks.
type listOnly struct {
	tickets []tracker.Ticket
	err     error
	calls   int
}

func (l *listOnly) Kind() tracker.Kind { return tracker.KindLinear }
func (l *listOnly) List(context.Context, string) ([]tracker.Ticket, error) {
	l.calls++
	return l.tickets, l.err
}
func (l *listOnly) Get(context.Context, string) (tracker.Ticket, error) {
	return tracker.Ticket{}, errors.New("not used by the poller")
}
func (l *listOnly) Transition(context.Context, string, string) error {
	return errors.New("not used by the poller")
}
func (l *listOnly) Comment(context.Context, string, string) error {
	return errors.New("not used by the poller")
}

func pollingService(t *testing.T, tr tracker.Client) (*service, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		CloudURL: "wss://cloud.example.com/channel",
		Token:    "rt_runnertoken",
		DataDir:  dir,
		Sandbox:  config.Sandbox{Image: "golang:1.26"},
		Tracker: config.Tracker{
			Kind: "linear", Project: "ENG", Repo: "https://github.com/acme/app.git",
			States: config.TrackerStates{Ready: "Ready for agent", InProgress: "In Progress"},
		},
	}
	var logs bytes.Buffer
	svc := &service{
		configPath: filepath.Join(dir, "config.json"),
		repos:      repo.New(filepath.Join(dir, "repos"), ""),
		log:        slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		running:    func() (string, bool) { return "", false },
	}
	svc.cfg.Store(&cfg)
	svc.creds.Store(&executor.Creds{Tracker: tr})
	return svc, &logs
}

func ready(ids ...string) []tracker.Ticket {
	out := make([]tracker.Ticket, 0, len(ids))
	for _, id := range ids {
		out = append(out, tracker.Ticket{ID: id, Title: "title " + id, Body: "body", URL: "https://linear.app/" + id})
	}
	return out
}

func TestPickStartsTheOldestReadyTicket(t *testing.T) {
	tr := &listOnly{tickets: ready("ENG-1", "ENG-2")}
	svc, _ := pollingService(t, tr)

	task, ok := svc.pick(context.Background(), map[string]time.Time{})
	if !ok {
		t.Fatal("nothing was picked")
	}
	if task.TaskID == "" || task.Repo != "https://github.com/acme/app.git" {
		t.Errorf("task = %+v", task)
	}
	want := protocol.Ticket{Provider: "linear", ID: "ENG-1", URL: "https://linear.app/ENG-1", Title: "title ENG-1", Body: "body"}
	if task.Ticket != want {
		t.Errorf("ticket = %+v\nwant     %+v", task.Ticket, want)
	}
}

func TestPickTakesOneTicketPerRound(t *testing.T) {
	tr := &listOnly{tickets: ready("ENG-1", "ENG-2")}
	svc, _ := pollingService(t, tr)
	tried := map[string]time.Time{}

	first, _ := svc.pick(context.Background(), tried)
	second, ok := svc.pick(context.Background(), tried)
	if !ok || second.Ticket.ID != "ENG-2" || first.Ticket.ID != "ENG-1" {
		t.Errorf("rounds picked %q then %q", first.Ticket.ID, second.Ticket.ID)
	}
	if first.TaskID == second.TaskID {
		t.Error("two tasks share an id")
	}
	if _, ok := svc.pick(context.Background(), tried); ok {
		t.Error("a third round picked a ticket that was already tried")
	}
}

func TestPickRetriesAStuckTicketAfterAWhile(t *testing.T) {
	tr := &listOnly{tickets: ready("ENG-1")}
	svc, _ := pollingService(t, tr)

	tried := map[string]time.Time{"ENG-1": time.Now().Add(-retryAfter / 2)}
	if _, ok := svc.pick(context.Background(), tried); ok {
		t.Error("a ticket tried a moment ago was picked again")
	}
	tried["ENG-1"] = time.Now().Add(-retryAfter)
	if _, ok := svc.pick(context.Background(), tried); !ok {
		t.Error("a ticket still ready after the retry window was not picked again")
	}
}

func TestPickWaitsWhileATaskRuns(t *testing.T) {
	tr := &listOnly{tickets: ready("ENG-1")}
	svc, _ := pollingService(t, tr)
	svc.running = func() (string, bool) { return "t-busy", true }

	if _, ok := svc.pick(context.Background(), map[string]time.Time{}); ok {
		t.Error("a ticket was picked while a task was running")
	}
	if tr.calls != 0 {
		t.Error("the tracker was asked although nothing could be started")
	}
}

func TestPickSurvivesATrackerError(t *testing.T) {
	tr := &listOnly{err: errors.New("tracker: http 503")}
	svc, logs := pollingService(t, tr)

	if _, ok := svc.pick(context.Background(), map[string]time.Time{}); ok {
		t.Error("a ticket was picked from a failed list")
	}
	if !bytes.Contains(logs.Bytes(), []byte("http 503")) {
		t.Errorf("the failure was not logged: %s", logs.String())
	}
}

func TestPickWithoutACredentialDoesNothing(t *testing.T) {
	svc, _ := pollingService(t, nil)
	if _, ok := svc.pick(context.Background(), map[string]time.Time{}); ok {
		t.Error("a ticket was picked with no tracker")
	}
}

// In Managed mode the task manager credential arrives from the dashboard, and
// the connector has to appear with it.
func TestSettingTheTaskManagerKeyTurnsOnTheTracker(t *testing.T) {
	svc, _ := pollingService(t, nil)
	cfg := svc.config()
	cfg.Creds.Mode = protocol.CredModeManaged
	if err := config.Save(svc.configPath, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	svc.cfg.Store(&cfg)

	if err := svc.setCred(protocol.CredTaskManager, "lin_api_key"); err != nil {
		t.Fatalf("setCred: %v", err)
	}
	got := svc.currentCreds().Tracker
	if got == nil || got.Kind() != tracker.KindLinear {
		t.Errorf("tracker = %v, want a linear connector", got)
	}
	if err := svc.setCred(protocol.CredTaskManager, ""); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if svc.currentCreds().Tracker != nil {
		t.Error("the connector outlived its credential")
	}
}

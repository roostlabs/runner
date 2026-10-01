package executor

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/agent"
	"github.com/roostlabs/runner/internal/alert"
	"github.com/roostlabs/runner/internal/forge"
)

type alerts struct {
	mu  sync.Mutex
	got []alert.Event
}

func (a *alerts) Alert(_ context.Context, ev alert.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = append(a.got, ev)
}

func (a *alerts) kinds() []alert.Kind {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []alert.Kind
	for _, ev := range a.got {
		out = append(out, ev.Kind)
	}
	return out
}

func TestAFinishedTaskIsReportedWithItsPullRequest(t *testing.T) {
	a := &alerts{}
	f := newFixture(t, Config{
		Alerts: a,
		Agent:  writingAgent("fix.txt", "fixed\n", agent.Result{Title: "fix it"}),
		Forge: func(context.Context, forge.Request) (forge.PR, error) {
			return forge.PR{URL: "https://github.com/example/app/pull/9"}, nil
		},
	})
	f.task.Ticket = protocol.Ticket{Provider: "linear", ID: "ENG-7", Title: "Fix the paginator"}

	if err := f.ex.Run(context.Background(), f.task, &recorder{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(a.got) != 1 || a.got[0].Kind != alert.KindDone {
		t.Fatalf("alerts = %+v, want one done", a.got)
	}
	ev := a.got[0]
	if ev.TaskID != "T-1" || ev.PRURL != "https://github.com/example/app/pull/9" || ev.Ticket.ID != "ENG-7" || ev.Ticket.Title != "Fix the paginator" {
		t.Errorf("event = %+v", ev)
	}
}

func TestAFailedTaskIsReportedWithItsReason(t *testing.T) {
	a := &alerts{}
	f := newFixture(t, Config{
		Alerts: a,
		Agent: funcAgent(func(context.Context, protocol.TaskRun, agent.Session) (agent.Result, error) {
			return agent.Result{}, errors.New("the build is broken")
		}),
	})
	if err := f.ex.Run(context.Background(), f.task, &recorder{}); err == nil {
		t.Fatal("Run did not fail")
	}
	if len(a.got) != 1 || a.got[0].Kind != alert.KindFailed || a.got[0].Reason != "the build is broken" {
		t.Errorf("alerts = %+v", a.got)
	}
}

func TestBudgetHasItsOwnAlert(t *testing.T) {
	a := &alerts{}
	f := newFixture(t, Config{
		Alerts: a,
		Agent: funcAgent(func(context.Context, protocol.TaskRun, agent.Session) (agent.Result, error) {
			return agent.Result{}, agent.ErrBudget
		}),
	})
	f.ex.Run(context.Background(), f.task, &recorder{})
	if k := a.kinds(); len(k) != 1 || k[0] != alert.KindBudget {
		t.Errorf("kinds = %v, want budget", k)
	}
}

func TestACancelledTaskIsNotReported(t *testing.T) {
	a := &alerts{}
	f := newFixture(t, Config{
		Alerts: a,
		Agent: funcAgent(func(ctx context.Context, _ protocol.TaskRun, _ agent.Session) (agent.Result, error) {
			return agent.Result{}, context.Canceled
		}),
	})
	f.ex.Run(context.Background(), f.task, &recorder{})
	if len(a.got) != 0 {
		t.Errorf("a cancel was reported: %+v", a.got)
	}
}

func TestAWaitingTaskIsReportedBeforeAndAfter(t *testing.T) {
	a := &alerts{}
	f, _ := approvalFixture(t, Config{Alerts: a})
	rec := &recorder{}

	done := make(chan error, 1)
	go func() { done <- f.ex.Run(context.Background(), f.task, rec) }()
	waitFor(t, rec, protocol.TaskAwaitingApproval)

	if k := a.kinds(); len(k) != 1 || k[0] != alert.KindApproval {
		t.Fatalf("kinds while waiting = %v, want awaiting_approval", k)
	}
	if a.got[0].Reason != "fix the paginator" {
		t.Errorf("the approval alert does not carry the title: %+v", a.got[0])
	}
	if err := f.ex.Approve("T-1", "", true); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if k := a.kinds(); len(k) != 2 || k[1] != alert.KindDone {
		t.Errorf("kinds = %v, want awaiting_approval then done", k)
	}
}

func TestNoAlerterMeansNoAlerts(t *testing.T) {
	f := newFixture(t, Config{})
	if err := f.ex.Run(context.Background(), f.task, &recorder{}); err != nil {
		t.Fatalf("Run without an alerter: %v", err)
	}
}

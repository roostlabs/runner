package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/agent"
	"github.com/roostlabs/runner/internal/forge"
	"github.com/roostlabs/runner/internal/redact"
	"github.com/roostlabs/runner/internal/tracker"
)

// fakeTracker records what the executor asked of it.
type fakeTracker struct {
	kind    tracker.Kind
	ticket  tracker.Ticket
	getErr  error
	moveErr error
	noteErr error

	mu       sync.Mutex
	gets     []string
	moves    []string // "id->state"
	comments []string
}

func (f *fakeTracker) Kind() tracker.Kind { return f.kind }

func (f *fakeTracker) Get(_ context.Context, id string) (tracker.Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, id)
	return f.ticket, f.getErr
}

func (f *fakeTracker) List(context.Context, string) ([]tracker.Ticket, error) {
	return nil, errors.New("not used by the executor")
}

func (f *fakeTracker) Transition(_ context.Context, id, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moves = append(f.moves, id+"->"+state)
	return f.moveErr
}

func (f *fakeTracker) Comment(_ context.Context, id, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, body)
	return f.noteErr
}

func (f *fakeTracker) calls() (gets, moves, comments []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, f.moves, f.comments
}

func openPR(context.Context, forge.Request) (forge.PR, error) {
	return forge.PR{Number: 42, URL: "https://github.com/example/app/pull/42"}, nil
}

func jiraTicket() *fakeTracker {
	return &fakeTracker{
		kind: tracker.KindJira,
		ticket: tracker.Ticket{
			ID: "APP-7", URL: "https://acme.atlassian.net/browse/APP-7",
			Title: "paginator drops the last page", Body: "The last page is missing.",
		},
	}
}

func TestATrackedTicketIsReadMovedAndHandedOver(t *testing.T) {
	tr := jiraTicket()
	var seen protocol.Ticket
	f := newFixture(t, Config{
		Agent: funcAgent(func(_ context.Context, task protocol.TaskRun, s agent.Session) (agent.Result, error) {
			seen = task.Ticket
			if err := s.WriteFile("fix.txt", "fixed\n"); err != nil {
				return agent.Result{}, err
			}
			return agent.Result{Title: "fix the paginator", Summary: "Off by one."}, nil
		}),
		Forge:   openPR,
		Tracker: tr,
		Tickets: Tickets{InProgress: "In Progress", InReview: "In Review"},
	})
	// The id only: Cloud had no text to send, which is what a polled ticket
	// looks like.
	f.task.Ticket = protocol.Ticket{Provider: "jira", ID: "APP-7"}

	rec := &recorder{}
	if err := f.ex.Run(context.Background(), f.task, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if seen.Title != "paginator drops the last page" || seen.Body != "The last page is missing." {
		t.Errorf("the agent saw ticket %+v, want the tracker's text", seen)
	}
	if seen.URL != "https://acme.atlassian.net/browse/APP-7" {
		t.Errorf("ticket url = %q", seen.URL)
	}

	gets, moves, comments := tr.calls()
	if len(gets) != 1 || gets[0] != "APP-7" {
		t.Errorf("gets = %v", gets)
	}
	if strings.Join(moves, ",") != "APP-7->In Progress,APP-7->In Review" {
		t.Errorf("moves = %v", moves)
	}
	if len(comments) != 1 || !strings.Contains(comments[0], "https://github.com/example/app/pull/42") ||
		!strings.Contains(comments[0], "Off by one.") {
		t.Errorf("comments = %q", comments)
	}

	stages := strings.Join(rec.stages(), ",")
	for _, want := range []string{"fetch-ticket", "ticket-in-progress", "update-ticket"} {
		if !strings.Contains(stages, want) {
			t.Errorf("stages %q lack %q", stages, want)
		}
	}
	if !strings.HasPrefix(stages, "fetch-ticket,ticket-in-progress,prepare-repo") {
		t.Errorf("stages = %q, want the ticket handled before the repo is touched", stages)
	}

	// The commit refers to the ticket by the url the tracker gave it.
	body := gitOutput(t, f.task.Repo, "log", "-1", "--format=%B", "refs/heads/roost/T-1")
	if !strings.Contains(body, "Refs: https://acme.atlassian.net/browse/APP-7") {
		t.Errorf("commit body = %q", body)
	}
}

func TestATicketWithTextIsNotReadAgain(t *testing.T) {
	tr := jiraTicket()
	f := newFixture(t, Config{Tracker: tr, Tickets: Tickets{InProgress: "In Progress"}})
	f.task.Ticket = protocol.Ticket{Provider: "jira", ID: "APP-7", Title: "from the dashboard"}

	if err := f.ex.Run(context.Background(), f.task, &recorder{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	gets, moves, _ := tr.calls()
	if len(gets) != 0 {
		t.Errorf("the ticket was read %d times although Cloud sent its text", len(gets))
	}
	if len(moves) != 1 {
		t.Errorf("moves = %v, want the ticket moved to in progress regardless", moves)
	}
}

func TestAManualTicketIsLeftAlone(t *testing.T) {
	tr := jiraTicket()
	f := newFixture(t, Config{
		Agent:   writingAgent("fix.txt", "fixed\n", agent.Result{Title: "fix"}),
		Forge:   openPR,
		Tracker: tr,
		Tickets: Tickets{InProgress: "In Progress", InReview: "In Review"},
	})
	f.task.Ticket = protocol.Ticket{Provider: "manual", ID: "APP-7"}

	if err := f.ex.Run(context.Background(), f.task, &recorder{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	gets, moves, comments := tr.calls()
	if len(gets)+len(moves)+len(comments) != 0 {
		t.Errorf("a manual ticket reached the tracker: gets=%v moves=%v comments=%v", gets, moves, comments)
	}
}

func TestNoTrackerMeansNoTicketCalls(t *testing.T) {
	f := newFixture(t, Config{Tickets: Tickets{InProgress: "In Progress"}})
	f.task.Ticket = protocol.Ticket{Provider: "jira", ID: "APP-7"}
	if err := f.ex.Run(context.Background(), f.task, &recorder{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestAnUnreachableTrackerStopsTheTaskBeforeTheRepo(t *testing.T) {
	tr := jiraTicket()
	tr.moveErr = errors.New("tracker: POST /transitions: http 401")
	ran := false
	f := newFixture(t, Config{
		Agent: funcAgent(func(context.Context, protocol.TaskRun, agent.Session) (agent.Result, error) {
			ran = true
			return agent.Result{}, nil
		}),
		Tracker: tr,
		Tickets: Tickets{InProgress: "In Progress"},
	})
	f.task.Ticket = protocol.Ticket{Provider: "jira", ID: "APP-7", Title: "t"}

	rec := &recorder{}
	err := f.ex.Run(context.Background(), f.task, rec)
	if err == nil || !strings.Contains(err.Error(), "http 401") {
		t.Fatalf("Run: %v, want the tracker's error", err)
	}
	if ran {
		t.Error("the agent ran although the ticket could not be picked up")
	}
	if stages := rec.stages(); strings.Contains(strings.Join(stages, ","), "prepare-repo") {
		t.Errorf("stages = %v, want none past the ticket", stages)
	}
	statuses := rec.statuses()
	if statuses[len(statuses)-1] != protocol.TaskFailed {
		t.Errorf("statuses = %v", statuses)
	}
}

func TestNothingChangedLeavesANoteOnTheTicket(t *testing.T) {
	tr := jiraTicket()
	f := newFixture(t, Config{
		Agent: funcAgent(func(context.Context, protocol.TaskRun, agent.Session) (agent.Result, error) {
			return agent.Result{Title: "nothing", Summary: "Already fixed on main."}, nil
		}),
		Forge:   openPR,
		Tracker: tr,
		Tickets: Tickets{InReview: "In Review"},
	})
	f.task.Ticket = protocol.Ticket{Provider: "jira", ID: "APP-7", Title: "t"}

	if err := f.ex.Run(context.Background(), f.task, &recorder{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, moves, comments := tr.calls()
	if len(comments) != 1 || !strings.Contains(comments[0], "nothing to change") || !strings.Contains(comments[0], "Already fixed on main.") {
		t.Errorf("comments = %q", comments)
	}
	if len(moves) != 0 {
		t.Errorf("moves = %v, want a ticket with no pull request left where it was", moves)
	}
}

func TestAFailedTaskLeavesANoteOnTheTicket(t *testing.T) {
	tr := jiraTicket()
	f := newFixture(t, Config{
		Agent: funcAgent(func(context.Context, protocol.TaskRun, agent.Session) (agent.Result, error) {
			return agent.Result{}, errors.New("the tests never passed")
		}),
		Tracker: tr,
		Filter:  redact.New("s3cret-token"),
	})
	f.task.Ticket = protocol.Ticket{Provider: "jira", ID: "APP-7", Title: "t"}

	err := f.ex.Run(context.Background(), f.task, &recorder{})
	if err == nil {
		t.Fatal("Run succeeded")
	}
	_, _, comments := tr.calls()
	if len(comments) != 1 || !strings.Contains(comments[0], "T-1 failed") || !strings.Contains(comments[0], "the tests never passed") {
		t.Errorf("comments = %q", comments)
	}
}

func TestTicketNotesAreScrubbed(t *testing.T) {
	tr := jiraTicket()
	f := newFixture(t, Config{
		Agent: funcAgent(func(context.Context, protocol.TaskRun, agent.Session) (agent.Result, error) {
			return agent.Result{}, errors.New("auth failed for s3cret-token")
		}),
		Tracker: tr,
		Filter:  redact.New("s3cret-token"),
	})
	f.task.Ticket = protocol.Ticket{Provider: "jira", ID: "APP-7", Title: "t"}

	f.ex.Run(context.Background(), f.task, &recorder{})
	_, _, comments := tr.calls()
	if len(comments) != 1 || strings.Contains(comments[0], "s3cret-token") {
		t.Errorf("comments = %q, want the credential masked", comments)
	}
}

func TestAHandOverFailureFailsTheTaskButKeepsThePullRequest(t *testing.T) {
	tr := jiraTicket()
	tr.noteErr = errors.New("tracker: POST /comment: http 403")
	f := newFixture(t, Config{
		Agent:   writingAgent("fix.txt", "fixed\n", agent.Result{Title: "fix"}),
		Forge:   openPR,
		Tracker: tr,
		Tickets: Tickets{InReview: "In Review"},
	})
	f.task.Ticket = protocol.Ticket{Provider: "jira", ID: "APP-7", Title: "t"}

	rec := &recorder{}
	err := f.ex.Run(context.Background(), f.task, rec)
	if err == nil || !strings.Contains(err.Error(), "http 403") {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.results) != 1 || rec.results[0].PRURL != "https://github.com/example/app/pull/42" {
		t.Errorf("result = %+v, want the pull request url kept", rec.results)
	}
	_, moves, comments := tr.calls()
	if len(moves) != 0 {
		t.Errorf("moves = %v, want no transition after a failed comment", moves)
	}
	if len(comments) != 1 {
		t.Errorf("comments = %q, want no second note after the hand-over failed", comments)
	}
}

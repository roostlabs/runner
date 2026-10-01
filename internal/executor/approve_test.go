package executor

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/agent"
	"github.com/roostlabs/runner/internal/forge"
)

// approvalFixture runs an agent that changes a file, behind the approval gate,
// against a forge that records whether a pull request was asked for.
func approvalFixture(t *testing.T, cfg Config) (fixture, *bool) {
	t.Helper()
	opened := new(bool)
	cfg.ApprovePR = true
	cfg.Agent = writingAgent("fix.txt", "fixed\n", agent.Result{
		Title:   "fix the paginator",
		Summary: "The last page was dropped.",
	})
	cfg.Forge = func(_ context.Context, _ forge.Request) (forge.PR, error) {
		*opened = true
		return forge.PR{Number: 7, URL: "https://github.com/example/app/pull/7"}, nil
	}
	return newFixture(t, cfg), opened
}

// waitFor polls until the task reports the state, or the test runs out of
// patience.
func waitFor(t *testing.T, rec *recorder, want protocol.TaskStatus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, st := range rec.statuses() {
			if st == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the task never reached %q; states: %v", want, rec.statuses())
}

// onOrigin reports whether the task branch reached the remote.
func onOrigin(remote string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/roost/T-1")
	cmd.Dir = remote
	return cmd.Run() == nil
}

func TestApprovedChangeBecomesAPullRequest(t *testing.T) {
	f, opened := approvalFixture(t, Config{})
	rec := &recorder{}

	done := make(chan error, 1)
	go func() { done <- f.ex.Run(context.Background(), f.task, rec) }()
	waitFor(t, rec, protocol.TaskAwaitingApproval)

	if *opened || onOrigin(f.task.Repo) {
		t.Fatal("the change left the server before anyone approved it")
	}
	if err := f.ex.Approve("T-1", ApprovalStep, true); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !*opened {
		t.Error("no pull request was opened after approval")
	}

	states := rec.statuses()
	var sawWait, sawResume bool
	for i, st := range states {
		if st == protocol.TaskAwaitingApproval {
			sawWait = true
			if i+1 < len(states) && states[i+1] == protocol.TaskRunning {
				sawResume = true
			}
		}
	}
	if !sawWait || !sawResume {
		t.Errorf("states = %v, want awaiting_approval followed by running", states)
	}

	// The question names the step, so Cloud can answer it; the live state
	// names it too, so Cloud need not dig through the trace.
	var asked bool
	for _, st := range rec.taskStates() {
		if st.State == protocol.TaskAwaitingApproval && st.StepID == ApprovalStep {
			asked = true
		}
	}
	if !asked {
		t.Error("the awaiting state did not name the step")
	}
	stages := rec.stages()
	if stages[len(stages)-2] != "approved" || stages[len(stages)-1] != "open-pull-request" {
		t.Errorf("stages = %v", stages)
	}
}

func TestDeclinedChangeStaysOnTheServer(t *testing.T) {
	f, opened := approvalFixture(t, Config{})
	rec := &recorder{}

	done := make(chan error, 1)
	go func() { done <- f.ex.Run(context.Background(), f.task, rec) }()
	waitFor(t, rec, protocol.TaskAwaitingApproval)

	if err := f.ex.Approve("T-1", "", false); err != nil {
		t.Fatalf("Approve with the pending step: %v", err)
	}
	if err := <-done; !errors.Is(err, ErrDeclined) {
		t.Fatalf("Run error = %v, want ErrDeclined", err)
	}
	if *opened {
		t.Error("a declined pull request was opened anyway")
	}
	if onOrigin(f.task.Repo) {
		t.Error("a declined change was pushed")
	}
	if got := rec.statuses(); got[len(got)-1] != protocol.TaskFailed {
		t.Errorf("final state = %q, want failed", got[len(got)-1])
	}
	if len(rec.results) != 1 || rec.results[0].PRURL != "" {
		t.Errorf("result = %+v, want no pull request", rec.results)
	}
}

func TestApprovalOfTheWrongStepIsRefused(t *testing.T) {
	f, _ := approvalFixture(t, Config{})
	rec := &recorder{}

	done := make(chan error, 1)
	go func() { done <- f.ex.Run(context.Background(), f.task, rec) }()
	waitFor(t, rec, protocol.TaskAwaitingApproval)

	if err := f.ex.Approve("T-1", "s3", true); !errors.Is(err, ErrNoApproval) {
		t.Errorf("Approve(s3) = %v, want ErrNoApproval", err)
	}
	if err := f.ex.Approve("T-other", ApprovalStep, true); !errors.Is(err, ErrNoApproval) {
		t.Errorf("Approve(other task) = %v, want ErrNoApproval", err)
	}
	// Still waiting: the refused answers changed nothing.
	if err := f.ex.Approve("T-1", ApprovalStep, true); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := f.ex.Approve("T-1", ApprovalStep, true); !errors.Is(err, ErrNoApproval) {
		t.Errorf("a second answer = %v, want ErrNoApproval", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestUnansweredApprovalTimesOut(t *testing.T) {
	f, opened := approvalFixture(t, Config{ApprovalTimeout: 50 * time.Millisecond})
	rec := &recorder{}

	err := f.ex.Run(context.Background(), f.task, rec)
	if err == nil || errors.Is(err, ErrDeclined) {
		t.Fatalf("Run error = %v, want a timeout", err)
	}
	if *opened || onOrigin(f.task.Repo) {
		t.Error("an unapproved change left the server")
	}
	if got := rec.statuses(); got[len(got)-1] != protocol.TaskFailed {
		t.Errorf("final state = %q, want failed", got[len(got)-1])
	}
}

func TestCancelWhileAwaitingApproval(t *testing.T) {
	f, opened := approvalFixture(t, Config{})
	rec := &recorder{}

	done := make(chan error, 1)
	go func() { done <- f.ex.Run(context.Background(), f.task, rec) }()
	waitFor(t, rec, protocol.TaskAwaitingApproval)

	if !f.ex.Cancel("T-1") {
		t.Fatal("Cancel reported that T-1 was not running")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run error = %v, want context.Canceled", err)
	}
	if *opened {
		t.Error("a cancelled task opened a pull request")
	}
	if err := f.ex.Approve("T-1", ApprovalStep, true); !errors.Is(err, ErrNoApproval) {
		t.Errorf("Approve after cancel = %v, want ErrNoApproval", err)
	}
}

func TestNothingToApproveWhenIdle(t *testing.T) {
	f := newFixture(t, Config{})
	if err := f.ex.Approve("T-1", ApprovalStep, true); !errors.Is(err, ErrNoApproval) {
		t.Errorf("Approve = %v, want ErrNoApproval", err)
	}
}

func TestNoChangeAsksForNoApproval(t *testing.T) {
	f := newFixture(t, Config{
		ApprovePR: true,
		Agent:     agent.FixedFromArgv([][]string{{"true"}}),
	})
	rec := &recorder{}
	if err := f.ex.Run(context.Background(), f.task, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, st := range rec.statuses() {
		if st == protocol.TaskAwaitingApproval {
			t.Fatal("a task with nothing to publish asked for approval")
		}
	}
}

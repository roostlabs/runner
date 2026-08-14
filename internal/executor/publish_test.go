package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/agent"
	"github.com/roostlabs/runner/internal/forge"
)

// gitOutput runs one read-only git command against a repository on disk.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *recorder) stages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, ev := range r.events {
		if ev.Event != protocol.EventStage {
			continue
		}
		var p protocol.StagePayload
		if err := json.Unmarshal(ev.Payload, &p); err == nil {
			out = append(out, p.Name)
		}
	}
	return out
}

func (r *recorder) pr() (protocol.PRPayload, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if ev.Event != protocol.EventPR {
			continue
		}
		var p protocol.PRPayload
		if err := json.Unmarshal(ev.Payload, &p); err == nil {
			return p, true
		}
	}
	return protocol.PRPayload{}, false
}

// writingAgent leaves a file in the checkout and submits.
func writingAgent(name, content string, result agent.Result) funcAgent {
	return func(_ context.Context, _ protocol.TaskRun, s agent.Session) (agent.Result, error) {
		if err := s.WriteFile(name, content); err != nil {
			return agent.Result{}, err
		}
		return result, nil
	}
}

func TestFinishedWorkBecomesAPullRequest(t *testing.T) {
	var got forge.Request
	f := newFixture(t, Config{
		Agent: writingAgent("fix.txt", "fixed\n", agent.Result{
			Title:   "fix the off-by-one in the paginator",
			Summary: "The last page was dropped.",
		}),
		Forge: func(_ context.Context, req forge.Request) (forge.PR, error) {
			got = req
			return forge.PR{Number: 42, URL: "https://github.com/example/app/pull/42"}, nil
		},
	})
	f.task.Ticket = protocol.Ticket{Provider: "linear", ID: "ENG-7", URL: "https://linear.app/ENG-7"}

	rec := &recorder{}
	if err := f.ex.Run(context.Background(), f.task, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got.Head != "roost/T-1" {
		t.Errorf("head branch = %q", got.Head)
	}
	if got.Base != "main" {
		t.Errorf("base branch = %q, want the remote's head", got.Base)
	}
	if got.Title != "fix the off-by-one in the paginator" {
		t.Errorf("title = %q", got.Title)
	}
	if got.RemoteURL != f.task.Repo {
		t.Errorf("remote = %q", got.RemoteURL)
	}

	pr, ok := rec.pr()
	if !ok {
		t.Fatal("no pr event was reported")
	}
	if pr.URL != "https://github.com/example/app/pull/42" || pr.Branch != "roost/T-1" {
		t.Errorf("pr event = %+v", pr)
	}
	if len(rec.results) != 1 || rec.results[0].PRURL != pr.URL {
		t.Errorf("result = %+v, want the pull request url", rec.results)
	}

	// The branch has to actually be on the origin, or the pull request points
	// at nothing.
	if subject := gitOutput(t, f.task.Repo, "log", "-1", "--format=%s", "refs/heads/roost/T-1"); subject !=
		"fix the off-by-one in the paginator" {
		t.Errorf("origin has %q", subject)
	}
	body := gitOutput(t, f.task.Repo, "log", "-1", "--format=%B", "refs/heads/roost/T-1")
	if !strings.Contains(body, "The last page was dropped.") {
		t.Errorf("commit body dropped the summary: %q", body)
	}
	if !strings.Contains(body, "Refs: https://linear.app/ENG-7") {
		t.Errorf("commit body does not reference the ticket: %q", body)
	}
	if !strings.Contains(gitOutput(t, f.task.Repo, "log", "-1", "--format=%an", "refs/heads/roost/T-1"), "Roost") {
		t.Error("the commit is not attributed to the service account")
	}
}

// An agent that read the ticket and found nothing to change has done its job.
// A pull request with nothing in it would be worse than none.
func TestNothingChangedMeansNoPullRequest(t *testing.T) {
	var opened bool
	f := newFixture(t, Config{
		Agent: funcAgent(func(context.Context, protocol.TaskRun, agent.Session) (agent.Result, error) {
			return agent.Result{Title: "nothing to do", Summary: "Already correct."}, nil
		}),
		Forge: func(context.Context, forge.Request) (forge.PR, error) {
			opened = true
			return forge.PR{}, nil
		},
	})

	rec := &recorder{}
	if err := f.ex.Run(context.Background(), f.task, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if opened {
		t.Error("a pull request was opened for an empty change")
	}
	if _, ok := rec.pr(); ok {
		t.Error("a pr event was reported for an empty change")
	}

	stages := rec.stages()
	if len(stages) == 0 || stages[len(stages)-1] != "no-changes" {
		t.Errorf("stages = %v, want the task to say it changed nothing", stages)
	}
	if got := rec.statuses(); got[len(got)-1] != protocol.TaskDone {
		t.Errorf("final state = %v, want done", got)
	}
}

// Changes with nowhere to go are a misconfiguration the developer has to hear
// about, not a commit quietly left on the VPS.
func TestChangesWithNoForgeFailTheTask(t *testing.T) {
	f := newFixture(t, Config{
		Agent: writingAgent("fix.txt", "fixed\n", agent.Result{Title: "fix it"}),
	})

	rec := &recorder{}
	err := f.ex.Run(context.Background(), f.task, rec)
	if err == nil {
		t.Fatal("Run reported success with nowhere to publish")
	}
	if !strings.Contains(err.Error(), "pull request") {
		t.Errorf("error does not say what is missing: %v", err)
	}
	if got := rec.statuses(); got[len(got)-1] != protocol.TaskFailed {
		t.Errorf("final state = %v, want failed", got)
	}
}

func TestForgeFailureFailsTheTask(t *testing.T) {
	f := newFixture(t, Config{
		Agent: writingAgent("fix.txt", "fixed\n", agent.Result{Title: "fix it"}),
		Forge: func(context.Context, forge.Request) (forge.PR, error) {
			return forge.PR{}, errors.New("forge: http 403: Resource not accessible by integration")
		},
	})

	rec := &recorder{}
	if err := f.ex.Run(context.Background(), f.task, rec); err == nil {
		t.Fatal("Run reported success after the forge refused")
	}
	// The branch is still pushed, so the work is not lost with the task.
	if subject := gitOutput(t, f.task.Repo, "log", "-1", "--format=%s", "refs/heads/roost/T-1"); subject != "fix it" {
		t.Errorf("origin has %q, want the work to survive the failure", subject)
	}
}

// A failed task must not leave a commit behind: the branch is only published
// once the agent has said it is finished.
func TestAFailedAgentPublishesNothing(t *testing.T) {
	var opened bool
	f := newFixture(t, Config{
		Agent: funcAgent(func(_ context.Context, _ protocol.TaskRun, s agent.Session) (agent.Result, error) {
			if err := s.WriteFile("half.txt", "incomplete\n"); err != nil {
				return agent.Result{}, err
			}
			return agent.Result{}, errors.New("agent: gave up")
		}),
		Forge: func(context.Context, forge.Request) (forge.PR, error) {
			opened = true
			return forge.PR{}, nil
		},
	})

	if err := f.ex.Run(context.Background(), f.task, &recorder{}); err == nil {
		t.Fatal("Run reported success for a failed agent")
	}
	if opened {
		t.Error("a pull request was opened for a failed task")
	}
	if err := exec.Command("git", "-C", f.task.Repo, "rev-parse", "--verify",
		"refs/heads/roost/T-1").Run(); err == nil {
		t.Error("a failed task pushed a branch")
	}
}

func TestCommitMessage(t *testing.T) {
	task := protocol.TaskRun{
		TaskID: "T-1",
		Ticket: protocol.Ticket{ID: "ENG-7"},
	}

	t.Run("subject summary and reference", func(t *testing.T) {
		got := commitMessage(task, agent.Result{Title: "fix the paginator", Summary: "It dropped a page."})
		want := "fix the paginator\n\nIt dropped a page.\n\nRefs: ENG-7"
		if got != want {
			t.Errorf("commitMessage() = %q, want %q", got, want)
		}
	})

	t.Run("a long title is cut to a subject", func(t *testing.T) {
		long := strings.Repeat("a", 200)
		got := commitMessage(task, agent.Result{Title: long})
		subject, _, _ := strings.Cut(got, "\n")
		if len(subject) > subjectLimit {
			t.Errorf("subject is %d characters, want at most %d", len(subject), subjectLimit)
		}
	})

	t.Run("a multi-line title keeps only its first line", func(t *testing.T) {
		got := commitMessage(task, agent.Result{Title: "fix it\nand also this"})
		if subject, _, _ := strings.Cut(got, "\n"); subject != "fix it" {
			t.Errorf("subject = %q", subject)
		}
	})

	t.Run("an empty title still names the task", func(t *testing.T) {
		got := commitMessage(task, agent.Result{})
		if !strings.Contains(got, "T-1") {
			t.Errorf("commitMessage() = %q", got)
		}
	})

	// The commit is read as part of the repository, not as a record of what
	// wrote it.
	t.Run("no tooling attribution", func(t *testing.T) {
		got := commitMessage(task, agent.Result{Title: "fix it", Summary: "Because."})
		for _, unwanted := range []string{"Co-Authored-By", "Generated", "Roost", "AI", "assistant"} {
			if strings.Contains(got, unwanted) {
				t.Errorf("commit message mentions %q: %q", unwanted, got)
			}
		}
	})
}

// The worktree is torn down after a task, but the branch it produced is the
// task's output and has to survive.
func TestTheBranchOutlivesTheWorktree(t *testing.T) {
	var workDir string
	f := newFixture(t, Config{
		Agent: func() funcAgent {
			write := writingAgent("fix.txt", "fixed\n", agent.Result{Title: "fix it"})
			return func(ctx context.Context, task protocol.TaskRun, s agent.Session) (agent.Result, error) {
				workDir = s.WorkDir()
				return write(ctx, task, s)
			}
		}(),
		Forge: func(context.Context, forge.Request) (forge.PR, error) {
			return forge.PR{URL: "https://example.com/pull/1"}, nil
		},
	})

	if err := f.ex.Run(context.Background(), f.task, &recorder{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "fix.txt")); err == nil {
		t.Error("the worktree was left on disk")
	}
	gitOutput(t, f.task.Repo, "rev-parse", "--verify", "refs/heads/roost/T-1")
}

package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/agent"
	"github.com/roostlabs/runner/internal/forge"
	"github.com/roostlabs/runner/internal/llm"
	"github.com/roostlabs/runner/internal/redact"
	"github.com/roostlabs/runner/internal/sandbox"
)

// funcAgent drives the session directly, so these tests exercise the executor's
// half of the contract without a model in the way.
type funcAgent func(ctx context.Context, task protocol.TaskRun, s agent.Session) (agent.Result, error)

func (f funcAgent) Run(ctx context.Context, task protocol.TaskRun, s agent.Session) (agent.Result, error) {
	return f(ctx, task, s)
}

// noForge stands in where a test changes files but is not about publishing.
func noForge(context.Context, forge.Request) (forge.PR, error) {
	return forge.PR{URL: "https://example.com/pull/1"}, nil
}

// stubAPI answers every call with one end_turn reply, counting the calls.
func stubAPI(t *testing.T, usage llm.Usage) (*llm.Client, *atomic.Int32) {
	t.Helper()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"id":"m1","model":"claude-opus-5","stop_reason":"end_turn",
			"content":[{"type":"text","text":"ok"}],
			"usage":{"input_tokens":%d,"output_tokens":%d}}`, usage.Input, usage.Output)
	}))
	t.Cleanup(srv.Close)

	client, err := llm.New(llm.Options{APIKey: "sk-test", BaseURL: srv.URL, HTTP: srv.Client()})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	return client, &calls
}

func (r *recorder) llmCalls() []protocol.LLMCallPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []protocol.LLMCallPayload
	for _, ev := range r.events {
		if ev.Event != protocol.EventLLMCall {
			continue
		}
		var p protocol.LLMCallPayload
		if err := json.Unmarshal(ev.Payload, &p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// The agent's reach is the task's checkout. Anything else on the VPS —
// the config file with every credential in it, most of all — is out of bounds.
func TestSessionRefusesPathsOutsideTheCheckout(t *testing.T) {
	var readErr, writeErr error
	f := newFixture(t, Config{
		Agent: funcAgent(func(_ context.Context, _ protocol.TaskRun, s agent.Session) (agent.Result, error) {
			_, readErr = s.ReadFile("../../../etc/roost/config.json")
			writeErr = s.WriteFile("/etc/passwd", "root::0:0::/:/bin/sh")
			return agent.Result{Title: "t"}, nil
		}),
	})

	if err := f.ex.Run(context.Background(), f.task, &recorder{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if readErr == nil {
		t.Error("a relative path out of the checkout was read")
	}
	if writeErr == nil {
		t.Error("an absolute path out of the checkout was written")
	}
	if _, err := os.Stat("/etc/roost-should-not-exist"); err == nil {
		t.Error("the test wrote outside its checkout")
	}
}

func TestSessionReadsAndWritesTheCheckout(t *testing.T) {
	var (
		readBack string
		readErr  error
		workDir  string
	)
	f := newFixture(t, Config{
		KeepWorktree: true, // so the file can be checked after the task
		Forge:        noForge,
		Agent: funcAgent(func(_ context.Context, _ protocol.TaskRun, s agent.Session) (agent.Result, error) {
			workDir = s.WorkDir()
			if err := s.WriteFile("internal/pager/pager.go", "package pager\n"); err != nil {
				return agent.Result{}, err
			}
			readBack, readErr = s.ReadFile("internal/pager/pager.go")
			return agent.Result{Title: "t"}, nil
		}),
	})

	if err := f.ex.Run(context.Background(), f.task, &recorder{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	if readBack != "package pager\n" {
		t.Errorf("read back %q", readBack)
	}
	// A missing parent directory is the common case for a new file, so the
	// write has to create it rather than fail.
	if _, err := os.Stat(filepath.Join(workDir, "internal", "pager", "pager.go")); err != nil {
		t.Errorf("the file is not on disk: %v", err)
	}
}

// Cloud is not the only place a credential must not reach. What the agent reads
// goes to a model provider, which is somewhere else it should not go.
func TestSessionMasksWhatTheAgentReads(t *testing.T) {
	const secret = "ghp_supersecrettoken"

	var fromFile, fromCommand string
	f := newFixture(t, Config{
		Filter:       redact.New(secret),
		KeepWorktree: true,
		Forge:        noForge,
		Run: func(_ context.Context, _ sandbox.Spec, _ []string, stdout, _ io.Writer) (sandbox.Result, error) {
			stdout.Write([]byte("token=" + secret + "\n"))
			return sandbox.Result{ExitCode: 0}, nil
		},
		Agent: funcAgent(func(ctx context.Context, _ protocol.TaskRun, s agent.Session) (agent.Result, error) {
			if err := s.WriteFile("leak.txt", "token="+secret+"\n"); err != nil {
				return agent.Result{}, err
			}
			var err error
			if fromFile, err = s.ReadFile("leak.txt"); err != nil {
				return agent.Result{}, err
			}
			result, err := s.Exec(ctx, []string{"env"})
			if err != nil {
				return agent.Result{}, err
			}
			fromCommand = result.Output
			return agent.Result{Title: "t"}, nil
		}),
	})

	rec := &recorder{}
	if err := f.ex.Run(context.Background(), f.task, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for name, got := range map[string]string{"read_file": fromFile, "bash": fromCommand} {
		if strings.Contains(got, secret) {
			t.Errorf("%s handed the agent the credential: %q", name, got)
		}
		if !strings.Contains(got, redact.Mask) {
			t.Errorf("%s did not mask anything: %q", name, got)
		}
	}
	if strings.Contains(rec.output(), secret) {
		t.Error("the credential reached the event stream")
	}
}

func TestSessionCapsWhatOneCommandHandsBack(t *testing.T) {
	var result agent.Exec
	f := newFixture(t, Config{
		Run: func(_ context.Context, _ sandbox.Spec, _ []string, stdout, _ io.Writer) (sandbox.Result, error) {
			stdout.Write([]byte(strings.Repeat("x", maxCapture*2)))
			return sandbox.Result{ExitCode: 0}, nil
		},
		Agent: funcAgent(func(ctx context.Context, _ protocol.TaskRun, s agent.Session) (agent.Result, error) {
			var err error
			result, err = s.Exec(ctx, []string{"noisy"})
			return agent.Result{Title: "t"}, err
		}),
	})

	rec := &recorder{}
	if err := f.ex.Run(context.Background(), f.task, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Output) > maxCapture {
		t.Errorf("handed the agent %d bytes, want at most %d", len(result.Output), maxCapture)
	}
	if !result.Truncated {
		t.Error("the agent was not told its output was cut")
	}
	// Only what the agent reads is capped. The developer still gets it all.
	if got := len(rec.output()); got != maxCapture*2 {
		t.Errorf("streamed %d bytes, want the whole %d", got, maxCapture*2)
	}
}

// Cost accounting is what every budget in Roost is built from, so a model call
// is an event in the trace like any other.
func TestSessionJournalsEveryModelCall(t *testing.T) {
	client, calls := stubAPI(t, llm.Usage{Input: 1_000_000, Output: 100_000})
	f := newFixture(t, Config{
		LLM: client,
		Agent: funcAgent(func(ctx context.Context, _ protocol.TaskRun, s agent.Session) (agent.Result, error) {
			for i := 0; i < 3; i++ {
				if _, err := s.Complete(ctx, llm.Request{Messages: []llm.Message{llm.UserText("go")}}); err != nil {
					return agent.Result{}, err
				}
			}
			return agent.Result{Title: "t"}, nil
		}),
	})

	rec := &recorder{}
	if err := f.ex.Run(context.Background(), f.task, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("made %d api calls, want 3", got)
	}

	journalled := rec.llmCalls()
	if len(journalled) != 3 {
		t.Fatalf("journalled %d llm_call events, want 3", len(journalled))
	}
	// 1M input at $5 plus 100K output at $25.
	const perCall = 5.0 + 2.5
	for i, call := range journalled {
		if call.Model != "claude-opus-5" {
			t.Errorf("call %d model = %q", i, call.Model)
		}
		if call.Tokens.In != 1_000_000 || call.Tokens.Out != 100_000 {
			t.Errorf("call %d tokens = %+v", i, call.Tokens)
		}
		if call.CostUSD != perCall {
			t.Errorf("call %d cost = %v, want %v", i, call.CostUSD, perCall)
		}
	}

	if len(rec.results) != 1 {
		t.Fatalf("got %d results, want 1", len(rec.results))
	}
	res := rec.results[0]
	if res.CostUSD != 3*perCall {
		t.Errorf("result cost = %v, want %v", res.CostUSD, 3*perCall)
	}
	if res.Tokens.In != 3_000_000 || res.Tokens.Out != 300_000 {
		t.Errorf("result tokens = %+v", res.Tokens)
	}
}

// A budget that is only reported is not a budget. It has to stop the task.
func TestSessionStopsOnBudget(t *testing.T) {
	client, calls := stubAPI(t, llm.Usage{Input: 1_000_000}) // $5 a call
	var completeErr error

	f := newFixture(t, Config{
		LLM: client,
		Agent: funcAgent(func(ctx context.Context, _ protocol.TaskRun, s agent.Session) (agent.Result, error) {
			for i := 0; i < 10; i++ {
				if _, err := s.Complete(ctx, llm.Request{Messages: []llm.Message{llm.UserText("go")}}); err != nil {
					completeErr = err
					return agent.Result{}, err
				}
			}
			return agent.Result{Title: "t"}, nil
		}),
	})
	f.task.BudgetUSD = 12

	rec := &recorder{}
	err := f.ex.Run(context.Background(), f.task, rec)
	if err == nil {
		t.Fatal("the task ran past its budget")
	}
	if completeErr == nil || !strings.Contains(completeErr.Error(), "budget") {
		t.Errorf("Complete returned %v, want a budget error", completeErr)
	}
	// What a call will cost is not knowable before making it, so the check is
	// made on what has already been spent. A task can therefore overshoot by
	// one call and no more: two calls fit inside $12, the third takes it to
	// $15, and the fourth is refused.
	if got := calls.Load(); got != 3 {
		t.Errorf("made %d api calls, want 3", got)
	}
	if got := rec.statuses(); got[len(got)-1] != protocol.TaskFailed {
		t.Errorf("final state = %v, want failed", got)
	}
	// The spend that did happen is still reported, or the developer cannot see
	// where the money went.
	if rec.results[0].CostUSD != 15 {
		t.Errorf("result cost = %v, want 15", rec.results[0].CostUSD)
	}
}

// An agent that asks for a model when none is configured has to hear so, not
// receive an empty answer it will act on.
func TestSessionWithoutAModelSaysSo(t *testing.T) {
	var completeErr error
	f := newFixture(t, Config{
		Agent: funcAgent(func(ctx context.Context, _ protocol.TaskRun, s agent.Session) (agent.Result, error) {
			_, completeErr = s.Complete(ctx, llm.Request{Messages: []llm.Message{llm.UserText("go")}})
			return agent.Result{Title: "t"}, nil
		}),
	})

	if err := f.ex.Run(context.Background(), f.task, &recorder{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if completeErr == nil || !strings.Contains(completeErr.Error(), "creds.llm") {
		t.Errorf("Complete returned %v, want an error naming the missing setting", completeErr)
	}
}

func TestSessionNumbersCommandsAndSteps(t *testing.T) {
	f := newFixture(t, Config{
		Agent: funcAgent(func(ctx context.Context, _ protocol.TaskRun, s agent.Session) (agent.Result, error) {
			s.Step(ctx, "first")
			if _, err := s.Exec(ctx, []string{"one"}); err != nil {
				return agent.Result{}, err
			}
			s.Step(ctx, "second")
			if _, err := s.Exec(ctx, []string{"two"}); err != nil {
				return agent.Result{}, err
			}
			return agent.Result{Title: "t"}, nil
		}),
	})

	rec := &recorder{}
	if err := f.ex.Run(context.Background(), f.task, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var steps, cmds []string
	rec.mu.Lock()
	for _, ev := range rec.events {
		switch ev.Event {
		case protocol.EventAgentStep:
			var p protocol.AgentStepPayload
			json.Unmarshal(ev.Payload, &p)
			steps = append(steps, p.StepID)
		case protocol.EventCmdStart:
			var p protocol.CmdStartPayload
			json.Unmarshal(ev.Payload, &p)
			cmds = append(cmds, p.CmdID)
		}
	}
	rec.mu.Unlock()

	if len(steps) != 2 || steps[0] != "s1" || steps[1] != "s2" {
		t.Errorf("step ids = %v", steps)
	}
	if len(cmds) != 2 || cmds[0] != "c1" || cmds[1] != "c2" {
		t.Errorf("command ids = %v", cmds)
	}
}

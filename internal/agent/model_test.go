package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/llm"
)

// fakeSession stands in for the executor: it records what the agent asked for
// and answers with canned model replies.
type fakeSession struct {
	replies []llm.Response
	// completeErr is returned instead of the reply at that index.
	completeErr map[int]error

	requests []llm.Request
	execs    [][]string
	reads    []string
	writes   map[string]string
	steps    []string

	files    map[string]string
	execFunc func(argv []string) (Exec, error)
}

func (f *fakeSession) WorkDir() string { return "/work" }

func (f *fakeSession) Exec(_ context.Context, argv []string) (Exec, error) {
	f.execs = append(f.execs, argv)
	if f.execFunc != nil {
		return f.execFunc(argv)
	}
	return Exec{ExitCode: 0, Output: "ok\n"}, nil
}

func (f *fakeSession) ReadFile(name string) (string, error) {
	f.reads = append(f.reads, name)
	content, ok := f.files[name]
	if !ok {
		return "", fmt.Errorf("%s does not exist", name)
	}
	return content, nil
}

func (f *fakeSession) WriteFile(name, content string) error {
	if f.writes == nil {
		f.writes = map[string]string{}
	}
	f.writes[name] = content
	return nil
}

func (f *fakeSession) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	n := len(f.requests)
	f.requests = append(f.requests, req)
	if err, ok := f.completeErr[n]; ok {
		return llm.Response{}, err
	}
	if n >= len(f.replies) {
		return llm.Response{}, fmt.Errorf("fake: no reply %d", n)
	}
	return f.replies[n], nil
}

func (f *fakeSession) Step(_ context.Context, text string) {
	f.steps = append(f.steps, text)
}

// reply builds a model turn, filling Raw the way the client would.
func reply(stop string, blocks ...llm.Block) llm.Response {
	raw, err := json.Marshal(blocks)
	if err != nil {
		panic(err)
	}
	return llm.Response{
		Model:      "claude-opus-5",
		StopReason: stop,
		Blocks:     blocks,
		Raw:        raw,
	}
}

func use(id, name string, input any) llm.Block {
	raw, err := json.Marshal(input)
	if err != nil {
		panic(err)
	}
	return llm.Block{Type: "tool_use", ID: id, Name: name, Input: raw}
}

func task() protocol.TaskRun {
	return protocol.TaskRun{
		TaskID: "T-1",
		Repo:   "https://github.com/example/app.git",
		Ticket: protocol.Ticket{
			Provider: "linear",
			ID:       "ENG-7",
			Title:    "Fix the off-by-one in the paginator",
			Body:     "The last page is dropped.",
		},
	}
}

func TestModelWorksThenSubmits(t *testing.T) {
	s := &fakeSession{
		files: map[string]string{"pager.go": "package pager\n"},
		replies: []llm.Response{
			reply(llm.StopToolUse,
				llm.Text("Reading the paginator."),
				use("tu_1", toolReadFile, map[string]string{"path": "pager.go"})),
			reply(llm.StopToolUse,
				use("tu_2", toolWriteFile, map[string]string{"path": "pager.go", "content": "package pager // fixed\n"})),
			reply(llm.StopToolUse,
				use("tu_3", toolBash, map[string]string{"command": "go test ./..."})),
			reply(llm.StopToolUse,
				use("tu_4", toolSubmit, map[string]string{
					"title":   "fix off-by-one in the paginator",
					"summary": "The last page was dropped.",
				})),
		},
	}

	result, err := (Model{}).Run(context.Background(), task(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Title != "fix off-by-one in the paginator" {
		t.Errorf("title = %q", result.Title)
	}
	if result.Summary != "The last page was dropped." {
		t.Errorf("summary = %q", result.Summary)
	}

	if len(s.reads) != 1 || s.reads[0] != "pager.go" {
		t.Errorf("reads = %v", s.reads)
	}
	if got := s.writes["pager.go"]; got != "package pager // fixed\n" {
		t.Errorf("wrote %q", got)
	}
	if len(s.execs) != 1 || s.execs[0][0] != "sh" || s.execs[0][2] != "go test ./..." {
		t.Errorf("execs = %v", s.execs)
	}
	// The model's own words are what the developer reads in the trace.
	if len(s.steps) != 1 || s.steps[0] != "Reading the paginator." {
		t.Errorf("steps = %v", s.steps)
	}
}

// Every tool_use has to be answered, or the next request is rejected by the
// API. That includes the submit call itself.
func TestEveryToolUseIsAnswered(t *testing.T) {
	s := &fakeSession{
		replies: []llm.Response{
			reply(llm.StopToolUse,
				use("tu_1", toolBash, map[string]string{"command": "ls"}),
				use("tu_2", toolBash, map[string]string{"command": "pwd"})),
			reply(llm.StopToolUse, use("tu_3", toolSubmit, map[string]string{"title": "t", "summary": "s"})),
		},
	}
	if _, err := (Model{}).Run(context.Background(), task(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The second request carries the first turn, its two results, and nothing
	// unanswered.
	second := s.requests[1].Messages
	results := map[string]bool{}
	for _, msg := range second {
		for _, block := range msg.Blocks {
			if block.Type == "tool_result" {
				results[block.ToolUseID] = true
			}
		}
	}
	for _, id := range []string{"tu_1", "tu_2"} {
		if !results[id] {
			t.Errorf("%s was never answered: %v", id, results)
		}
	}
}

// A failing command is information, not a crash: the model is expected to read
// the error and try something else.
func TestFailingCommandComesBackAsAToolError(t *testing.T) {
	s := &fakeSession{
		execFunc: func([]string) (Exec, error) {
			return Exec{ExitCode: 1, Output: "undefined: foo\n"}, nil
		},
		replies: []llm.Response{
			reply(llm.StopToolUse, use("tu_1", toolBash, map[string]string{"command": "go build ./..."})),
			reply(llm.StopToolUse, use("tu_2", toolSubmit, map[string]string{"title": "t", "summary": "s"})),
		},
	}
	if _, err := (Model{}).Run(context.Background(), task(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var result llm.Block
	for _, msg := range s.requests[1].Messages {
		for _, block := range msg.Blocks {
			if block.ToolUseID == "tu_1" {
				result = block
			}
		}
	}
	if !result.IsError {
		t.Error("a non-zero exit was not reported as a tool error")
	}
	if !strings.Contains(result.Content, "exit code 1") || !strings.Contains(result.Content, "undefined: foo") {
		t.Errorf("tool result = %q", result.Content)
	}
}

func TestUnknownToolIsRefusedWithoutEndingTheTask(t *testing.T) {
	s := &fakeSession{
		replies: []llm.Response{
			reply(llm.StopToolUse, use("tu_1", "rm_minus_rf", map[string]string{})),
			reply(llm.StopToolUse, use("tu_2", toolSubmit, map[string]string{"title": "t", "summary": "s"})),
		},
	}
	if _, err := (Model{}).Run(context.Background(), task(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(s.execs) != 0 {
		t.Errorf("an unknown tool reached the sandbox: %v", s.execs)
	}
}

func TestRefusalEndsTheTask(t *testing.T) {
	s := &fakeSession{replies: []llm.Response{reply(llm.StopRefusal, llm.Text("no"))}}

	if _, err := (Model{}).Run(context.Background(), task(), s); err == nil {
		t.Fatal("a refusal did not end the task")
	}
	if len(s.requests) != 1 {
		t.Errorf("made %d calls after a refusal, want 1", len(s.requests))
	}
}

// A paused turn is the API interrupting a long reply, not a failure. Sending
// the conversation back unchanged continues it.
func TestPausedTurnIsContinued(t *testing.T) {
	s := &fakeSession{
		replies: []llm.Response{
			reply(llm.StopPauseTurn, llm.Text("thinking")),
			reply(llm.StopToolUse, use("tu_1", toolSubmit, map[string]string{"title": "t", "summary": "s"})),
		},
	}
	if _, err := (Model{}).Run(context.Background(), task(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(s.requests) != 2 {
		t.Errorf("made %d calls, want 2", len(s.requests))
	}
}

func TestTruncatedReplyEndsTheTask(t *testing.T) {
	s := &fakeSession{replies: []llm.Response{reply(llm.StopMaxTokens, llm.Text("half a th"))}}

	_, err := (Model{}).Run(context.Background(), task(), s)
	if err == nil {
		t.Fatal("a truncated reply did not end the task")
	}
	if !strings.Contains(err.Error(), "maxTokens") {
		t.Errorf("error does not say what to change: %v", err)
	}
}

func TestStoppingWithoutSubmittingIsNudgedThenFails(t *testing.T) {
	var replies []llm.Response
	for i := 0; i < 10; i++ {
		replies = append(replies, reply(llm.StopEndTurn, llm.Text("all done")))
	}
	s := &fakeSession{replies: replies}

	if _, err := (Model{}).Run(context.Background(), task(), s); err == nil {
		t.Fatal("Run succeeded without a submit")
	}
	if got, want := len(s.requests), maxNudges+1; got != want {
		t.Errorf("made %d calls, want %d", got, want)
	}
	// The nudge has to reach the model, or it is not a nudge.
	last := s.requests[len(s.requests)-1].Messages
	if !strings.Contains(last[len(last)-1].Blocks[0].Text, "submit") {
		t.Errorf("the model was never told to submit: %+v", last[len(last)-1])
	}
}

// A submit with no title would produce a nameless commit and pull request, so
// it is bounced back rather than accepted.
func TestSubmitWithoutATitleIsBounced(t *testing.T) {
	s := &fakeSession{
		replies: []llm.Response{
			reply(llm.StopToolUse, use("tu_1", toolSubmit, map[string]string{"summary": "s"})),
			reply(llm.StopToolUse, use("tu_2", toolSubmit, map[string]string{"title": "t", "summary": "s"})),
		},
	}
	result, err := (Model{}).Run(context.Background(), task(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Title != "t" {
		t.Errorf("title = %q", result.Title)
	}
}

func TestBudgetEndsTheTask(t *testing.T) {
	s := &fakeSession{
		replies:     []llm.Response{reply(llm.StopToolUse, use("tu_1", toolBash, map[string]string{"command": "ls"}))},
		completeErr: map[int]error{1: fmt.Errorf("%w: spent $1.00 of $1.00", ErrBudget)},
	}

	_, err := (Model{}).Run(context.Background(), task(), s)
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v, want ErrBudget", err)
	}
}

func TestGivesUpAfterMaxSteps(t *testing.T) {
	var replies []llm.Response
	for i := 0; i < 10; i++ {
		replies = append(replies, reply(llm.StopToolUse,
			use(fmt.Sprintf("tu_%d", i), toolBash, map[string]string{"command": "true"})))
	}
	s := &fakeSession{replies: replies}

	_, err := (Model{MaxSteps: 3}).Run(context.Background(), task(), s)
	if err == nil {
		t.Fatal("an endless loop was not stopped")
	}
	if len(s.requests) != 3 {
		t.Errorf("made %d calls, want 3", len(s.requests))
	}
}

func TestCancelStopsTheLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := &fakeSession{replies: []llm.Response{reply(llm.StopEndTurn, llm.Text("hi"))}}
	if _, err := (Model{}).Run(ctx, task(), s); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if len(s.requests) != 0 {
		t.Errorf("a cancelled task still called the model %d times", len(s.requests))
	}
}

func TestTaskPromptCarriesTheTicket(t *testing.T) {
	got := taskPrompt(task())
	for _, want := range []string{
		"https://github.com/example/app.git",
		"linear ENG-7",
		"Fix the off-by-one in the paginator",
		"The last page is dropped.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q:\n%s", want, got)
		}
	}
}

// Cloud reads the ticket and sends its text. Without it there is nothing to act
// on, and a model given an empty task will invent a plausible one.
func TestTaskPromptSaysWhenTheTicketIsEmpty(t *testing.T) {
	bare := protocol.TaskRun{TaskID: "T-2", Repo: "r", Ticket: protocol.Ticket{ID: "ENG-8"}}
	if got := taskPrompt(bare); !strings.Contains(got, "without any text") {
		t.Errorf("prompt does not flag an empty ticket:\n%s", got)
	}
}

func TestFixedRunsEveryStep(t *testing.T) {
	s := &fakeSession{}
	result, err := FixedFromArgv([][]string{{"go", "build"}, {"go", "test"}}).
		Run(context.Background(), task(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(s.execs) != 2 {
		t.Errorf("ran %d commands, want 2", len(s.execs))
	}
	if result.Title == "" {
		t.Error("Fixed produced no title")
	}
}

func TestFixedStopsAtTheFirstFailure(t *testing.T) {
	s := &fakeSession{
		execFunc: func(argv []string) (Exec, error) {
			if argv[1] == "build" {
				return Exec{ExitCode: 2}, nil
			}
			return Exec{}, nil
		},
	}
	if _, err := (FixedFromArgv([][]string{{"go", "build"}, {"go", "test"}})).
		Run(context.Background(), task(), s); err == nil {
		t.Fatal("Run ignored a failing step")
	}
	if len(s.execs) != 1 {
		t.Errorf("ran %d commands after a failure, want 1", len(s.execs))
	}
}

func TestFixedNeedsCommands(t *testing.T) {
	if _, err := (Fixed{}).Run(context.Background(), task(), &fakeSession{}); err == nil {
		t.Error("Fixed ran with no commands configured")
	}
}

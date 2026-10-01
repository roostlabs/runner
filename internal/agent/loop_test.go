package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/roostlabs/runner/internal/llm"
)

// sameCommand builds n turns that each run the same shell command.
func sameCommand(n int, command string) []llm.Response {
	var replies []llm.Response
	for i := 0; i < n; i++ {
		replies = append(replies, reply(llm.StopToolUse,
			use(fmt.Sprintf("tu_%d", i), toolBash, map[string]string{"command": command})))
	}
	return replies
}

// lastToolResult is the content the model was handed for the most recent call.
func lastToolResult(t *testing.T, s *fakeSession) string {
	t.Helper()
	req := s.requests[len(s.requests)-1]
	msg := req.Messages[len(req.Messages)-1]
	for _, b := range msg.Blocks {
		if b.Type == "tool_result" {
			return b.Content
		}
	}
	t.Fatal("the last message carries no tool result")
	return ""
}

func TestRepeatedCallIsWarnedThenStopped(t *testing.T) {
	s := &fakeSession{
		replies: sameCommand(10, "go test ./..."),
		execFunc: func([]string) (Exec, error) {
			return Exec{ExitCode: 1, Output: "FAIL: TestPager\n"}, nil
		},
	}

	_, err := (Model{}).Run(context.Background(), task(), s)
	if !errors.Is(err, ErrLoop) {
		t.Fatalf("err = %v, want ErrLoop", err)
	}
	// Two passes earn the warning, the third ends the task: the model was
	// asked three times, and the third answer was never sent back.
	if len(s.requests) != 3 {
		t.Fatalf("made %d model calls, want 3", len(s.requests))
	}
	if got := lastToolResult(t, s); !strings.Contains(got, loopWarning) {
		t.Errorf("the second result carried no warning:\n%s", got)
	}
	if len(s.steps) != 1 || !strings.Contains(s.steps[0], "repeated") {
		t.Errorf("steps = %v, want one warning step", s.steps)
	}
}

func TestFirstRepeatIsNotWarned(t *testing.T) {
	s := &fakeSession{replies: append(sameCommand(1, "ls"),
		reply(llm.StopToolUse, use("tu_s", toolSubmit, map[string]string{"title": "done"})))}

	if _, err := (Model{}).Run(context.Background(), task(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := lastToolResult(t, s); strings.Contains(got, "[roost]") {
		t.Errorf("a first call was warned about:\n%s", got)
	}
}

func TestChangedOutputIsProgress(t *testing.T) {
	runs := 0
	s := &fakeSession{
		replies: append(sameCommand(5, "go test ./..."),
			reply(llm.StopToolUse, use("tu_s", toolSubmit, map[string]string{"title": "fix"}))),
		execFunc: func([]string) (Exec, error) {
			runs++
			return Exec{ExitCode: 1, Output: fmt.Sprintf("FAIL after %d\n", runs)}, nil
		},
	}

	if _, err := (Model{}).Run(context.Background(), task(), s); err != nil {
		t.Fatalf("a command whose output changes was called a loop: %v", err)
	}
	if runs != 5 {
		t.Errorf("ran %d times, want 5", runs)
	}
}

func TestTrailingNewlineDoesNotCountAsProgress(t *testing.T) {
	outputs := []string{"ok\n", "ok", "ok\n\n"}
	s := &fakeSession{
		replies: sameCommand(3, "make"),
		execFunc: func([]string) (Exec, error) {
			out := outputs[0]
			outputs = outputs[1:]
			return Exec{Output: out}, nil
		},
	}
	if _, err := (Model{}).Run(context.Background(), task(), s); !errors.Is(err, ErrLoop) {
		t.Fatalf("err = %v, want ErrLoop", err)
	}
}

func TestRepeatedWritesAreNotALoop(t *testing.T) {
	var replies []llm.Response
	for i := 0; i < 4; i++ {
		replies = append(replies, reply(llm.StopToolUse,
			use(fmt.Sprintf("tu_%d", i), toolWriteFile, map[string]string{"path": "a.go", "content": "x"})))
	}
	replies = append(replies, reply(llm.StopToolUse, use("tu_s", toolSubmit, map[string]string{"title": "fix"})))
	s := &fakeSession{replies: replies}

	if _, err := (Model{}).Run(context.Background(), task(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestLoopIsCountedAcrossOtherCalls(t *testing.T) {
	// A repeat does not have to be consecutive: ls, test, ls, test, ls, test
	// is the same two calls going nowhere.
	var replies []llm.Response
	for i := 0; i < 3; i++ {
		replies = append(replies,
			reply(llm.StopToolUse, use(fmt.Sprintf("a_%d", i), toolBash, map[string]string{"command": "ls"})),
			reply(llm.StopToolUse, use(fmt.Sprintf("b_%d", i), toolBash, map[string]string{"command": "go test"})))
	}
	s := &fakeSession{replies: replies}

	if _, err := (Model{}).Run(context.Background(), task(), s); !errors.Is(err, ErrLoop) {
		t.Fatalf("err = %v, want ErrLoop", err)
	}
	if len(s.requests) != 5 {
		t.Errorf("made %d model calls, want 5", len(s.requests))
	}
}

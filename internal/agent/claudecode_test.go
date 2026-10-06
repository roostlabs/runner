package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/roostlabs/protocol"
)

// TestMain lets the test binary stand in for the claude CLI: ClaudeCode is
// pointed at os.Args[0], and this environment variable turns the binary into
// a fake that speaks the CLI's stream and calls back over MCP.
func TestMain(m *testing.M) {
	if os.Getenv("ROOST_FAKE_CLAUDE") != "" {
		os.Exit(fakeClaude())
	}
	os.Exit(m.Run())
}

// fakeClaude plays the CLI: finds the MCP server in --mcp-config, reads the
// file the test planted, writes one, submits, and prints a result record.
// ROOST_FAKE_CLAUDE picks the scenario.
func fakeClaude() int {
	args := os.Args[1:]
	var cfgPath, budget string
	for i, a := range args {
		switch a {
		case "--mcp-config":
			cfgPath = args[i+1]
		case "--max-budget-usd":
			budget = args[i+1]
		}
	}
	if !contains(args, "--tools", "") {
		fmt.Fprintln(os.Stderr, "fake: built-in tools were not switched off")
		return 3
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake:", err)
		return 3
	}
	var cfg struct {
		Servers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "fake:", err)
		return 3
	}
	server := cfg.Servers[mcpName]

	var prompt bytes.Buffer
	prompt.ReadFrom(os.Stdin)

	call := func(name string, arguments any) string {
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": arguments},
		})
		req, _ := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(body))
		for k, v := range server.Headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake:", err)
			os.Exit(3)
		}
		defer resp.Body.Close()
		var out struct {
			Result struct {
				Content []struct{ Text string }
			}
		}
		json.NewDecoder(resp.Body).Decode(&out)
		if len(out.Result.Content) == 0 {
			return ""
		}
		return out.Result.Content[0].Text
	}

	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	say := func(text string) {
		emit(map[string]any{"type": "assistant", "message": map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}}}})
	}
	result := func(subtype string, cost float64) {
		emit(map[string]any{
			"type": "result", "subtype": subtype, "is_error": subtype != "success",
			"num_turns": 3, "result": "done", "total_cost_usd": cost, "duration_api_ms": 1200,
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 20, "cache_read_input_tokens": 5},
			"modelUsage": map[string]any{"claude-test": map[string]any{
				"inputTokens": 10, "outputTokens": 20, "cacheReadInputTokens": 5, "costUSD": cost}},
		})
	}

	switch os.Getenv("ROOST_FAKE_CLAUDE") {
	case "submit":
		say("Reading the repository. Prompt mentions: " + strings.TrimSpace(strings.Split(prompt.String(), "\n")[0]))
		content := call(toolReadFile, map[string]any{"path": "go.mod"})
		call(toolWriteFile, map[string]any{"path": "NOTES.md", "content": "budget " + budget + "\n" + content})
		call(toolSubmit, map[string]any{"title": "Add notes", "summary": "wrote NOTES.md"})
		result("success", 0.42)
	case "no-submit":
		say("I am not sure what to do.")
		result("success", 0.1)
	case "budget":
		result("error_max_budget_usd", 5)
	case "crash":
		fmt.Fprintln(os.Stderr, "Not logged in · run claude login")
		return 1
	}
	return 0
}

func contains(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func runFake(t *testing.T, scenario string, s *fakeSession) (Result, error) {
	t.Helper()
	t.Setenv("ROOST_FAKE_CLAUDE", scenario)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The CLI runs in the checkout; the fake session has none, so any
	// directory that exists stands in.
	s.workDir = t.TempDir()
	task := protocol.TaskRun{TaskID: "T-1", Repo: "https://example.com/x.git",
		Ticket: protocol.Ticket{ID: "ROO-7", Title: "Add notes"}}
	return ClaudeCode{Bin: exe, Model: "claude-test"}.Run(context.Background(), task, s)
}

func TestClaudeCodeWorksThroughTheSessionAndSubmits(t *testing.T) {
	s := &fakeSession{files: map[string]string{"go.mod": "module x\n"}, budget: 2.5}
	result, err := runFake(t, "submit", s)
	if err != nil {
		t.Fatal(err)
	}
	if result.Title != "Add notes" {
		t.Fatalf("result %+v", result)
	}
	if got := s.writes["NOTES.md"]; got != "budget 2.5000\nmodule x\n" {
		t.Fatalf("the fake did not see the budget and the file through MCP: %q", got)
	}
	if len(s.charges) != 1 || s.charges[0].CostUSD != 0.42 || s.charges[0].Model != "claude-test" ||
		s.charges[0].Tokens.In != 15 || s.charges[0].Tokens.Out != 20 || s.charges[0].DurationMs != 1200 {
		t.Fatalf("charges %+v", s.charges)
	}
	if len(s.steps) == 0 || !strings.Contains(s.steps[0], "Repository: https://example.com/x.git") {
		t.Fatalf("the model's words were not journaled, or the prompt did not reach it: %v", s.steps)
	}
}

func TestClaudeCodeWithoutSubmitFails(t *testing.T) {
	_, err := runFake(t, "no-submit", &fakeSession{})
	if err == nil || !strings.Contains(err.Error(), "without calling submit") {
		t.Fatalf("err = %v", err)
	}
}

func TestClaudeCodeBudgetStopIsErrBudget(t *testing.T) {
	s := &fakeSession{budget: 5}
	_, err := runFake(t, "budget", s)
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v, want ErrBudget", err)
	}
	if len(s.charges) != 1 || s.charges[0].CostUSD != 5 {
		t.Fatalf("the spend was not booked: %+v", s.charges)
	}
}

func TestClaudeCodeCrashReportsStderr(t *testing.T) {
	_, err := runFake(t, "crash", &fakeSession{})
	if err == nil || !strings.Contains(err.Error(), "claude login") {
		t.Fatalf("err = %v, want the CLI's stderr", err)
	}
}

func TestClaudeCodeNeedsTheCLI(t *testing.T) {
	_, err := ClaudeCode{Bin: "definitely-not-claude-xyz"}.Run(context.Background(), protocol.TaskRun{}, &fakeSession{})
	if !errors.Is(err, errNoClaude) {
		t.Fatalf("err = %v", err)
	}
}

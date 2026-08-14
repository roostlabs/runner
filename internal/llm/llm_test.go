package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const testKey = "sk-ant-testkey-do-not-leak"

// stub is a fake Messages API. handler receives the decoded request body.
func stub(t *testing.T, handler func(w http.ResponseWriter, body map[string]any)) *Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		handler(w, body)
	}))
	t.Cleanup(srv.Close)

	c, err := New(Options{APIKey: testKey, BaseURL: srv.URL, HTTP: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func answer(w http.ResponseWriter, raw string) {
	w.Header().Set("content-type", "application/json")
	w.Write([]byte(raw))
}

// The request shape is the part a stale training prior gets wrong, so it is
// asserted rather than assumed: adaptive thinking, effort in output_config, and
// no fixed thinking budget.
func TestRequestShape(t *testing.T) {
	var got map[string]any
	c := stub(t, func(w http.ResponseWriter, body map[string]any) {
		got = body
		answer(w, `{"id":"m1","model":"claude-opus-5","stop_reason":"end_turn",
			"content":[{"type":"text","text":"hi"}],
			"usage":{"input_tokens":1,"output_tokens":1}}`)
	})

	if _, err := c.Complete(context.Background(), Request{
		System:   "be useful",
		Messages: []Message{UserText("hello")},
		Tools:    []Tool{{Name: "bash", Description: "run", InputSchema: Object(nil, "command")}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if got["model"] != DefaultModel {
		t.Errorf("model = %v, want %s", got["model"], DefaultModel)
	}
	if got["system"] != "be useful" {
		t.Errorf("system = %v", got["system"])
	}
	thinking, _ := got["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Errorf("thinking = %v, want adaptive", got["thinking"])
	}
	if _, ok := thinking["budget_tokens"]; ok {
		t.Error("request carried budget_tokens, which the current models reject")
	}
	output, _ := got["output_config"].(map[string]any)
	if output["effort"] != EffortXHigh {
		t.Errorf("output_config.effort = %v, want %s", output["effort"], EffortXHigh)
	}
	if _, ok := got["temperature"]; ok {
		t.Error("request carried temperature, which the current models reject")
	}
	if tools, _ := got["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools = %v, want exactly one", got["tools"])
	}
}

func TestCompleteParsesToolUse(t *testing.T) {
	c := stub(t, func(w http.ResponseWriter, _ map[string]any) {
		answer(w, `{"id":"m1","model":"claude-opus-5-20260115","stop_reason":"tool_use",
			"content":[
				{"type":"text","text":"looking"},
				{"type":"tool_use","id":"tu_1","name":"bash","input":{"command":"ls"}}],
			"usage":{"input_tokens":1000,"output_tokens":500}}`)
	})

	resp, err := c.Complete(context.Background(), Request{Messages: []Message{UserText("go")}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.StopReason != StopToolUse {
		t.Errorf("stopReason = %q", resp.StopReason)
	}
	if resp.Text() != "looking" {
		t.Errorf("Text() = %q", resp.Text())
	}
	uses := resp.ToolUses()
	if len(uses) != 1 || uses[0].Name != "bash" || uses[0].ID != "tu_1" {
		t.Fatalf("ToolUses() = %+v", uses)
	}
	if string(uses[0].Input) != `{"command":"ls"}` {
		t.Errorf("input = %s", uses[0].Input)
	}
	// A dated model id has to price as its base model, or every real call is
	// silently free.
	if want := 1000*5.0/1e6 + 500*25.0/1e6; resp.CostUSD != want {
		t.Errorf("cost = %v, want %v", resp.CostUSD, want)
	}
}

// Extended thinking signs its blocks, and the signature has to come back
// unchanged on the next request. Re-encoding a parsed struct would drop it, so
// assistant turns are replayed verbatim.
func TestAssistantTurnsReplayVerbatim(t *testing.T) {
	const content = `[{"type":"thinking","thinking":"hmm","signature":"sig-abc","unknown_field":7},` +
		`{"type":"text","text":"done"}]`

	var sent []any
	c := stub(t, func(w http.ResponseWriter, body map[string]any) {
		sent, _ = body["messages"].([]any)
		answer(w, `{"id":"m2","model":"claude-opus-5","stop_reason":"end_turn",
			"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})

	first := Response{Raw: json.RawMessage(content)}
	_, err := c.Complete(context.Background(), Request{Messages: []Message{
		UserText("go"),
		first.Message(),
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want 2", len(sent))
	}
	assistant, _ := sent[1].(map[string]any)
	if assistant["role"] != string(RoleAssistant) {
		t.Errorf("role = %v", assistant["role"])
	}
	blocks, _ := assistant["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("content = %v", assistant["content"])
	}
	thinking, _ := blocks[0].(map[string]any)
	if thinking["signature"] != "sig-abc" {
		t.Errorf("signature did not survive the round trip: %v", thinking)
	}
	if thinking["unknown_field"] != float64(7) {
		t.Errorf("an unknown field was dropped from a signed block: %v", thinking)
	}
}

func TestRetriesOverload(t *testing.T) {
	var calls atomic.Int32
	c := stub(t, func(w http.ResponseWriter, _ map[string]any) {
		if calls.Add(1) == 1 {
			w.Header().Set("retry-after", "0")
			w.WriteHeader(529)
			w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
			return
		}
		answer(w, `{"id":"m3","model":"claude-opus-5","stop_reason":"end_turn",
			"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})

	resp, err := c.Complete(context.Background(), Request{Messages: []Message{UserText("go")}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text() != "ok" {
		t.Errorf("Text() = %q", resp.Text())
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("made %d calls, want 2", got)
	}
}

// A bad request is a bug in the Runner, not a blip. Retrying it three times
// only spends the developer's rate limit on the same mistake.
func TestDoesNotRetryBadRequest(t *testing.T) {
	var calls atomic.Int32
	c := stub(t, func(w http.ResponseWriter, _ map[string]any) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad tool schema"}}`))
	})

	_, err := c.Complete(context.Background(), Request{Messages: []Message{UserText("go")}})
	if err == nil {
		t.Fatal("Complete accepted a 400")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("made %d calls, want 1", got)
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if apiErr.Status != http.StatusBadRequest || apiErr.Type != "invalid_request_error" {
		t.Errorf("apiErr = %+v", apiErr)
	}
	if !strings.Contains(err.Error(), "bad tool schema") {
		t.Errorf("error dropped the api's explanation: %v", err)
	}
}

// The key is the one secret this package holds. An error that quoted it would
// put it in the Runner's log and, through the error event, in Cloud's.
func TestErrorsNeverCarryTheKey(t *testing.T) {
	c := stub(t, func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	})

	_, err := c.Complete(context.Background(), Request{Messages: []Message{UserText("go")}})
	if err == nil {
		t.Fatal("Complete accepted a 401")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("error leaked the api key: %v", err)
	}
}

func TestCost(t *testing.T) {
	tests := []struct {
		name  string
		model string
		usage Usage
		want  float64
		known bool
	}{
		{"opus 5", "claude-opus-5", Usage{Input: 1_000_000}, 5, true},
		{"opus 5 output", "claude-opus-5", Usage{Output: 1_000_000}, 25, true},
		{"dated id prices as its base", "claude-opus-5-20260115", Usage{Output: 1_000_000}, 25, true},
		{"sonnet 5", "claude-sonnet-5", Usage{Input: 1_000_000}, 3, true},
		{"haiku 4.5", "claude-haiku-4-5", Usage{Output: 1_000_000}, 5, true},
		{"cache reads are a tenth", "claude-opus-5", Usage{CacheRead: 1_000_000}, 0.5, true},
		{"cache writes are a quarter more", "claude-opus-5", Usage{CacheWrite: 1_000_000}, 6.25, true},
		{"unknown model", "gpt-9", Usage{Input: 1_000_000}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, known := Cost(tt.model, tt.usage)
			if known != tt.known {
				t.Fatalf("known = %v, want %v", known, tt.known)
			}
			if got != tt.want {
				t.Errorf("Cost() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewRequiresAKey(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New accepted an empty api key")
	}
}

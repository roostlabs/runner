package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestMCP(t *testing.T, s Session) (*mcpServer, *httptest.Server, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m := &mcpServer{s: s, token: "secret", stop: cancel}
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	t.Cleanup(cancel)
	return m, srv, ctx
}

func rpc(t *testing.T, srv *httptest.Server, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	if buf.Len() > 0 && resp.StatusCode < 400 {
		if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", buf.String(), err)
		}
	}
	return resp.StatusCode, out
}

func callText(t *testing.T, out map[string]any) (string, bool) {
	t.Helper()
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", out)
	}
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	isErr, _ := result["isError"].(bool)
	return text, isErr
}

func TestMCPRefusesTheWrongToken(t *testing.T) {
	_, srv, _ := newTestMCP(t, &fakeSession{})
	status, _ := rpc(t, srv, "nope", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", status)
	}
}

func TestMCPInitializeAndListTheModelTools(t *testing.T) {
	_, srv, _ := newTestMCP(t, &fakeSession{})

	_, out := rpc(t, srv, "secret", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	result := out["result"].(map[string]any)
	if result["protocolVersion"] != "2025-03-26" {
		t.Fatalf("protocol version %v, want the client's", result["protocolVersion"])
	}

	status, _ := rpc(t, srv, "secret", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if status != http.StatusAccepted {
		t.Fatalf("notification status %d, want 202", status)
	}

	_, out = rpc(t, srv, "secret", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list := out["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, item := range list {
		names[item.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{toolBash, toolReadFile, toolWriteFile, toolSubmit} {
		if !names[want] {
			t.Errorf("tools/list is missing %s: %v", want, names)
		}
	}
	if len(list) != len(tools) {
		t.Errorf("%d tools listed, Model has %d", len(list), len(tools))
	}
}

func TestMCPRunsToolsThroughTheSession(t *testing.T) {
	s := &fakeSession{
		files: map[string]string{"go.mod": "module x\n"},
		execFunc: func(argv []string) (Exec, error) {
			return Exec{ExitCode: 0, Output: strings.Join(argv, " ")}, nil
		},
	}
	m, srv, _ := newTestMCP(t, s)

	_, out := rpc(t, srv, "secret", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"go.mod"}}}`)
	if text, isErr := callText(t, out); isErr || text != "module x\n" {
		t.Fatalf("read_file → %q err=%v", text, isErr)
	}

	_, out = rpc(t, srv, "secret", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"bash","arguments":{"command":"go test ./..."}}}`)
	if text, _ := callText(t, out); !strings.Contains(text, "sh -c go test ./...") {
		t.Fatalf("bash did not go through Exec: %q", text)
	}

	_, out = rpc(t, srv, "secret", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"write_file","arguments":{"path":"a.txt","content":"hi"}}}`)
	if _, isErr := callText(t, out); isErr || s.writes["a.txt"] != "hi" {
		t.Fatalf("write_file did not reach the session: %v", s.writes)
	}

	_, out = rpc(t, srv, "secret", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"submit","arguments":{"title":"Fix it","summary":"done"}}}`)
	if _, isErr := callText(t, out); isErr {
		t.Fatal("submit reported an error")
	}
	result, err := m.outcome()
	if err != nil || result == nil || result.Title != "Fix it" {
		t.Fatalf("outcome = %v, %v", result, err)
	}

	_, out = rpc(t, srv, "secret", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"rm_rf","arguments":{}}}`)
	if _, isErr := callText(t, out); !isErr {
		t.Fatal("an unknown tool was not an error")
	}
}

func TestMCPLoopGuardStopsTheModel(t *testing.T) {
	s := &fakeSession{execFunc: func([]string) (Exec, error) { return Exec{ExitCode: 1, Output: "FAIL"}, nil }}
	m, srv, ctx := newTestMCP(t, s)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bash","arguments":{"command":"go test"}}}`
	rpc(t, srv, "secret", body)
	_, out := rpc(t, srv, "secret", body)
	if text, _ := callText(t, out); !strings.Contains(text, "[roost]") {
		t.Fatalf("second repeat was not warned: %q", text)
	}
	rpc(t, srv, "secret", body)

	if ctx.Err() == nil {
		t.Fatal("the third repeat did not stop the model")
	}
	if _, err := m.outcome(); err == nil || !strings.Contains(err.Error(), "looping") {
		t.Fatalf("outcome err = %v, want ErrLoop", err)
	}
}

func TestMCPAnswersABatch(t *testing.T) {
	_, srv, _ := newTestMCP(t, &fakeSession{})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp",
		strings.NewReader(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"nope"}]`))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1]["error"] == nil {
		t.Fatalf("batch answer %v", out)
	}
}

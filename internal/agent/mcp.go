package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// mcpServer hands the Runner's tools to a model that runs outside the Runner,
// over MCP's streamable HTTP transport.
//
// It is the same four tools Model has, behind the same Session, so what the
// model can do does not change with who drives it: every command still goes
// into the container, every path is still checked against the checkout, and
// the loop guard still watches. The server listens on loopback only and
// expects a bearer token minted for the one task it serves, so another
// process on the VPS cannot borrow the Runner's hands.
type mcpServer struct {
	s     Session
	token string
	// stop ends the model when the server decides the task is over, which is
	// what a tripped loop guard does.
	stop context.CancelFunc

	mu     sync.Mutex
	guard  loopGuard
	result *Result
	err    error
}

// mcpVersion is the protocol revision offered when the client names none.
const mcpVersion = "2025-06-18"

// mcpName is the server's name, which is also the prefix of its tools as the
// client sees them (mcp__roost__bash).
const mcpName = "roost"

// mcpMaxBody bounds a request. A write_file call carries a whole file, so the
// cap is generous; it is still a cap.
const mcpMaxBody = 8 << 20

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// listen starts serving on a free loopback port and returns the URL the
// client should call. Closing shuts the listener; in-flight calls finish.
func (m *mcpServer) listen() (url string, closeFn func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("agent: mcp listen: %w", err)
	}
	srv := &http.Server{Handler: m, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	closeFn = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	return "http://" + ln.Addr().String() + "/mcp", closeFn, nil
}

func (m *mcpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer ") ||
		!constantTimeEqual(strings.TrimPrefix(auth, "Bearer "), m.token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		// A client ending its session. There is nothing to tear down.
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		// The server never opens a stream of its own, so a GET for one is
		// declined, which the transport allows.
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, mcpMaxBody+1))
	if err != nil || len(body) > mcpMaxBody {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	body = []byte(strings.TrimSpace(string(body)))

	// A batch is an array of requests, answered with an array; a single one is
	// answered on its own.
	var reqs []rpcRequest
	batch := len(body) > 0 && body[0] == '['
	if batch {
		err = json.Unmarshal(body, &reqs)
	} else {
		var one rpcRequest
		err = json.Unmarshal(body, &one)
		reqs = []rpcRequest{one}
	}
	if err != nil {
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()}})
		return
	}

	var out []rpcResponse
	for _, req := range reqs {
		if resp, ok := m.handle(r.Context(), req); ok {
			out = append(out, resp)
		}
	}
	switch {
	case len(out) == 0:
		// Notifications only: acknowledged without a body.
		w.WriteHeader(http.StatusAccepted)
	case batch:
		writeJSON(w, http.StatusOK, out)
	default:
		writeJSON(w, http.StatusOK, out[0])
	}
}

// handle answers one request. A notification (no id) gets no response.
func (m *mcpServer) handle(ctx context.Context, req rpcRequest) (rpcResponse, bool) {
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if isNotification {
		// notifications/initialized, notifications/cancelled: nothing to do.
		return rpcResponse{}, false
	}

	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := params.ProtocolVersion
		if version == "" {
			version = mcpVersion
		}
		resp.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpName, "version": "1"},
		}

	case "ping":
		resp.Result = map[string]any{}

	case "tools/list":
		list := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			list = append(list, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		resp.Result = map[string]any{"tools": list}

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
			break
		}
		if len(params.Arguments) == 0 {
			params.Arguments = json.RawMessage("{}")
		}
		text, isErr := m.call(ctx, params.Name, params.Arguments)
		resp.Result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": isErr,
		}

	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp, true
}

// call runs one tool. Failures are reported to the model as tool errors, the
// way Model does, except a tripped loop guard, which ends the task.
func (m *mcpServer) call(ctx context.Context, name string, input json.RawMessage) (string, bool) {
	known := false
	for _, t := range tools {
		if t.Name == name {
			known = true
			break
		}
	}
	if !known {
		return fmt.Sprintf("unknown tool %q", name), true
	}

	if name == toolSubmit {
		result, out, err := parseSubmit(input)
		if err != nil {
			return err.Error(), true
		}
		m.mu.Lock()
		m.result = &result
		m.mu.Unlock()
		return out, false
	}

	out, isErr := runTool(ctx, m.s, name, input)

	m.mu.Lock()
	repeats := m.guard.observe(name, input, out)
	if repeats >= loopStopAt && m.err == nil {
		m.err = fmt.Errorf("%w: %s repeated %d times with the same result", ErrLoop, name, repeats)
	}
	stop := m.err != nil
	m.mu.Unlock()

	switch {
	case stop:
		m.stop()
		return "the task is being stopped: this exact call has been repeated with the same result", true
	case repeats >= loopWarnAt:
		m.s.Step(ctx, "warned the model: "+name+" repeated with the same result")
		out += loopWarning
	}
	return out, isErr
}

// outcome is what the model left behind: a submitted result, or the reason the
// server ended the task.
func (m *mcpServer) outcome() (*Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.result, m.err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// constantTimeEqual compares two strings without leaking where they differ.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

var errNoClaude = errors.New("agent: the claude-code backend needs the claude CLI on PATH; " +
	"install it with `npm install -g @anthropic-ai/claude-code` and sign in with `claude login`")

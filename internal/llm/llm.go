// Package llm talks to the Anthropic Messages API.
//
// It speaks HTTP directly instead of using the official SDK, for the same
// reason the rest of the Runner keeps its dependency list at two: the binary an
// installer drops onto someone's VPS has to stay a single static file, and the
// Runner needs a narrow slice of the API — one endpoint, tool use, and the
// token counts that budgets are built from. That slice is this file.
//
// The API key never leaves this package. It goes into a request header and is
// not carried on any value that gets logged.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the Anthropic API.
	DefaultBaseURL = "https://api.anthropic.com"
	// DefaultModel is what an agent runs on unless the config says otherwise.
	DefaultModel = "claude-opus-5"
	// DefaultMaxTokens caps one response. Thinking counts towards it.
	DefaultMaxTokens = 16384
	// DefaultTimeout bounds one request, including retries of its own body.
	DefaultTimeout = 10 * time.Minute
	// DefaultMaxRetries is how often a retryable failure is tried again.
	DefaultMaxRetries = 3

	apiVersion = "2023-06-01"
)

// Reasoning effort levels. Higher spends more thinking tokens per turn and
// needs fewer turns; xhigh is what Anthropic recommends for agentic coding.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
	EffortXHigh  = "xhigh"
	EffortMax    = "max"
)

// Stop reasons. Refusal and PauseTurn are the two an agent loop has to handle
// beyond the obvious ones: the first ends the task, the second means the turn
// was interrupted mid-flight and should simply be continued.
const (
	StopEndTurn      = "end_turn"
	StopMaxTokens    = "max_tokens"
	StopStopSequence = "stop_sequence"
	StopToolUse      = "tool_use"
	StopRefusal      = "refusal"
	StopPauseTurn    = "pause_turn"
)

// Role is who a message is from.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Block is one content block. The fields are a union over the block types the
// Runner produces or reads; the ones that do not apply stay empty.
type Block struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Text returns a text block.
func Text(s string) Block { return Block{Type: "text", Text: s} }

// ToolResult returns the answer to one tool_use block.
func ToolResult(toolUseID, content string, isError bool) Block {
	return Block{Type: "tool_result", ToolUseID: toolUseID, Content: content, IsError: isError}
}

// Message is one turn of the conversation.
//
// Raw exists because of extended thinking: a thinking block carries a signature
// that has to come back byte for byte on the next request, and re-encoding a
// parsed struct would drop whatever this build does not know about. Assistant
// turns are therefore replayed verbatim, and only the messages the Runner
// composes itself are built from Blocks.
type Message struct {
	Role   Role
	Blocks []Block
	Raw    json.RawMessage
}

// UserMessage composes a user turn.
func UserMessage(blocks ...Block) Message {
	return Message{Role: RoleUser, Blocks: blocks}
}

// UserText composes a user turn holding one text block.
func UserText(s string) Message { return UserMessage(Text(s)) }

// MarshalJSON writes the wire form, preferring Raw when it is set.
func (m Message) MarshalJSON() ([]byte, error) {
	content := m.Raw
	if content == nil {
		encoded, err := json.Marshal(m.Blocks)
		if err != nil {
			return nil, err
		}
		content = encoded
	}
	return json.Marshal(struct {
		Role    Role            `json:"role"`
		Content json.RawMessage `json:"content"`
	}{Role: m.Role, Content: content})
}

// Tool is a function the model may call.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema Schema `json:"input_schema"`
}

// Schema is the JSON Schema subset the Runner's tools need.
type Schema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties,omitempty"`
	Required   []string            `json:"required,omitempty"`
}

// Property is one field of a tool's input.
type Property struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// Object returns an object schema whose listed fields are required.
func Object(props map[string]Property, required ...string) Schema {
	return Schema{Type: "object", Properties: props, Required: required}
}

// Request is one call.
type Request struct {
	System   string
	Messages []Message
	Tools    []Tool
}

// Usage counts the tokens one call consumed.
type Usage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

// Response is one answer.
type Response struct {
	ID         string
	Model      string
	StopReason string
	// Blocks is the parsed view, for deciding what to do next.
	Blocks []Block
	// Raw is the same content verbatim, for replaying into the next request.
	Raw      json.RawMessage
	Usage    Usage
	CostUSD  float64
	Duration time.Duration
}

// Message returns the response as an assistant turn to append to the history.
func (r Response) Message() Message {
	return Message{Role: RoleAssistant, Raw: r.Raw}
}

// Text concatenates the response's text blocks.
func (r Response) Text() string {
	var b strings.Builder
	for _, block := range r.Blocks {
		if block.Type == "text" && block.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// ToolUses returns the tool calls the model asked for.
func (r Response) ToolUses() []Block {
	var uses []Block
	for _, block := range r.Blocks {
		if block.Type == "tool_use" {
			uses = append(uses, block)
		}
	}
	return uses
}

// Options configures a Client.
type Options struct {
	APIKey     string
	BaseURL    string
	Model      string
	Effort     string
	MaxTokens  int
	Timeout    time.Duration
	MaxRetries int
	HTTP       *http.Client
}

// Client calls the Messages API.
type Client struct {
	http       *http.Client
	key        string
	baseURL    string
	model      string
	effort     string
	maxTokens  int
	maxRetries int
}

// New returns a Client, filling in the defaults.
func New(o Options) (*Client, error) {
	if o.APIKey == "" {
		return nil, errors.New("llm: no api key; set creds.llm in the runner config")
	}
	c := &Client{
		http:       o.HTTP,
		key:        o.APIKey,
		baseURL:    strings.TrimSuffix(o.BaseURL, "/"),
		model:      o.Model,
		effort:     o.Effort,
		maxTokens:  o.MaxTokens,
		maxRetries: o.MaxRetries,
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	if c.model == "" {
		c.model = DefaultModel
	}
	if c.effort == "" {
		c.effort = EffortXHigh
	}
	if c.maxTokens <= 0 {
		c.maxTokens = DefaultMaxTokens
	}
	if c.maxRetries < 0 {
		c.maxRetries = 0
	} else if c.maxRetries == 0 {
		c.maxRetries = DefaultMaxRetries
	}
	if c.http == nil {
		timeout := o.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		c.http = &http.Client{Timeout: timeout}
	}
	return c, nil
}

// Model reports which model the client calls, for the trace.
func (c *Client) Model() string { return c.model }

type thinkingConfig struct {
	Type string `json:"type"`
}

type outputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type wireRequest struct {
	Model        string          `json:"model"`
	MaxTokens    int             `json:"max_tokens"`
	System       string          `json:"system,omitempty"`
	Messages     []Message       `json:"messages"`
	Tools        []Tool          `json:"tools,omitempty"`
	Thinking     *thinkingConfig `json:"thinking,omitempty"`
	OutputConfig *outputConfig   `json:"output_config,omitempty"`
}

type wireResponse struct {
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Content    json.RawMessage `json:"content"`
	Usage      Usage           `json:"usage"`
}

// Complete makes one call.
//
// Thinking is adaptive: the model decides how much to spend per turn rather
// than being handed a fixed budget, which is what the current models take.
func (c *Client) Complete(ctx context.Context, req Request) (Response, error) {
	if len(req.Messages) == 0 {
		return Response{}, errors.New("llm: no messages")
	}
	body, err := json.Marshal(wireRequest{
		Model:        c.model,
		MaxTokens:    c.maxTokens,
		System:       req.System,
		Messages:     req.Messages,
		Tools:        req.Tools,
		Thinking:     &thinkingConfig{Type: "adaptive"},
		OutputConfig: &outputConfig{Effort: c.effort},
	})
	if err != nil {
		return Response{}, fmt.Errorf("llm: encode request: %w", err)
	}

	start := time.Now()
	raw, err := c.post(ctx, body)
	if err != nil {
		return Response{}, err
	}

	var wire wireResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Response{}, fmt.Errorf("llm: decode response: %w", err)
	}
	var blocks []Block
	if len(wire.Content) > 0 {
		if err := json.Unmarshal(wire.Content, &blocks); err != nil {
			return Response{}, fmt.Errorf("llm: decode content: %w", err)
		}
	}
	cost, _ := Cost(wire.Model, wire.Usage)
	return Response{
		ID:         wire.ID,
		Model:      wire.Model,
		StopReason: wire.StopReason,
		Blocks:     blocks,
		Raw:        wire.Content,
		Usage:      wire.Usage,
		CostUSD:    cost,
		Duration:   time.Since(start),
	}, nil
}

// post sends one request body, retrying the failures that are worth retrying.
func (c *Client) post(ctx context.Context, body []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		raw, err := c.attempt(ctx, body)
		if err == nil {
			return raw, nil
		}
		lastErr = err

		var apiErr *APIError
		retryable := errors.As(err, &apiErr) && apiErr.Retryable()
		if !retryable && !isTransport(err) {
			return nil, err
		}
		if attempt >= c.maxRetries {
			return nil, fmt.Errorf("llm: gave up after %d attempts: %w", attempt+1, lastErr)
		}

		wait := backoff(attempt)
		if apiErr != nil && apiErr.retryAfterSet {
			wait = apiErr.RetryAfter
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) attempt(ctx context.Context, body []byte) ([]byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("anthropic-version", apiVersion)
	httpReq.Header.Set("x-api-key", c.key)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// The URL is in the error but the key is a header, so this is safe to
		// pass upward.
		return nil, fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("llm: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, newAPIError(resp, raw)
	}
	return raw, nil
}

// maxResponseBytes bounds what one answer may be, so a runaway response cannot
// exhaust a 2 GB box.
const maxResponseBytes = 32 << 20

// APIError is a non-2xx answer from the API.
type APIError struct {
	Status     int
	Type       string
	Message    string
	RetryAfter time.Duration

	// retryAfterSet distinguishes a header the server did send with a value of
	// zero — retry now — from one it did not send at all.
	retryAfterSet bool
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("llm: api returned %d", e.Status)
	}
	return fmt.Sprintf("llm: api returned %d (%s): %s", e.Status, e.Type, e.Message)
}

// Retryable reports whether trying the same request again could work.
func (e *APIError) Retryable() bool {
	switch e.Status {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}

func newAPIError(resp *http.Response, raw []byte) *APIError {
	e := &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}

	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error.Message != "" {
		e.Type = envelope.Error.Type
		e.Message = envelope.Error.Message
	}
	if v := resp.Header.Get("retry-after"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
			e.retryAfterSet = true
		}
	}
	return e
}

// isTransport reports whether an error came from the connection rather than
// from the API, which is worth retrying for the same reasons a 503 is.
func isTransport(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return false
	}
	// A cancelled or expired context is the caller's decision, not a blip.
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// backoff grows exponentially with jitter, so a rate limit hit by several
// requests at once does not turn into a synchronised retry.
func backoff(attempt int) time.Duration {
	const base = time.Second
	const max = 30 * time.Second

	wait := base << attempt
	if wait > max || wait <= 0 {
		wait = max
	}
	return wait/2 + time.Duration(rand.Int64N(int64(wait/2)+1))
}

// Price is what a model costs, in US dollars per million tokens.
type Price struct {
	In  float64
	Out float64
}

// Published list prices. Cost accounting is the basis of every budget in Roost,
// so an unknown model is reported as unpriced rather than as free.
var prices = map[string]Price{
	"claude-fable-5":    {In: 10, Out: 50},
	"claude-opus-5":     {In: 5, Out: 25},
	"claude-opus-4-8":   {In: 5, Out: 25},
	"claude-opus-4-7":   {In: 5, Out: 25},
	"claude-opus-4-6":   {In: 5, Out: 25},
	"claude-sonnet-5":   {In: 3, Out: 15},
	"claude-sonnet-4-6": {In: 3, Out: 15},
	"claude-haiku-4-5":  {In: 1, Out: 5},
}

// Cache reads are a tenth of the input price; writing a five-minute cache entry
// is a quarter more than the input price.
const (
	cacheReadFactor  = 0.1
	cacheWriteFactor = 1.25
)

// Cost prices one call, reporting false when the model is not in the table.
//
// The API answers with a dated model id, so the lookup is by longest matching
// prefix: claude-opus-5-20260115 prices as claude-opus-5.
func Cost(model string, u Usage) (float64, bool) {
	price, ok := priceOf(model)
	if !ok {
		return 0, false
	}
	const perToken = 1_000_000.0
	dollars := float64(u.Input)*price.In +
		float64(u.CacheRead)*price.In*cacheReadFactor +
		float64(u.CacheWrite)*price.In*cacheWriteFactor +
		float64(u.Output)*price.Out
	return dollars / perToken, true
}

func priceOf(model string) (Price, bool) {
	if p, ok := prices[model]; ok {
		return p, true
	}
	var best string
	for name := range prices {
		if strings.HasPrefix(model, name) && len(name) > len(best) {
			best = name
		}
	}
	if best == "" {
		return Price{}, false
	}
	return prices[best], true
}

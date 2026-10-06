package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/roostlabs/protocol"
)

// ClaudeCode is an agent that thinks through the developer's own Claude Code.
//
// A developer on a Claude subscription has the claude CLI and no API key, and
// this backend is for them. The Runner runs `claude -p` on the VPS with the
// CLI's own tools switched off and one MCP server switched on: this Runner,
// serving bash, read_file, write_file and submit over loopback. The model
// decides on the host; everything it does happens in the sandbox, through the
// same Session as any other agent. Sign-in is the CLI's own (`claude login`),
// and the Runner never reads or forwards it.
//
// Cost comes from the CLI's result record, and the remaining budget is passed
// down as --max-budget-usd so the CLI stops before spending what the task does
// not have. The trace is assembled from the MCP calls, which the Runner sees
// anyway, plus the model's own words from the CLI's event stream.
type ClaudeCode struct {
	// Bin is the CLI to run. Empty means "claude" on PATH.
	Bin string
	// Model is passed as --model. Empty leaves the CLI's default.
	Model string
	// MaxTurns bounds the conversation. Zero uses DefaultMaxSteps.
	MaxTurns int
}

// claudeTools are the tools the CLI may use: ours and only ours.
var claudeTools = []string{
	"mcp__" + mcpName + "__" + toolBash,
	"mcp__" + mcpName + "__" + toolReadFile,
	"mcp__" + mcpName + "__" + toolWriteFile,
	"mcp__" + mcpName + "__" + toolSubmit,
}

// claudePromptAddendum tells the model the tools it sees are the whole set.
// Claude Code's own prompt is replaced, so nothing describes tools that have
// been switched off.
const claudePromptAddendum = `

Your tools are bash, read_file, write_file and submit from the roost server, and nothing else. There is no file editor, no shell of your own and no web; everything goes through those four. Finish by calling submit.`

// Run carries out the task.
func (c ClaudeCode) Run(ctx context.Context, task protocol.TaskRun, s Session) (Result, error) {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return Result{}, errNoClaude
	}
	maxTurns := c.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxSteps
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	token, err := randomToken()
	if err != nil {
		return Result{}, err
	}
	srv := &mcpServer{s: s, token: token, stop: cancel}
	url, closeSrv, err := srv.listen()
	if err != nil {
		return Result{}, err
	}
	defer closeSrv()

	// The MCP config goes through a file rather than argv so the token is not
	// in the process list.
	cfgPath, err := writeMCPConfig(url, token)
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(cfgPath)

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--no-session-persistence",
		"--strict-mcp-config",
		"--mcp-config", cfgPath,
		"--tools", "",
		"--allowedTools", strings.Join(claudeTools, ","),
		"--permission-mode", "dontAsk",
		"--system-prompt", systemPrompt + claudePromptAddendum,
		"--max-turns", strconv.Itoa(maxTurns),
	}
	if budget := s.Budget(); budget > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(budget, 'f', 4, 64))
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = s.WorkDir()
	cmd.Stdin = strings.NewReader(taskPrompt(task))
	cmd.WaitDelay = 5 * time.Second
	stderr := &tail{limit: 4096}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("agent: start %s: %w", bin, err)
	}

	var final *claudeResult
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		var ev claudeEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			for _, block := range ev.Message.Content {
				if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
					s.Step(ctx, block.Text)
				}
			}
		case "result":
			var res claudeResult
			if err := json.Unmarshal(line, &res); err == nil {
				final = &res
			}
		}
	}
	waitErr := cmd.Wait()

	// Whatever happened, what was spent is booked.
	var chargeErr error
	if final != nil {
		chargeErr = c.charge(context.WithoutCancel(ctx), s, final)
	}

	submitted, loopErr := srv.outcome()
	switch {
	case loopErr != nil:
		return Result{}, loopErr
	case ctx.Err() != nil:
		return Result{}, ctx.Err()
	case final == nil:
		return Result{}, fmt.Errorf("agent: %s ended without a result: %w%s", bin, exitError(waitErr), stderr.hint())
	}

	switch final.Subtype {
	case "success":
	case "error_max_turns":
		return Result{}, fmt.Errorf("agent: gave up after %d turns without finishing", final.NumTurns)
	case "error_max_budget_usd":
		return Result{}, fmt.Errorf("%w: claude stopped at $%.4f", ErrBudget, final.TotalCostUSD)
	default:
		return Result{}, fmt.Errorf("agent: claude %s: %s%s", final.Subtype, final.reason(), stderr.hint())
	}

	if submitted != nil {
		// Work that was handed over is worth more than a budget already
		// spent: the pull request still gets opened.
		return *submitted, nil
	}
	if chargeErr != nil {
		return Result{}, chargeErr
	}
	return Result{}, errors.New("agent: the model stopped without calling submit")
}

// charge books what the CLI reports it spent, one record per model so the
// trace says who answered.
func (c ClaudeCode) charge(ctx context.Context, s Session, res *claudeResult) error {
	if len(res.ModelUsage) == 0 {
		if res.TotalCostUSD == 0 && res.Usage.Output == 0 {
			return nil
		}
		model := c.Model
		if model == "" {
			model = "claude-code"
		}
		return s.Charge(ctx, Charge{
			Model:      model,
			Tokens:     protocol.Tokens{In: res.Usage.inputTotal(), Out: res.Usage.Output},
			CostUSD:    res.TotalCostUSD,
			DurationMs: res.DurationAPIMs,
		})
	}
	var first error
	duration := res.DurationAPIMs
	for model, u := range res.ModelUsage {
		err := s.Charge(ctx, Charge{
			Model:      model,
			Tokens:     protocol.Tokens{In: u.Input + u.CacheRead + u.CacheWrite, Out: u.Output},
			CostUSD:    u.CostUSD,
			DurationMs: duration,
		})
		// The CLI reports one duration for the whole run; it goes on the
		// first record so the task total is right.
		duration = 0
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

// claudeEvent is the shape shared by every line of the CLI's stream.
type claudeEvent struct {
	Type    string `json:"type"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// claudeResult is the CLI's final record.
type claudeResult struct {
	Subtype       string      `json:"subtype"`
	IsError       bool        `json:"is_error"`
	NumTurns      int         `json:"num_turns"`
	Result        string      `json:"result"`
	Errors        []string    `json:"errors"`
	TotalCostUSD  float64     `json:"total_cost_usd"`
	DurationAPIMs int64       `json:"duration_api_ms"`
	Usage         claudeUsage `json:"usage"`
	ModelUsage    map[string]struct {
		Input      int64   `json:"inputTokens"`
		Output     int64   `json:"outputTokens"`
		CacheRead  int64   `json:"cacheReadInputTokens"`
		CacheWrite int64   `json:"cacheCreationInputTokens"`
		CostUSD    float64 `json:"costUSD"`
	} `json:"modelUsage"`
}

type claudeUsage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

func (u claudeUsage) inputTotal() int64 { return u.Input + u.CacheRead + u.CacheWrite }

func (r *claudeResult) reason() string {
	if len(r.Errors) > 0 {
		return strings.Join(r.Errors, "; ")
	}
	if r.Result != "" {
		return r.Result
	}
	return "no detail"
}

func writeMCPConfig(url, token string) (string, error) {
	f, err := os.CreateTemp("", "roost-mcp-*.json")
	if err != nil {
		return "", fmt.Errorf("agent: mcp config: %w", err)
	}
	cfg := map[string]any{
		"mcpServers": map[string]any{
			mcpName: map[string]any{
				"type":    "http",
				"url":     url,
				"headers": map[string]string{"Authorization": "Bearer " + token},
			},
		},
	}
	if err := json.NewEncoder(f).Encode(cfg); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("agent: mcp config: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func randomToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func exitError(err error) error {
	if err == nil {
		return errors.New("exit status 0")
	}
	return err
}

// tail keeps the end of a stream, which is where a CLI says why it died.
type tail struct {
	limit int
	buf   []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tail) hint() string {
	s := strings.TrimSpace(string(t.buf))
	if s == "" {
		return ""
	}
	return "\n" + s
}

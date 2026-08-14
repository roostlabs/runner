package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/llm"
)

// DefaultMaxSteps bounds one task's conversation. A model that has not finished
// in this many turns is looping, and a loop with a token meter attached is the
// expensive kind of bug.
const DefaultMaxSteps = 40

// maxToolOutput caps what one command reports back to the model. A build log is
// worth reading the end of; it is not worth paying to send in full.
const maxToolOutput = 16000

// maxNudges is how often the agent is reminded to finish before the task is
// called failed.
const maxNudges = 2

// Model is an agent that decides through an LLM.
//
// The loop is the ordinary one: send the conversation, run whatever tools come
// back, append the results, repeat until the model calls submit. Everything it
// can do is a Session call, so the container, the redaction and the budget hold
// no matter what the model decides to try.
type Model struct {
	// MaxSteps bounds the conversation. Zero uses DefaultMaxSteps.
	MaxSteps int
}

// Run carries out the task.
func (m Model) Run(ctx context.Context, task protocol.TaskRun, s Session) (Result, error) {
	maxSteps := m.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}

	messages := []llm.Message{llm.UserText(taskPrompt(task))}
	nudges := 0

	for step := 0; step < maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}

		resp, err := s.Complete(ctx, llm.Request{
			System:   systemPrompt,
			Messages: messages,
			Tools:    tools,
		})
		if err != nil {
			return Result{}, err
		}
		messages = append(messages, resp.Message())

		if text := resp.Text(); text != "" {
			s.Step(ctx, text)
		}

		switch resp.StopReason {
		case llm.StopRefusal:
			// The model declined. Retrying would only spend more of the budget
			// on the same answer.
			return Result{}, errors.New("agent: the model declined this task")

		case llm.StopPauseTurn:
			// A long turn was interrupted rather than finished. Sending the
			// conversation back unchanged continues it.
			continue

		case llm.StopMaxTokens:
			// A cut-off turn can hold a half-written tool call, which is not
			// something to guess at.
			return Result{}, errors.New(
				"agent: the model's reply was cut off at the token limit; raise agent.maxTokens")
		}

		uses := resp.ToolUses()
		if len(uses) == 0 {
			if nudges >= maxNudges {
				return Result{}, errors.New("agent: the model stopped without calling submit")
			}
			nudges++
			messages = append(messages, llm.UserText(
				"You have not finished yet. Either use a tool to keep working, or call submit "+
					"to hand over what you have. If the task cannot be done, say so through submit."))
			continue
		}

		var (
			results []llm.Block
			done    *Result
		)
		for _, use := range uses {
			if use.Name == toolSubmit {
				result, out, err := parseSubmit(use.Input)
				if err != nil {
					results = append(results, llm.ToolResult(use.ID, err.Error(), true))
					continue
				}
				done = &result
				results = append(results, llm.ToolResult(use.ID, out, false))
				continue
			}
			out, isErr := m.runTool(ctx, s, use)
			results = append(results, llm.ToolResult(use.ID, out, isErr))
		}

		// The results are appended even when submitting, so the conversation
		// stays a valid transcript if anything later wants to read it.
		messages = append(messages, llm.UserMessage(results...))
		if done != nil {
			return *done, nil
		}
	}
	return Result{}, fmt.Errorf("agent: gave up after %d steps without finishing", maxSteps)
}

// Tool names.
const (
	toolBash      = "bash"
	toolReadFile  = "read_file"
	toolWriteFile = "write_file"
	toolSubmit    = "submit"
)

var tools = []llm.Tool{
	{
		Name: toolBash,
		Description: "Run a shell command in the sandbox container, from the root of the " +
			"repository checkout. Use it to explore the code, build, and run tests. " +
			"stdout and stderr come back interleaved, truncated if very long.",
		InputSchema: llm.Object(map[string]llm.Property{
			"command": {Type: "string", Description: "The shell command to run."},
		}, "command"),
	},
	{
		Name:        toolReadFile,
		Description: "Read a file from the repository checkout.",
		InputSchema: llm.Object(map[string]llm.Property{
			"path": {Type: "string", Description: "Path relative to the repository root."},
		}, "path"),
	},
	{
		Name: toolWriteFile,
		Description: "Write a file in the repository checkout, creating it and any missing " +
			"parent directories, and replacing it entirely if it exists.",
		InputSchema: llm.Object(map[string]llm.Property{
			"path":    {Type: "string", Description: "Path relative to the repository root."},
			"content": {Type: "string", Description: "The complete new contents of the file."},
		}, "path", "content"),
	},
	{
		Name: toolSubmit,
		Description: "Finish the task. Call this once the work is done, or to explain why it " +
			"cannot be. Whatever is left in the checkout is committed and opened as a " +
			"pull request for a human to review.",
		InputSchema: llm.Object(map[string]llm.Property{
			"title": {Type: "string", Description: "One line describing the change, in the imperative mood."},
			"summary": {Type: "string", Description: "What was changed and why, and anything a " +
				"reviewer should check. Markdown."},
		}, "title", "summary"),
	},
}

const systemPrompt = `You are Roost, an autonomous software engineer working on a developer's own machine.

You are given a ticket and a checkout of the repository it belongs to. Work through the ticket and leave the checkout in the state you want reviewed.

How to work:
- Read before you write. Look at how the surrounding code is written and match it: its naming, its structure, its error handling, its level of comment.
- Prefer the smallest change that genuinely fixes the ticket. Do not reformat, rename or refactor code the ticket did not ask about.
- Run the repository's own build and tests with bash before you finish, and fix what you break.
- Do not commit, push, or create a branch. When you call submit, whatever is in the checkout is committed and opened as a pull request.
- Do not touch version control history, CI configuration, or credentials.

When you are done, or when you are certain the ticket cannot be carried out, call submit and say so plainly. A submit that explains why something is impossible is a useful answer; a change that pretends to work is not.

Everything you run happens inside a disposable container with a CPU and memory limit, so a runaway command costs the developer real time.`

// taskPrompt states the ticket.
func taskPrompt(task protocol.TaskRun) string {
	var b strings.Builder
	b.WriteString("Repository: ")
	b.WriteString(task.Repo)
	b.WriteString("\n")

	if task.Ticket.ID != "" {
		b.WriteString("Ticket: ")
		if task.Ticket.Provider != "" {
			b.WriteString(task.Ticket.Provider)
			b.WriteString(" ")
		}
		b.WriteString(task.Ticket.ID)
		if task.Ticket.URL != "" {
			b.WriteString(" (")
			b.WriteString(task.Ticket.URL)
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	if task.Ticket.Title != "" {
		b.WriteString("\n")
		b.WriteString(task.Ticket.Title)
		b.WriteString("\n")
	}
	if task.Ticket.Body != "" {
		b.WriteString("\n")
		b.WriteString(task.Ticket.Body)
		b.WriteString("\n")
	}
	if task.Ticket.Title == "" && task.Ticket.Body == "" {
		// Cloud sends the ticket text; without it there is nothing to act on,
		// and inventing a plausible task would be worse than saying so.
		b.WriteString("\nThe ticket arrived without any text. Call submit and report that.\n")
	}
	return b.String()
}

// runTool carries out one tool call, reporting failures back to the model
// rather than ending the task: a bad path or a failing command is something the
// model can recover from, and usually does.
func (m Model) runTool(ctx context.Context, s Session, use llm.Block) (string, bool) {
	switch use.Name {
	case toolBash:
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(use.Input, &in); err != nil {
			return "invalid input: " + err.Error(), true
		}
		if strings.TrimSpace(in.Command) == "" {
			return "command is empty", true
		}
		result, err := s.Exec(ctx, []string{"sh", "-c", in.Command})
		if err != nil {
			if ctx.Err() != nil {
				return "cancelled", true
			}
			return "could not run the command: " + err.Error(), true
		}
		return formatExec(result), result.ExitCode != 0

	case toolReadFile:
		var in struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(use.Input, &in); err != nil {
			return "invalid input: " + err.Error(), true
		}
		content, err := s.ReadFile(in.Path)
		if err != nil {
			return err.Error(), true
		}
		return truncate(content), false

	case toolWriteFile:
		var in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(use.Input, &in); err != nil {
			return "invalid input: " + err.Error(), true
		}
		if err := s.WriteFile(in.Path, in.Content); err != nil {
			return err.Error(), true
		}
		return fmt.Sprintf("wrote %s (%d bytes)", in.Path, len(in.Content)), false

	default:
		return fmt.Sprintf("unknown tool %q", use.Name), true
	}
}

func parseSubmit(input json.RawMessage) (Result, string, error) {
	var in struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{}, "", fmt.Errorf("invalid input: %w", err)
	}
	in.Title = strings.TrimSpace(in.Title)
	in.Summary = strings.TrimSpace(in.Summary)
	if in.Title == "" {
		return Result{}, "", errors.New("title is required")
	}
	return Result{Title: in.Title, Summary: in.Summary}, "submitted", nil
}

func formatExec(result Exec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit code %d\n", result.ExitCode)
	if result.Output == "" {
		b.WriteString("(no output)")
		return b.String()
	}
	if result.Truncated {
		b.WriteString("(output truncated)\n")
	}
	b.WriteString(result.Output)
	return b.String()
}

// truncate keeps the end, which is where a build says what went wrong.
func truncate(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	return "(truncated)\n" + s[len(s)-maxToolOutput:]
}

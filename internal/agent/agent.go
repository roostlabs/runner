// Package agent decides what a task should do.
//
// An agent does not execute anything itself. It is handed a Session and can act
// only through it, which is what keeps the Runner's promises out of the
// agent's hands: the Session is implemented by the executor, so every command
// goes into a container, every byte of output is scrubbed of credentials,
// every event reaches the journal before the network, and every model call is
// priced and checked against the budget. An agent that tried to skip one of
// those would have to be given a way to, and it is not.
//
// Fixed is the agent that runs a configured list of commands. It exists because
// the execution path was worth getting right against a plan that is the same
// every time, and it stays because a repository with a fixed build and test
// sequence does not need a model to rediscover it.
package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/llm"
)

// ErrBudget means the task has spent its allowance. It ends the task: the
// alternative is a loop that quietly keeps billing.
var ErrBudget = errors.New("agent: budget exhausted")

// Exec is how one command finished.
//
// Output is stdout and stderr interleaved, already masked, and capped — an
// agent needs to read what happened, not receive a whole build log back.
type Exec struct {
	ExitCode   int
	Output     string
	Truncated  bool
	DurationMs int64
}

// Session is everything an agent is allowed to do.
type Session interface {
	// WorkDir is the task's checkout on the host. It is mounted into every
	// sandbox, so a path is the same to the agent and to a command it runs.
	WorkDir() string

	// Exec runs argv in a sandbox. A non-zero exit is a result, not an error;
	// an error means the command could not be run at all.
	Exec(ctx context.Context, argv []string) (Exec, error)

	// ReadFile and WriteFile work on the checkout, on paths relative to it.
	// A path that leaves the checkout is refused.
	ReadFile(name string) (string, error)
	WriteFile(name, content string) error

	// Complete calls the model. It journals the call with its tokens and cost
	// and returns ErrBudget once the task has spent its allowance.
	Complete(ctx context.Context, req llm.Request) (llm.Response, error)

	// Step records what the agent is doing, for the trace the developer reads.
	Step(ctx context.Context, text string)
}

// Result is what the agent produced.
type Result struct {
	// Title is one line, used for the commit subject and the pull request.
	Title string
	// Summary explains what was changed and why, for the pull request body.
	Summary string
}

// Agent carries out a task through a Session.
type Agent interface {
	Run(ctx context.Context, task protocol.TaskRun, s Session) (Result, error)
}

// Step is one command Fixed runs.
type Step struct {
	// Name is what the step is called in the trace.
	Name string
	// Argv is the command and its arguments. It is not a shell line: no
	// quoting, no expansion, no injection.
	Argv []string
}

// Fixed runs the same steps for every task, taken from the Runner's config.
type Fixed struct {
	Steps []Step
}

// Run executes each step in order and stops at the first failure.
func (f Fixed) Run(ctx context.Context, task protocol.TaskRun, s Session) (Result, error) {
	if len(f.Steps) == 0 {
		return Result{}, errors.New("agent: no commands configured; set sandbox.commands in the runner config")
	}
	for _, step := range f.Steps {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		s.Step(ctx, "running "+step.Name)

		result, err := s.Exec(ctx, step.Argv)
		if err != nil {
			return Result{}, err
		}
		if result.ExitCode != 0 {
			return Result{}, fmt.Errorf("agent: %s exited with code %d", step.Name, result.ExitCode)
		}
	}
	return Result{
		Title:   "run configured commands",
		Summary: fmt.Sprintf("Ran %d configured commands for %s.", len(f.Steps), task.TaskID),
	}, nil
}

// FixedFromArgv builds a Fixed agent from raw argv lists, naming each step after
// its command.
func FixedFromArgv(commands [][]string) Fixed {
	steps := make([]Step, 0, len(commands))
	for _, argv := range commands {
		if len(argv) == 0 {
			continue
		}
		steps = append(steps, Step{Name: argv[0], Argv: argv})
	}
	return Fixed{Steps: steps}
}

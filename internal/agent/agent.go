// Package agent decides what a task should do inside the sandbox.
//
// The interface exists before there is anything intelligent behind it. The
// execution path — prepare a repo, isolate a worktree, run a command under
// limits, stream its output with credentials masked — is worth getting right on
// its own, and it is far easier to get right against a plan that is the same
// every time. Fixed is that plan; an LLM-driven implementation replaces it
// without the pipeline changing.
package agent

import (
	"context"
	"errors"

	"github.com/roostlabs/protocol"
)

// Step is one command to run in the sandbox.
type Step struct {
	// Name is what the step is called in the trace.
	Name string
	// Argv is the command and its arguments. It is not a shell line: no
	// quoting, no expansion, no injection.
	Argv []string
}

// Agent turns a task into the steps that carry it out.
//
// workDir is the path of the task's worktree on the host, for an agent that
// needs to read the repository before deciding.
type Agent interface {
	Plan(ctx context.Context, task protocol.TaskRun, workDir string) ([]Step, error)
}

// Fixed runs the same steps for every task, taken from the Runner's config.
type Fixed struct {
	Steps []Step
}

// Plan returns the configured steps.
func (f Fixed) Plan(context.Context, protocol.TaskRun, string) ([]Step, error) {
	if len(f.Steps) == 0 {
		return nil, errors.New("agent: no commands configured; set sandbox.commands in the runner config")
	}
	return f.Steps, nil
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

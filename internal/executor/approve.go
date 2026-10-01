package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/agent"
	"github.com/roostlabs/runner/internal/repo"
)

// ApprovalStep is the step id a task waits on before it opens a pull request.
// There is one gate and it has one name, so a Cloud that lost the live state
// can still answer it.
const ApprovalStep = "approve-pr"

// DefaultApprovalTimeout is how long a task waits to be approved when the
// config names no limit. A ticket filed in the evening is read in the morning,
// so a day covers the case this exists for; a task that nobody answered in a
// day is not going to be answered, and it is holding the one execution slot.
const DefaultApprovalTimeout = 24 * time.Hour

// ErrDeclined means the developer looked at what the agent did and said no.
// The commit stays on the task's branch in the clone on the VPS; nothing is
// pushed and no pull request is opened.
var ErrDeclined = errors.New("executor: the pull request was declined")

// ErrNoApproval means no task is waiting on the step that was answered.
var ErrNoApproval = errors.New("executor: no task is awaiting approval on that step")

// approval is a pending question and the channel its answer arrives on.
type approval struct {
	step   string
	answer chan bool
}

// Approve answers the step a task is waiting on. An empty stepID means the
// pending step, whichever it is. It reports ErrNoApproval when the task is not
// running, not waiting, or waiting on a different step; a second answer to the
// same step is also refused, since the first one has already been acted on.
func (e *Executor) Approve(taskID, stepID string, approved bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active == nil || e.active.id != taskID || e.active.pending == nil {
		return ErrNoApproval
	}
	pending := e.active.pending
	if stepID != "" && stepID != pending.step {
		return fmt.Errorf("%w: waiting on %s, not %s", ErrNoApproval, pending.step, stepID)
	}
	e.active.pending = nil
	pending.answer <- approved
	return nil
}

// awaitApproval parks the task until the developer answers, the wait times
// out, or the task is cancelled.
//
// The task is put into awaiting_approval with the step named, so the dashboard
// can show a button; the step itself is an agent_step event carrying what is
// being asked, so the question is in the trace and survives a reconnect. The
// answer is journalled as a stage, for the same reason.
func (e *Executor) awaitApproval(
	ctx context.Context,
	task protocol.TaskRun,
	worktree *repo.Worktree,
	result agent.Result,
	sess *session,
	rep Reporter,
) error {
	pending := &approval{step: ApprovalStep, answer: make(chan bool, 1)}

	e.mu.Lock()
	if e.active == nil || e.active.id != task.TaskID {
		e.mu.Unlock()
		return errors.New("executor: the task is no longer active")
	}
	e.active.pending = pending
	e.mu.Unlock()

	// Whatever happens, the question is withdrawn: an answer that arrives
	// after a timeout or a cancel must find nothing to answer.
	defer func() {
		e.mu.Lock()
		if e.active != nil && e.active.pending == pending {
			e.active.pending = nil
		}
		e.mu.Unlock()
	}()

	timeout := e.cfg.ApprovalTimeout
	if timeout <= 0 {
		timeout = DefaultApprovalTimeout
	}

	e.emit(ctx, task.TaskID, protocol.EventAgentStep, protocol.AgentStepPayload{
		StepID: ApprovalStep,
		Text:   sess.scrub(approvalQuestion(worktree.Branch, result)),
	}, rep)
	e.stateWith(ctx, task.TaskID, protocol.TaskState{
		State:  protocol.TaskAwaitingApproval,
		StepID: ApprovalStep,
	}, rep)
	e.log.Info("waiting for approval", "taskId", task.TaskID, "branch", worktree.Branch, "timeout", timeout)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("executor: nobody approved the pull request within %s", timeout)
	case approved := <-pending.answer:
		if !approved {
			e.emit(ctx, task.TaskID, protocol.EventStage, protocol.StagePayload{Name: "declined"}, rep)
			return ErrDeclined
		}
		e.emit(ctx, task.TaskID, protocol.EventStage, protocol.StagePayload{Name: "approved"}, rep)
		e.state(ctx, task.TaskID, protocol.TaskRunning, "", rep)
		return nil
	}
}

// approvalQuestion is what the developer reads before deciding. It is the
// agent's own account of the change: the commit is on the VPS and can be
// inspected there, but the summary is what a phone at breakfast shows.
func approvalQuestion(branch string, result agent.Result) string {
	var b strings.Builder
	b.WriteString("Open a pull request from ")
	b.WriteString(branch)
	b.WriteString("?\n\n")
	b.WriteString(strings.TrimSpace(result.Title))
	if summary := strings.TrimSpace(result.Summary); summary != "" {
		b.WriteString("\n\n")
		b.WriteString(summary)
	}
	return b.String()
}

package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/agent"
	"github.com/roostlabs/runner/internal/tracker"
)

// Tickets says how a task moves its ticket through the tracker. Empty states
// are skipped: a developer who only wants the comment configures neither.
type Tickets struct {
	// InProgress is the state a ticket moves to when its task starts.
	InProgress string
	// InReview is the state a ticket moves to once a pull request is open.
	InReview string
}

// tracked returns the tracker that owns this task's ticket, or nil.
//
// Ownership is by provider: a ticket the dashboard typed in by hand carries
// "manual" and is nobody's to update, even when a tracker is configured and the
// id happens to look like one of its keys.
func (s *session) tracked() tracker.Client {
	t := s.creds.Tracker
	if t == nil || s.task.Ticket.ID == "" || s.task.Ticket.Provider != string(t.Kind()) {
		return nil
	}
	return t
}

// pickUp reads the ticket's text when the task arrived without it, and moves
// the ticket into the in-progress state.
//
// Both happen before the repository is touched. A tracker that cannot be
// reached now will not take the pull request later either, and finding that out
// costs nothing yet; finding it out after the agent has run costs the whole
// task's spend.
func (e *Executor) pickUp(ctx context.Context, task *protocol.TaskRun, sess *session, rep Reporter) error {
	t := sess.tracked()
	if t == nil {
		return nil
	}

	if task.Ticket.Title == "" && task.Ticket.Body == "" {
		e.emit(ctx, task.TaskID, protocol.EventStage, protocol.StagePayload{Name: "fetch-ticket"}, rep)
		ticket, err := t.Get(ctx, task.Ticket.ID)
		if err != nil {
			return err
		}
		task.Ticket.Title = ticket.Title
		task.Ticket.Body = ticket.Body
		if task.Ticket.URL == "" {
			task.Ticket.URL = ticket.URL
		}
		sess.task = *task
		e.log.Info("read the ticket from the tracker",
			"taskId", task.TaskID, "ticket", task.Ticket.ID, "title", ticket.Title)
	}

	if state := e.cfg.Tickets.InProgress; state != "" {
		e.emit(ctx, task.TaskID, protocol.EventStage, protocol.StagePayload{Name: "ticket-in-progress"}, rep)
		if err := t.Transition(ctx, task.Ticket.ID, state); err != nil {
			return err
		}
	}
	return nil
}

// handOver tells the ticket about the pull request and moves it to review.
//
// This is part of the task, not a courtesy after it: the promise is a ticket
// that says where its pull request is, and a task whose ticket does not is not
// finished. The pull request url is already in the result by the time this
// runs, so a failure here is a failed task with a link to what it did open.
func (e *Executor) handOver(ctx context.Context, task protocol.TaskRun, prURL string, result agent.Result, sess *session, rep Reporter) error {
	t := sess.tracked()
	if t == nil {
		return nil
	}
	e.emit(ctx, task.TaskID, protocol.EventStage, protocol.StagePayload{Name: "update-ticket"}, rep)

	body := "Pull request opened for review: " + prURL
	if summary := strings.TrimSpace(result.Summary); summary != "" {
		body += "\n\n" + summary
	}
	if err := t.Comment(ctx, task.Ticket.ID, sess.scrub(body)); err != nil {
		return err
	}
	if state := e.cfg.Tickets.InReview; state != "" {
		if err := t.Transition(ctx, task.Ticket.ID, state); err != nil {
			return err
		}
	}
	e.log.Info("updated the ticket", "taskId", task.TaskID, "ticket", task.Ticket.ID, "url", prURL)
	return nil
}

// leaveNote comments on the ticket when the task ends without a pull request:
// the agent changed nothing, or something failed. Best effort, because the task
// is already over and there is nothing left to fail.
func (e *Executor) leaveNote(ctx context.Context, sess *session, text string) {
	t := sess.tracked()
	if t == nil {
		return
	}
	if err := t.Comment(ctx, sess.task.Ticket.ID, sess.scrub(text)); err != nil {
		e.log.Warn("could not comment on the ticket",
			"taskId", sess.task.TaskID, "ticket", sess.task.Ticket.ID, "err", err)
	}
}

// noChangesNote is what a ticket learns from a task that found nothing to do.
func noChangesNote(result agent.Result) string {
	text := "The agent read this ticket and found nothing to change, so no pull request was opened."
	if summary := strings.TrimSpace(result.Summary); summary != "" {
		text += "\n\n" + summary
	}
	return text
}

// failureNote is what a ticket learns from a task that did not finish.
func failureNote(taskID string, err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Sprintf("Task %s was cancelled before it opened a pull request.", taskID)
	case errors.Is(err, agent.ErrBudget):
		return fmt.Sprintf("Task %s stopped: it reached its budget before it opened a pull request.", taskID)
	default:
		return fmt.Sprintf("Task %s failed before it opened a pull request: %v", taskID, err)
	}
}

// scrub masks credential values in text that leaves the box. A comment on a
// ticket is outside, the same as a line on the channel.
func (s *session) scrub(text string) string {
	if s.creds.Filter == nil {
		return text
	}
	return s.creds.Filter.String(text)
}

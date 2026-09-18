package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/roostlabs/protocol"
)

// pollTimeout bounds one request to the tracker.
const pollTimeout = 30 * time.Second

// retryAfter is how long a ticket that was started but is still in the ready
// state on the next poll is left alone.
//
// A ticket normally leaves the ready state the moment its task begins, so
// seeing it again means the task could not pick it up: the tracker refused the
// transition, the repository was unreachable, or the executor was busy. None of
// those are fixed by asking again in a minute, and asking every minute would
// start a failing task every minute.
const retryAfter = 10 * time.Minute

// poll asks the tracker for ready tickets on an interval and starts a task for
// the first one it has not tried recently. One at a time: concurrency is 1, and
// the rest are still there on the next round.
func (s *service) poll(ctx context.Context) {
	cfg := s.config().Tracker
	interval := cfg.PollInterval()
	s.log.Info("polling the tracker for ready tickets",
		"tracker", cfg.Kind, "project", cfg.Project, "state", cfg.States.Ready,
		"repo", cfg.Repo, "every", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	tried := map[string]time.Time{}
	for {
		if task, ok := s.pick(ctx, tried); ok {
			s.log.Info("picked up a ready ticket",
				"taskId", task.TaskID, "ticket", task.Ticket.ID, "title", task.Ticket.Title)
			s.startTask(task, "")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pick is one round: the task to start, if there is one. tried remembers which
// tickets were started and when, so a ticket that stays ready is retried on a
// schedule rather than every round.
func (s *service) pick(ctx context.Context, tried map[string]time.Time) (protocol.TaskRun, bool) {
	if id, busy := s.running(); busy {
		s.log.Debug("not polling: a task is running", "taskId", id)
		return protocol.TaskRun{}, false
	}

	cfg := s.config().Tracker
	t := s.currentCreds().Tracker
	if t == nil {
		// The credential was cleared from the dashboard. Polling stays
		// alive and picks up again when a new one arrives.
		s.log.Debug("not polling: no tracker credential")
		return protocol.TaskRun{}, false
	}

	listCtx, cancel := context.WithTimeout(ctx, pollTimeout)
	tickets, err := t.List(listCtx, cfg.States.Ready)
	cancel()
	if err != nil {
		s.log.Warn("could not list ready tickets", "tracker", t.Kind(), "err", err)
		return protocol.TaskRun{}, false
	}

	now := time.Now()
	for id, at := range tried {
		if now.Sub(at) >= retryAfter {
			delete(tried, id)
		}
	}

	for _, ticket := range tickets {
		if _, recent := tried[ticket.ID]; recent {
			continue
		}
		tried[ticket.ID] = now

		return protocol.TaskRun{
			TaskID: "t-" + newTaskID(),
			Repo:   cfg.Repo,
			Ticket: protocol.Ticket{
				Provider: string(t.Kind()),
				ID:       ticket.ID,
				URL:      ticket.URL,
				Title:    ticket.Title,
				Body:     ticket.Body,
			},
		}, true
	}
	return protocol.TaskRun{}, false
}

// newTaskID names a task the Runner started itself. Cloud names the ones it
// sends; these need to be told apart from nothing else, only from each other.
func newTaskID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

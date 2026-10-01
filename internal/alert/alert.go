// Package alert tells the developer when a task needs them.
//
// It runs on the VPS, beside the credentials, because the Telegram token and
// the webhook secret are credentials and credentials live on the VPS. What goes
// out is the same text a ticket comment would carry: task, ticket, state,
// reason — already masked by the Runner's redaction filter, since the message
// leaves the box.
//
// Delivery is best effort and never on the task's path: a sink that is down
// loses one message and a log line says so. An alert is not the record; the
// journal is.
package alert

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Kind says why the developer is being told.
type Kind string

const (
	// KindDone is a task that finished, with or without a pull request.
	KindDone Kind = "done"
	// KindFailed is a task that stopped on an error, a loop, a timeout or a
	// declined pull request.
	KindFailed Kind = "failed"
	// KindBudget is a task that stopped because it reached its budget. It is
	// a failure too, but the one that costs money is worth its own name.
	KindBudget Kind = "budget"
	// KindApproval is a task waiting for the developer to approve its pull
	// request. It is the alert that exists so the developer does not have to
	// keep the dashboard open.
	KindApproval Kind = "awaiting_approval"
)

// Kinds lists every kind, which is also the default set a Notifier sends.
var Kinds = []Kind{KindDone, KindFailed, KindBudget, KindApproval}

// Event is one thing worth telling the developer.
type Event struct {
	Kind   Kind   `json:"kind"`
	TaskID string `json:"taskId"`
	// Ticket is what the task was for; zero when it had none.
	Ticket Ticket `json:"ticket"`
	// Reason is why, for failures: the error, already masked.
	Reason string `json:"reason,omitempty"`
	// PRURL is set when a pull request was opened.
	PRURL string `json:"prUrl,omitempty"`
	// CostUSD is what the task spent.
	CostUSD float64 `json:"costUsd"`
	// TS is when it happened, Unix milliseconds.
	TS int64 `json:"ts"`
}

// Ticket is the part of a ticket an alert needs.
type Ticket struct {
	Provider string `json:"provider,omitempty"`
	ID       string `json:"id,omitempty"`
	URL      string `json:"url,omitempty"`
	Title    string `json:"title,omitempty"`
}

// Sink delivers one event somewhere.
type Sink interface {
	Name() string
	Send(ctx context.Context, ev Event) error
}

// sendTimeout bounds one delivery. The task is not waiting on it, so this is
// about not leaking goroutines against a sink that never answers.
const sendTimeout = 15 * time.Second

// Notifier fans events out to its sinks, in the background.
type Notifier struct {
	sinks []Sink
	kinds map[Kind]bool
	log   *slog.Logger
	wg    sync.WaitGroup
}

// New returns a Notifier for the sinks. kinds narrows what is sent; empty
// means everything.
func New(log *slog.Logger, kinds []Kind, sinks ...Sink) *Notifier {
	n := &Notifier{sinks: sinks, log: log, kinds: map[Kind]bool{}}
	if len(kinds) == 0 {
		kinds = Kinds
	}
	for _, k := range kinds {
		n.kinds[k] = true
	}
	return n
}

// Alert delivers ev to every sink without waiting for any of them. ctx may be
// a finished task's context: delivery is detached from it, since a cancelled
// task is still something to report.
func (n *Notifier) Alert(ctx context.Context, ev Event) {
	if n == nil || len(n.sinks) == 0 || !n.kinds[ev.Kind] {
		return
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	for _, sink := range n.sinks {
		n.wg.Add(1)
		go func(sink Sink) {
			defer n.wg.Done()
			sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
			defer cancel()
			if err := sink.Send(sendCtx, ev); err != nil {
				n.log.Warn("alert not delivered", "sink", sink.Name(), "kind", ev.Kind, "taskId", ev.TaskID, "err", err)
				return
			}
			n.log.Debug("alert delivered", "sink", sink.Name(), "kind", ev.Kind, "taskId", ev.TaskID)
		}(sink)
	}
}

// Wait blocks until every delivery started so far has finished, for shutdown
// and tests.
func (n *Notifier) Wait() {
	if n != nil {
		n.wg.Wait()
	}
}

// Text renders an event as the plain message a chat shows. It is the same for
// every sink that carries text, so a developer reading two of them reads one
// thing.
func Text(ev Event) string {
	var b strings.Builder
	switch ev.Kind {
	case KindDone:
		if ev.PRURL != "" {
			fmt.Fprintf(&b, "Roost: task %s opened a pull request", ev.TaskID)
		} else {
			fmt.Fprintf(&b, "Roost: task %s finished with nothing to change", ev.TaskID)
		}
	case KindFailed:
		fmt.Fprintf(&b, "Roost: task %s failed", ev.TaskID)
	case KindBudget:
		fmt.Fprintf(&b, "Roost: task %s stopped at its budget", ev.TaskID)
	case KindApproval:
		fmt.Fprintf(&b, "Roost: task %s is waiting for your approval", ev.TaskID)
	default:
		fmt.Fprintf(&b, "Roost: task %s: %s", ev.TaskID, ev.Kind)
	}
	if ev.Ticket.ID != "" || ev.Ticket.Title != "" {
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(ev.Ticket.ID + " " + ev.Ticket.Title))
	}
	if ev.Reason != "" {
		b.WriteString("\n")
		b.WriteString(ev.Reason)
	}
	if ev.CostUSD > 0 {
		fmt.Fprintf(&b, "\n$%.2f spent", ev.CostUSD)
	}
	if ev.PRURL != "" {
		b.WriteString("\n")
		b.WriteString(ev.PRURL)
	} else if ev.Ticket.URL != "" {
		b.WriteString("\n")
		b.WriteString(ev.Ticket.URL)
	}
	return b.String()
}

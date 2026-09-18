// Package tracker talks to the task manager a ticket lives in.
//
// It is the other end of the promise: a ticket goes in and a pull request comes
// out, and the ticket should say so. The package reads a ticket's text, lists
// the ones waiting for an agent, moves a ticket between states and leaves a
// comment on it. Nothing here decides anything; the executor and the poller do.
//
// Jira and Linear are both implemented behind one interface. Two from the start
// rather than one, because a single implementation never shows where the
// abstraction leaks — the second one does, and it is cheaper to find that out
// now than on the second connector a month later.
//
// The credential is the Runner's, on the VPS, like the git token. Cloud never
// sees it and never has to: the Runner is the one that reaches the tracker.
package tracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Kind names a tracker. It doubles as the Ticket.Provider value a task carries,
// which is how the executor knows a ticket is this tracker's to update.
type Kind string

const (
	KindJira   Kind = "jira"
	KindLinear Kind = "linear"
)

// DefaultLinearAPI is Linear's GraphQL endpoint. Jira has no default: every
// site has its own hostname.
const DefaultLinearAPI = "https://api.linear.app/graphql"

const (
	maxResponseBytes = 1 << 20
	defaultTimeout   = time.Minute
	// listLimit bounds one List call. A solo developer does not have fifty
	// tickets waiting for an agent, and if they do the rest are read next time.
	listLimit = 50
)

// Ticket is a work item as the tracker holds it.
type Ticket struct {
	// ID is the human key: PROJ-12 in Jira, ENG-12 in Linear.
	ID    string
	URL   string
	Title string
	// Body is the description as plain text. Jira stores it as a document
	// tree; it is flattened here so the agent reads prose, not JSON.
	Body string
	// State is the workflow state's name as the tracker displays it.
	State string
}

// Client is what a tracker can do for the Runner.
//
// States are named, not identified: the developer configures "In Progress",
// not the id their instance gave that column. Each implementation resolves the
// name when it needs to.
type Client interface {
	Kind() Kind
	// Get reads one ticket by its key.
	Get(ctx context.Context, id string) (Ticket, error)
	// List returns the project's tickets in the named state, oldest first.
	List(ctx context.Context, state string) ([]Ticket, error)
	// Transition moves a ticket into the named state.
	Transition(ctx context.Context, id, state string) error
	// Comment adds a comment. Body is plain text; paragraphs are separated by
	// blank lines.
	Comment(ctx context.Context, id, body string) error
}

// Options configures a Client.
type Options struct {
	Kind Kind
	// Token is the tracker credential, from creds.taskManager.
	Token string
	// User is the account the token belongs to. Jira Cloud authenticates an
	// API token with basic auth as email:token, so it is required there.
	// Linear authenticates with the key alone and ignores it.
	User string
	// BaseURL is the Jira site, e.g. https://acme.atlassian.net, and required
	// for Jira. For Linear it overrides the GraphQL endpoint, for a test.
	BaseURL string
	// Project is the Jira project key or the Linear team key: the set of
	// tickets List reads from.
	Project string
	HTTP    *http.Client
}

// New returns the Client for o.Kind.
func New(o Options) (Client, error) {
	if o.Token == "" {
		return nil, errors.New("tracker: no task manager token; set creds.taskManager in the runner config")
	}
	if o.Project == "" {
		return nil, errors.New("tracker: no project; set tracker.project in the runner config")
	}
	h := o.HTTP
	if h == nil {
		h = &http.Client{Timeout: defaultTimeout}
	}

	switch o.Kind {
	case KindJira:
		return newJira(o, h)
	case KindLinear:
		return newLinear(o, h)
	case "":
		return nil, fmt.Errorf("tracker: no kind; set tracker.kind to %q or %q", KindJira, KindLinear)
	default:
		return nil, fmt.Errorf("tracker: unknown tracker %q; use %q or %q", o.Kind, KindJira, KindLinear)
	}
}

// call is one API request.
type call struct {
	method  string
	url     string
	body    []byte
	headers map[string]string
	// explain turns an error body into one line. The trackers disagree about
	// the shape of those, so each provides its own.
	explain func(raw []byte, status int) string
}

// send performs a call and reports the status alongside the error, so a caller
// can tell a missing ticket from a broken connection.
func send(ctx context.Context, h *http.Client, c call) ([]byte, int, error) {
	var reader io.Reader
	if c.body != nil {
		reader = bytes.NewReader(c.body)
	}
	req, err := http.NewRequestWithContext(ctx, c.method, c.url, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("tracker: %w", err)
	}
	for name, value := range c.headers {
		req.Header.Set(name, value)
	}
	if c.body != nil {
		req.Header.Set("content-type", "application/json")
	}
	req.Header.Set("accept", "application/json")

	resp, err := h.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("tracker: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("tracker: read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		// The path, not the full URL: a base URL can carry credentials in its
		// userinfo, and this error is journalled and streamed.
		path := c.url
		if u, parseErr := url.Parse(c.url); parseErr == nil {
			path = u.Path
		}
		return raw, resp.StatusCode, fmt.Errorf("tracker: %s %s: %s",
			c.method, path, c.explain(raw, resp.StatusCode))
	}
	return raw, resp.StatusCode, nil
}

// paragraphs splits a plain-text body on blank lines, trimming each part. Both
// trackers want a comment as a sequence of paragraphs in their own shape.
func paragraphs(body string) []string {
	var out []string
	for _, part := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// sameState compares state names the way a developer types them: case and
// surrounding space do not matter.
func sameState(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// Package forge opens the pull request a finished task becomes.
//
// A pull request rather than a push to the default branch, and a service
// account rather than the developer's own credential: the promise Roost makes
// is a change waiting for review in the morning, not a change already merged.
// Nothing here can merge anything, and the token it uses should not be able to
// either.
//
// GitHub is the only forge implemented. Another one is a matter of another file
// in this package, not of a different design.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultAPIBase is GitHub's API.
const DefaultAPIBase = "https://api.github.com"

const (
	apiVersion       = "2022-11-28"
	maxResponseBytes = 1 << 20
	defaultTimeout   = time.Minute
)

// Request is one pull request to open.
type Request struct {
	// RemoteURL is the repository's git remote, in either the https or the ssh
	// form. It names the repository; it is not what is called.
	RemoteURL string
	// Base is the branch to merge into, Head the branch holding the work.
	Base string
	Head string

	Title string
	Body  string
}

// PR is an opened pull request.
type PR struct {
	Number int
	URL    string
	// Existed reports that the pull request was already open, which is what a
	// retried task finds.
	Existed bool
}

// Options configures a Client.
type Options struct {
	// Token is the service account's. It needs to write the repository and
	// open pull requests, and should not be able to merge them.
	Token string
	// APIBase overrides the endpoint, for GitHub Enterprise.
	APIBase string
	HTTP    *http.Client
}

// Client opens pull requests on GitHub.
type Client struct {
	http    *http.Client
	token   string
	apiBase string
}

// New returns a Client.
func New(o Options) (*Client, error) {
	if o.Token == "" {
		return nil, errors.New("forge: no git token; set creds.git in the runner config")
	}
	c := &Client{
		http:    o.HTTP,
		token:   o.Token,
		apiBase: strings.TrimSuffix(o.APIBase, "/"),
	}
	if c.apiBase == "" {
		c.apiBase = DefaultAPIBase
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: defaultTimeout}
	}
	return c, nil
}

// Open opens the pull request, or finds the one a previous attempt left.
func (c *Client) Open(ctx context.Context, req Request) (PR, error) {
	owner, name, err := Repository(req.RemoteURL)
	if err != nil {
		return PR{}, err
	}
	if req.Head == "" || req.Base == "" {
		return PR{}, errors.New("forge: a pull request needs both a head and a base branch")
	}
	if req.Title == "" {
		return PR{}, errors.New("forge: a pull request needs a title")
	}

	body, err := json.Marshal(map[string]any{
		"title": req.Title,
		"head":  req.Head,
		"base":  req.Base,
		"body":  req.Body,
	})
	if err != nil {
		return PR{}, fmt.Errorf("forge: encode request: %w", err)
	}

	path := fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(name))
	raw, status, err := c.do(ctx, http.MethodPost, path, body)
	switch {
	case err == nil:
		var created struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
		}
		if err := json.Unmarshal(raw, &created); err != nil {
			return PR{}, fmt.Errorf("forge: decode response: %w", err)
		}
		return PR{Number: created.Number, URL: created.HTMLURL}, nil

	case status == http.StatusUnprocessableEntity:
		// The likeliest cause is a retried task whose branch already has a pull
		// request. Finding it is more useful than reporting a conflict.
		if pr, found := c.find(ctx, owner, name, req.Head); found {
			return pr, nil
		}
		return PR{}, err

	default:
		return PR{}, err
	}
}

// find looks for an open pull request from head.
func (c *Client) find(ctx context.Context, owner, name, head string) (PR, bool) {
	query := url.Values{}
	query.Set("head", owner+":"+head)
	query.Set("state", "open")
	path := fmt.Sprintf("/repos/%s/%s/pulls?%s",
		url.PathEscape(owner), url.PathEscape(name), query.Encode())

	raw, _, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return PR{}, false
	}
	var found []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &found); err != nil || len(found) == 0 {
		return PR{}, false
	}
	return PR{Number: found[0].Number, URL: found[0].HTMLURL, Existed: true}, true
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("forge: %w", err)
	}
	req.Header.Set("accept", "application/vnd.github+json")
	req.Header.Set("x-github-api-version", apiVersion)
	req.Header.Set("authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("forge: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("forge: read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return raw, resp.StatusCode, fmt.Errorf("forge: %s %s: %s", method, path, explain(raw, resp.StatusCode))
	}
	return raw, resp.StatusCode, nil
}

// explain turns GitHub's error body into one line, without repeating anything
// that was sent.
func explain(raw []byte, status int) string {
	var body struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Message == "" {
		return fmt.Sprintf("http %d", status)
	}

	parts := []string{body.Message}
	for _, e := range body.Errors {
		if e.Message != "" {
			parts = append(parts, e.Message)
		}
	}
	return fmt.Sprintf("http %d: %s", status, strings.Join(parts, "; "))
}

// Repository picks the owner and name out of a git remote.
//
// Both the https and the ssh forms are accepted, because which one a repository
// was cloned with says nothing about which forge it lives on.
func Repository(remote string) (owner, name string, err error) {
	trimmed := strings.TrimSpace(remote)
	if trimmed == "" {
		return "", "", errors.New("forge: no remote url")
	}

	path := trimmed
	switch {
	case strings.HasPrefix(trimmed, "http://"), strings.HasPrefix(trimmed, "https://"),
		strings.HasPrefix(trimmed, "ssh://"), strings.HasPrefix(trimmed, "git://"):
		u, parseErr := url.Parse(trimmed)
		if parseErr != nil {
			return "", "", fmt.Errorf("forge: remote %q: %w", remote, parseErr)
		}
		path = u.Path
	case strings.Contains(trimmed, ":"):
		// The scp-like form: git@github.com:owner/name.git
		path = trimmed[strings.Index(trimmed, ":")+1:]
	}

	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[len(parts)-1] == "" || parts[len(parts)-2] == "" {
		return "", "", fmt.Errorf("forge: cannot tell the owner and repository from %q", remote)
	}
	return parts[len(parts)-2], parts[len(parts)-1], nil
}

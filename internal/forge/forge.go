// Package forge opens the pull request a finished task becomes.
//
// A pull request rather than a push to the default branch, and a service
// account rather than the developer's own credential: the promise Roost makes
// is a change waiting for review in the morning, not a change already merged.
// Nothing here can merge anything, and the token it uses should not be able to
// either.
//
// GitHub and GitLab are both implemented. Which one a repository lives on is
// read from its remote when the host says so, and configured when it does not —
// a self-hosted forge is not something to guess at, because a wrong guess
// pushes a branch and then fails to open anything on top of it.
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

// Kind names a forge.
type Kind string

const (
	KindGitHub Kind = "github"
	KindGitLab Kind = "gitlab"
)

// Default API roots. A self-hosted forge overrides these through Options.
const (
	DefaultGitHubAPI = "https://api.github.com"
	DefaultGitLabAPI = "https://gitlab.com"
)

const (
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

// PR is an opened pull request. GitLab calls it a merge request; the number is
// its iid, which is what appears in the URL.
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
	// Kind forces the forge. Empty reads it from the remote's host, which only
	// works for the hosted services: a self-hosted GitLab is just a hostname.
	Kind Kind
	// APIBase overrides the API root. For GitHub Enterprise that includes the
	// path, e.g. https://git.example.com/api/v3; for a self-hosted GitLab it
	// does not, e.g. https://git.example.com, because GitLab's own paths start
	// with /api/v4.
	APIBase string
	HTTP    *http.Client
}

// Client opens pull requests.
type Client struct {
	http    *http.Client
	token   string
	kind    Kind
	apiBase string
}

// New returns a Client.
func New(o Options) (*Client, error) {
	if o.Token == "" {
		return nil, errors.New("forge: no git token; set creds.git in the runner config")
	}
	switch o.Kind {
	case "", KindGitHub, KindGitLab:
	default:
		return nil, fmt.Errorf("forge: unknown forge %q; use %q or %q", o.Kind, KindGitHub, KindGitLab)
	}

	c := &Client{
		http:    o.HTTP,
		token:   o.Token,
		kind:    o.Kind,
		apiBase: strings.TrimSuffix(o.APIBase, "/"),
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: defaultTimeout}
	}
	return c, nil
}

// Open opens the pull request, or finds the one a previous attempt left.
func (c *Client) Open(ctx context.Context, req Request) (PR, error) {
	remote, err := Parse(req.RemoteURL)
	if err != nil {
		return PR{}, err
	}
	if req.Head == "" || req.Base == "" {
		return PR{}, errors.New("forge: a pull request needs both a head and a base branch")
	}
	if req.Title == "" {
		return PR{}, errors.New("forge: a pull request needs a title")
	}

	kind, err := c.kindFor(remote)
	if err != nil {
		return PR{}, err
	}
	switch kind {
	case KindGitHub:
		return c.openGitHub(ctx, remote, req)
	case KindGitLab:
		return c.openGitLab(ctx, remote, req)
	default:
		return PR{}, fmt.Errorf("forge: %q is not implemented", kind)
	}
}

// kindFor decides which forge a remote is on.
//
// The hosted services are known by name. Anything else has to be configured:
// guessing from a hostname would push a branch and then fail to open a pull
// request on it, which is a worse failure than refusing up front.
func (c *Client) kindFor(remote Remote) (Kind, error) {
	if c.kind != "" {
		return c.kind, nil
	}
	switch remote.Host {
	case "github.com", "www.github.com":
		return KindGitHub, nil
	case "gitlab.com", "www.gitlab.com":
		return KindGitLab, nil
	default:
		return "", fmt.Errorf(
			"forge: cannot tell which forge %s is; set git.forge to %q or %q in the runner config",
			remote.Host, KindGitHub, KindGitLab)
	}
}

func (c *Client) base(kind Kind) string {
	if c.apiBase != "" {
		return c.apiBase
	}
	if kind == KindGitLab {
		return DefaultGitLabAPI
	}
	return DefaultGitHubAPI
}

// Remote is a repository as its git remote names it.
type Remote struct {
	// Host is the forge's hostname, without any userinfo or port.
	Host string
	// Path is everything below the host, without a .git suffix. GitLab
	// subgroups make this more than two segments, which is why it is kept whole
	// rather than split into an owner and a name.
	Path string
}

// Owner is the first path segment: a user, an organisation, or a top-level
// GitLab group.
func (r Remote) Owner() string {
	owner, _, _ := strings.Cut(r.Path, "/")
	return owner
}

// Name is the last path segment: the repository itself.
func (r Remote) Name() string {
	if i := strings.LastIndex(r.Path, "/"); i >= 0 {
		return r.Path[i+1:]
	}
	return r.Path
}

// Parse picks the host and the repository path out of a git remote.
//
// Both the https and the ssh forms are accepted, because which one a repository
// was cloned with says nothing about which forge it lives on.
func Parse(remote string) (Remote, error) {
	trimmed := strings.TrimSpace(remote)
	if trimmed == "" {
		return Remote{}, errors.New("forge: no remote url")
	}

	var host, path string
	switch {
	case strings.HasPrefix(trimmed, "http://"), strings.HasPrefix(trimmed, "https://"),
		strings.HasPrefix(trimmed, "ssh://"), strings.HasPrefix(trimmed, "git://"):
		u, err := url.Parse(trimmed)
		if err != nil {
			return Remote{}, fmt.Errorf("forge: remote %q: %w", remote, err)
		}
		host, path = u.Hostname(), u.Path

	case strings.Contains(trimmed, ":"):
		// The scp-like form: git@github.com:group/sub/project.git
		hostPart, rest, _ := strings.Cut(trimmed, ":")
		if _, after, found := strings.Cut(hostPart, "@"); found {
			hostPart = after
		}
		host, path = hostPart, rest

	default:
		return Remote{}, fmt.Errorf("forge: cannot tell the host from %q", remote)
	}

	host = strings.ToLower(strings.TrimSpace(host))
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")

	if host == "" {
		return Remote{}, fmt.Errorf("forge: remote %q has no host", remote)
	}
	segments := strings.Split(path, "/")
	if len(segments) < 2 {
		return Remote{}, fmt.Errorf("forge: %q does not name a repository", remote)
	}
	for _, segment := range segments {
		if segment == "" {
			return Remote{}, fmt.Errorf("forge: %q has an empty path segment", remote)
		}
	}
	return Remote{Host: host, Path: path}, nil
}

// call is one API request.
type call struct {
	method  string
	url     string
	body    []byte
	headers map[string]string
	// explain turns an error body into one line. The forges disagree about the
	// shape of those, so each provides its own.
	explain func(raw []byte, status int) string
}

// send performs a call and reports the status alongside the error, because
// which status came back is how a duplicate is told from a real failure.
func (c *Client) send(ctx context.Context, call call) ([]byte, int, error) {
	var reader io.Reader
	if call.body != nil {
		reader = bytes.NewReader(call.body)
	}
	req, err := http.NewRequestWithContext(ctx, call.method, call.url, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("forge: %w", err)
	}
	for name, value := range call.headers {
		req.Header.Set(name, value)
	}
	if call.body != nil {
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
		// The path, not the full URL: an API base can carry credentials in its
		// userinfo, and this error is journalled and streamed.
		path := call.url
		if u, parseErr := url.Parse(call.url); parseErr == nil {
			path = u.Path
		}
		return raw, resp.StatusCode, fmt.Errorf("forge: %s %s: %s",
			call.method, path, call.explain(raw, resp.StatusCode))
	}
	return raw, resp.StatusCode, nil
}

func encodeBody(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("forge: encode request: %w", err)
	}
	return raw, nil
}

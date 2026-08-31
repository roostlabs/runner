package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const githubAPIVersion = "2022-11-28"

func (c *Client) githubHeaders() map[string]string {
	return map[string]string{
		"accept":               "application/vnd.github+json",
		"x-github-api-version": githubAPIVersion,
		"authorization":        "Bearer " + c.token,
	}
}

type githubPR struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

func (c *Client) openGitHub(ctx context.Context, remote Remote, req Request) (PR, error) {
	body, err := encodeBody(map[string]any{
		"title": req.Title,
		"head":  req.Head,
		"base":  req.Base,
		"body":  req.Body,
	})
	if err != nil {
		return PR{}, err
	}

	// GitHub addresses a repository as owner/name, so a path with more segments
	// than that is not something it can answer for.
	if strings.Count(remote.Path, "/") != 1 {
		return PR{}, fmt.Errorf("forge: %q is not a github owner/repository path", remote.Path)
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls",
		c.base(KindGitHub), url.PathEscape(remote.Owner()), url.PathEscape(remote.Name()))

	raw, status, err := c.send(ctx, call{
		method:  http.MethodPost,
		url:     endpoint,
		body:    body,
		headers: c.githubHeaders(),
		explain: explainGitHub,
	})
	switch {
	case err == nil:
		var created githubPR
		if err := json.Unmarshal(raw, &created); err != nil {
			return PR{}, fmt.Errorf("forge: decode response: %w", err)
		}
		return PR{Number: created.Number, URL: created.HTMLURL}, nil

	case status == http.StatusUnprocessableEntity:
		// The likeliest cause is a retried task whose branch already has a pull
		// request. Finding it is more useful than reporting a conflict.
		if pr, found := c.findGitHub(ctx, remote, req.Head); found {
			return pr, nil
		}
		return PR{}, err

	default:
		return PR{}, err
	}
}

func (c *Client) findGitHub(ctx context.Context, remote Remote, head string) (PR, bool) {
	query := url.Values{}
	query.Set("head", remote.Owner()+":"+head)
	query.Set("state", "open")

	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls?%s",
		c.base(KindGitHub), url.PathEscape(remote.Owner()), url.PathEscape(remote.Name()),
		query.Encode())

	raw, _, err := c.send(ctx, call{
		method:  http.MethodGet,
		url:     endpoint,
		headers: c.githubHeaders(),
		explain: explainGitHub,
	})
	if err != nil {
		return PR{}, false
	}

	var found []githubPR
	if err := json.Unmarshal(raw, &found); err != nil || len(found) == 0 {
		return PR{}, false
	}
	return PR{Number: found[0].Number, URL: found[0].HTMLURL, Existed: true}, true
}

// explainGitHub turns GitHub's error body into one line, without repeating
// anything that was sent.
func explainGitHub(raw []byte, status int) string {
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

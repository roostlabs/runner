package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

func (c *Client) gitlabHeaders() map[string]string {
	// GitLab authenticates a project or personal access token with its own
	// header; Bearer is for OAuth tokens, which a service account does not use.
	return map[string]string{
		"accept":        "application/json",
		"private-token": c.token,
	}
}

type gitlabMR struct {
	IID    int    `json:"iid"`
	WebURL string `json:"web_url"`
}

func (c *Client) openGitLab(ctx context.Context, remote Remote, req Request) (PR, error) {
	body, err := encodeBody(map[string]any{
		"source_branch": req.Head,
		"target_branch": req.Base,
		"title":         req.Title,
		"description":   req.Body,
		// Nothing here may merge. Squashing is the reviewer's decision, and
		// removing the branch would delete the task's own output.
		"remove_source_branch": false,
	})
	if err != nil {
		return PR{}, err
	}

	endpoint := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests", c.base(KindGitLab), projectID(remote))

	raw, status, err := c.send(ctx, call{
		method:  http.MethodPost,
		url:     endpoint,
		body:    body,
		headers: c.gitlabHeaders(),
		explain: explainGitLab,
	})
	switch {
	case err == nil:
		var created gitlabMR
		if err := json.Unmarshal(raw, &created); err != nil {
			return PR{}, fmt.Errorf("forge: decode response: %w", err)
		}
		return PR{Number: created.IID, URL: created.WebURL}, nil

	// GitLab answers a duplicate with 409, and a rejected one with 400 or 422.
	// A retried task is the likeliest cause of any of them, so the existing
	// merge request is looked for before the failure is reported.
	case status == http.StatusConflict,
		status == http.StatusBadRequest,
		status == http.StatusUnprocessableEntity:
		if pr, found := c.findGitLab(ctx, remote, req.Head); found {
			return pr, nil
		}
		return PR{}, err

	default:
		return PR{}, err
	}
}

func (c *Client) findGitLab(ctx context.Context, remote Remote, head string) (PR, bool) {
	query := url.Values{}
	query.Set("source_branch", head)
	query.Set("state", "opened")

	endpoint := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests?%s",
		c.base(KindGitLab), projectID(remote), query.Encode())

	raw, _, err := c.send(ctx, call{
		method:  http.MethodGet,
		url:     endpoint,
		headers: c.gitlabHeaders(),
		explain: explainGitLab,
	})
	if err != nil {
		return PR{}, false
	}

	var found []gitlabMR
	if err := json.Unmarshal(raw, &found); err != nil || len(found) == 0 {
		return PR{}, false
	}
	return PR{Number: found[0].IID, URL: found[0].WebURL, Existed: true}, true
}

// projectID is how GitLab addresses a project by path: the whole namespace,
// URL-encoded, slashes included. Subgroups are why the path is kept whole
// rather than reduced to an owner and a name.
func projectID(remote Remote) string {
	return url.PathEscape(remote.Path)
}

// explainGitLab turns GitLab's error body into one line.
//
// GitLab is inconsistent about the shape: `message` is sometimes a string,
// sometimes an array, and sometimes an object keyed by field, and some
// endpoints use `error` instead. All of them are rendered rather than one being
// picked and the rest dropped.
func explainGitLab(raw []byte, status int) string {
	var body struct {
		Message json.RawMessage `json:"message"`
		Error   string          `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Sprintf("http %d", status)
	}
	if text := renderGitLabMessage(body.Message); text != "" {
		return fmt.Sprintf("http %d: %s", status, text)
	}
	if body.Error != "" {
		return fmt.Sprintf("http %d: %s", status, body.Error)
	}
	return fmt.Sprintf("http %d", status)
}

func renderGitLabMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}

	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return strings.Join(list, "; ")
	}

	var fields map[string][]string
	if err := json.Unmarshal(raw, &fields); err == nil {
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		// Sorted so the same failure reads the same way twice.
		sort.Strings(keys)

		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+": "+strings.Join(fields[key], ", "))
		}
		return strings.Join(parts, "; ")
	}
	return strings.TrimSpace(string(raw))
}

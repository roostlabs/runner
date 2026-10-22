package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
)

// Account is who a token belongs to.
type Account struct {
	Login string
}

// RepoInfo is what setup wants to know about a repository before a task is
// ever run against it: whether the token can push, and what the code is
// written in, which is a strong hint for the sandbox image.
type RepoInfo struct {
	Push bool
	// Languages is ordered by weight, heaviest first, as the forge reports.
	Languages []string
}

// WhoAmI asks the forge who the token is. It is the first thing setup checks:
// a token that cannot name its own account cannot do anything else either.
func (c *Client) WhoAmI(ctx context.Context) (Account, error) {
	kind, err := c.defaultKind()
	if err != nil {
		return Account{}, err
	}
	switch kind {
	case KindGitLab:
		var user struct {
			Username string `json:"username"`
		}
		if err := c.getJSON(ctx, kind, c.base(kind)+"/api/v4/user", &user); err != nil {
			return Account{}, err
		}
		return Account{Login: user.Username}, nil
	default:
		var user struct {
			Login string `json:"login"`
		}
		if err := c.getJSON(ctx, kind, c.base(kind)+"/user", &user); err != nil {
			return Account{}, err
		}
		return Account{Login: user.Login}, nil
	}
}

// Inspect reports whether the token can push to the repository and what it is
// written in.
func (c *Client) Inspect(ctx context.Context, remoteURL string) (RepoInfo, error) {
	remote, err := Parse(remoteURL)
	if err != nil {
		return RepoInfo{}, err
	}
	kind, err := c.kindFor(remote)
	if err != nil {
		return RepoInfo{}, err
	}
	var info RepoInfo
	switch kind {
	case KindGitLab:
		project := c.base(kind) + "/api/v4/projects/" + projectID(remote)
		var p struct {
			Permissions struct {
				Project *struct {
					AccessLevel int `json:"access_level"`
				} `json:"project_access"`
				Group *struct {
					AccessLevel int `json:"access_level"`
				} `json:"group_access"`
			} `json:"permissions"`
		}
		if err := c.getJSON(ctx, kind, project, &p); err != nil {
			return RepoInfo{}, err
		}
		// Developer (30) is the first level that can push.
		info.Push = (p.Permissions.Project != nil && p.Permissions.Project.AccessLevel >= 30) ||
			(p.Permissions.Group != nil && p.Permissions.Group.AccessLevel >= 30)
		var langs map[string]float64
		if err := c.getJSON(ctx, kind, project+"/languages", &langs); err == nil {
			info.Languages = rank(langs)
		}
	default:
		repo := c.base(kind) + "/repos/" + remote.Path
		var r struct {
			Permissions struct {
				Push bool `json:"push"`
			} `json:"permissions"`
		}
		if err := c.getJSON(ctx, kind, repo, &r); err != nil {
			return RepoInfo{}, err
		}
		info.Push = r.Permissions.Push
		var langs map[string]float64
		if err := c.getJSON(ctx, kind, repo+"/languages", &langs); err == nil {
			info.Languages = rank(langs)
		}
	}
	return info, nil
}

// defaultKind is the forge when no remote says: the configured one, or
// github.com, which is what a bare token most often belongs to.
func (c *Client) defaultKind() (Kind, error) {
	if c.kind != "" {
		return c.kind, nil
	}
	if c.apiBase != "" {
		return "", errors.New("forge: git.apiBase is set but git.forge is not; which forge is it?")
	}
	return KindGitHub, nil
}

func (c *Client) getJSON(ctx context.Context, kind Kind, url string, out any) error {
	headers, explain := c.githubHeaders(), explainGitHub
	if kind == KindGitLab {
		headers, explain = c.gitlabHeaders(), explainGitLab
	}
	raw, _, err := c.send(ctx, call{method: http.MethodGet, url: url, headers: headers, explain: explain})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("forge: decode response: %w", err)
	}
	return nil
}

// rank orders languages by weight, heaviest first.
func rank(weights map[string]float64) []string {
	names := make([]string, 0, len(weights))
	for name := range weights {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if weights[names[i]] != weights[names[j]] {
			return weights[names[i]] > weights[names[j]]
		}
		return names[i] < names[j]
	})
	return names
}

package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
)

// Account is who a tracker token belongs to.
type Account struct {
	Name string
}

// Project is one place tickets live: a Jira project or a Linear team.
type Project struct {
	Key  string
	Name string
}

// Explorer is what setup needs from a tracker before any project is chosen:
// who the token is, which projects it can see, and what states a project's
// tickets move through, so the developer picks names rather than typing
// them.
type Explorer interface {
	Me(ctx context.Context) (Account, error)
	Projects(ctx context.Context) ([]Project, error)
	States(ctx context.Context, project string) ([]string, error)
}

// NewExplorer returns the Explorer for o.Kind. Unlike New it needs no project.
func NewExplorer(o Options) (Explorer, error) {
	if o.Token == "" {
		return nil, fmt.Errorf("tracker: no task manager token")
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
	default:
		return nil, fmt.Errorf("tracker: unknown tracker %q; use %q or %q", o.Kind, KindJira, KindLinear)
	}
}

func (j *jira) get(ctx context.Context, path string, out any) error {
	raw, _, err := send(ctx, j.http, call{
		method: http.MethodGet, url: j.base + path, headers: j.headers(), explain: explainJira,
	})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("tracker: decode response: %w", err)
	}
	return nil
}

func (j *jira) Me(ctx context.Context) (Account, error) {
	var me struct {
		DisplayName string `json:"displayName"`
		Email       string `json:"emailAddress"`
	}
	if err := j.get(ctx, "/rest/api/3/myself", &me); err != nil {
		return Account{}, err
	}
	name := me.DisplayName
	if name == "" {
		name = me.Email
	}
	return Account{Name: name}, nil
}

func (j *jira) Projects(ctx context.Context) ([]Project, error) {
	var page struct {
		Values []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"values"`
	}
	if err := j.get(ctx, "/rest/api/3/project/search?maxResults=100&orderBy=key", &page); err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(page.Values))
	for _, p := range page.Values {
		out = append(out, Project{Key: p.Key, Name: p.Name})
	}
	return out, nil
}

// States is the union of statuses over the project's issue types, in the
// order Jira lists them, each once.
func (j *jira) States(ctx context.Context, project string) ([]string, error) {
	var types []struct {
		Statuses []struct {
			Name string `json:"name"`
		} `json:"statuses"`
	}
	if err := j.get(ctx, "/rest/api/3/project/"+url.PathEscape(project)+"/statuses", &types); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range types {
		for _, s := range t.Statuses {
			if !seen[s.Name] {
				seen[s.Name] = true
				out = append(out, s.Name)
			}
		}
	}
	return out, nil
}

func (l *linear) Me(ctx context.Context) (Account, error) {
	var data struct {
		Viewer struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"viewer"`
	}
	if err := l.query(ctx, `query { viewer { name email } }`, nil, &data); err != nil {
		return Account{}, err
	}
	name := data.Viewer.Name
	if name == "" {
		name = data.Viewer.Email
	}
	return Account{Name: name}, nil
}

func (l *linear) Projects(ctx context.Context) ([]Project, error) {
	var data struct {
		Teams struct {
			Nodes []struct {
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := l.query(ctx, `query { teams(first: 100) { nodes { key name } } }`, nil, &data); err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(data.Teams.Nodes))
	for _, t := range data.Teams.Nodes {
		out = append(out, Project{Key: t.Key, Name: t.Name})
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Key < out[k].Key })
	return out, nil
}

// States lists the team's workflow states in board order.
func (l *linear) States(ctx context.Context, team string) ([]string, error) {
	var data struct {
		States struct {
			Nodes []struct {
				Name     string  `json:"name"`
				Position float64 `json:"position"`
			} `json:"nodes"`
		} `json:"workflowStates"`
	}
	const q = `query($key: String!) {
		workflowStates(filter: { team: { key: { eq: $key } } }, first: 100) { nodes { name position } }
	}`
	if err := l.query(ctx, q, map[string]any{"key": team}, &data); err != nil {
		return nil, err
	}
	nodes := data.States.Nodes
	sort.SliceStable(nodes, func(i, k int) bool { return nodes[i].Position < nodes[k].Position })
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out, nil
}

package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// linear is Linear through its GraphQL API.
type linear struct {
	http     *http.Client
	endpoint string
	team     string
	token    string
}

func newLinear(o Options, h *http.Client) (*linear, error) {
	endpoint := o.BaseURL
	if endpoint == "" {
		endpoint = DefaultLinearAPI
	}
	return &linear{
		http:     h,
		endpoint: strings.TrimSuffix(endpoint, "/"),
		team:     o.Project,
		token:    o.Token,
	}, nil
}

func (l *linear) Kind() Kind { return KindLinear }

// issueFields is the slice of an issue this package reads. Id is Linear's uuid,
// which mutations want; Identifier is the ENG-12 a developer knows it by.
const issueFields = `id identifier title description url team { key } state { name }`

type linearIssue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Team        struct {
		Key string `json:"key"`
	} `json:"team"`
	State struct {
		Name string `json:"name"`
	} `json:"state"`
}

func (issue linearIssue) ticket() Ticket {
	return Ticket{
		ID:    issue.Identifier,
		URL:   issue.URL,
		Title: issue.Title,
		Body:  strings.TrimSpace(issue.Description),
		State: issue.State.Name,
	}
}

func (l *linear) Get(ctx context.Context, id string) (Ticket, error) {
	issue, err := l.issue(ctx, id)
	if err != nil {
		return Ticket{}, err
	}
	return issue.ticket(), nil
}

// issue looks a ticket up by identifier or uuid; Linear's issue query accepts
// either.
func (l *linear) issue(ctx context.Context, id string) (linearIssue, error) {
	if id == "" {
		return linearIssue{}, errors.New("tracker: no ticket id")
	}
	var data struct {
		Issue *linearIssue `json:"issue"`
	}
	err := l.query(ctx,
		`query($id: String!) { issue(id: $id) { `+issueFields+` } }`,
		map[string]any{"id": id}, &data)
	if err != nil {
		return linearIssue{}, err
	}
	if data.Issue == nil {
		return linearIssue{}, fmt.Errorf("tracker: no issue %s", id)
	}
	return *data.Issue, nil
}

func (l *linear) List(ctx context.Context, state string) ([]Ticket, error) {
	if state == "" {
		return nil, errors.New("tracker: no state to list")
	}
	var data struct {
		Issues struct {
			Nodes []linearIssue `json:"nodes"`
		} `json:"issues"`
	}
	err := l.query(ctx,
		`query($team: String!, $state: String!, $first: Int!) {
		  issues(
		    filter: { team: { key: { eq: $team } }, state: { name: { eqIgnoreCase: $state } } }
		    orderBy: createdAt
		    first: $first
		  ) { nodes { `+issueFields+` } }
		}`,
		map[string]any{"team": l.team, "state": strings.TrimSpace(state), "first": listLimit}, &data)
	if err != nil {
		return nil, err
	}
	out := make([]Ticket, 0, len(data.Issues.Nodes))
	for _, issue := range data.Issues.Nodes {
		out = append(out, issue.ticket())
	}
	return out, nil
}

// Transition resolves the state's id within the issue's own team and updates
// the issue. The team comes from the issue rather than from the config, so a
// ticket from another team that a task was pointed at still moves.
func (l *linear) Transition(ctx context.Context, id, state string) error {
	issue, err := l.issue(ctx, id)
	if err != nil {
		return err
	}
	if sameState(issue.State.Name, state) {
		return nil
	}

	var states struct {
		WorkflowStates struct {
			Nodes []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"workflowStates"`
	}
	err = l.query(ctx,
		`query($team: String!) {
		  workflowStates(filter: { team: { key: { eq: $team } } }) { nodes { id name } }
		}`,
		map[string]any{"team": issue.Team.Key}, &states)
	if err != nil {
		return err
	}
	var stateID string
	names := make([]string, 0, len(states.WorkflowStates.Nodes))
	for _, s := range states.WorkflowStates.Nodes {
		names = append(names, s.Name)
		if sameState(s.Name, state) {
			stateID = s.ID
			break
		}
	}
	if stateID == "" {
		return fmt.Errorf("tracker: team %s has no state %q; it has: %s",
			issue.Team.Key, state, strings.Join(names, ", "))
	}

	var result struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	err = l.query(ctx,
		`mutation($id: String!, $state: String!) {
		  issueUpdate(id: $id, input: { stateId: $state }) { success }
		}`,
		map[string]any{"id": issue.ID, "state": stateID}, &result)
	if err != nil {
		return err
	}
	if !result.IssueUpdate.Success {
		return fmt.Errorf("tracker: linear did not move %s to %q", id, state)
	}
	return nil
}

func (l *linear) Comment(ctx context.Context, id, body string) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("tracker: empty comment")
	}
	issue, err := l.issue(ctx, id)
	if err != nil {
		return err
	}
	var result struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	err = l.query(ctx,
		`mutation($issue: String!, $body: String!) {
		  commentCreate(input: { issueId: $issue, body: $body }) { success }
		}`,
		map[string]any{"issue": issue.ID, "body": strings.Join(paragraphs(body), "\n\n")}, &result)
	if err != nil {
		return err
	}
	if !result.CommentCreate.Success {
		return fmt.Errorf("tracker: linear did not add the comment to %s", id)
	}
	return nil
}

// query performs one GraphQL request and decodes data into out. GraphQL
// reports most failures as a 200 with an errors list, so both are checked.
func (l *linear) query(ctx context.Context, q string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": q, "variables": vars})
	if err != nil {
		return fmt.Errorf("tracker: encode query: %w", err)
	}
	raw, _, err := send(ctx, l.http, call{
		method: http.MethodPost,
		url:    l.endpoint,
		body:   body,
		// A personal or service API key goes in as is; Bearer is for OAuth
		// tokens, which a service account does not use.
		headers: map[string]string{"authorization": l.token},
		explain: explainLinear,
	})
	if err != nil {
		return err
	}
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("tracker: decode response: %w", err)
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("tracker: linear: %s", strings.Join(msgs, "; "))
	}
	if len(resp.Data) == 0 {
		return errors.New("tracker: linear returned no data")
	}
	if err := json.Unmarshal(resp.Data, out); err != nil {
		return fmt.Errorf("tracker: decode data: %w", err)
	}
	return nil
}

func explainLinear(raw []byte, status int) string {
	var body struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || len(body.Errors) == 0 {
		return fmt.Sprintf("http %d", status)
	}
	msgs := make([]string, 0, len(body.Errors))
	for _, e := range body.Errors {
		msgs = append(msgs, e.Message)
	}
	return fmt.Sprintf("http %d: %s", status, strings.Join(msgs, "; "))
}

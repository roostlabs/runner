package tracker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// jira is Jira Cloud through its REST API v3.
type jira struct {
	http    *http.Client
	base    string
	project string
	auth    string
}

func newJira(o Options, h *http.Client) (*jira, error) {
	if o.BaseURL == "" {
		return nil, errors.New("tracker: jira needs its site url; set tracker.baseUrl, e.g. https://acme.atlassian.net")
	}
	if o.User == "" {
		return nil, errors.New("tracker: jira authenticates an api token as email:token; set tracker.user to the account's email")
	}
	return &jira{
		http:    h,
		base:    strings.TrimSuffix(o.BaseURL, "/"),
		project: o.Project,
		auth:    "Basic " + base64.StdEncoding.EncodeToString([]byte(o.User+":"+o.Token)),
	}, nil
}

func (j *jira) Kind() Kind { return KindJira }

func (j *jira) headers() map[string]string {
	return map[string]string{"authorization": j.auth}
}

// jiraIssue is the slice of an issue this package reads.
type jiraIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Status      struct {
			Name string `json:"name"`
		} `json:"status"`
	} `json:"fields"`
}

func (j *jira) ticket(issue jiraIssue) Ticket {
	return Ticket{
		ID:    issue.Key,
		URL:   j.base + "/browse/" + issue.Key,
		Title: issue.Fields.Summary,
		Body:  adfText(issue.Fields.Description),
		State: issue.Fields.Status.Name,
	}
}

func (j *jira) Get(ctx context.Context, id string) (Ticket, error) {
	if id == "" {
		return Ticket{}, errors.New("tracker: no ticket id")
	}
	endpoint := fmt.Sprintf("%s/rest/api/3/issue/%s?fields=summary,description,status",
		j.base, url.PathEscape(id))

	raw, _, err := send(ctx, j.http, call{
		method:  http.MethodGet,
		url:     endpoint,
		headers: j.headers(),
		explain: explainJira,
	})
	if err != nil {
		return Ticket{}, err
	}
	var issue jiraIssue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return Ticket{}, fmt.Errorf("tracker: decode issue: %w", err)
	}
	return j.ticket(issue), nil
}

// List searches with JQL. The state is quoted for JQL, since "Ready for agent"
// is a perfectly good state name and would otherwise be three tokens.
func (j *jira) List(ctx context.Context, state string) ([]Ticket, error) {
	if state == "" {
		return nil, errors.New("tracker: no state to list")
	}
	body, err := json.Marshal(map[string]any{
		"jql": fmt.Sprintf("project = %s AND status = %s ORDER BY created ASC",
			jqlString(j.project), jqlString(state)),
		"fields":     []string{"summary", "description", "status"},
		"maxResults": listLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("tracker: encode search: %w", err)
	}

	raw, _, err := send(ctx, j.http, call{
		method:  http.MethodPost,
		url:     j.base + "/rest/api/3/search/jql",
		body:    body,
		headers: j.headers(),
		explain: explainJira,
	})
	if err != nil {
		return nil, err
	}
	var page struct {
		Issues []jiraIssue `json:"issues"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("tracker: decode search: %w", err)
	}
	out := make([]Ticket, 0, len(page.Issues))
	for _, issue := range page.Issues {
		out = append(out, j.ticket(issue))
	}
	return out, nil
}

// Transition finds the transition whose destination is the named state and
// performs it. Jira transitions are per-issue and per-workflow, so they are
// looked up each time rather than remembered.
func (j *jira) Transition(ctx context.Context, id, state string) error {
	if id == "" {
		return errors.New("tracker: no ticket id")
	}
	endpoint := fmt.Sprintf("%s/rest/api/3/issue/%s/transitions", j.base, url.PathEscape(id))

	raw, _, err := send(ctx, j.http, call{
		method:  http.MethodGet,
		url:     endpoint,
		headers: j.headers(),
		explain: explainJira,
	})
	if err != nil {
		return err
	}
	var available struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(raw, &available); err != nil {
		return fmt.Errorf("tracker: decode transitions: %w", err)
	}

	var transitionID string
	names := make([]string, 0, len(available.Transitions))
	for _, t := range available.Transitions {
		names = append(names, t.To.Name)
		// The destination is what the developer configured; the transition's
		// own name ("Start progress") is accepted too, for workflows that
		// name them differently.
		if sameState(t.To.Name, state) || sameState(t.Name, state) {
			transitionID = t.ID
			break
		}
	}
	if transitionID == "" {
		return fmt.Errorf("tracker: %s has no transition to %q; it can move to: %s",
			id, state, strings.Join(names, ", "))
	}

	body, err := json.Marshal(map[string]any{
		"transition": map[string]string{"id": transitionID},
	})
	if err != nil {
		return fmt.Errorf("tracker: encode transition: %w", err)
	}
	_, _, err = send(ctx, j.http, call{
		method:  http.MethodPost,
		url:     endpoint,
		body:    body,
		headers: j.headers(),
		explain: explainJira,
	})
	return err
}

func (j *jira) Comment(ctx context.Context, id, body string) error {
	if id == "" {
		return errors.New("tracker: no ticket id")
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("tracker: empty comment")
	}
	payload, err := json.Marshal(map[string]any{"body": adfDocument(body)})
	if err != nil {
		return fmt.Errorf("tracker: encode comment: %w", err)
	}
	_, _, err = send(ctx, j.http, call{
		method:  http.MethodPost,
		url:     fmt.Sprintf("%s/rest/api/3/issue/%s/comment", j.base, url.PathEscape(id)),
		body:    payload,
		headers: j.headers(),
		explain: explainJira,
	})
	return err
}

// jqlString quotes a value for JQL. Backslashes and quotes are escaped so a
// state name cannot end the string and start a clause.
func jqlString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// explainJira turns Jira's error body into one line. Jira reports either a list
// of messages or a map keyed by field, and often both.
func explainJira(raw []byte, status int) string {
	var body struct {
		Messages []string          `json:"errorMessages"`
		Errors   map[string]string `json:"errors"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Sprintf("http %d", status)
	}
	parts := append([]string(nil), body.Messages...)
	for field, msg := range body.Errors {
		parts = append(parts, field+": "+msg)
	}
	if len(parts) == 0 {
		return fmt.Sprintf("http %d", status)
	}
	return fmt.Sprintf("http %d: %s", status, strings.Join(parts, "; "))
}

// Atlassian Document Format. Jira Cloud stores descriptions and comments as a
// tree of nodes rather than text, so reading one means flattening it and
// writing one means building it.

type adfNode struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	Attrs   map[string]any  `json:"attrs,omitempty"`
	Content []adfNode       `json:"content,omitempty"`
	Version int             `json:"version,omitempty"`
	Marks   json.RawMessage `json:"marks,omitempty"`
}

// adfText flattens a document to plain text. Block nodes become lines,
// inline nodes are concatenated, and a mention or a card is rendered as what a
// reader would see rather than as its id.
func adfText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	// A description that arrived as a plain string, which the v2 API and some
	// older sites still produce.
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var doc adfNode
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	var b strings.Builder
	writeADF(&b, doc, 0)
	return strings.TrimSpace(b.String())
}

func writeADF(b *strings.Builder, n adfNode, depth int) {
	switch n.Type {
	case "text":
		b.WriteString(n.Text)
	case "hardBreak":
		b.WriteString("\n")
	case "mention", "emoji", "status", "date":
		if t, ok := n.Attrs["text"].(string); ok {
			b.WriteString(t)
		}
	case "inlineCard", "blockCard", "embedCard":
		if u, ok := n.Attrs["url"].(string); ok {
			b.WriteString(u)
		}
	case "rule":
		b.WriteString("---\n")
	case "listItem":
		// Each child block ends its own line; the item adds the bullet and
		// exactly one line break, however many blocks it holds.
		var item strings.Builder
		for _, child := range n.Content {
			writeADF(&item, child, depth)
		}
		b.WriteString(strings.Repeat("  ", max(depth-1, 0)) + "- ")
		b.WriteString(strings.TrimRight(item.String(), "\n"))
		b.WriteString("\n")
	case "codeBlock":
		b.WriteString("```\n")
		for _, child := range n.Content {
			writeADF(b, child, depth)
		}
		b.WriteString("\n```\n")
	case "doc", "bulletList", "orderedList", "blockquote", "panel", "expand",
		"table", "tableRow", "tableCell", "tableHeader", "taskList", "decisionList",
		"nestedExpand", "layoutSection", "layoutColumn", "bodiedExtension":
		next := depth
		if n.Type == "bulletList" || n.Type == "orderedList" || n.Type == "taskList" {
			next++
		}
		for _, child := range n.Content {
			writeADF(b, child, next)
		}
	default:
		// paragraph, heading, mediaSingle, taskItem, decisionItem and anything
		// added since: a block of inline content, ended by a line.
		for _, child := range n.Content {
			writeADF(b, child, depth)
		}
		b.WriteString("\n")
	}
}

// adfDocument builds a document of paragraphs from plain text.
func adfDocument(body string) adfNode {
	doc := adfNode{Type: "doc", Version: 1}
	for _, para := range paragraphs(body) {
		p := adfNode{Type: "paragraph"}
		lines := strings.Split(para, "\n")
		for i, line := range lines {
			if i > 0 {
				p.Content = append(p.Content, adfNode{Type: "hardBreak"})
			}
			p.Content = append(p.Content, adfNode{Type: "text", Text: line})
		}
		doc.Content = append(doc.Content, p)
	}
	if len(doc.Content) == 0 {
		doc.Content = []adfNode{{Type: "paragraph", Content: []adfNode{{Type: "text", Text: body}}}}
	}
	return doc
}

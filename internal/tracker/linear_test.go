package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const linearIssueJSON = `{
  "id": "uuid-7", "identifier": "ENG-7",
  "title": "paginator drops the last page",
  "description": "The last page is missing.\n",
  "url": "https://linear.app/acme/issue/ENG-7",
  "team": {"key": "ENG"},
  "state": {"name": "Todo"}
}`

// graphQL records each request and answers from a script keyed by the
// operation's first field.
type graphQL struct {
	t        *testing.T
	requests []map[string]any
	answers  map[string]string
}

func (g *graphQL) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("authorization") != token {
		g.t.Errorf("authorization = %q, want the raw api key", r.Header.Get("authorization"))
	}
	var req map[string]any
	json.NewDecoder(r.Body).Decode(&req)
	g.requests = append(g.requests, req)

	query, _ := req["query"].(string)
	for field, answer := range g.answers {
		if strings.Contains(query, field+"(") {
			w.Write([]byte(answer))
			return
		}
	}
	g.t.Errorf("unexpected query: %s", query)
	w.WriteHeader(http.StatusBadRequest)
}

func (g *graphQL) vars(i int) map[string]any {
	if i >= len(g.requests) {
		g.t.Fatalf("only %d requests were made", len(g.requests))
	}
	vars, _ := g.requests[i]["variables"].(map[string]any)
	return vars
}

func linearClient(t *testing.T, answers map[string]string) (Client, *graphQL) {
	t.Helper()
	g := &graphQL{t: t, answers: answers}
	srv := httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(srv.Close)

	c, err := New(Options{Kind: KindLinear, Token: token, BaseURL: srv.URL, Project: "ENG", HTTP: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, g
}

func TestLinearGet(t *testing.T) {
	c, g := linearClient(t, map[string]string{
		"issue": `{"data":{"issue":` + linearIssueJSON + `}}`,
	})
	ticket, err := c.Get(context.Background(), "ENG-7")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if g.vars(0)["id"] != "ENG-7" {
		t.Errorf("variables = %v", g.vars(0))
	}
	want := Ticket{
		ID: "ENG-7", URL: "https://linear.app/acme/issue/ENG-7",
		Title: "paginator drops the last page", Body: "The last page is missing.", State: "Todo",
	}
	if ticket != want {
		t.Errorf("ticket = %+v\nwant     %+v", ticket, want)
	}
}

func TestLinearGetMissing(t *testing.T) {
	c, _ := linearClient(t, map[string]string{
		"issue": `{"data":{"issue":null},"errors":[{"message":"Entity not found: Issue"}]}`,
	})
	_, err := c.Get(context.Background(), "ENG-404")
	if err == nil || !strings.Contains(err.Error(), "Entity not found") {
		t.Errorf("err = %v", err)
	}
}

func TestLinearList(t *testing.T) {
	c, g := linearClient(t, map[string]string{
		"issues": `{"data":{"issues":{"nodes":[` + linearIssueJSON + `]}}}`,
	})
	tickets, err := c.List(context.Background(), " Ready for agent ")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	vars := g.vars(0)
	if vars["team"] != "ENG" || vars["state"] != "Ready for agent" {
		t.Errorf("variables = %v", vars)
	}
	if len(tickets) != 1 || tickets[0].ID != "ENG-7" {
		t.Errorf("tickets = %+v", tickets)
	}
}

func TestLinearTransition(t *testing.T) {
	c, g := linearClient(t, map[string]string{
		"issue":          `{"data":{"issue":` + linearIssueJSON + `}}`,
		"workflowStates": `{"data":{"workflowStates":{"nodes":[{"id":"st-1","name":"Todo"},{"id":"st-2","name":"In Progress"}]}}}`,
		"issueUpdate":    `{"data":{"issueUpdate":{"success":true}}}`,
	})
	if err := c.Transition(context.Background(), "ENG-7", "in progress"); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if len(g.requests) != 3 {
		t.Fatalf("%d requests, want lookup, states, update", len(g.requests))
	}
	if g.vars(1)["team"] != "ENG" {
		t.Errorf("states were looked up for team %v, want the issue's own", g.vars(1)["team"])
	}
	update := g.vars(2)
	if update["id"] != "uuid-7" || update["state"] != "st-2" {
		t.Errorf("update variables = %v", update)
	}
}

func TestLinearTransitionAlreadyThere(t *testing.T) {
	c, g := linearClient(t, map[string]string{
		"issue": `{"data":{"issue":` + linearIssueJSON + `}}`,
	})
	if err := c.Transition(context.Background(), "ENG-7", "todo"); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if len(g.requests) != 1 {
		t.Errorf("%d requests; a ticket already in the state should not be updated", len(g.requests))
	}
}

func TestLinearTransitionUnknownState(t *testing.T) {
	c, _ := linearClient(t, map[string]string{
		"issue":          `{"data":{"issue":` + linearIssueJSON + `}}`,
		"workflowStates": `{"data":{"workflowStates":{"nodes":[{"id":"st-1","name":"Todo"}]}}}`,
	})
	err := c.Transition(context.Background(), "ENG-7", "In Review")
	if err == nil || !strings.Contains(err.Error(), `no state "In Review"`) || !strings.Contains(err.Error(), "Todo") {
		t.Errorf("err = %v", err)
	}
}

func TestLinearComment(t *testing.T) {
	c, g := linearClient(t, map[string]string{
		"issue":         `{"data":{"issue":` + linearIssueJSON + `}}`,
		"commentCreate": `{"data":{"commentCreate":{"success":true}}}`,
	})
	err := c.Comment(context.Background(), "ENG-7", "Pull request opened: https://x/pull/1\r\n\r\n\r\nDetails.")
	if err != nil {
		t.Fatalf("Comment: %v", err)
	}
	vars := g.vars(1)
	if vars["issue"] != "uuid-7" {
		t.Errorf("comment went to %v, want the issue's uuid", vars["issue"])
	}
	if vars["body"] != "Pull request opened: https://x/pull/1\n\nDetails." {
		t.Errorf("body = %q", vars["body"])
	}
}

func TestLinearHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"errors":[{"message":"Authentication required, not authenticated"}]}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{Kind: KindLinear, Token: token, BaseURL: srv.URL, Project: "ENG", HTTP: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Get(context.Background(), "ENG-7")
	if err == nil || !strings.Contains(err.Error(), "http 401") || !strings.Contains(err.Error(), "not authenticated") {
		t.Errorf("err = %v", err)
	}
}

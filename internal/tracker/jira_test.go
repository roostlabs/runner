package tracker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const token = "atl_apitoken_do_not_leak"

func jiraClient(t *testing.T, handler http.HandlerFunc) Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := New(Options{
		Kind:    KindJira,
		Token:   token,
		User:    "bot@example.com",
		BaseURL: srv.URL,
		Project: "APP",
		HTTP:    srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

const jiraIssueJSON = `{
  "key": "APP-7",
  "fields": {
    "summary": "paginator drops the last page",
    "status": {"name": "Ready for agent"},
    "description": {
      "type": "doc", "version": 1,
      "content": [
        {"type": "paragraph", "content": [
          {"type": "text", "text": "The last page is "},
          {"type": "text", "text": "missing", "marks": [{"type": "strong"}]},
          {"type": "text", "text": "."}
        ]},
        {"type": "bulletList", "content": [
          {"type": "listItem", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "open /items?page=3"}]}]},
          {"type": "listItem", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "see 404"}]}]}
        ]},
        {"type": "codeBlock", "attrs": {"language": "go"}, "content": [{"type": "text", "text": "for i := 0; i < n-1; i++ {"}]},
        {"type": "paragraph", "content": [
          {"type": "mention", "attrs": {"id": "5b10", "text": "@Dana"}},
          {"type": "text", "text": " reported it via "},
          {"type": "inlineCard", "attrs": {"url": "https://example.com/report"}}
        ]}
      ]
    }
  }
}`

func TestJiraGet(t *testing.T) {
	var gotPath, gotAuth, gotQuery string
	c := jiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotQuery = r.URL.Path, r.Header.Get("authorization"), r.URL.RawQuery
		w.Write([]byte(jiraIssueJSON))
	})

	ticket, err := c.Get(context.Background(), "APP-7")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotPath != "/rest/api/3/issue/APP-7" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotQuery, "fields=summary") {
		t.Errorf("query = %q, want the fields narrowed", gotQuery)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@example.com:"+token))
	if gotAuth != wantAuth {
		t.Errorf("authorization = %q", gotAuth)
	}

	if ticket.ID != "APP-7" || ticket.Title != "paginator drops the last page" || ticket.State != "Ready for agent" {
		t.Errorf("ticket = %+v", ticket)
	}
	if !strings.HasSuffix(ticket.URL, "/browse/APP-7") {
		t.Errorf("url = %q", ticket.URL)
	}
	want := "The last page is missing.\n- open /items?page=3\n- see 404\n```\nfor i := 0; i < n-1; i++ {\n```\n@Dana reported it via https://example.com/report"
	if ticket.Body != want {
		t.Errorf("body =\n%q\nwant\n%q", ticket.Body, want)
	}
}

func TestJiraGetPlainDescription(t *testing.T) {
	c := jiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"key":"APP-8","fields":{"summary":"s","description":"  plain text  ","status":{"name":"To Do"}}}`))
	})
	ticket, err := c.Get(context.Background(), "APP-8")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ticket.Body != "plain text" {
		t.Errorf("body = %q", ticket.Body)
	}
}

func TestJiraGetError(t *testing.T) {
	c := jiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"errorMessages":["Issue does not exist or you do not have permission to see it."],"errors":{}}`))
	})
	_, err := c.Get(context.Background(), "APP-404")
	if err == nil {
		t.Fatal("Get succeeded on a 404")
	}
	if !strings.Contains(err.Error(), "http 404") || !strings.Contains(err.Error(), "Issue does not exist") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error leaks the token: %v", err)
	}
}

func TestJiraList(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	c := jiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"issues":[` + jiraIssueJSON + `,{"key":"APP-9","fields":{"summary":"second","status":{"name":"Ready for agent"}}}]}`))
	})

	tickets, err := c.List(context.Background(), `Ready "for" agent`)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if gotPath != "/rest/api/3/search/jql" {
		t.Errorf("path = %q", gotPath)
	}
	wantJQL := `project = "APP" AND status = "Ready \"for\" agent" ORDER BY created ASC`
	if gotBody["jql"] != wantJQL {
		t.Errorf("jql = %q\nwant  %q", gotBody["jql"], wantJQL)
	}
	if len(tickets) != 2 || tickets[0].ID != "APP-7" || tickets[1].ID != "APP-9" || tickets[1].Body != "" {
		t.Errorf("tickets = %+v", tickets)
	}
}

func TestJiraTransition(t *testing.T) {
	var posted map[string]any
	c := jiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/issue/APP-7/transitions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			w.Write([]byte(`{"transitions":[
			  {"id":"11","name":"To Do","to":{"name":"To Do"}},
			  {"id":"21","name":"Start progress","to":{"name":"In Progress"}},
			  {"id":"31","name":"Done","to":{"name":"Done"}}]}`))
		case http.MethodPost:
			json.NewDecoder(r.Body).Decode(&posted)
			w.WriteHeader(http.StatusNoContent)
		}
	})

	if err := c.Transition(context.Background(), "APP-7", "in progress"); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	transition, _ := posted["transition"].(map[string]any)
	if transition["id"] != "21" {
		t.Errorf("posted = %v, want transition 21", posted)
	}
}

func TestJiraTransitionUnknownState(t *testing.T) {
	c := jiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("a transition was posted for a state that does not exist")
		}
		w.Write([]byte(`{"transitions":[{"id":"11","name":"To Do","to":{"name":"To Do"}}]}`))
	})
	err := c.Transition(context.Background(), "APP-7", "In Review")
	if err == nil || !strings.Contains(err.Error(), `no transition to "In Review"`) || !strings.Contains(err.Error(), "To Do") {
		t.Errorf("err = %v", err)
	}
}

func TestJiraComment(t *testing.T) {
	var gotPath string
	var posted struct {
		Body adfNode `json:"body"`
	}
	c := jiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&posted)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"10000"}`))
	})

	err := c.Comment(context.Background(), "APP-7", "Pull request opened: https://github.com/example/app/pull/42\n\nSummary line one\nline two")
	if err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if gotPath != "/rest/api/3/issue/APP-7/comment" {
		t.Errorf("path = %q", gotPath)
	}
	if posted.Body.Type != "doc" || posted.Body.Version != 1 || len(posted.Body.Content) != 2 {
		t.Fatalf("body = %+v", posted.Body)
	}
	first := posted.Body.Content[0]
	if first.Type != "paragraph" || len(first.Content) != 1 || first.Content[0].Text != "Pull request opened: https://github.com/example/app/pull/42" {
		t.Errorf("first paragraph = %+v", first)
	}
	second := posted.Body.Content[1]
	if len(second.Content) != 3 || second.Content[1].Type != "hardBreak" || second.Content[2].Text != "line two" {
		t.Errorf("second paragraph = %+v", second)
	}
}

func TestJiraNeedsSiteAndUser(t *testing.T) {
	_, err := New(Options{Kind: KindJira, Token: token, Project: "APP", User: "u"})
	if err == nil || !strings.Contains(err.Error(), "baseUrl") {
		t.Errorf("without a site: err = %v", err)
	}
	_, err = New(Options{Kind: KindJira, Token: token, Project: "APP", BaseURL: "https://x.atlassian.net"})
	if err == nil || !strings.Contains(err.Error(), "tracker.user") {
		t.Errorf("without a user: err = %v", err)
	}
}

func TestNewRefusesBadOptions(t *testing.T) {
	cases := map[string]Options{
		"no token":   {Kind: KindJira, Project: "APP"},
		"no project": {Kind: KindLinear, Token: token},
		"no kind":    {Token: token, Project: "APP"},
		"bad kind":   {Kind: "asana", Token: token, Project: "APP"},
	}
	for name, o := range cases {
		if _, err := New(o); err == nil {
			t.Errorf("%s: New accepted %+v", name, o)
		}
	}
}

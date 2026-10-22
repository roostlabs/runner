package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJiraExplorer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/rest/api/3/myself":
			w.Write([]byte(`{"displayName":"Roost Bot","emailAddress":"bot@acme.test"}`))
		case "/rest/api/3/project/search":
			w.Write([]byte(`{"values":[{"key":"APP","name":"App"},{"key":"WEB","name":"Web"}]}`))
		case "/rest/api/3/project/APP/statuses":
			w.Write([]byte(`[{"name":"Task","statuses":[{"name":"To Do"},{"name":"In Progress"}]},
				{"name":"Bug","statuses":[{"name":"To Do"},{"name":"Done"}]}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ex, err := NewExplorer(Options{Kind: KindJira, Token: "t", User: "bot@acme.test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	me, err := ex.Me(ctx)
	if err != nil || me.Name != "Roost Bot" {
		t.Fatalf("me = %+v, %v", me, err)
	}
	projects, err := ex.Projects(ctx)
	if err != nil || len(projects) != 2 || projects[1].Key != "WEB" {
		t.Fatalf("projects = %+v, %v", projects, err)
	}
	states, err := ex.States(ctx, "APP")
	if err != nil || strings.Join(states, ",") != "To Do,In Progress,Done" {
		t.Fatalf("states = %v, %v", states, err)
	}
}

func TestLinearExplorer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		switch {
		case strings.Contains(body.Query, "viewer"):
			w.Write([]byte(`{"data":{"viewer":{"name":"","email":"bot@acme.test"}}}`))
		case strings.Contains(body.Query, "teams"):
			w.Write([]byte(`{"data":{"teams":{"nodes":[{"key":"ENG","name":"Engineering"}]}}}`))
		case strings.Contains(body.Query, "workflowStates"):
			w.Write([]byte(`{"data":{"workflowStates":{"nodes":[{"name":"Done","position":3},{"name":"Todo","position":1},{"name":"In Progress","position":2}]}}}`))
		}
	}))
	defer srv.Close()

	ex, err := NewExplorer(Options{Kind: KindLinear, Token: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if me, err := ex.Me(ctx); err != nil || me.Name != "bot@acme.test" {
		t.Fatalf("me = %+v, %v", me, err)
	}
	if projects, err := ex.Projects(ctx); err != nil || len(projects) != 1 || projects[0].Key != "ENG" {
		t.Fatalf("projects = %+v, %v", projects, err)
	}
	if states, err := ex.States(ctx, "ENG"); err != nil || strings.Join(states, ",") != "Todo,In Progress,Done" {
		t.Fatalf("states = %v, %v", states, err)
	}
}

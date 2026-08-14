package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const token = "ghp_servicetoken_do_not_leak"

func client(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := New(Options{Token: token, APIBase: srv.URL, HTTP: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func request() Request {
	return Request{
		RemoteURL: "https://github.com/example/app.git",
		Base:      "main",
		Head:      "roost/T-1",
		Title:     "fix the off-by-one in the paginator",
		Body:      "The last page was dropped.",
	}
}

func TestOpen(t *testing.T) {
	var (
		gotPath string
		gotAuth string
		gotBody map[string]any
	)
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)

		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"number":42,"html_url":"https://github.com/example/app/pull/42"}`))
	})

	pr, err := c.Open(context.Background(), request())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if pr.Number != 42 || pr.URL != "https://github.com/example/app/pull/42" {
		t.Errorf("pr = %+v", pr)
	}
	if pr.Existed {
		t.Error("a newly created pull request was reported as pre-existing")
	}

	if gotPath != "/repos/example/app/pulls" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotBody["head"] != "roost/T-1" || gotBody["base"] != "main" {
		t.Errorf("body = %v", gotBody)
	}
	if gotBody["title"] != "fix the off-by-one in the paginator" {
		t.Errorf("title = %v", gotBody["title"])
	}
}

// A retried ticket pushes to the same branch, so the second attempt finds a
// pull request already open. Reporting that is more useful than a conflict.
func TestOpenFindsAnExistingPullRequest(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"message":"Validation Failed","errors":[
				{"message":"A pull request already exists for example:roost/T-1."}]}`))
			return
		}
		if got := r.URL.Query().Get("head"); got != "example:roost/T-1" {
			t.Errorf("head filter = %q", got)
		}
		w.Write([]byte(`[{"number":7,"html_url":"https://github.com/example/app/pull/7"}]`))
	})

	pr, err := c.Open(context.Background(), request())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if pr.Number != 7 || !pr.Existed {
		t.Errorf("pr = %+v", pr)
	}
}

// A 422 that is not a duplicate — an empty diff, a protected base — has to
// surface, not be swallowed by the lookup.
func TestOpenReportsARealValidationFailure(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"No commits between main and roost/T-1"}]}`))
			return
		}
		w.Write([]byte(`[]`))
	})

	_, err := c.Open(context.Background(), request())
	if err == nil {
		t.Fatal("Open swallowed a validation failure")
	}
	if !strings.Contains(err.Error(), "No commits between") {
		t.Errorf("error dropped the explanation: %v", err)
	}
}

// The service account's token is the credential that can write the developer's
// repository. An error that quoted it would put it in the trace.
func TestErrorsNeverCarryTheToken(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	})

	_, err := c.Open(context.Background(), request())
	if err == nil {
		t.Fatal("Open accepted a 401")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error leaked the token: %v", err)
	}
}

func TestOpenNeedsABranchAndATitle(t *testing.T) {
	c := client(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an incomplete request reached the api")
	})

	for name, mutate := range map[string]func(*Request){
		"no head":  func(r *Request) { r.Head = "" },
		"no base":  func(r *Request) { r.Base = "" },
		"no title": func(r *Request) { r.Title = "" },
	} {
		t.Run(name, func(t *testing.T) {
			req := request()
			mutate(&req)
			if _, err := c.Open(context.Background(), req); err == nil {
				t.Error("Open accepted an incomplete request")
			}
		})
	}
}

func TestRepository(t *testing.T) {
	tests := []struct {
		remote string
		owner  string
		name   string
		bad    bool
	}{
		{remote: "https://github.com/example/app.git", owner: "example", name: "app"},
		{remote: "https://github.com/example/app", owner: "example", name: "app"},
		{remote: "http://github.com/example/app.git", owner: "example", name: "app"},
		{remote: "git@github.com:example/app.git", owner: "example", name: "app"},
		{remote: "ssh://git@github.com/example/app.git", owner: "example", name: "app"},
		{remote: "https://github.example.com/team/sub/app.git", owner: "sub", name: "app"},
		{remote: "https://user:pass@github.com/example/app.git", owner: "example", name: "app"},
		{remote: "", bad: true},
		{remote: "app.git", bad: true},
		{remote: "https://github.com/app.git", bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.remote, func(t *testing.T) {
			owner, name, err := Repository(tt.remote)
			if tt.bad {
				if err == nil {
					t.Errorf("Repository(%q) = %q/%q, want an error", tt.remote, owner, name)
				}
				return
			}
			if err != nil {
				t.Fatalf("Repository(%q): %v", tt.remote, err)
			}
			if owner != tt.owner || name != tt.name {
				t.Errorf("Repository(%q) = %q/%q, want %q/%q", tt.remote, owner, name, tt.owner, tt.name)
			}
		})
	}
}

func TestNewRequiresAToken(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New accepted an empty token")
	}
}

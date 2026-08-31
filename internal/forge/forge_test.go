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

// clientKind pins the forge, which is what a self-hosted instance needs.
func clientKind(t *testing.T, kind Kind, handler http.HandlerFunc) *Client {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := New(Options{Token: token, Kind: kind, APIBase: srv.URL, HTTP: srv.Client()})
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

func TestParse(t *testing.T) {
	tests := []struct {
		remote string
		host   string
		path   string
		owner  string
		name   string
		bad    bool
	}{
		{remote: "https://github.com/example/app.git", host: "github.com", path: "example/app", owner: "example", name: "app"},
		{remote: "https://github.com/example/app", host: "github.com", path: "example/app", owner: "example", name: "app"},
		{remote: "http://github.com/example/app.git", host: "github.com", path: "example/app", owner: "example", name: "app"},
		{remote: "git@github.com:example/app.git", host: "github.com", path: "example/app", owner: "example", name: "app"},
		{remote: "ssh://git@github.com/example/app.git", host: "github.com", path: "example/app", owner: "example", name: "app"},
		{remote: "https://user:pass@github.com/example/app.git", host: "github.com", path: "example/app", owner: "example", name: "app"},
		{remote: "https://GitHub.com/example/app.git", host: "github.com", path: "example/app", owner: "example", name: "app"},
		{remote: "ssh://git@github.com:2222/example/app.git", host: "github.com", path: "example/app", owner: "example", name: "app"},

		// A GitLab subgroup is why the path is kept whole: reducing it to the
		// last two segments would address the wrong project.
		{
			remote: "https://gitlab.com/group/sub/app.git",
			host:   "gitlab.com", path: "group/sub/app", owner: "group", name: "app",
		},
		{
			remote: "git@gitlab.example.com:group/sub/deeper/app.git",
			host:   "gitlab.example.com", path: "group/sub/deeper/app", owner: "group", name: "app",
		},

		{remote: "", bad: true},
		{remote: "app.git", bad: true},
		{remote: "https://github.com/app.git", bad: true},
		{remote: "https:///example/app.git", bad: true},
		{remote: "https://github.com/example//app.git", bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.remote, func(t *testing.T) {
			got, err := Parse(tt.remote)
			if tt.bad {
				if err == nil {
					t.Errorf("Parse(%q) = %+v, want an error", tt.remote, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.remote, err)
			}
			if got.Host != tt.host || got.Path != tt.path {
				t.Errorf("Parse(%q) = %+v, want host %q path %q", tt.remote, got, tt.host, tt.path)
			}
			if got.Owner() != tt.owner || got.Name() != tt.name {
				t.Errorf("Parse(%q): owner/name = %q/%q, want %q/%q",
					tt.remote, got.Owner(), got.Name(), tt.owner, tt.name)
			}
		})
	}
}

// A self-hosted forge is just a hostname. Guessing would push a branch and then
// fail to open anything on it, so it is refused until it is configured.
func TestUnknownHostIsRefused(t *testing.T) {
	c := client(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an unknown host reached an api")
	})

	req := request()
	req.RemoteURL = "https://git.example.com/team/app.git"

	_, err := c.Open(context.Background(), req)
	if err == nil {
		t.Fatal("Open guessed the forge")
	}
	for _, want := range []string{"git.example.com", "git.forge"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestKindOverridesHostDetection(t *testing.T) {
	var gotPath string
	c := clientKind(t, KindGitLab, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"iid":3,"web_url":"https://git.example.com/team/app/-/merge_requests/3"}`))
	})

	req := request()
	req.RemoteURL = "https://git.example.com/team/app.git"

	pr, err := c.Open(context.Background(), req)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if pr.Number != 3 {
		t.Errorf("pr = %+v", pr)
	}
	if !strings.HasPrefix(gotPath, "/api/v4/projects/") {
		t.Errorf("path = %q, want the gitlab api", gotPath)
	}
}

func TestNewRefusesAnUnknownForge(t *testing.T) {
	if _, err := New(Options{Token: token, Kind: "bitbucket"}); err == nil {
		t.Error("New accepted a forge it cannot talk to")
	}
}

// GitHub addresses a repository as owner/name and nothing else, so a subgroup
// path has to be refused rather than silently truncated to its last two parts.
func TestGitHubRefusesASubgroupPath(t *testing.T) {
	c := clientKind(t, KindGitHub, func(http.ResponseWriter, *http.Request) {
		t.Error("a subgroup path reached the github api")
	})

	req := request()
	req.RemoteURL = "https://github.com/group/sub/app.git"

	if _, err := c.Open(context.Background(), req); err == nil {
		t.Fatal("Open accepted a path github cannot address")
	}
}

func TestNewRequiresAToken(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New accepted an empty token")
	}
}

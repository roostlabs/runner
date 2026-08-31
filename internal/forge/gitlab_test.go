package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func gitlabRequest() Request {
	req := request()
	req.RemoteURL = "https://gitlab.com/example/app.git"
	return req
}

func TestGitLabOpen(t *testing.T) {
	var (
		gotPath  string
		gotToken string
		gotAuth  string
		gotBody  map[string]any
	)
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotToken = r.Header.Get("private-token")
		gotAuth = r.Header.Get("authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)

		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"iid":12,"web_url":"https://gitlab.com/example/app/-/merge_requests/12"}`))
	})

	pr, err := c.Open(context.Background(), gitlabRequest())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if pr.Number != 12 || pr.URL != "https://gitlab.com/example/app/-/merge_requests/12" {
		t.Errorf("pr = %+v", pr)
	}
	if pr.Existed {
		t.Error("a new merge request was reported as pre-existing")
	}

	if gotPath != "/api/v4/projects/example%2Fapp/merge_requests" {
		t.Errorf("path = %q", gotPath)
	}
	// A project access token goes in GitLab's own header. Bearer is for OAuth,
	// and sending both would hand the token to two code paths.
	if gotToken != token {
		t.Errorf("private-token = %q", gotToken)
	}
	if gotAuth != "" {
		t.Errorf("authorization = %q, want it unset", gotAuth)
	}

	// GitLab names the branches differently from GitHub, and getting them the
	// wrong way round would open a merge request pointing backwards.
	if gotBody["source_branch"] != "roost/T-1" || gotBody["target_branch"] != "main" {
		t.Errorf("body = %v", gotBody)
	}
	if gotBody["description"] != "The last page was dropped." {
		t.Errorf("description = %v", gotBody["description"])
	}
	if gotBody["remove_source_branch"] != false {
		t.Errorf("remove_source_branch = %v, want false: the branch is the task's output",
			gotBody["remove_source_branch"])
	}
}

// A subgroup path is addressed whole and URL-encoded. Splitting it into an
// owner and a name would point at a project that does not exist.
func TestGitLabAddressesSubgroups(t *testing.T) {
	var gotPath string
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath, not Path: the slashes in a project id must stay encoded,
		// and Path has already decoded them.
		gotPath = r.URL.EscapedPath()
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"iid":1,"web_url":"https://gitlab.com/g/s/app/-/merge_requests/1"}`))
	})

	req := gitlabRequest()
	req.RemoteURL = "git@gitlab.com:g/s/app.git"

	if _, err := c.Open(context.Background(), req); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if gotPath != "/api/v4/projects/g%2Fs%2Fapp/merge_requests" {
		t.Errorf("path = %q", gotPath)
	}
}

// A retried ticket pushes to the same branch, and GitLab answers a duplicate
// with a 409. Finding the open merge request beats reporting a conflict.
func TestGitLabFindsAnExistingMergeRequest(t *testing.T) {
	statuses := []int{http.StatusConflict, http.StatusBadRequest, http.StatusUnprocessableEntity}
	for _, status := range statuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var gotQuery string
			c := client(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "application/json")
				if r.Method == http.MethodPost {
					w.WriteHeader(status)
					w.Write([]byte(`{"message":["Another open merge request already exists for this source branch"]}`))
					return
				}
				gotQuery = r.URL.RawQuery
				w.Write([]byte(`[{"iid":9,"web_url":"https://gitlab.com/example/app/-/merge_requests/9"}]`))
			})

			pr, err := c.Open(context.Background(), gitlabRequest())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if pr.Number != 9 || !pr.Existed {
				t.Errorf("pr = %+v", pr)
			}
			for _, want := range []string{"source_branch=roost%2FT-1", "state=opened"} {
				if !strings.Contains(gotQuery, want) {
					t.Errorf("query %q is missing %q", gotQuery, want)
				}
			}
		})
	}
}

// A rejection that is not a duplicate has to surface, not be swallowed by the
// lookup that follows it.
func TestGitLabReportsARealRejection(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"message":["Source branch does not exist"]}`))
			return
		}
		w.Write([]byte(`[]`))
	})

	_, err := c.Open(context.Background(), gitlabRequest())
	if err == nil {
		t.Fatal("Open swallowed a rejection")
	}
	if !strings.Contains(err.Error(), "Source branch does not exist") {
		t.Errorf("error dropped the explanation: %v", err)
	}
}

// GitLab is inconsistent about the shape of an error body, and an installer
// reading "http 400" instead of the reason is a support ticket.
func TestGitLabErrorShapes(t *testing.T) {
	tests := map[string]struct {
		body string
		want string
	}{
		"message as a string": {
			body: `{"message":"403 Forbidden"}`,
			want: "403 Forbidden",
		},
		"message as an array": {
			body: `{"message":["Title can't be blank","Source branch is invalid"]}`,
			want: "Title can't be blank; Source branch is invalid",
		},
		"message keyed by field": {
			body: `{"message":{"source_branch":["is invalid"],"base":["cannot be blank"]}}`,
			want: "base: cannot be blank; source_branch: is invalid",
		},
		"error instead of message": {
			body: `{"error":"insufficient_scope"}`,
			want: "insufficient_scope",
		},
		"not json at all": {
			body: `<html>502 Bad Gateway</html>`,
			want: "http 500",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := explainGitLab([]byte(tt.body), http.StatusInternalServerError); !strings.Contains(got, tt.want) {
				t.Errorf("explainGitLab(%s) = %q, want it to mention %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestGitLabErrorsNeverCarryTheToken(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"401 Unauthorized"}`))
	})

	_, err := c.Open(context.Background(), gitlabRequest())
	if err == nil {
		t.Fatal("Open accepted a 401")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error leaked the token: %v", err)
	}
}

// An API base can carry credentials in its userinfo, and this error is
// journalled and streamed to the cloud.
func TestErrorsCarryThePathNotTheURL(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"404 Project Not Found"}`))
	})

	_, err := c.Open(context.Background(), gitlabRequest())
	if err == nil {
		t.Fatal("Open accepted a 404")
	}
	if strings.Contains(err.Error(), "http://127.0.0.1") {
		t.Errorf("error quoted the whole url: %v", err)
	}
	if !strings.Contains(err.Error(), "/api/v4/projects/") {
		t.Errorf("error dropped the path: %v", err)
	}
}

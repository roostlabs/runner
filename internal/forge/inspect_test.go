package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWhoAmIAndInspectGitHub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/user":
			w.Write([]byte(`{"login":"roost-bot"}`))
		case "/repos/acme/app":
			w.Write([]byte(`{"permissions":{"push":true}}`))
		case "/repos/acme/app/languages":
			w.Write([]byte(`{"Go":9000,"Shell":100,"TypeScript":500}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := New(Options{Token: "tok", Kind: KindGitHub, APIBase: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	me, err := c.WhoAmI(context.Background())
	if err != nil || me.Login != "roost-bot" {
		t.Fatalf("whoami = %+v, %v", me, err)
	}
	info, err := c.Inspect(context.Background(), "https://github.com/acme/app.git")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Push || len(info.Languages) != 3 || info.Languages[0] != "Go" || info.Languages[1] != "TypeScript" {
		t.Fatalf("info = %+v", info)
	}

	bad, _ := New(Options{Token: "nope", Kind: KindGitHub, APIBase: srv.URL})
	if _, err := bad.WhoAmI(context.Background()); err == nil {
		t.Fatal("a rejected token was not an error")
	}
}

func TestInspectGitLab(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/user":
			w.Write([]byte(`{"username":"bot"}`))
		case "/api/v4/projects/group%2Fsub%2Fapp", "/api/v4/projects/group/sub/app":
			w.Write([]byte(`{"permissions":{"project_access":null,"group_access":{"access_level":30}}}`))
		case "/api/v4/projects/group%2Fsub%2Fapp/languages", "/api/v4/projects/group/sub/app/languages":
			w.Write([]byte(`{"Python":80.5,"HTML":19.5}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, _ := New(Options{Token: "tok", Kind: KindGitLab, APIBase: srv.URL})
	me, err := c.WhoAmI(context.Background())
	if err != nil || me.Login != "bot" {
		t.Fatalf("whoami = %+v, %v", me, err)
	}
	info, err := c.Inspect(context.Background(), "git@gitlab.com:group/sub/app.git")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Push || len(info.Languages) != 2 || info.Languages[0] != "Python" {
		t.Fatalf("info = %+v", info)
	}
}

func TestWhoAmINeedsAKindWithACustomBase(t *testing.T) {
	c, _ := New(Options{Token: "tok", APIBase: "https://git.example.com"})
	if _, err := c.WhoAmI(context.Background()); err == nil {
		t.Fatal("an api base with no forge kind was accepted")
	}
}

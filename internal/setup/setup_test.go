package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roostlabs/runner/internal/agent"
	"github.com/roostlabs/runner/internal/alert"
	"github.com/roostlabs/runner/internal/config"
	"github.com/roostlabs/runner/internal/forge"
	"github.com/roostlabs/runner/internal/tracker"
)

// fakeForge answers the forge checks.
type fakeForge struct {
	login string
	push  bool
	langs []string
	err   error
}

func (f fakeForge) WhoAmI(context.Context) (forge.Account, error) {
	return forge.Account{Login: f.login}, f.err
}

func (f fakeForge) Inspect(context.Context, string) (forge.RepoInfo, error) {
	return forge.RepoInfo{Push: f.push, Languages: f.langs}, f.err
}

// fakeTracker answers the tracker checks.
type fakeTracker struct {
	projects []tracker.Project
	states   []string
}

func (f fakeTracker) Me(context.Context) (tracker.Account, error) {
	return tracker.Account{Name: "Roost Bot"}, nil
}

func (f fakeTracker) Projects(context.Context) ([]tracker.Project, error) { return f.projects, nil }

func (f fakeTracker) States(context.Context, string) ([]string, error) { return f.states, nil }

type run struct {
	out    string
	cfg    config.Config
	err    error
	alerts []alert.Sink
	dialed []string
}

// answer drives the wizard with scripted input and records what it wrote.
func answer(t *testing.T, path string, section string, lines []string, probes func(*Probes)) run {
	t.Helper()
	var out bytes.Buffer
	p := Probes{
		Cloud: func(_ context.Context, url, token, _ string) (string, error) {
			if token == "" {
				return "", errors.New("no token")
			}
			return "c-1234", nil
		},
		LLM:    func(context.Context, string, string) error { return nil },
		Claude: func(context.Context, string, []string) (agent.ClaudeAuth, error) { return agent.ClaudeAuth{}, nil },
		Forge: func(forge.Options) (ForgeProbe, error) {
			return fakeForge{login: "roost-bot", push: true}, nil
		},
		Tracker: func(tracker.Options) (tracker.Explorer, error) { return fakeTracker{}, nil },
		Alert:   func(context.Context, alert.Sink) error { return nil },
	}
	var r run
	base := p.Cloud
	p.Cloud = func(ctx context.Context, url, token, v string) (string, error) {
		r.dialed = append(r.dialed, url)
		return base(ctx, url, token, v)
	}
	baseAlert := p.Alert
	p.Alert = func(ctx context.Context, sink alert.Sink) error {
		r.alerts = append(r.alerts, sink)
		return baseAlert(ctx, sink)
	}
	if probes != nil {
		probes(&p)
	}

	w := &Wizard{
		In:         strings.NewReader(strings.Join(lines, "\n") + "\n"),
		Out:        &out,
		ConfigPath: path,
		Section:    section,
		Version:    "test",
		Probes:     p,
	}
	r.err = w.Run(context.Background())
	r.out = out.String()
	if raw, err := os.ReadFile(path); err == nil {
		json.Unmarshal(raw, &r.cfg)
	}
	return r
}

// fullAnswers is a complete pass: Cloud, the subscription agent, GitHub,
// Jira with one project, an image and no alerts.
func fullAnswers() []string {
	return []string{
		"wss://cloud.example.com/channel", // cloud url
		"tok-abc",                         // runner token
		"1",                               // agent: claude subscription
		"claude",                          // binary
		"5",                               // budget
		"n",                               // approval gate
		"1",                               // git: github.com
		"ghp_x",                           // git token
		"Roost",                           // author name
		"roost@example.test",              // author email
		"2",                               // tracker: jira
		"https://acme.atlassian.net",      // site
		"bot@acme.test",                   // email
		"jira-token",                      // token
		"1",                               // projects: the first
		"https://github.com/acme/app.git", // repo for it
		"1",                               // ready state
		"2",                               // in progress
		"4",                               // in review: the list is offset by "leave it where it is"
		"30",                              // poll interval
		"golang:1.26",                     // image
		"n",                               // telegram
		"n",                               // webhook
	}
}

func TestWizardWritesAWorkingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	r := answer(t, path, "", fullAnswers(), func(p *Probes) {
		p.Claude = func(context.Context, string, []string) (agent.ClaudeAuth, error) {
			return agent.ClaudeAuth{LoggedIn: true, Email: "dev@example.test", Subscription: "max"}, nil
		}
		p.Tracker = func(tracker.Options) (tracker.Explorer, error) {
			return fakeTracker{
				projects: []tracker.Project{{Key: "APP", Name: "App"}, {Key: "WEB", Name: "Web"}},
				states:   []string{"To Do", "In Progress", "In Review"},
			}, nil
		}
	})
	if r.err != nil {
		t.Fatalf("err = %v\n%s", r.err, r.out)
	}

	cfg := r.cfg
	if cfg.CloudURL != "wss://cloud.example.com/channel" || cfg.Token != "tok-abc" {
		t.Errorf("cloud = %q %q", cfg.CloudURL, cfg.Token)
	}
	if cfg.Agent.Backend != config.BackendClaudeCode || cfg.Creds.LLM != "" || cfg.Agent.BudgetUSD != 5 {
		t.Errorf("agent = %+v", cfg.Agent)
	}
	if cfg.Creds.Git != "ghp_x" || cfg.Git.Forge != "" || cfg.Git.AuthorEmail != "roost@example.test" {
		t.Errorf("git = %+v %q", cfg.Git, cfg.Creds.Git)
	}
	if cfg.Tracker.Kind != "jira" || cfg.Creds.TaskManager != "jira-token" ||
		cfg.Tracker.Projects["APP"] != "https://github.com/acme/app.git" || len(cfg.Tracker.Projects) != 1 {
		t.Errorf("tracker = %+v", cfg.Tracker)
	}
	if cfg.Tracker.States.Ready != "To Do" || cfg.Tracker.States.InProgress != "In Progress" ||
		cfg.Tracker.States.InReview != "In Review" || cfg.Tracker.PollIntervalSec != 30 {
		t.Errorf("states = %+v", cfg.Tracker.States)
	}
	if cfg.Sandbox.Image != "golang:1.26" {
		t.Errorf("image = %q", cfg.Sandbox.Image)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the written config does not validate: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != config.FileMode.Perm() {
		t.Errorf("mode = %v, %v", info.Mode().Perm(), err)
	}
	if !strings.Contains(r.out, "signed in as dev@example.test (max)") {
		t.Errorf("the sign-in check was not reported:\n%s", r.out)
	}
	if !strings.Contains(r.out, "the token belongs to roost-bot") {
		t.Errorf("the git check was not reported:\n%s", r.out)
	}
}

func TestWizardRetriesAFailedCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	answers := append([]string{
		"wss://cloud.example.com/channel", "wrong-token",
		"1", // retry
		"wss://cloud.example.com/channel", "right-token",
	}, fullAnswers()[2:]...)

	calls := 0
	r := answer(t, path, "", answers, func(p *Probes) {
		p.Cloud = func(_ context.Context, _, token, _ string) (string, error) {
			calls++
			if token != "right-token" {
				return "", errors.New("cloud refused the token")
			}
			return "c-9", nil
		}
		p.Claude = func(context.Context, string, []string) (agent.ClaudeAuth, error) {
			return agent.ClaudeAuth{LoggedIn: true, Email: "dev@example.test"}, nil
		}
		p.Tracker = func(tracker.Options) (tracker.Explorer, error) {
			return fakeTracker{projects: []tracker.Project{{Key: "APP"}}, states: []string{"To Do", "In Progress", "In Review"}}, nil
		}
	})
	if r.err != nil {
		t.Fatalf("err = %v\n%s", r.err, r.out)
	}
	if calls != 2 || r.cfg.Token != "right-token" {
		t.Fatalf("calls = %d, token = %q", calls, r.cfg.Token)
	}
	if !strings.Contains(r.out, "cloud refused the token") {
		t.Errorf("the failure was not shown:\n%s", r.out)
	}
}

func TestWizardAbortsWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	r := answer(t, path, "", []string{"wss://c/channel", "bad", "3"}, func(p *Probes) {
		p.Cloud = func(context.Context, string, string, string) (string, error) {
			return "", errors.New("nope")
		}
	})
	if !strings.Contains(r.err.Error(), "aborted") {
		t.Fatalf("err = %v", r.err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("an aborted run wrote the config")
	}
}

func TestWizardEditsOneSectionAndKeepsSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	existing := config.Config{
		CloudURL: "wss://cloud.example.com/channel",
		Token:    "tok-abc",
		DataDir:  dir,
		Creds:    config.Creds{Git: "ghp_old", TaskManager: "jira-old"},
		Sandbox:  config.Sandbox{Image: "golang:1.26"},
		Agent:    config.Agent{Backend: config.BackendClaudeCode},
		Git:      config.Git{AuthorName: "Roost", AuthorEmail: "roost@example.test"},
	}
	if err := config.Save(path, existing); err != nil {
		t.Fatal(err)
	}

	// Only the alerts section: a webhook, keeping everything else.
	r := answer(t, path, "alerts", []string{"n", "y", "https://hooks.example.com/roost", "s3cret"}, nil)
	if r.err != nil {
		t.Fatalf("err = %v\n%s", r.err, r.out)
	}
	if r.cfg.Alerts.Webhook.URL != "https://hooks.example.com/roost" || r.cfg.Alerts.Webhook.Secret != "s3cret" {
		t.Errorf("alerts = %+v", r.cfg.Alerts)
	}
	if r.cfg.Creds.Git != "ghp_old" || r.cfg.Creds.TaskManager != "jira-old" || r.cfg.Token != "tok-abc" {
		t.Errorf("a section edit lost credentials: %+v", r.cfg.Creds)
	}
	if len(r.alerts) != 1 {
		t.Errorf("the webhook was not tested: %v", r.alerts)
	}
	if strings.Contains(r.out, "ghp_old") || strings.Contains(r.out, "s3cret") {
		t.Errorf("a secret was echoed:\n%s", r.out)
	}
}

func TestWizardSuggestsAnImageFromTheRepository(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	existing := config.Config{
		CloudURL: "wss://c.example.com/channel", Token: "t", DataDir: dir,
		Creds:   config.Creds{Git: "ghp_x"},
		Tracker: config.Tracker{Kind: "jira", Project: "APP", Repo: "https://github.com/acme/api.git"},
	}
	if err := config.Save(path, existing); err != nil {
		t.Fatal(err)
	}
	r := answer(t, path, "sandbox", []string{""}, func(p *Probes) {
		p.Forge = func(forge.Options) (ForgeProbe, error) {
			return fakeForge{login: "bot", push: true, langs: []string{"Python", "Dockerfile"}}, nil
		}
	})
	if r.err != nil {
		t.Fatalf("err = %v\n%s", r.err, r.out)
	}
	if r.cfg.Sandbox.Image != "python:3.13" {
		t.Fatalf("image = %q, want the one the language suggests\n%s", r.cfg.Sandbox.Image, r.out)
	}
}

func TestWizardRefusesAnUnknownSection(t *testing.T) {
	r := answer(t, filepath.Join(t.TempDir(), "c.json"), "billing", []string{""}, nil)
	if r.err == nil || !strings.Contains(r.err.Error(), "unknown section") {
		t.Fatalf("err = %v", r.err)
	}
}

func TestImageForKnowsTheCommonStacks(t *testing.T) {
	for language, want := range map[string]string{
		"Go": "golang:1.26", "TypeScript": "node:22", "python": "python:3.13",
		"Rust": "rust:1", "COBOL": "",
	} {
		if got := imageFor(language); got != want {
			t.Errorf("imageFor(%q) = %q, want %q", language, got, want)
		}
	}
}

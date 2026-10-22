// Package setup is `roost-runner init`: the questions a developer answers
// once, on the server, to turn a fresh install into a Runner that works.
//
// Every credential is tried against its service before it is written, so a
// mistyped token fails here, in front of the developer, rather than in the
// morning as a failed task. The answers go into the same config file the
// Runner reads, at 0600, and nothing else: the wizard is a way of writing
// that file, not a second source of truth.
package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/agent"
	"github.com/roostlabs/runner/internal/alert"
	"github.com/roostlabs/runner/internal/channel"
	"github.com/roostlabs/runner/internal/config"
	"github.com/roostlabs/runner/internal/forge"
	"github.com/roostlabs/runner/internal/llm"
	"github.com/roostlabs/runner/internal/tracker"
)

// Sections, in the order they are asked. --section picks one.
var Sections = []string{"cloud", "agent", "git", "tracker", "sandbox", "alerts"}

// ServiceName is the systemd unit the installer creates, and ServiceUser the
// account it runs as.
const (
	ServiceName = "roost-runner"
	ServiceUser = "roost"
)

// ForgeProbe is the slice of a forge client setup uses.
type ForgeProbe interface {
	WhoAmI(ctx context.Context) (forge.Account, error)
	Inspect(ctx context.Context, remoteURL string) (forge.RepoInfo, error)
}

// Probes are the live checks, as functions so tests can answer them.
type Probes struct {
	Cloud   func(ctx context.Context, url, token, version string) (runnerID string, err error)
	LLM     func(ctx context.Context, key, baseURL string) error
	Claude  func(ctx context.Context, bin string, env []string) (agent.ClaudeAuth, error)
	Forge   func(o forge.Options) (ForgeProbe, error)
	Tracker func(o tracker.Options) (tracker.Explorer, error)
	Alert   func(ctx context.Context, sink alert.Sink) error
	// Restart restarts the Runner service and says what it did. An error
	// means it could not; the wizard then prints what to run by hand.
	Restart func(ctx context.Context) (string, error)
}

// DefaultProbes talk to the real services.
func DefaultProbes() Probes {
	return Probes{
		Cloud: func(ctx context.Context, url, token, version string) (string, error) {
			c, err := channel.Dial(ctx, channel.Options{
				URL: url, Token: token, RunnerVersion: version,
				Host: protocol.HostInfo{OS: runtime.GOOS, Arch: runtime.GOARCH},
			})
			if err != nil {
				return "", err
			}
			defer c.CloseNow()
			return c.RunnerID(), nil
		},
		LLM: func(ctx context.Context, key, baseURL string) error {
			c, err := llm.New(llm.Options{APIKey: key, BaseURL: baseURL})
			if err != nil {
				return err
			}
			return c.Verify(ctx)
		},
		Claude: agent.ClaudeStatus,
		Forge: func(o forge.Options) (ForgeProbe, error) {
			return forge.New(o)
		},
		Tracker: tracker.NewExplorer,
		Alert: func(ctx context.Context, sink alert.Sink) error {
			return sink.Send(ctx, alert.Event{Kind: alert.KindTest})
		},
		Restart: restartService,
	}
}

// Wizard asks the questions.
type Wizard struct {
	In  io.Reader
	Out io.Writer
	// ConfigPath is the file to read and write.
	ConfigPath string
	// Section limits the run to one section; empty asks everything.
	Section string
	// Version is reported to Cloud in the handshake.
	Version string
	Probes  Probes
	// HideEcho turns terminal echo off and returns the call that turns it
	// back on. Nil echoes, which is what a test wants and a pipe gets
	// anyway. It is a switch rather than a reader on purpose: a second
	// reader on the same input would swallow whatever the first buffered.
	HideEcho func() (restore func())

	in  *bufio.Reader
	eof bool
}

var errAbort = errors.New("setup: aborted")

// Run asks, checks, writes, and restarts.
func (w *Wizard) Run(ctx context.Context) error {
	w.in = bufio.NewReader(w.In)
	if w.Section != "" && !contains(Sections, w.Section) {
		return fmt.Errorf("setup: unknown section %q; one of %s", w.Section, strings.Join(Sections, ", "))
	}

	cfg, existed, err := read(w.ConfigPath)
	if err != nil {
		return err
	}
	if existed {
		w.say("Editing %s. Press Enter to keep a current value.", w.ConfigPath)
	} else {
		w.say("Writing %s.", w.ConfigPath)
	}
	if cfg.DataDir == "" {
		cfg.DataDir = config.DefaultDataDir()
	}

	steps := []struct {
		name string
		fn   func(context.Context, *config.Config) error
	}{
		{"cloud", w.cloud},
		{"agent", w.agent},
		{"git", w.git},
		{"tracker", w.tracker},
		{"sandbox", w.sandbox},
		{"alerts", w.alerts},
	}
	for _, step := range steps {
		if w.Section != "" && w.Section != step.name {
			continue
		}
		w.say("")
		w.say("== %s ==", step.name)
		if err := step.fn(ctx, &cfg); err != nil {
			return err
		}
		if w.eof {
			return errors.New("setup: input ended before the questions did")
		}
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("setup: the answers do not make a working config: %w", err)
	}
	if err := write(w.ConfigPath, cfg); err != nil {
		return err
	}
	w.say("")
	w.say("Wrote %s.", w.ConfigPath)

	if w.Probes.Restart != nil {
		if did, err := w.Probes.Restart(ctx); err == nil && did != "" {
			w.say("%s", did)
		} else {
			w.say("Start or restart the Runner to pick the config up: systemctl restart %s", ServiceName)
		}
	}
	return nil
}

// ---- sections ----

func (w *Wizard) cloud(ctx context.Context, cfg *config.Config) error {
	def := cfg.CloudURL
	if def == "" {
		def = "wss://"
	}
	for {
		cfg.CloudURL = w.ask("Cloud channel url (from the dashboard)", def)
		cfg.Token = w.secret("Runner token (shown once when the Runner was created)", cfg.Token)
		keep, err := w.check(ctx, "connecting to Cloud", func() (string, error) {
			id, err := w.Probes.Cloud(ctx, cfg.CloudURL, cfg.Token, w.Version)
			if err != nil {
				return "", err
			}
			return "connected; Cloud knows this Runner as " + id, nil
		})
		if err != nil {
			return err
		}
		if keep {
			return nil
		}
	}
}

func (w *Wizard) agent(ctx context.Context, cfg *config.Config) error {
	def := 1
	if cfg.Agent.Backend == config.BackendClaudeCode {
		def = 0
	}
	choice := w.choose("How should the agent think?", []string{
		"Claude subscription: the claude CLI on this server, signed in with `claude login`",
		"Anthropic API key",
	}, def)

	if choice == 0 {
		cfg.Agent.Backend = config.BackendClaudeCode
		cfg.Creds.LLM = ""
		cfg.Agent.ClaudeBin = w.ask("claude binary", orDefault(cfg.Agent.ClaudeBin, "claude"))
		if cfg.Agent.ClaudeBin == "claude" {
			cfg.Agent.ClaudeBin = ""
		}
		env, who := serviceEnv()
		for {
			keep, err := w.check(ctx, "asking the CLI whether it is signed in"+who, func() (string, error) {
				auth, err := w.Probes.Claude(ctx, cfg.Agent.ClaudeBin, env)
				if err != nil {
					return "", err
				}
				if !auth.LoggedIn {
					return "", errors.New("not signed in; run `claude login` as the account the Runner runs as" + who)
				}
				return fmt.Sprintf("signed in as %s (%s)", auth.Email, orDefault(auth.Subscription, auth.Method)), nil
			})
			if err != nil {
				return err
			}
			if keep {
				break
			}
		}
	} else {
		cfg.Agent.Backend = config.BackendAPI
		for {
			cfg.Creds.LLM = w.secret("Anthropic API key", cfg.Creds.LLM)
			keep, err := w.check(ctx, "checking the key", func() (string, error) {
				return "the key works", w.Probes.LLM(ctx, cfg.Creds.LLM, cfg.Agent.BaseURL)
			})
			if err != nil {
				return err
			}
			if keep {
				break
			}
		}
		cfg.Agent.Model = w.ask("Model", orDefault(cfg.Agent.Model, llm.DefaultModel))
		if cfg.Agent.Model == llm.DefaultModel {
			cfg.Agent.Model = ""
		}
	}

	budget := w.ask("Budget per task, in dollars (0 = no cap)", strconv.FormatFloat(orFloat(cfg.Agent.BudgetUSD, 5), 'f', -1, 64))
	if v, err := strconv.ParseFloat(budget, 64); err == nil && v >= 0 {
		cfg.Agent.BudgetUSD = v
	}
	cfg.Agent.ApprovePR = w.confirm("Hold every pull request for your approval in the dashboard before it is pushed?", cfg.Agent.ApprovePR)
	return nil
}

func (w *Wizard) git(ctx context.Context, cfg *config.Config) error {
	kinds := []string{"GitHub (github.com)", "GitLab (gitlab.com)", "GitHub Enterprise", "self-hosted GitLab"}
	def := 0
	switch {
	case cfg.Git.Forge == "gitlab" && cfg.Git.APIBase != "":
		def = 3
	case cfg.Git.Forge == "github" && cfg.Git.APIBase != "":
		def = 2
	case cfg.Git.Forge == "gitlab":
		def = 1
	}
	switch w.choose("Where do the repositories live?", kinds, def) {
	case 0:
		cfg.Git.Forge, cfg.Git.APIBase = "", ""
	case 1:
		cfg.Git.Forge, cfg.Git.APIBase = "gitlab", ""
	case 2:
		cfg.Git.Forge = "github"
		cfg.Git.APIBase = w.ask("API root, with its path", orDefault(cfg.Git.APIBase, "https://git.example.com/api/v3"))
	case 3:
		cfg.Git.Forge = "gitlab"
		cfg.Git.APIBase = w.ask("GitLab url", orDefault(cfg.Git.APIBase, "https://git.example.com"))
	}

	for {
		cfg.Creds.Git = w.secret("Token of the service account (needs to push and open pull requests, not merge)", cfg.Creds.Git)
		keep, err := w.check(ctx, "asking the forge who the token is", func() (string, error) {
			probe, err := w.Probes.Forge(forge.Options{Token: cfg.Creds.Git, Kind: forge.Kind(cfg.Git.Forge), APIBase: cfg.Git.APIBase})
			if err != nil {
				return "", err
			}
			me, err := probe.WhoAmI(ctx)
			if err != nil {
				return "", err
			}
			return "the token belongs to " + me.Login, nil
		})
		if err != nil {
			return err
		}
		if keep {
			break
		}
	}
	cfg.Git.AuthorName = w.ask("Commit author name", orDefault(cfg.Git.AuthorName, "Roost"))
	cfg.Git.AuthorEmail = w.ask("Commit author email", orDefault(cfg.Git.AuthorEmail, "roost@"+hostname()))
	return nil
}

func (w *Wizard) tracker(ctx context.Context, cfg *config.Config) error {
	def := 0
	switch cfg.Tracker.Kind {
	case "jira":
		def = 1
	case "linear":
		def = 2
	}
	switch w.choose("Which task manager files the tickets?", []string{"none: tasks are started from the dashboard", "Jira", "Linear"}, def) {
	case 0:
		cfg.Tracker = config.Tracker{}
		cfg.Creds.TaskManager = ""
		return nil
	case 1:
		cfg.Tracker.Kind = "jira"
		cfg.Tracker.BaseURL = w.ask("Jira site", orDefault(cfg.Tracker.BaseURL, "https://acme.atlassian.net"))
		cfg.Tracker.User = w.ask("Email of the account the API token belongs to", cfg.Tracker.User)
	case 2:
		cfg.Tracker.Kind = "linear"
		cfg.Tracker.BaseURL, cfg.Tracker.User = "", ""
	}

	var ex tracker.Explorer
	for {
		cfg.Creds.TaskManager = w.secret("API token", cfg.Creds.TaskManager)
		keep, err := w.check(ctx, "asking the tracker who the token is", func() (string, error) {
			var err error
			ex, err = w.Probes.Tracker(tracker.Options{
				Kind: tracker.Kind(cfg.Tracker.Kind), Token: cfg.Creds.TaskManager,
				User: cfg.Tracker.User, BaseURL: cfg.Tracker.BaseURL,
			})
			if err != nil {
				return "", err
			}
			me, err := ex.Me(ctx)
			if err != nil {
				return "", err
			}
			return "the token belongs to " + me.Name, nil
		})
		if err != nil {
			return err
		}
		if keep {
			break
		}
	}
	if ex == nil {
		// The token was kept unverified; the rest needs a working one.
		w.say("Skipping the project and state lists: the token could not be checked. Fill tracker.projects and tracker.states by hand.")
		return nil
	}

	// Projects → repositories. The old map is the default for keys it has.
	projects, err := ex.Projects(ctx)
	if err != nil {
		return fmt.Errorf("setup: listing projects: %w", err)
	}
	if len(projects) == 0 {
		w.say("The token sees no projects.")
		return nil
	}
	labels := make([]string, len(projects))
	var preselected []int
	old := cfg.Tracker.Repos()
	for i, p := range projects {
		labels[i] = p.Key + "  " + p.Name
		if _, ok := old[p.Key]; ok {
			preselected = append(preselected, i)
		}
	}
	chosen := w.pick("Which projects should the Runner work on?", labels, preselected)
	if len(chosen) == 0 {
		w.say("No project chosen; polling stays off.")
		cfg.Tracker.Projects, cfg.Tracker.Project, cfg.Tracker.Repo = nil, "", ""
		cfg.Tracker.States = config.TrackerStates{}
		return nil
	}

	repos := map[string]string{}
	for _, i := range chosen {
		key := projects[i].Key
		for {
			remote := w.ask("Repository for "+key+" (git url)", old[key])
			if remote == "" {
				w.say("A project needs a repository.")
				if w.eof {
					return nil
				}
				continue
			}
			keep, err := w.check(ctx, "checking push access to "+remote, func() (string, error) {
				if cfg.Creds.Git == "" {
					return "no git token yet; skipped", nil
				}
				probe, err := w.Probes.Forge(forge.Options{Token: cfg.Creds.Git, Kind: forge.Kind(cfg.Git.Forge), APIBase: cfg.Git.APIBase})
				if err != nil {
					return "", err
				}
				info, err := probe.Inspect(ctx, remote)
				if err != nil {
					return "", err
				}
				if !info.Push {
					return "", errors.New("the git token cannot push there")
				}
				return "the git token can push", nil
			})
			if err != nil {
				return err
			}
			if keep {
				repos[key] = remote
				break
			}
		}
	}
	cfg.Tracker.Projects = repos
	cfg.Tracker.Project, cfg.Tracker.Repo = "", ""

	// States, from the first chosen project's workflow.
	first := projects[chosen[0]].Key
	states, err := ex.States(ctx, first)
	if err != nil {
		return fmt.Errorf("setup: listing states of %s: %w", first, err)
	}
	if len(states) == 0 {
		w.say("%s reports no workflow states; fill tracker.states by hand.", first)
		return nil
	}
	w.say("Workflow states of %s, as the tracker names them:", first)
	cfg.Tracker.States.Ready = states[w.choose("Which state means a ticket is ready for the agent?", states, index(states, cfg.Tracker.States.Ready, 0))]
	cfg.Tracker.States.InProgress = states[w.choose("Which state does a ticket move to while the agent works?", states, index(states, cfg.Tracker.States.InProgress, min(1, len(states)-1)))]
	review := append([]string{"(leave the ticket where it is)"}, states...)
	r := w.choose("Which state after the pull request is open?", review, index(states, cfg.Tracker.States.InReview, -1)+1)
	if r == 0 {
		cfg.Tracker.States.InReview = ""
	} else {
		cfg.Tracker.States.InReview = states[r-1]
	}

	interval := w.ask("Poll every N seconds", strconv.Itoa(orInt(cfg.Tracker.PollIntervalSec, 60)))
	if n, err := strconv.Atoi(interval); err == nil && n > 0 {
		cfg.Tracker.PollIntervalSec = n
	}
	return nil
}

func (w *Wizard) sandbox(ctx context.Context, cfg *config.Config) error {
	suggested := cfg.Sandbox.Image
	repos := cfg.Tracker.Repos()
	if suggested == "" && len(repos) > 0 && cfg.Creds.Git != "" {
		keys := make([]string, 0, len(repos))
		for k := range repos {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if probe, err := w.Probes.Forge(forge.Options{Token: cfg.Creds.Git, Kind: forge.Kind(cfg.Git.Forge), APIBase: cfg.Git.APIBase}); err == nil {
			if info, err := probe.Inspect(ctx, repos[keys[0]]); err == nil && len(info.Languages) > 0 {
				if img := imageFor(info.Languages[0]); img != "" {
					suggested = img
					w.say("%s is mostly %s.", repos[keys[0]], info.Languages[0])
				}
			}
		}
	}
	for {
		cfg.Sandbox.Image = w.ask("Container image tasks run in (the repository's toolchain)", suggested)
		if cfg.Sandbox.Image != "" || w.eof {
			return nil
		}
		w.say("An image is needed: the Runner ships none, because which one is a property of the repository.")
	}
}

func (w *Wizard) alerts(ctx context.Context, cfg *config.Config) error {
	if w.confirm("Send alerts to Telegram?", cfg.Alerts.Telegram.Token != "") {
		for {
			cfg.Alerts.Telegram.Token = w.secret("Bot token (from @BotFather)", cfg.Alerts.Telegram.Token)
			cfg.Alerts.Telegram.ChatID = w.ask("Chat id (message the bot, then see https://api.telegram.org/bot<token>/getUpdates)", cfg.Alerts.Telegram.ChatID)
			keep, err := w.check(ctx, "sending a test message", func() (string, error) {
				return "sent; check the chat", w.Probes.Alert(ctx, alert.Telegram{Token: cfg.Alerts.Telegram.Token, ChatID: cfg.Alerts.Telegram.ChatID})
			})
			if err != nil {
				return err
			}
			if keep {
				break
			}
		}
	} else {
		cfg.Alerts.Telegram = config.TelegramAlerts{}
	}

	if w.confirm("Send alerts to a webhook (Slack, Discord, n8n, your own script)?", cfg.Alerts.Webhook.URL != "") {
		for {
			cfg.Alerts.Webhook.URL = w.ask("Webhook url (https)", cfg.Alerts.Webhook.URL)
			cfg.Alerts.Webhook.Secret = w.secret("Shared secret for the HMAC signature (optional)", cfg.Alerts.Webhook.Secret)
			keep, err := w.check(ctx, "posting a test event", func() (string, error) {
				return "posted", w.Probes.Alert(ctx, alert.Webhook{URL: cfg.Alerts.Webhook.URL, Secret: cfg.Alerts.Webhook.Secret})
			})
			if err != nil {
				return err
			}
			if keep {
				break
			}
		}
	} else {
		cfg.Alerts.Webhook = config.WebhookAlerts{}
	}
	if cfg.Alerts.Enabled() && len(cfg.Alerts.Events) == 0 {
		cfg.Alerts.Events = nil // all kinds, which is the default
	}
	return nil
}

// ---- asking ----

func (w *Wizard) say(format string, a ...any) {
	fmt.Fprintf(w.Out, format+"\n", a...)
}

func (w *Wizard) readLine() (string, bool) {
	if w.eof {
		return "", false
	}
	line, err := w.in.ReadString('\n')
	if err != nil && line == "" {
		w.eof = true
		fmt.Fprintln(w.Out)
		return "", false
	}
	return strings.TrimSpace(line), true
}

// ask prompts for a value, with the default shown and taken on Enter.
func (w *Wizard) ask(label, def string) string {
	if def != "" {
		fmt.Fprintf(w.Out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(w.Out, "%s: ", label)
	}
	line, ok := w.readLine()
	if !ok || line == "" {
		return def
	}
	return line
}

// secret prompts for a credential. The current value is never shown; Enter
// keeps it.
func (w *Wizard) secret(label, current string) string {
	prompt := label + ": "
	if current != "" {
		prompt = label + " [Enter keeps the current one]: "
	}
	fmt.Fprint(w.Out, prompt)
	if w.HideEcho != nil {
		restore := w.HideEcho()
		defer fmt.Fprintln(w.Out)
		defer restore()
	}
	line, ok := w.readLine()
	if !ok || line == "" {
		return current
	}
	return line
}

func (w *Wizard) confirm(label string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Fprintf(w.Out, "%s [%s]: ", label, hint)
	line, ok := w.readLine()
	if !ok || line == "" {
		return def
	}
	switch strings.ToLower(line) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return def
}

// choose prints numbered options and returns the index picked.
func (w *Wizard) choose(label string, options []string, def int) int {
	w.say("%s", label)
	for i, opt := range options {
		w.say("  %d) %s", i+1, opt)
	}
	for {
		hint := ""
		if def >= 0 && def < len(options) {
			hint = fmt.Sprintf(" [%d]", def+1)
		}
		fmt.Fprintf(w.Out, "Choice%s: ", hint)
		line, ok := w.readLine()
		if !ok || line == "" {
			if def >= 0 && def < len(options) {
				return def
			}
			if !ok {
				return 0
			}
			continue
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(options) {
			return n - 1
		}
		w.say("Type a number from 1 to %d.", len(options))
	}
}

// pick is choose for several: "1,3" or "all".
func (w *Wizard) pick(label string, options []string, def []int) []int {
	w.say("%s (numbers separated by commas, or \"all\")", label)
	for i, opt := range options {
		w.say("  %d) %s", i+1, opt)
	}
	for {
		hint := ""
		if len(def) > 0 {
			parts := make([]string, len(def))
			for i, d := range def {
				parts[i] = strconv.Itoa(d + 1)
			}
			hint = " [" + strings.Join(parts, ",") + "]"
		}
		fmt.Fprintf(w.Out, "Choice%s: ", hint)
		line, ok := w.readLine()
		if !ok || line == "" {
			return def
		}
		if strings.EqualFold(line, "all") {
			all := make([]int, len(options))
			for i := range options {
				all[i] = i
			}
			return all
		}
		var out []int
		good := true
		for _, part := range strings.Split(line, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 1 || n > len(options) {
				good = false
				break
			}
			out = append(out, n-1)
		}
		if good && len(out) > 0 {
			return out
		}
		w.say("Type numbers from 1 to %d, separated by commas.", len(options))
	}
}

// check runs a live check and, when it fails, asks whether to retry the
// questions (false), keep the value unverified (true) or abort (error).
func (w *Wizard) check(ctx context.Context, what string, fn func() (string, error)) (keep bool, err error) {
	fmt.Fprintf(w.Out, "%s… ", what)
	msg, err := fn()
	if err == nil {
		w.say("ok: %s", msg)
		return true, nil
	}
	w.say("failed: %v", err)
	if w.eof {
		// Nothing is left to answer with, and the default is retry, which
		// would ask the same question forever.
		return false, fmt.Errorf("setup: %s failed and the input ended: %w", what, err)
	}
	switch w.choose("What now?", []string{"retry", "keep it anyway", "abort"}, 0) {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, errAbort
	}
}

// ---- helpers ----

// read loads the config without validating it: a half-written one is what
// this wizard exists to finish.
func read(path string) (config.Config, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config.Config{}, false, nil
	}
	if err != nil {
		return config.Config{}, false, fmt.Errorf("setup: %w", err)
	}
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return config.Config{}, false, fmt.Errorf("setup: %s is not valid JSON: %w", path, err)
	}
	return cfg, true, nil
}

// write saves the config, keeping the owner of the file it replaces: the
// installer makes it belong to the service account, and a root running the
// wizard must not take that away.
func write(path string, cfg config.Config) error {
	uid, gid := -1, -1
	if info, err := os.Stat(path); err == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	if uid >= 0 && os.Geteuid() == 0 {
		if err := os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
	}
	return nil
}

// serviceEnv is the environment of the account the Runner runs as, when the
// wizard runs as root on a machine the installer set up. The CLI's sign-in
// lives in that account's home, so that is where it has to be looked for.
func serviceEnv() (env []string, who string) {
	if os.Geteuid() != 0 {
		return nil, ""
	}
	u, err := user.Lookup(ServiceUser)
	if err != nil || u.HomeDir == "" {
		return nil, ""
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HOME=") {
			env = append(env, kv)
		}
	}
	env = append(env, "HOME="+u.HomeDir)
	return env, " as " + ServiceUser + " (HOME=" + u.HomeDir + ")"
}

// restartService restarts the installer's unit when there is one.
func restartService(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return "", err
	}
	if err := exec.CommandContext(ctx, "systemctl", "cat", ServiceName).Run(); err != nil {
		return "", err
	}
	if os.Geteuid() != 0 {
		return "", errors.New("not root")
	}
	if err := exec.CommandContext(ctx, "systemctl", "restart", ServiceName).Run(); err != nil {
		return "", err
	}
	return "Restarted " + ServiceName + "; follow it with: journalctl -u " + ServiceName + " -f", nil
}

// imageFor suggests a sandbox image for a language, as the forge names it.
func imageFor(language string) string {
	switch strings.ToLower(language) {
	case "go":
		return "golang:1.26"
	case "typescript", "javascript", "vue", "svelte":
		return "node:22"
	case "python":
		return "python:3.13"
	case "rust":
		return "rust:1"
	case "ruby":
		return "ruby:3.4"
	case "java", "kotlin":
		return "eclipse-temurin:21"
	case "php":
		return "php:8.4-cli"
	case "elixir":
		return "elixir:1.18"
	case "c#":
		return "mcr.microsoft.com/dotnet/sdk:9.0"
	}
	return ""
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "localhost"
	}
	return h
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func index(list []string, s string, def int) int {
	for i, item := range list {
		if item == s {
			return i
		}
	}
	return def
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func orFloat(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}

func orInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// HideEcho turns terminal echo off through stty, so the Runner takes on no
// dependency for one prompt, and returns the call that turns it back on. A
// non-terminal input is left alone.
func HideEcho() func() {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return func() {}
	}
	off := exec.Command("stty", "-echo")
	off.Stdin = os.Stdin
	if off.Run() != nil {
		return func() {}
	}
	return func() {
		on := exec.Command("stty", "echo")
		on.Stdin = os.Stdin
		_ = on.Run()
	}
}

// ConfigPathFor is where init writes when told nothing: the installer's
// file when it exists, else the Runner's default.
func ConfigPathFor() string {
	if p := os.Getenv("ROOST_CONFIG"); p != "" {
		return p
	}
	installer := filepath.Join("/etc", "roost", "config.json")
	if _, err := os.Stat(installer); err == nil {
		return installer
	}
	return config.DefaultPath()
}

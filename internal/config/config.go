// Package config loads the Runner's local configuration.
//
// Credentials live here and nowhere else. This file on the developer's VPS is
// the only place that holds the git, task-manager and LLM tokens; they leave it
// only as environment variables injected into a sandbox for the length of one
// task. Cloud is told which credentials exist, never what they are.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"

	"github.com/roostlabs/protocol"
)

const redacted = "[REDACTED]"

// FileMode is the only permission set the config file may carry.
//
// A wider mode is refused rather than quietly corrected: if another account on
// the box could already read the file, the tokens in it should be rotated, and
// silently chmodding would hide that.
const FileMode fs.FileMode = 0o600

// Config is the Runner's on-disk configuration.
type Config struct {
	// CloudURL is the channel endpoint the Runner dials, e.g.
	// wss://api.example.com/channel.
	CloudURL string `json:"cloudUrl"`
	// Token authenticates this Runner. It comes from the dashboard.
	Token string `json:"token"`
	// DataDir holds the event-store and the persistent repo clones.
	DataDir string `json:"dataDir"`
	// Creds are the credentials handed to sandboxes.
	Creds Creds `json:"creds"`
	// Sandbox configures how tasks are executed.
	Sandbox Sandbox `json:"sandbox"`
	// Agent configures the model that decides what to execute.
	Agent Agent `json:"agent"`
	// Git configures how the work is committed and published.
	Git Git `json:"git"`
}

// Git configures the commit and the pull request a finished task becomes.
type Git struct {
	// AuthorName and AuthorEmail attribute the Runner's commits. They belong
	// to a service account, not to the developer: a commit claiming to be
	// theirs would put their name on work they have not read yet.
	AuthorName  string `json:"authorName,omitempty"`
	AuthorEmail string `json:"authorEmail,omitempty"`
	// APIBase overrides the forge's API endpoint, for GitHub Enterprise.
	APIBase string `json:"apiBase,omitempty"`
}

// Agent configures the LLM-driven agent.
//
// It is used when creds.llm is set. Without that key the Runner falls back to
// running sandbox.commands, because an agent with no model is not an agent.
type Agent struct {
	// Model is the model id. Empty uses the package default.
	Model string `json:"model,omitempty"`
	// Effort is how hard the model thinks per turn: low, medium, high, xhigh
	// or max. Higher costs more per turn and usually needs fewer of them.
	Effort string `json:"effort,omitempty"`
	// MaxTokens caps one reply, thinking included.
	MaxTokens int `json:"maxTokens,omitempty"`
	// MaxSteps bounds the conversation. A task that has not finished by then
	// is looping, and a loop with a token meter attached is expensive.
	MaxSteps int `json:"maxSteps,omitempty"`
	// BudgetUSD caps what one task may spend when Cloud sends no budget of its
	// own. Zero leaves the task uncapped, which is a choice worth making
	// deliberately.
	BudgetUSD float64 `json:"budgetUsd,omitempty"`
	// BaseURL overrides the API endpoint, for a proxy or a test.
	BaseURL string `json:"baseUrl,omitempty"`
}

// Sandbox configures the containers tasks run in.
type Sandbox struct {
	// Image is the container image, e.g. golang:1.26. The Runner ships no image
	// of its own: which one a task needs is a property of the repository being
	// worked on, not of the Runner.
	Image string `json:"image"`
	// Commands are the steps the fixed agent runs, as argv lists rather than
	// shell lines. They stand in until an LLM-driven agent exists.
	Commands [][]string `json:"commands,omitempty"`

	CPUs      string `json:"cpus,omitempty"`
	MemoryMB  int    `json:"memoryMb,omitempty"`
	PidsLimit int    `json:"pidsLimit,omitempty"`

	// Network is "bridge" or "none".
	//
	// Bridge is the default because an agent has to reach the LLM API and the
	// git remote. It is also the route by which generated code could send the
	// repository somewhere it should not go; narrowing that to an allowlist is
	// open work, and "none" is available for tasks that need no network at all.
	Network string `json:"network,omitempty"`

	// WritableRoot is negated deliberately, so the zero value gives a container
	// whose filesystem is read-only apart from the working copy and /tmp.
	WritableRoot bool `json:"writableRoot,omitempty"`

	// TimeoutMs caps one command. Zero uses the sandbox default.
	TimeoutMs int64 `json:"timeoutMs,omitempty"`
	// KeepWorktree leaves each task's checkout on disk, for debugging.
	KeepWorktree bool `json:"keepWorktree,omitempty"`
}

// Creds holds the credentials a task may need. An empty field means the
// credential is not configured, which is reported upward as a false flag.
type Creds struct {
	Git         string `json:"git,omitempty"`
	TaskManager string `json:"taskManager,omitempty"`
	LLM         string `json:"llm,omitempty"`
}

// Status renders the credentials as the flags-only form that goes to Cloud.
func (c Creds) Status() protocol.CredStatus {
	return protocol.CredStatus{
		Git:         c.Git != "",
		TaskManager: c.TaskManager != "",
		LLM:         c.LLM != "",
	}
}

// Env renders the configured credentials as environment entries for a sandbox.
// Unset credentials are omitted entirely rather than exported empty, so a task
// fails loudly on a missing token instead of authenticating as nobody.
func (c Creds) Env() []string {
	var env []string
	if c.Git != "" {
		env = append(env, "ROOST_GIT_TOKEN="+c.Git)
	}
	if c.TaskManager != "" {
		env = append(env, "ROOST_TASKMANAGER_TOKEN="+c.TaskManager)
	}
	if c.LLM != "" {
		env = append(env, "ROOST_LLM_KEY="+c.LLM)
	}
	return env
}

// Values lists the credential strings that are set.
//
// This is what the redaction filter masks in command output before it is
// streamed to Cloud: a build that echoes its environment is the likeliest way
// for a token to escape, and the filter can only mask values it knows.
func (c Creds) Values() []string {
	var vals []string
	for _, v := range []string{c.Git, c.TaskManager, c.LLM} {
		if v != "" {
			vals = append(vals, v)
		}
	}
	return vals
}

// LogValue reports which credentials are set, never their values.
func (c Creds) LogValue() slog.Value {
	s := c.Status()
	return slog.GroupValue(
		slog.Bool("git", s.Git),
		slog.Bool("taskManager", s.TaskManager),
		slog.Bool("llm", s.LLM),
	)
}

// LogValue redacts the Runner token and reduces the credentials to flags.
func (c Config) LogValue() slog.Value {
	token := ""
	if c.Token != "" {
		token = redacted
	}
	return slog.GroupValue(
		slog.String("cloudUrl", c.CloudURL),
		slog.String("token", token),
		slog.String("dataDir", c.DataDir),
		slog.Any("creds", c.Creds),
	)
}

// Load reads and validates the config at path.
//
// Unknown fields are ignored, so a config written by a newer Runner still loads
// in an older one.
func Load(path string) (Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return Config{}, fmt.Errorf(
			"config: %s is mode %04o, want %04o: credentials must not be readable by other accounts",
			path, perm, FileMode.Perm())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir()
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Save writes the config atomically at FileMode.
//
// The explicit Chmod matters: WriteFile's mode is filtered through the process
// umask, so the file can land wider than asked for.
func Save(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	raw = append(raw, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, FileMode); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := os.Chmod(tmp, FileMode); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// Validate checks the config is usable before the Runner acts on it.
func (c Config) Validate() error {
	if c.Token == "" {
		return errors.New("config: token is empty; generate one in the dashboard")
	}
	if c.CloudURL == "" {
		return errors.New("config: cloudUrl is empty")
	}
	u, err := url.Parse(c.CloudURL)
	if err != nil {
		return fmt.Errorf("config: cloudUrl %q: %w", c.CloudURL, err)
	}
	switch u.Scheme {
	case "wss":
	case "ws":
		// Plain ws is allowed against a local stub Cloud during development,
		// and nowhere else: the handshake carries the Runner token.
		if !isLoopback(u.Hostname()) {
			return fmt.Errorf(
				"config: cloudUrl %q uses ws:// to a remote host; the token would cross the network in clear text",
				c.CloudURL)
		}
	default:
		return fmt.Errorf("config: cloudUrl %q has scheme %q, want wss", c.CloudURL, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("config: cloudUrl %q has no host", c.CloudURL)
	}
	if c.DataDir == "" {
		return errors.New("config: dataDir is empty")
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DefaultPath is where the Runner looks for its config. ROOST_CONFIG overrides
// it, which is what the systemd unit uses.
func DefaultPath() string {
	if p := os.Getenv("ROOST_CONFIG"); p != "" {
		return p
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "roost", "config.json")
	}
	return "roost.json"
}

// DefaultDataDir is where the event-store and repo clones live when the config
// does not say.
func DefaultDataDir() string {
	if p := os.Getenv("ROOST_DATA_DIR"); p != "" {
		return p
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "roost")
	}
	return "roost-data"
}

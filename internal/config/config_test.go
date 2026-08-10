package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		CloudURL: "wss://api.example.com/channel",
		Token:    "rt_runnertoken",
		DataDir:  "/var/lib/roost",
		Creds: Creds{
			Git: "ghp_gittoken",
			LLM: "sk_llmkey",
		},
	}
}

func TestSaveThenLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	want := validConfig()
	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != FileMode.Perm() {
		t.Errorf("saved mode = %04o, want %04o", got, FileMode.Perm())
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// A config another account can read has already leaked; refusing is the point,
// so this must not be silently repaired.
func TestLoadRefusesReadableByOthers(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			raw, err := json.Marshal(validConfig())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := os.WriteFile(path, raw, mode); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatalf("Chmod: %v", err)
			}

			if _, err := Load(path); err == nil {
				t.Errorf("Load accepted mode %04o, want refusal", mode.Perm())
			}
		})
	}
}

func TestLoadIgnoresUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{"cloudUrl":"wss://api.example.com/channel","token":"t",
		"dataDir":"/tmp/roost","sandboxFlavour":"exotic"}`)
	if err := os.WriteFile(path, raw, FileMode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, FileMode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CloudURL != "wss://api.example.com/channel" {
		t.Errorf("cloudUrl = %q", got.CloudURL)
	}
}

func TestLoadFillsDefaultDataDir(t *testing.T) {
	t.Setenv("ROOST_DATA_DIR", "/tmp/roost-default")

	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{"cloudUrl":"wss://api.example.com/channel","token":"t"}`)
	if err := os.WriteFile(path, raw, FileMode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, FileMode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DataDir != "/tmp/roost-default" {
		t.Errorf("dataDir = %q, want the default", got.DataDir)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid", func(*Config) {}, false},
		{"ws to localhost is allowed for a local stub", func(c *Config) {
			c.CloudURL = "ws://localhost:8080/channel"
		}, false},
		{"ws to loopback ip is allowed", func(c *Config) {
			c.CloudURL = "ws://127.0.0.1:8080/channel"
		}, false},
		{"ws to a remote host is refused", func(c *Config) {
			c.CloudURL = "ws://api.example.com/channel"
		}, true},
		{"https is not a channel scheme", func(c *Config) {
			c.CloudURL = "https://api.example.com/channel"
		}, true},
		{"no scheme", func(c *Config) { c.CloudURL = "api.example.com/channel" }, true},
		{"no host", func(c *Config) { c.CloudURL = "wss:///channel" }, true},
		{"empty url", func(c *Config) { c.CloudURL = "" }, true},
		{"empty token", func(c *Config) { c.Token = "" }, true},
		{"empty data dir", func(c *Config) { c.DataDir = "" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(&c)
			if err := c.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCredsStatus(t *testing.T) {
	got := Creds{Git: "g", LLM: "l"}.Status()
	if !got.Git || !got.LLM {
		t.Errorf("set credentials reported false: %+v", got)
	}
	if got.TaskManager {
		t.Error("unset taskManager reported true")
	}
}

// An unset credential must be absent, not exported empty, so a task fails on a
// missing token instead of authenticating as nobody.
func TestCredsEnvOmitsUnset(t *testing.T) {
	env := Creds{Git: "ghp_x"}.Env()
	if len(env) != 1 || env[0] != "ROOST_GIT_TOKEN=ghp_x" {
		t.Fatalf("Env() = %v, want only the git token", env)
	}
	for _, e := range env {
		if strings.HasSuffix(e, "=") {
			t.Errorf("Env() exported an empty value: %q", e)
		}
	}
}

func TestCredsValues(t *testing.T) {
	got := Creds{Git: "a", TaskManager: "", LLM: "c"}.Values()
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("Values() = %v, want [a c]", got)
	}
}

// The config holds every secret on the box, so no logging path may print one.
func TestLogValueRedactsSecrets(t *testing.T) {
	cfg := validConfig()

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("loaded", "config", cfg)
	out := buf.String()

	for _, secret := range []string{cfg.Token, cfg.Creds.Git, cfg.Creds.LLM} {
		if strings.Contains(out, secret) {
			t.Errorf("log leaked %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, cfg.CloudURL) {
		t.Errorf("log dropped the non-secret cloudUrl: %s", out)
	}
}

func TestDefaultPathHonoursEnv(t *testing.T) {
	t.Setenv("ROOST_CONFIG", "/etc/roost/runner.json")
	if got := DefaultPath(); got != "/etc/roost/runner.json" {
		t.Errorf("DefaultPath() = %q, want the env override", got)
	}
}

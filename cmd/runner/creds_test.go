package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/roostlabs/protocol"
	"github.com/roostlabs/runner/internal/config"
	"github.com/roostlabs/runner/internal/repo"
)

const pushed = "ghp_pushedfromthedashboard"

// newService builds a service around a real config file, which is what setCred
// writes to and what the test then reads back.
func newService(t *testing.T, mode protocol.CredMode) (*service, *bytes.Buffer) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := config.Config{
		CloudURL: "wss://cloud.example.com/channel",
		Token:    "rt_runnertoken",
		DataDir:  dir,
		Creds:    config.Creds{Mode: mode},
		Sandbox:  config.Sandbox{Image: "golang:1.26"},
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var logs bytes.Buffer
	creds, err := buildCreds(cfg)
	if err != nil {
		t.Fatalf("buildCreds: %v", err)
	}

	svc := &service{
		configPath: path,
		repos:      repo.New(filepath.Join(dir, "repos"), cfg.Creds.Git),
		log:        slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		running:    func() (string, bool) { return "", false },
	}
	svc.cfg.Store(&cfg)
	svc.creds.Store(&creds)
	return svc, &logs
}

func onDisk(t *testing.T, svc *service) config.Config {
	t.Helper()

	raw, err := os.ReadFile(svc.configPath)
	if err != nil {
		t.Fatalf("read the config: %v", err)
	}
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse the config: %v", err)
	}
	return cfg
}

// Local is the default, and in it the values are the developer's to set on their
// own machine. A credential arriving from Cloud is refused, not written.
func TestLocalModeRefusesAPushedCredential(t *testing.T) {
	svc, logs := newService(t, protocol.CredModeLocal)

	err := svc.setCred(protocol.CredGit, pushed)
	if err == nil {
		t.Fatal("local mode accepted a credential from cloud")
	}
	if !strings.Contains(err.Error(), "managed") {
		t.Errorf("the error does not say why: %v", err)
	}
	if got := onDisk(t, svc).Creds.Git; got != "" {
		t.Error("the refused credential was written to the config anyway")
	}
	if strings.Contains(logs.String(), pushed) {
		t.Errorf("the value reached the log: %s", logs.String())
	}
}

// An unset mode is an older config that predates the field, and it has to mean
// Local: the safe direction to fail is the one that writes nothing.
func TestAnUnsetModeMeansLocal(t *testing.T) {
	svc, _ := newService(t, "")

	if svc.config().Creds.Managed() {
		t.Fatal("a config with no mode was treated as managed")
	}
	if err := svc.setCred(protocol.CredGit, pushed); err == nil {
		t.Error("a config with no mode accepted a credential from cloud")
	}
}

func TestManagedModeStoresAndAppliesTheCredential(t *testing.T) {
	svc, logs := newService(t, protocol.CredModeManaged)

	// Without a git credential there is nothing to open a pull request with.
	if svc.currentCreds().Forge != nil {
		t.Fatal("a forge existed before any credential did")
	}

	if err := svc.setCred(protocol.CredGit, pushed); err != nil {
		t.Fatalf("setCred: %v", err)
	}

	if got := onDisk(t, svc).Creds.Git; got != pushed {
		t.Errorf("the config holds %q", got)
	}
	info, err := os.Stat(svc.configPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != config.FileMode.Perm() {
		t.Errorf("config is mode %04o, want %04o", perm, config.FileMode.Perm())
	}

	// The point of applying it: the pieces a task runs with now have it.
	creds := svc.currentCreds()
	if creds.Forge == nil {
		t.Error("the credential was stored but no forge was built from it")
	}
	if masked := creds.Filter.String("token " + pushed); strings.Contains(masked, pushed) {
		t.Errorf("the new credential is not masked in output: %s", masked)
	}
	if env := strings.Join(creds.Env, " "); !strings.Contains(env, "ROOST_GIT_TOKEN="+pushed) {
		t.Error("the new credential is not exposed to the sandbox")
	}
	// That the repo manager now authenticates with it is repo's own test; the
	// manager deliberately exposes no way to read a credential back out.

	if status := svc.config().Creds.Status(); !status.Git || status.Mode != protocol.CredModeManaged {
		t.Errorf("status = %+v", status)
	}
	if strings.Contains(logs.String(), pushed) {
		t.Errorf("the value reached the log: %s", logs.String())
	}
}

// An LLM key is the switch between an agent that thinks and one that runs fixed
// commands, so setting it has to change what the next task runs.
func TestSettingTheModelKeyTurnsOnTheAgent(t *testing.T) {
	svc, _ := newService(t, protocol.CredModeManaged)

	if svc.currentCreds().LLM != nil {
		t.Fatal("a model client existed before any key did")
	}
	if err := svc.setCred(protocol.CredLLM, "sk-ant-pushedkey"); err != nil {
		t.Fatalf("setCred: %v", err)
	}
	if svc.currentCreds().LLM == nil {
		t.Error("the key was stored but no model client was built from it")
	}
}

// A revoked token has to be retirable from the dashboard, or Managed mode would
// be a one-way door.
func TestAnEmptyValueClearsTheSlot(t *testing.T) {
	svc, _ := newService(t, protocol.CredModeManaged)

	if err := svc.setCred(protocol.CredGit, pushed); err != nil {
		t.Fatalf("setCred: %v", err)
	}
	if err := svc.setCred(protocol.CredGit, ""); err != nil {
		t.Fatalf("clearing setCred: %v", err)
	}

	if got := onDisk(t, svc).Creds.Git; got != "" {
		t.Errorf("the config still holds %q", got)
	}
	if svc.currentCreds().Forge != nil {
		t.Error("a cleared credential left a forge behind")
	}
	if svc.config().Creds.Status().Git {
		t.Error("a cleared credential still reports as configured")
	}
}

// A task that started with one identity must not find itself pushing with
// another, so the change waits rather than landing halfway through.
func TestARunningTaskBlocksACredentialChange(t *testing.T) {
	svc, _ := newService(t, protocol.CredModeManaged)
	svc.running = func() (string, bool) { return "t-1", true }

	err := svc.setCred(protocol.CredGit, pushed)
	if err == nil {
		t.Fatal("a credential was swapped under a running task")
	}
	if !strings.Contains(err.Error(), "t-1") {
		t.Errorf("the error does not name the task: %v", err)
	}
	if got := onDisk(t, svc).Creds.Git; got != "" {
		t.Error("the refused credential was written to the config anyway")
	}
}

func TestAnUnknownKeyIsRefused(t *testing.T) {
	svc, _ := newService(t, protocol.CredModeManaged)

	if err := svc.setCred(protocol.CredKey("aws"), pushed); err == nil {
		t.Fatal("setCred accepted a slot that does not exist")
	}
	if raw, err := os.ReadFile(svc.configPath); err == nil && bytes.Contains(raw, []byte(pushed)) {
		t.Error("the value was written under an unknown key")
	}
}

// A value the Runner cannot build a working set out of is refused before
// anything is written, so a typo in the dashboard cannot leave the config
// holding a credential no task can use.
func TestABadConfigurationIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	svc, _ := newService(t, protocol.CredModeManaged)

	cfg := svc.config()
	cfg.Git.Forge = "bitbucket"
	svc.cfg.Store(&cfg)

	if err := svc.setCred(protocol.CredGit, pushed); err == nil {
		t.Fatal("setCred accepted a credential for a forge it cannot talk to")
	}
	if got := onDisk(t, svc).Creds.Git; got != "" {
		t.Error("the credential was written despite the failure")
	}
	if svc.currentCreds().Forge != nil {
		t.Error("a forge was built from a failed update")
	}
}

// Two updates arriving together must not each build on the same old config, or
// the second would silently undo the first.
func TestConcurrentUpdatesDoNotLoseEachOther(t *testing.T) {
	svc, _ := newService(t, protocol.CredModeManaged)

	var wg sync.WaitGroup
	for key, value := range map[protocol.CredKey]string{
		protocol.CredGit:         pushed,
		protocol.CredTaskManager: "lin_pushedkey",
		protocol.CredLLM:         "sk-ant-pushedkey",
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.setCred(key, value); err != nil {
				t.Errorf("setCred(%s): %v", key, err)
			}
		}()
	}
	wg.Wait()

	got := onDisk(t, svc).Creds
	if got.Git != pushed || got.TaskManager != "lin_pushedkey" || got.LLM != "sk-ant-pushedkey" {
		t.Errorf("an update was lost: %+v", got.Status())
	}
}

package sandbox

import (
	"bytes"
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func testSpec() Spec {
	return Spec{
		Image:        "golang:1.26",
		HostPath:     "/var/lib/roost/repos/app/wt-T-1",
		WorkDir:      "/work",
		ReadOnlyRoot: true,
		User:         "1000:1000",
	}
}

// The one that matters: a token must never reach the command line, where any
// user on the host could read it out of ps.
func TestArgsNeverCarryCredentialValues(t *testing.T) {
	spec := testSpec()
	spec.Env = []string{
		"ROOST_GIT_TOKEN=ghp_realtokenvalue",
		"ROOST_LLM_KEY=sk_realkeyvalue",
	}

	args := spec.Args([]string{"go", "test", "./..."})
	joined := strings.Join(args, " ")

	for _, secret := range []string{"ghp_realtokenvalue", "sk_realkeyvalue"} {
		if strings.Contains(joined, secret) {
			t.Errorf("command line leaked %q: %s", secret, joined)
		}
	}
	// The names still have to be there, or the container gets nothing.
	for _, name := range []string{"ROOST_GIT_TOKEN", "ROOST_LLM_KEY"} {
		if !slices.Contains(args, name) {
			t.Errorf("args dropped %s entirely: %v", name, args)
		}
	}
}

func TestArgsAppliesLimits(t *testing.T) {
	spec := testSpec()
	spec.CPUs = "2"
	spec.MemoryMB = 512
	spec.PidsLimit = 64

	args := spec.Args([]string{"true"})
	for _, want := range []string{
		"--cpus=2",
		"--memory=512m",
		"--memory-swap=512m", // swap disabled, or the memory cap is advisory
		"--pids-limit=64",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--read-only",
		"--rm",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("args missing %s: %v", want, args)
		}
	}
}

func TestArgsDefaultsSizedForASmallBox(t *testing.T) {
	args := Spec{Image: "alpine", HostPath: "/tmp/wt"}.Args([]string{"true"})
	for _, want := range []string{
		"--cpus=" + DefaultCPUs,
		"--memory=2048m",
		"--network=" + NetworkBridge,
	} {
		if !slices.Contains(args, want) {
			t.Errorf("args missing default %s: %v", want, args)
		}
	}
}

// The working copy is the only writable mount, and commands must start in it.
func TestArgsMountsWorkingCopy(t *testing.T) {
	args := testSpec().Args([]string{"go", "build", "./..."})

	mountAt := slices.Index(args, "-v")
	if mountAt < 0 || mountAt+1 >= len(args) {
		t.Fatalf("args carry no mount: %v", args)
	}
	if want := "/var/lib/roost/repos/app/wt-T-1:/work"; args[mountAt+1] != want {
		t.Errorf("mount = %q, want %q", args[mountAt+1], want)
	}

	workAt := slices.Index(args, "-w")
	if workAt < 0 || args[workAt+1] != "/work" {
		t.Errorf("working directory not set to the mount: %v", args)
	}
}

// The image and the command have to come last, after every flag, or docker
// reads the command's own flags as its own.
func TestArgsPutImageAndCommandLast(t *testing.T) {
	args := testSpec().Args([]string{"go", "test", "-race", "./..."})

	imageAt := slices.Index(args, "golang:1.26")
	if imageAt < 0 {
		t.Fatalf("image missing: %v", args)
	}
	if got, want := args[imageAt+1:], []string{"go", "test", "-race", "./..."}; !slices.Equal(got, want) {
		t.Errorf("command after image = %v, want %v", got, want)
	}
}

func TestArgsOmitsTmpfsWhenRootIsWritable(t *testing.T) {
	spec := testSpec()
	spec.ReadOnlyRoot = false

	joined := strings.Join(spec.Args([]string{"true"}), " ")
	if strings.Contains(joined, "--read-only") || strings.Contains(joined, "--tmpfs") {
		t.Errorf("args still restrict the root filesystem: %s", joined)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Spec)
		wantErr bool
	}{
		{"valid", func(*Spec) {}, false},
		{"no image", func(s *Spec) { s.Image = "" }, true},
		{"no working copy", func(s *Spec) { s.HostPath = "" }, true},
		{"network none", func(s *Spec) { s.Network = NetworkNone }, false},
		{"unknown network", func(s *Spec) { s.Network = "host" }, true},
		// A colon in the path is read as a mount separator, which silently
		// changes what is mounted where.
		{"path with a separator", func(s *Spec) { s.HostPath = "/tmp/we:ird" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := testSpec()
			tt.mutate(&spec)
			if err := spec.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRunRejectsEmptyCommand(t *testing.T) {
	if _, err := Run(context.Background(), testSpec(), nil, nil, nil); err == nil {
		t.Error("Run accepted an empty command, want an error")
	}
}

func TestRunRejectsInvalidSpec(t *testing.T) {
	spec := testSpec()
	spec.Image = ""
	if _, err := Run(context.Background(), spec, []string{"true"}, nil, nil); err == nil {
		t.Error("Run accepted a spec with no image, want an error")
	}
}

// dockerAvailable reports whether a daemon is actually reachable. Everything
// below needs one and is skipped without it, so the suite still runs on a
// machine that only builds the Runner.
func dockerAvailable(t *testing.T) bool {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return ServerVersion(ctx) != ""
}

func TestRunStreamsOutput(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("no docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	spec := Spec{
		Image:        "alpine:3",
		HostPath:     t.TempDir(),
		Network:      NetworkNone,
		ReadOnlyRoot: true,
		User:         CurrentUser(),
	}

	var stdout, stderr bytes.Buffer
	res, err := Run(ctx, spec, []string{"echo", "hello from the sandbox"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run: %v (stderr: %s)", err, stderr.String())
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0 (stderr: %s)", res.ExitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "hello from the sandbox") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// A failing command is a result to report, not an error to abort on.
func TestRunReportsNonZeroExit(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("no docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	spec := Spec{Image: "alpine:3", HostPath: t.TempDir(), Network: NetworkNone}

	res, err := Run(ctx, spec, []string{"sh", "-c", "exit 3"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Run returned an error for a non-zero exit: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
}

func TestRunEnforcesTimeout(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("no docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	spec := Spec{
		Image:    "alpine:3",
		HostPath: t.TempDir(),
		Network:  NetworkNone,
		Timeout:  2 * time.Second,
	}

	res, err := Run(ctx, spec, []string{"sleep", "60"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("Run returned nil for a command that outran its timeout")
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true (err: %v)", err)
	}
}

// The container must receive the value without it ever passing through argv.
func TestRunPassesEnvWithoutExposingIt(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("no docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	spec := Spec{
		Image:    "alpine:3",
		HostPath: t.TempDir(),
		Network:  NetworkNone,
		Env:      []string{"ROOST_GIT_TOKEN=ghp_visible_inside_only"},
	}

	var stdout bytes.Buffer
	res, err := Run(ctx, spec, []string{"sh", "-c", "echo $ROOST_GIT_TOKEN"}, &stdout, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(stdout.String(), "ghp_visible_inside_only") {
		t.Errorf("the container did not receive the value: %q", stdout.String())
	}
	if strings.Contains(strings.Join(spec.Args([]string{"sh"}), " "), "ghp_visible_inside_only") {
		t.Error("the value reached the command line")
	}
}

func TestRunReadOnlyRootBlocksWrites(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("no docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	spec := Spec{
		Image:        "alpine:3",
		HostPath:     t.TempDir(),
		Network:      NetworkNone,
		ReadOnlyRoot: true,
	}

	// Outside the working copy and /tmp, nothing should be writable.
	res, err := Run(ctx, spec, []string{"sh", "-c", "touch /should-not-work"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("wrote to the container root filesystem, want it read-only")
	}

	// The working copy is the one place that must be writable.
	res, err = Run(ctx, spec, []string{"sh", "-c", "touch /work/file"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("could not write to the working copy, exit code %d", res.ExitCode)
	}
}

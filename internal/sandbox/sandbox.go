// Package sandbox runs commands inside disposable Docker containers.
//
// The agent writes code and then wants to execute it. That code runs here and
// nowhere else: never on the host, never with the host's capabilities, and never
// with more of the machine than it was granted. A container per task, removed
// when the task ends.
//
// Docker is driven through its CLI rather than its Go SDK. The SDK would add
// dozens of dependencies to a binary that currently has two, and Docker is
// already a stated prerequisite, so the CLI costs nothing extra to require.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Defaults sized for the target machine: one core and two gigabytes.
const (
	DefaultCPUs      = "1"
	DefaultMemoryMB  = 2048
	DefaultPidsLimit = 512
	DefaultTimeout   = 30 * time.Minute
	// DefaultTmpfsMB bounds the writable scratch space a read-only container
	// gets at /tmp. It counts against the container's memory limit.
	DefaultTmpfsMB = 256
)

// Network options. Bridge is the default because an agent needs to reach the
// LLM API and the git remote; none is available for commands that do not.
const (
	NetworkBridge = "bridge"
	NetworkNone   = "none"
)

// Spec describes one container.
type Spec struct {
	// Image is the container image, e.g. golang:1.26. It comes from the repo's
	// configuration: the Runner ships no image of its own.
	Image string
	// HostPath is the prepared working copy on the VPS, mounted read-write.
	// It is the only writable mount the container gets.
	HostPath string
	// WorkDir is where HostPath appears inside the container, and the working
	// directory commands run in.
	WorkDir string
	// Env are KEY=VALUE pairs to expose. The values are passed through the
	// Runner's own environment rather than the command line — see Args.
	Env []string
	// User is the uid:gid to run as. Set it to the Runner's own so files the
	// container writes into the working copy stay usable on the host; empty
	// means root, which leaves root-owned files behind in a persistent repo.
	User string

	CPUs      string
	MemoryMB  int
	PidsLimit int
	Network   string
	// ReadOnlyRoot mounts the container filesystem read-only, leaving the
	// working copy and a tmpfs at /tmp as the only writable places.
	ReadOnlyRoot bool
	TmpfsMB      int
	Timeout      time.Duration
}

func (s *Spec) applyDefaults() {
	if s.CPUs == "" {
		s.CPUs = DefaultCPUs
	}
	if s.MemoryMB <= 0 {
		s.MemoryMB = DefaultMemoryMB
	}
	if s.PidsLimit <= 0 {
		s.PidsLimit = DefaultPidsLimit
	}
	if s.Network == "" {
		s.Network = NetworkBridge
	}
	if s.TmpfsMB <= 0 {
		s.TmpfsMB = DefaultTmpfsMB
	}
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	if s.WorkDir == "" {
		s.WorkDir = "/work"
	}
}

// Validate reports whether the spec can be run.
func (s Spec) Validate() error {
	if s.Image == "" {
		return errors.New("sandbox: no image")
	}
	if s.HostPath == "" {
		return errors.New("sandbox: no working copy to mount")
	}
	if strings.ContainsAny(s.HostPath, ":,") {
		// A colon would be read as a mount separator, so a path containing one
		// silently changes what gets mounted where.
		return fmt.Errorf("sandbox: working copy path %q contains a character docker reads as a separator", s.HostPath)
	}
	switch s.Network {
	case "", NetworkBridge, NetworkNone:
	default:
		return fmt.Errorf("sandbox: unsupported network %q", s.Network)
	}
	return nil
}

// Args builds the docker command line for argv.
//
// Credential values never appear here. Docker's -e accepts a bare NAME, meaning
// "take it from my environment", and Run puts the values there instead. Passing
// -e NAME=VALUE would publish every token in the container's command line, where
// any user on the host can read it out of ps.
func (s Spec) Args(argv []string) []string {
	s.applyDefaults()

	args := []string{
		"run", "--rm",
		// The agent's code gets no capabilities and no way to acquire any.
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--cpus=" + s.CPUs,
		"--memory=" + strconv.Itoa(s.MemoryMB) + "m",
		// Equal to --memory, which disables swap: without it a container can
		// exceed its memory budget by paging, and on a 2GB box that takes the
		// whole machine down with it.
		"--memory-swap=" + strconv.Itoa(s.MemoryMB) + "m",
		"--pids-limit=" + strconv.Itoa(s.PidsLimit),
		"--network=" + s.Network,
	}
	if s.ReadOnlyRoot {
		args = append(args,
			"--read-only",
			fmt.Sprintf("--tmpfs=/tmp:rw,exec,nosuid,size=%dm", s.TmpfsMB))
	}
	if s.User != "" {
		args = append(args, "--user="+s.User)
	}
	for _, kv := range s.Env {
		if name, _, ok := strings.Cut(kv, "="); ok && name != "" {
			args = append(args, "-e", name)
		}
	}
	args = append(args,
		"-v", s.HostPath+":"+s.WorkDir,
		"-w", s.WorkDir,
		s.Image,
	)
	return append(args, argv...)
}

// Result is how a command finished.
type Result struct {
	ExitCode int
	Duration time.Duration
	// TimedOut reports that the command was killed by Spec.Timeout rather than
	// exiting on its own.
	TimedOut bool
}

// Run executes argv in a fresh container and streams its output.
//
// stdout and stderr should be redaction writers: this is where a build that
// echoes its environment would otherwise put a token on the wire.
//
// A command that exits non-zero is a Result, not an error. An error means the
// container could not be run at all.
func Run(ctx context.Context, spec Spec, argv []string, stdout, stderr io.Writer) (Result, error) {
	if len(argv) == 0 {
		return Result{}, errors.New("sandbox: no command")
	}
	if err := spec.Validate(); err != nil {
		return Result{}, err
	}
	spec.applyDefaults()

	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", spec.Args(argv)...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// The credential values live here rather than in the command line, and
	// docker copies them into the container for the -e NAMEs Args listed.
	cmd.Env = append(os.Environ(), spec.Env...)

	start := time.Now()
	err := cmd.Run()
	result := Result{Duration: time.Since(start)}

	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		result.TimedOut = true
		result.ExitCode = -1
		return result, fmt.Errorf("sandbox: %s timed out after %s", argv[0], spec.Timeout)
	}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		result.ExitCode = 0
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
	default:
		result.ExitCode = -1
		return result, fmt.Errorf("sandbox: run %s: %w", argv[0], err)
	}
	return result, nil
}

// ServerVersion reports the Docker daemon version, or an empty string when no
// daemon answers.
func ServerVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// CurrentUser is the uid:gid the Runner runs as, for Spec.User.
func CurrentUser() string {
	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
}

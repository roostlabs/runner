package executor

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/roostlabs/runner/internal/agent"
	"github.com/roostlabs/runner/internal/redact"
	"github.com/roostlabs/runner/internal/sandbox"
)

func credsWith(secret string) Creds {
	return Creds{
		Agent:  agent.FixedFromArgv([][]string{{"printenv"}}),
		Forge:  noForge,
		Filter: redact.New(secret),
		Env:    []string{"ROOST_GIT_TOKEN=" + secret},
	}
}

// A credential swapped while a task is running must not reach that task: it
// would push as one identity having authenticated as another, and the filter
// masking its output would be the set from before the swap.
//
// The next task is a different matter — it is exactly what a change is for.
func TestCredentialsAreResolvedWhenATaskStarts(t *testing.T) {
	var live atomic.Pointer[Creds]
	before := credsWith("secret-before")
	live.Store(&before)

	envs := make(chan []string, 2)
	f := newFixture(t, Config{
		Creds: func() Creds { return *live.Load() },
		Run: func(_ context.Context, spec sandbox.Spec, _ []string, stdout, _ io.Writer) (sandbox.Result, error) {
			envs <- spec.Env

			// The dashboard pushing a new credential, mid-task.
			after := credsWith("secret-after")
			live.Store(&after)

			stdout.Write([]byte("ROOST_GIT_TOKEN=secret-before\n"))
			return sandbox.Result{ExitCode: 0}, nil
		},
	})

	rec := &recorder{}
	if err := f.ex.Run(context.Background(), f.task, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if env := strings.Join(<-envs, " "); !strings.Contains(env, "secret-before") {
		t.Errorf("the task ran with %q, want the credential it started with", env)
	}
	// The masking is the visible half of the same guarantee: the filter the task
	// began with is the one still cleaning its output at the end.
	if out := rec.output(); strings.Contains(out, "secret-before") {
		t.Errorf("the running task's output leaked its credential: %s", out)
	}

	second := f.task
	second.TaskID = "T-2"
	if err := f.ex.Run(context.Background(), second, &recorder{}); err != nil {
		t.Fatalf("Run the second task: %v", err)
	}
	if env := strings.Join(<-envs, " "); !strings.Contains(env, "secret-after") {
		t.Errorf("the next task ran with %q, want the replacement", env)
	}
}

// The static fields are what a Local-mode Runner and every other test use, and
// they have to keep meaning the same thing.
func TestTheStaticFieldsStillWork(t *testing.T) {
	f := newFixture(t, Config{
		Filter: redact.New("static-secret"),
		Env:    []string{"ROOST_GIT_TOKEN=static-secret"},
		Run: func(_ context.Context, spec sandbox.Spec, _ []string, stdout, _ io.Writer) (sandbox.Result, error) {
			if env := strings.Join(spec.Env, " "); !strings.Contains(env, "static-secret") {
				t.Errorf("the task ran with %q", env)
			}
			stdout.Write([]byte("token static-secret\n"))
			return sandbox.Result{ExitCode: 0}, nil
		},
	})

	rec := &recorder{}
	if err := f.ex.Run(context.Background(), f.task, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out := rec.output(); strings.Contains(out, "static-secret") {
		t.Errorf("output leaked the credential: %s", out)
	}
}

// An agent is a credential-derived part too: a Runner with no model key runs
// fixed commands, and a set with no agent at all is a misconfiguration rather
// than a task that does nothing.
func TestATaskWithoutAnAgentFails(t *testing.T) {
	f := newFixture(t, Config{
		Creds: func() Creds { return Creds{} },
	})

	err := f.ex.Run(context.Background(), f.task, &recorder{})
	if err == nil || !strings.Contains(err.Error(), "agent") {
		t.Errorf("Run error = %v, want it to name the missing agent", err)
	}
}

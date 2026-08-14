package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// origin builds a small repository on disk to clone from, so these tests need
// git but no network.
func origin(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}

	run("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("origin\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")
	return dir
}

func TestPrepareClonesOnceThenFetches(t *testing.T) {
	ctx := context.Background()
	m := New(t.TempDir(), "")
	url := origin(t)

	first, err := m.Prepare(ctx, url, "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(first.Dir, ".git")); err != nil {
		t.Fatalf("no clone on disk: %v", err)
	}

	// Mark the clone so a second Prepare re-cloning would be visible.
	marker := filepath.Join(first.Dir, ".roost-marker")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	second, err := m.Prepare(ctx, url, "")
	if err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	if second.Dir != first.Dir {
		t.Errorf("second Prepare used %s, want the existing %s", second.Dir, first.Dir)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("the repository was re-cloned; the persistent clone is the whole point")
	}
}

func TestPrepareRejectsEmptyURL(t *testing.T) {
	if _, err := New(t.TempDir(), "").Prepare(context.Background(), "", ""); err == nil {
		t.Error("Prepare accepted an empty url")
	}
}

func TestWorktreeStartsFromTheRemoteHead(t *testing.T) {
	ctx := context.Background()
	m := New(t.TempDir(), "")

	r, err := m.Prepare(ctx, origin(t), "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	wt, err := r.Worktree(ctx, "T-1")
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	if _, err := os.Stat(filepath.Join(wt.Path, "README.md")); err != nil {
		t.Errorf("worktree is missing the repository content: %v", err)
	}
	if want := BranchPrefix + "T-1"; wt.Branch != want {
		t.Errorf("branch = %q, want %q", wt.Branch, want)
	}
	// The worktree must be somewhere else, or the clone is what gets mounted.
	if strings.HasPrefix(wt.Path, r.Dir+string(filepath.Separator)) {
		t.Errorf("worktree %s sits inside the clone %s", wt.Path, r.Dir)
	}
}

// The hazard a persistent clone creates: one task inheriting what another left
// behind. A retried task must start clean.
func TestWorktreeDiscardsAPreviousAttempt(t *testing.T) {
	ctx := context.Background()
	m := New(t.TempDir(), "")

	r, err := m.Prepare(ctx, origin(t), "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	first, err := r.Worktree(ctx, "T-1")
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	junk := filepath.Join(first.Path, "left-behind.txt")
	if err := os.WriteFile(junk, []byte("from the last attempt"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	second, err := r.Worktree(ctx, "T-1")
	if err != nil {
		t.Fatalf("second Worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second.Path, "left-behind.txt")); err == nil {
		t.Error("the retry inherited a file from the previous attempt")
	}
}

func TestWorktreesAreIsolatedFromEachOther(t *testing.T) {
	ctx := context.Background()
	m := New(t.TempDir(), "")

	r, err := m.Prepare(ctx, origin(t), "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	one, err := r.Worktree(ctx, "T-1")
	if err != nil {
		t.Fatalf("Worktree T-1: %v", err)
	}
	two, err := r.Worktree(ctx, "T-2")
	if err != nil {
		t.Fatalf("Worktree T-2: %v", err)
	}

	if one.Path == two.Path {
		t.Fatal("both tasks got the same directory")
	}
	if err := os.WriteFile(filepath.Join(one.Path, "only-in-one.txt"), nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(two.Path, "only-in-one.txt")); err == nil {
		t.Error("a file written in one task's worktree appeared in another's")
	}
}

// A task writing in its worktree must not dirty the shared clone, or the next
// task would start from a polluted baseline.
func TestWorktreeWritesDoNotDirtyTheClone(t *testing.T) {
	ctx := context.Background()
	m := New(t.TempDir(), "")

	r, err := m.Prepare(ctx, origin(t), "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	wt, err := r.Worktree(ctx, "T-1")
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if r.Dirty(ctx) {
		t.Error("the shared clone went dirty from a write inside a worktree")
	}
}

func TestWorktreeRemove(t *testing.T) {
	ctx := context.Background()
	m := New(t.TempDir(), "")

	r, err := m.Prepare(ctx, origin(t), "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	wt, err := r.Worktree(ctx, "T-1")
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.Remove(ctx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(wt.Path); err == nil {
		t.Error("the worktree directory is still on disk")
	}
}

func TestStatusReportsPreparedRepos(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	m := New(root, "")
	url := origin(t)

	if _, err := m.Prepare(ctx, url, ""); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// A worktree directory sits next to the clone and must not be reported as
	// a repository of its own.
	r, _ := m.Prepare(ctx, url, "")
	if _, err := r.Worktree(ctx, "T-1"); err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	status := m.Status(ctx)
	if len(status) != 1 {
		t.Fatalf("Status reported %d repos, want 1: %+v", len(status), status)
	}
	if status[0].Branch == "" {
		t.Error("Status reported no branch")
	}
	if status[0].Dirty {
		t.Error("a freshly prepared clone was reported dirty")
	}
}

// Two remotes must never share a directory, however similar their names.
func TestDirNameSeparatesSimilarRemotes(t *testing.T) {
	tests := []struct{ a, b string }{
		{"git@github.com:acme/app.git", "git@gitlab.com:acme/app.git"},
		{"git@github.com:acme/app.git", "https://github.com/acme/app.git"},
		{"git@github.com:one/app.git", "git@github.com:two/app.git"},
	}
	for _, tt := range tests {
		if dirName(tt.a) == dirName(tt.b) {
			t.Errorf("%s and %s share the directory %s", tt.a, tt.b, dirName(tt.a))
		}
	}
	if got := dirName("git@github.com:acme/app.git"); !strings.HasPrefix(got, "app-") {
		t.Errorf("dirName = %q, want it to start with the readable repo name", got)
	}
}

func TestSlugRejectsPathTraversal(t *testing.T) {
	for _, in := range []string{"../../etc/passwd", "..", "/absolute", "a/b"} {
		got := slug(in)
		if strings.Contains(got, "/") || got == ".." || strings.HasPrefix(got, ".") {
			t.Errorf("slug(%q) = %q, which can still escape its directory", in, got)
		}
	}
}

// The token must reach git through the environment, never through argv.
func TestConfigArgsDoNotCarryTheToken(t *testing.T) {
	m := New(t.TempDir(), "ghp_realtokenvalue")
	joined := strings.Join(m.configArgs(), " ")

	if strings.Contains(joined, "ghp_realtokenvalue") {
		t.Errorf("git config args leaked the token: %s", joined)
	}
	if !strings.Contains(joined, "ROOST_GIT_TOKEN") {
		t.Errorf("git config args never read the token: %s", joined)
	}
	if strings.Contains(strings.Join(New(t.TempDir(), "").configArgs(), " "), "credential.helper") {
		t.Error("a manager with no token still configured a credential helper")
	}
}

// A repository's hooks are files in the tree git is being asked to check out,
// and git runs them on the host, outside the container everything else is
// confined to. Cloning a hostile repository must not execute it.
func TestHooksNeverRunOnTheHost(t *testing.T) {
	ctx := context.Background()
	src := origin(t)

	// A post-checkout hook fires on clone and on every worktree add.
	hook := filepath.Join(src, ".git", "hooks", "post-checkout")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m := New(t.TempDir(), "")
	r, err := m.Prepare(ctx, src, "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := r.Worktree(ctx, "T-1"); err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	if _, err := os.Stat(marker); err == nil {
		t.Error("a hook from the cloned repository executed on the host")
	}
}

func TestCommitAndPush(t *testing.T) {
	ctx := context.Background()
	src := origin(t)

	m := New(t.TempDir(), "")
	r, err := m.Prepare(ctx, src, "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	w, err := r.Worktree(ctx, "T-1")
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	// Nothing changed yet, so there is nothing to commit.
	changed, err := w.Commit(ctx, "should not happen", Author{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if changed {
		t.Error("Commit made a commit with nothing staged")
	}

	if err := os.WriteFile(filepath.Join(w.Path, "new.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	author := Author{Name: "Roost", Email: "roost@example.com"}
	changed, err = w.Commit(ctx, "add new.txt\n\nBecause the ticket asked.", author)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !changed {
		t.Fatal("Commit ignored a new file")
	}

	out, err := m.git(ctx, w.Path, "log", "-1", "--format=%an <%ae>%n%B")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if !strings.Contains(out, "Roost <roost@example.com>") {
		t.Errorf("commit author = %q", out)
	}
	if !strings.Contains(out, "Because the ticket asked.") {
		t.Errorf("commit body = %q", out)
	}

	if err := w.Push(ctx); err != nil {
		t.Fatalf("Push: %v", err)
	}
	pushed, err := m.git(ctx, src, "log", "-1", "--format=%s", "refs/heads/"+w.Branch)
	if err != nil {
		t.Fatalf("the branch did not reach the origin: %v", err)
	}
	if strings.TrimSpace(pushed) != "add new.txt" {
		t.Errorf("origin has %q", strings.TrimSpace(pushed))
	}
}

// A retried ticket works on the same branch, so its push has to replace what
// the previous attempt left rather than be rejected.
func TestPushReplacesAPreviousAttempt(t *testing.T) {
	ctx := context.Background()
	src := origin(t)
	m := New(t.TempDir(), "")

	commitAndPush := func(content string) {
		t.Helper()
		r, err := m.Prepare(ctx, src, "")
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		w, err := r.Worktree(ctx, "T-1")
		if err != nil {
			t.Fatalf("Worktree: %v", err)
		}
		if err := os.WriteFile(filepath.Join(w.Path, "new.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := w.Commit(ctx, "attempt "+content, Author{}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := w.Push(ctx); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}

	commitAndPush("one")
	commitAndPush("two")

	out, err := m.git(ctx, src, "log", "-1", "--format=%s", "refs/heads/roost/T-1")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if strings.TrimSpace(out) != "attempt two" {
		t.Errorf("origin has %q, want the second attempt", strings.TrimSpace(out))
	}
}

func TestCommitRefusesAnEmptyMessage(t *testing.T) {
	ctx := context.Background()
	m := New(t.TempDir(), "")
	r, err := m.Prepare(ctx, origin(t), "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	w, err := r.Worktree(ctx, "T-1")
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := w.Commit(ctx, "  \n ", Author{}); err == nil {
		t.Error("Commit accepted an empty message")
	}
}

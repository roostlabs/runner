// Package repo manages the persistent working copies on the VPS.
//
// A repository is cloned once and kept. Later tasks fetch into it and reuse its
// object store and dependency caches, because re-cloning a large repository for
// every ticket is the difference between a task starting in seconds and starting
// in minutes.
//
// Persistence creates its own hazard: without care, one task would inherit the
// files another left behind. Every task therefore gets a fresh git worktree
// branched from the remote's current head, which is a clean baseline that still
// shares the object store it was cheap to keep.
package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/roostlabs/protocol"
)

// BranchPrefix namespaces the branches the Runner creates, so a human looking at
// the repository can tell which branches an agent made.
const BranchPrefix = "roost/"

// gitTimeout bounds a single git invocation. Network operations against a large
// repository are the slow case.
const gitTimeout = 10 * time.Minute

// Manager owns the repository directory under the Runner's data directory.
type Manager struct {
	root  string
	token string
}

// New returns a Manager storing clones under root.
//
// token, when set, authenticates https remotes. It is handed to git through the
// environment, never through the command line.
func New(root, token string) *Manager {
	return &Manager{root: root, token: token}
}

// Repo is one prepared clone on disk.
type Repo struct {
	URL string
	Dir string
	// DefaultBranch is the remote's head, which worktrees branch from.
	DefaultBranch string

	mgr *Manager
}

// Prepare makes url ready to work in: cloned if the VPS has never seen it,
// fetched if it has.
func (m *Manager) Prepare(ctx context.Context, url, branch string) (*Repo, error) {
	if url == "" {
		return nil, errors.New("repo: no url")
	}
	dir := filepath.Join(m.root, dirName(url))

	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("repo: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return nil, fmt.Errorf("repo: %w", err)
		}
		if _, err := m.git(ctx, "", "clone", url, dir); err != nil {
			return nil, err
		}
	} else if _, err := m.git(ctx, dir, "fetch", "--prune", "origin"); err != nil {
		return nil, err
	}

	r := &Repo{URL: url, Dir: dir, mgr: m}
	if branch != "" {
		r.DefaultBranch = branch
	} else {
		head, err := m.git(ctx, dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
		if err != nil {
			// A repository whose origin/HEAD was never set still has to work.
			r.DefaultBranch = "main"
		} else {
			r.DefaultBranch = strings.TrimPrefix(strings.TrimSpace(head), "origin/")
		}
	}
	return r, nil
}

// Worktree is a task's private checkout.
type Worktree struct {
	// Path is the directory to mount into the sandbox.
	Path string
	// Branch is where the agent's commits land.
	Branch string

	repo *Repo
}

// Worktree gives taskID a clean baseline branched from the remote's head.
//
// Any previous worktree for the same task is discarded first: a task that is
// being retried must not inherit what its last attempt left behind, and that is
// the whole reason worktrees are used instead of working in the clone directly.
func (r *Repo) Worktree(ctx context.Context, taskID string) (*Worktree, error) {
	if taskID == "" {
		return nil, errors.New("repo: no task id")
	}
	path := filepath.Join(r.Dir+".worktrees", slug(taskID))
	branch := BranchPrefix + slug(taskID)

	// Ignore the error: usually there is nothing to remove.
	r.mgr.git(ctx, r.Dir, "worktree", "remove", "--force", path)
	if err := os.RemoveAll(path); err != nil {
		return nil, fmt.Errorf("repo: clearing %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("repo: %w", err)
	}

	base := "origin/" + r.DefaultBranch
	if _, err := r.mgr.git(ctx, r.Dir, "worktree", "add", "--force", "-B", branch, path, base); err != nil {
		return nil, err
	}
	return &Worktree{Path: path, Branch: branch, repo: r}, nil
}

// Remove discards the worktree. The branch survives, because the commits on it
// are the task's output.
func (w *Worktree) Remove(ctx context.Context) error {
	if _, err := w.repo.mgr.git(ctx, w.repo.Dir, "worktree", "remove", "--force", w.Path); err != nil {
		// Fall back to deleting the directory and letting git reconcile, so a
		// failure here cannot strand disk space forever.
		if rmErr := os.RemoveAll(w.Path); rmErr != nil {
			return fmt.Errorf("repo: %w", errors.Join(err, rmErr))
		}
		w.repo.mgr.git(ctx, w.repo.Dir, "worktree", "prune")
	}
	return nil
}

// Dirty reports whether the clone has uncommitted changes, which would mean
// something wrote to it outside a worktree.
func (r *Repo) Dirty(ctx context.Context) bool {
	out, err := r.mgr.git(ctx, r.Dir, "status", "--porcelain")
	return err == nil && strings.TrimSpace(out) != ""
}

// Status describes every prepared repository, for the repo.status message.
func (m *Manager) Status(ctx context.Context) protocol.RepoStatus {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil
	}

	var status protocol.RepoStatus
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasSuffix(entry.Name(), ".worktrees") {
			continue
		}
		dir := filepath.Join(m.root, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			continue
		}

		info := protocol.RepoInfo{Repo: entry.Name()}
		if url, err := m.git(ctx, dir, "remote", "get-url", "origin"); err == nil {
			info.Repo = strings.TrimSpace(url)
		}
		if branch, err := m.git(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			info.Branch = strings.TrimSpace(branch)
		}
		if st, err := os.Stat(filepath.Join(dir, ".git", "FETCH_HEAD")); err == nil {
			info.LastFetch = st.ModTime().UnixMilli()
		}
		if out, err := m.git(ctx, dir, "status", "--porcelain"); err == nil {
			info.Dirty = strings.TrimSpace(out) != ""
		}
		status = append(status, info)
	}
	return status
}

// git runs one git command in dir, or outside any repository when dir is empty.
func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	full := append(m.configArgs(), args...)
	if dir != "" {
		full = append([]string{"-C", dir}, full...)
	}

	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(),
		// Never stop for a prompt: an unattended Runner would hang forever
		// waiting for a password nobody is there to type.
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
	)
	if m.token != "" {
		cmd.Env = append(cmd.Env, "ROOST_GIT_TOKEN="+m.token)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("repo: git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// configArgs authenticates https remotes without the token ever appearing in the
// command line.
//
// The helper is a shell snippet that reads the value out of the environment, so
// what lands in ps is the snippet, not the credential. The empty helper first
// clears any inherited one, so a keychain or store on the host cannot answer
// instead and quietly use the wrong identity.
func (m *Manager) configArgs() []string {
	if m.token == "" {
		return nil
	}
	return []string{
		"-c", "credential.helper=",
		"-c", `credential.helper=!f() { echo "username=x-access-token"; echo "password=$ROOST_GIT_TOKEN"; }; f`,
	}
}

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// slug makes an identifier safe to use as a path element.
func slug(s string) string {
	out := unsafeChars.ReplaceAllString(s, "-")
	out = strings.Trim(out, "-.")
	if out == "" {
		out = "unnamed"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// dirName maps a remote URL to a directory.
//
// The readable part is for a human looking at the disk; the hash suffix is what
// actually keeps two remotes apart, since ssh and https forms of the same
// repository — and same-named repositories from different hosts — would
// otherwise collide.
func dirName(url string) string {
	name := url
	if i := strings.LastIndexAny(name, "/:"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, ".git")

	sum := sha256.Sum256([]byte(url))
	return slug(name) + "-" + hex.EncodeToString(sum[:4])
}

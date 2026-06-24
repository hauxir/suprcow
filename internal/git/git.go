// Package git manages per-PR source checkouts using a shared mirror clone plus
// git worktrees, so each PR environment has its own working tree at a specific
// SHA without re-cloning the whole repo every time.
package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hauxir/suprcow/internal/shell"
)

// Repo manages checkouts for a single remote under a data directory layout:
//
//	<dataDir>/mirror.git              shared bare mirror of the remote
//	<dataDir>/worktrees/pr-<n>        per-PR working tree
type Repo struct {
	// Remote is the git URL to clone/fetch.
	Remote string
	// DataDir is where the mirror and worktrees live.
	DataDir string
	// AuthHeader, if set, returns an HTTP header (e.g. "Authorization: Basic …")
	// injected into network git operations so private repos can be cloned with a
	// short-lived credential. It is passed via env (GIT_CONFIG_*) to keep the
	// token out of the process argument list.
	AuthHeader func(ctx context.Context) (string, error)

	run shell.Runner
}

// New returns a Repo using the given runner (use shell.Exec{} in production).
func New(remote, dataDir string, run shell.Runner) *Repo {
	return &Repo{Remote: remote, DataDir: dataDir, run: run}
}

func (r *Repo) mirrorPath() string { return filepath.Join(r.DataDir, "mirror.git") }
func (r *Repo) worktreePath(pr int) string {
	return filepath.Join(r.DataDir, "worktrees", fmt.Sprintf("pr-%d", pr))
}

// authEnv returns GIT_CONFIG_* env that injects the auth header into git's HTTP
// requests, or nil when no credential is configured.
func (r *Repo) authEnv(ctx context.Context) ([]string, error) {
	if r.AuthHeader == nil {
		return nil, nil
	}
	header, err := r.AuthHeader(ctx)
	if err != nil {
		return nil, fmt.Errorf("git auth: %w", err)
	}
	if header == "" {
		return nil, nil
	}
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=" + header,
	}, nil
}

// EnsureMirror creates the bare mirror on first use and fetches the latest refs
// otherwise. It is safe to call before every checkout.
func (r *Repo) EnsureMirror(ctx context.Context) error {
	env, err := r.authEnv(ctx)
	if err != nil {
		return err
	}
	if _, err := os.Stat(r.mirrorPath()); os.IsNotExist(err) {
		if err := os.MkdirAll(r.DataDir, 0o755); err != nil {
			return err
		}
		_, err := r.run.Run(ctx, r.DataDir, env, "git", "clone", "--mirror", r.Remote, r.mirrorPath())
		return err
	}
	_, err = r.run.Run(ctx, r.mirrorPath(), env, "git", "fetch", "--prune", "origin", "+refs/*:refs/*")
	return err
}

// Checkout ensures the PR's worktree exists and points at sha. It fetches the
// mirror first, so calling Checkout on a push event performs the auto-pull.
// It returns the worktree path.
func (r *Repo) Checkout(ctx context.Context, pr int, sha string) (string, error) {
	if err := r.EnsureMirror(ctx); err != nil {
		return "", fmt.Errorf("ensure mirror: %w", err)
	}
	wt := r.worktreePath(pr)
	if _, err := os.Stat(wt); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
			return "", err
		}
		// worktree add reads from the local mirror (no network), so no auth env.
		if _, err := r.run.Run(ctx, r.mirrorPath(), nil, "git", "worktree", "add", "--detach", wt, sha); err != nil {
			return "", fmt.Errorf("worktree add: %w", err)
		}
		return wt, nil
	}
	// Existing worktree: refresh the mirror (above) brought new refs in; reset
	// the worktree to the new SHA (auto-pull / synchronize). Both are local.
	if _, err := r.run.Run(ctx, wt, nil, "git", "reset", "--hard", sha); err != nil {
		return "", fmt.Errorf("reset to %s: %w", sha, err)
	}
	return wt, nil
}

// ChangedFiles returns the paths that differ between two commits, used to decide
// whether a push can hot-reload or needs a rebuild. Both commits must already
// be present (Checkout fetches them); returns nil when fromSHA is empty/equal.
func (r *Repo) ChangedFiles(ctx context.Context, pr int, fromSHA, toSHA string) ([]string, error) {
	if fromSHA == "" || fromSHA == toSHA {
		return nil, nil
	}
	out, err := r.run.Run(ctx, r.worktreePath(pr), nil, "git", "diff", "--name-only", fromSHA, toSHA)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			files = append(files, s)
		}
	}
	return files, nil
}

// Remove deletes a PR's worktree (called on teardown).
func (r *Repo) Remove(ctx context.Context, pr int) error {
	wt := r.worktreePath(pr)
	if _, err := os.Stat(wt); os.IsNotExist(err) {
		return nil
	}
	// Prune the worktree registration, then remove the directory.
	_, _ = r.run.Run(ctx, r.mirrorPath(), nil, "git", "worktree", "remove", "--force", wt)
	return os.RemoveAll(wt)
}

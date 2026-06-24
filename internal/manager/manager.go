// Package manager orchestrates the full lifecycle of per-PR environments: it
// ties together config, git checkouts, the container backend, and the state
// store to implement lazy spawn-on-demand, auto-pull, LRU eviction, and idle
// reaping.
package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hauxir/suprcow/internal/compose"
	"github.com/hauxir/suprcow/internal/config"
	"github.com/hauxir/suprcow/internal/engine"
	"github.com/hauxir/suprcow/internal/env"
	"github.com/hauxir/suprcow/internal/store"
)

// GitRepo checks out PR source. *git.Repo implements it; abstracted for testing.
type GitRepo interface {
	// Checkout ensures the PR's worktree exists at sha and returns its path.
	Checkout(ctx context.Context, pr int, sha string) (string, error)
	// ChangedFiles lists paths that differ between two commits.
	ChangedFiles(ctx context.Context, pr int, fromSHA, toSHA string) ([]string, error)
	// Remove deletes the PR's worktree.
	Remove(ctx context.Context, pr int) error
}

// ErrUnknownPR is returned when a request targets a PR suprcow has no record of
// (no webhook seen). Lazy spawn needs the branch/SHA a webhook provides.
var ErrUnknownPR = errors.New("unknown PR (no webhook received yet)")

// Options configures a Manager.
type Options struct {
	Project       string         // logical project key, e.g. "demo"
	Config        *config.Config // parsed preview.yml
	BaseDomain    string         // wildcard root, e.g. "preview.example.com"
	DataDir       string         // root for worktrees/state
	SharedNetwork string         // external docker network the daemon shares with stacks
	EnvFiles      []string       // absolute paths to env files passed to compose

	Store   store.Store
	Repo    GitRepo
	Backend engine.Backend

	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

// Manager owns the environments for one project.
type Manager struct {
	project       string
	cfg           *config.Config
	baseDomain    string
	dataDir       string
	sharedNetwork string
	envFiles      []string

	store   store.Store
	repo    GitRepo
	backend engine.Backend
	now     func() time.Time

	// ready polls a single service's health gate; injectable for tests.
	ready func(ctx context.Context, alias string, hc config.HealthCheck) error

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New constructs a Manager.
func New(o Options) *Manager {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	net := o.SharedNetwork
	if net == "" {
		net = "suprcow"
	}
	m := &Manager{
		project:       o.Project,
		cfg:           o.Config,
		baseDomain:    o.BaseDomain,
		dataDir:       o.DataDir,
		sharedNetwork: net,
		envFiles:      o.EnvFiles,
		store:         o.Store,
		repo:          o.Repo,
		backend:       o.Backend,
		now:           now,
		locks:         map[string]*sync.Mutex{},
	}
	m.ready = m.defaultReady
	return m
}

// lockFor returns a per-PR mutex so concurrent requests for the same env
// serialize (only one spawn at a time) while different envs proceed in parallel.
func (m *Manager) lockFor(pr int) *sync.Mutex {
	key := env.Key(m.project, pr)
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[key]
	if !ok {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	return l
}

// Notify records a PR lifecycle event from a webhook.
//
//   - opened/reopened: upsert a pending env (no build — lazy).
//   - synchronize: update the SHA; if the stack is live, re-checkout + restart
//     so the env reflects the new push (auto-pull).
//   - closed/merged: tear the env down (containers + volumes + worktree).
func (m *Manager) Notify(ctx context.Context, pr int, branch, sha, action string) error {
	switch action {
	case "opened", "reopened":
		return m.upsertPending(pr, branch, sha)
	case "synchronize":
		return m.onSynchronize(ctx, pr, branch, sha)
	case "closed", "merged":
		return m.Teardown(ctx, pr)
	default:
		return nil // ignore unrelated actions
	}
}

func (m *Manager) upsertPending(pr int, branch, sha string) error {
	l := m.lockFor(pr)
	l.Lock()
	defer l.Unlock()

	e, ok, err := m.store.Get(m.project, pr)
	if err != nil {
		return err
	}
	now := m.now()
	if !ok {
		e = &env.Environment{Project: m.project, PR: pr, CreatedAt: now}
	}
	e.Branch, e.SHA, e.Status, e.UpdatedAt = branch, sha, env.StatusPending, now
	return m.store.Put(e)
}

func (m *Manager) onSynchronize(ctx context.Context, pr int, branch, sha string) error {
	l := m.lockFor(pr)
	l.Lock()
	defer l.Unlock()

	e, ok, err := m.store.Get(m.project, pr)
	if err != nil {
		return err
	}
	now := m.now()
	if !ok {
		e = &env.Environment{Project: m.project, PR: pr, CreatedAt: now}
	}
	oldSHA := e.SHA
	e.Branch, e.SHA, e.UpdatedAt = branch, sha, now

	if e.IsLive() {
		// Auto-pull into the running env: hot-reload in place when possible,
		// rebuild only when a change can't be hot-reloaded.
		if err := m.autoPull(ctx, e, oldSHA); err != nil {
			e.Status, e.Message = env.StatusError, err.Error()
			_ = m.store.Put(e)
			return err
		}
	} else if e.Status != env.StatusStopped {
		e.Status = env.StatusPending
	}
	return m.store.Put(e)
}

// autoPull updates a running env to its new SHA. It updates the worktree files
// in place so a dev server with file watching hot-reloads instantly, and only
// falls back to a full rebuild+restart when a change can't be hot-reloaded
// (dependency manifests, Dockerfiles, or the compose file).
func (m *Manager) autoPull(ctx context.Context, e *env.Environment, oldSHA string) error {
	wt, err := m.repo.Checkout(ctx, e.PR, e.SHA)
	if err != nil {
		return fmt.Errorf("update worktree: %w", err)
	}
	e.Worktree = wt

	changed, err := m.repo.ChangedFiles(ctx, e.PR, oldSHA, e.SHA)
	if err != nil || m.needsRebuild(changed) {
		// Rebuild path: recreate the stack (re-renders config, compose up --build).
		if err := m.spawn(ctx, e); err != nil {
			return err
		}
		return m.runUpdateHooks(ctx, e)
	}

	// Hot-reload path: files are already updated in the worktree; the running
	// dev server picks them up with no container restart (no waiting page).
	rc := m.cfg.RenderContext(e.PR, e.Branch, e.SHA, m.baseDomain)
	if err := m.writeInjectFiles(rc, wt); err != nil {
		return err
	}
	if err := m.runUpdateHooks(ctx, e); err != nil {
		return err
	}
	now := m.now()
	e.Status, e.Message, e.UpdatedAt = env.StatusRunning, "", now
	return nil
}

// needsRebuild reports whether any changed path requires a rebuild rather than
// a hot reload.
func (m *Manager) needsRebuild(changed []string) bool {
	for _, f := range changed {
		if m.isRebuildPath(f) {
			return true
		}
	}
	return false
}

func (m *Manager) isRebuildPath(f string) bool {
	base := path.Base(f)
	if f == m.cfg.Compose || base == path.Base(m.cfg.Compose) {
		return true
	}
	if strings.HasPrefix(base, "Dockerfile") {
		return true
	}
	for _, p := range m.cfg.RebuildOn {
		if base == p || f == p || strings.HasPrefix(f, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

// runUpdateHooks runs configured post-update commands inside service containers
// (e.g. migrations) after a push.
func (m *Manager) runUpdateHooks(ctx context.Context, e *env.Environment) error {
	for _, h := range m.cfg.OnUpdate {
		if err := m.backend.Exec(ctx, e.ComposeProject(), h.Service, []string{"sh", "-c", h.Run}); err != nil {
			return fmt.Errorf("on_update %s: %w", h.Service, err)
		}
	}
	return nil
}

// EnsureUp lazily brings a PR's stack up (or warm-restarts it) and returns the
// running environment. It is the entry point the router calls on a request.
func (m *Manager) EnsureUp(ctx context.Context, pr int) (*env.Environment, error) {
	l := m.lockFor(pr)
	l.Lock()
	defer l.Unlock()

	e, ok, err := m.store.Get(m.project, pr)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrUnknownPR
	}

	// Already running: just record the access for idle/LRU bookkeeping.
	if e.Status == env.StatusRunning {
		state, err := m.backend.State(ctx, e.ComposeProject())
		if err == nil && state == engine.StateRunning {
			e.LastAccess = m.now()
			_ = m.store.Put(e)
			return e, nil
		}
		// Drifted (e.g. daemon restarted, containers gone) — fall through to spawn.
	}

	if err := m.enforceCapacity(ctx, pr); err != nil {
		return nil, err
	}
	if err := m.spawn(ctx, e); err != nil {
		e.Status, e.Message = env.StatusError, err.Error()
		_ = m.store.Put(e)
		return nil, err
	}
	return e, m.store.Put(e)
}

// spawn checks out the SHA, renders inject config, brings the stack up, and
// waits for health gates. Caller holds the per-PR lock and persists e.
func (m *Manager) spawn(ctx context.Context, e *env.Environment) error {
	e.Status, e.Message = env.StatusStarting, ""
	_ = m.store.Put(e)

	worktree, err := m.repo.Checkout(ctx, e.PR, e.SHA)
	if err != nil {
		return fmt.Errorf("checkout: %w", err)
	}
	e.Worktree = worktree

	// The compose file comes from the PR's checkout and the daemon runs it with
	// the host Docker socket, so reject any host-escape config before running it.
	composeData, err := os.ReadFile(filepath.Join(worktree, m.cfg.Compose))
	if err != nil {
		return fmt.Errorf("read compose %q: %w", m.cfg.Compose, err)
	}
	if err := compose.Sanitize(composeData); err != nil {
		return fmt.Errorf("preview compose rejected: %w", err)
	}

	rc := m.cfg.RenderContext(e.PR, e.Branch, e.SHA, m.baseDomain)
	overridePath, err := m.writeOverride(rc, e.PR, worktree)
	if err != nil {
		return err
	}
	if err := m.writeInjectFiles(rc, worktree); err != nil {
		return err
	}

	spec := engine.Spec{
		Project:      e.ComposeProject(),
		WorkingDir:   worktree,
		ComposeFiles: []string{m.cfg.Compose, filepath.Base(overridePath)},
		EnvFiles:     m.envFiles,
	}
	if err := m.backend.Up(ctx, spec); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}

	if err := m.waitHealthy(ctx, e.PR); err != nil {
		return fmt.Errorf("health: %w", err)
	}

	now := m.now()
	e.Status, e.Message, e.LastAccess, e.UpdatedAt = env.StatusRunning, "", now, now
	return nil
}

// Touch records that traffic hit an environment (drives idle reaping + LRU).
func (m *Manager) Touch(pr int) {
	l := m.lockFor(pr)
	l.Lock()
	defer l.Unlock()
	if e, ok, _ := m.store.Get(m.project, pr); ok {
		e.LastAccess = m.now()
		_ = m.store.Put(e)
	}
}

// enforceCapacity stops the least-recently-accessed running env when starting
// another would exceed MaxRunning. Volumes + worktree are kept (warm restart).
func (m *Manager) enforceCapacity(ctx context.Context, excludePR int) error {
	all, err := m.store.List()
	if err != nil {
		return err
	}
	var running []*env.Environment
	for _, e := range all {
		if e.Project == m.project && e.Status == env.StatusRunning && e.PR != excludePR {
			running = append(running, e)
		}
	}
	if len(running) < m.cfg.MaxRunning {
		return nil
	}
	// Sort oldest-access first and stop enough to make room for one more.
	sort.Slice(running, func(i, j int) bool {
		return running[i].LastAccess.Before(running[j].LastAccess)
	})
	toStop := len(running) - m.cfg.MaxRunning + 1
	for i := 0; i < toStop; i++ {
		if err := m.stopEnv(ctx, running[i]); err != nil {
			return err
		}
	}
	return nil
}

// stopEnv stops a stack's containers but keeps its volumes.
func (m *Manager) stopEnv(ctx context.Context, e *env.Environment) error {
	if err := m.backend.Stop(ctx, e.ComposeProject()); err != nil {
		return fmt.Errorf("stop %s: %w", e.ComposeProject(), err)
	}
	e.Status, e.UpdatedAt = env.StatusStopped, m.now()
	return m.store.Put(e)
}

// ReapIdle stops every running env whose last access is older than IdleTimeout.
// Intended to be called periodically by the daemon.
func (m *Manager) ReapIdle(ctx context.Context) error {
	all, err := m.store.List()
	if err != nil {
		return err
	}
	cutoff := m.now().Add(-m.cfg.IdleTimeout.Duration())
	var firstErr error
	for _, e := range all {
		if e.Project != m.project || e.Status != env.StatusRunning {
			continue
		}
		if e.LastAccess.Before(cutoff) {
			l := m.lockFor(e.PR)
			l.Lock()
			if cur, ok, _ := m.store.Get(m.project, e.PR); ok && cur.Status == env.StatusRunning && cur.LastAccess.Before(cutoff) {
				if err := m.stopEnv(ctx, cur); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			l.Unlock()
		}
	}
	return firstErr
}

// Teardown removes a PR's containers, volumes, worktree, and state record.
func (m *Manager) Teardown(ctx context.Context, pr int) error {
	l := m.lockFor(pr)
	l.Lock()
	defer l.Unlock()

	e, ok, err := m.store.Get(m.project, pr)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := m.backend.Down(ctx, e.ComposeProject()); err != nil {
		return fmt.Errorf("compose down: %w", err)
	}
	if err := m.repo.Remove(ctx, pr); err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	return m.store.Delete(m.project, pr)
}

// Get returns a snapshot of an env's state for the router/UI.
func (m *Manager) Get(pr int) (*env.Environment, bool) {
	e, ok, _ := m.store.Get(m.project, pr)
	return e, ok
}

// Project returns the logical project key.
func (m *Manager) Project() string { return m.project }

// ServiceTarget returns the host:port the daemon proxies to for a service on a
// PR's stack, reachable on the shared network by its stable alias.
func (m *Manager) ServiceTarget(pr int, service string, port int) string {
	return fmt.Sprintf("%s:%d", serviceAlias(m.project, pr, service), port)
}

package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hauxir/suprcow/internal/config"
	"github.com/hauxir/suprcow/internal/engine"
	"github.com/hauxir/suprcow/internal/env"
	"github.com/hauxir/suprcow/internal/store"
)

type fakeRepo struct {
	dir         string
	removed     []int
	changed     []string // returned by ChangedFiles
	baseChanged []string // returned by ChangedAgainst (lite decision)
}

func (f *fakeRepo) Checkout(_ context.Context, pr int, _ string) (string, error) {
	wt := filepath.Join(f.dir, fmt.Sprintf("pr-%d", pr))
	if err := os.MkdirAll(wt, 0o755); err != nil {
		return "", err
	}
	// A minimal, safe compose file so the spawn path's sanitize check has
	// something to read (default compose name is docker-compose.yml).
	stub := "services:\n  web:\n    image: nginx\n"
	if err := os.WriteFile(filepath.Join(wt, "docker-compose.yml"), []byte(stub), 0o644); err != nil {
		return "", err
	}
	return wt, nil
}

func (f *fakeRepo) ChangedFiles(_ context.Context, _ int, _, _ string) ([]string, error) {
	return f.changed, nil
}

func (f *fakeRepo) ChangedAgainst(_ context.Context, _ int, _, _ string) ([]string, error) {
	return f.baseChanged, nil
}

func (f *fakeRepo) Remove(_ context.Context, pr int) error {
	f.removed = append(f.removed, pr)
	return nil
}

type fakeBackend struct {
	up, stop, down []string
	upSpecs        []engine.Spec // full specs from each Up (to assert env/profiles)
	exec           []string
	rmVolumes      []string
	state          map[string]engine.RunState
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{state: map[string]engine.RunState{}}
}
func (b *fakeBackend) Exec(_ context.Context, project, service string, _ []string) error {
	b.exec = append(b.exec, project+"/"+service)
	return nil
}
func (b *fakeBackend) Up(_ context.Context, spec engine.Spec) error {
	b.up = append(b.up, spec.Project)
	b.upSpecs = append(b.upSpecs, spec)
	b.state[spec.Project] = engine.StateRunning
	return nil
}
func (b *fakeBackend) Stop(_ context.Context, project string) error {
	b.stop = append(b.stop, project)
	b.state[project] = engine.StateStopped
	return nil
}
func (b *fakeBackend) Down(_ context.Context, project string) error {
	b.down = append(b.down, project)
	delete(b.state, project)
	return nil
}
func (b *fakeBackend) RemoveVolumes(_ context.Context, project string, names []string) error {
	for _, n := range names {
		b.rmVolumes = append(b.rmVolumes, project+"_"+n)
	}
	return nil
}
func (b *fakeBackend) State(_ context.Context, project string) (engine.RunState, error) {
	if s, ok := b.state[project]; ok {
		return s, nil
	}
	return engine.StateAbsent, nil
}

func newTestManager(t *testing.T, maxRunning int, clock *time.Time) (*Manager, *fakeBackend, *fakeRepo) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
repo: github.com/me/app
expose:
  - { service: web, subdomain: "pr-{n}", port: 80 }
idle_timeout: 30m
max_running: 100
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxRunning = maxRunning

	be := newFakeBackend()
	repo := &fakeRepo{dir: t.TempDir()}
	m := New(Options{
		Project:    "demo",
		Config:     cfg,
		BaseDomain: "preview.example.com",
		DataDir:    t.TempDir(),
		Store:      store.NewMemory(),
		Repo:       repo,
		Backend:    be,
		Now:        func() time.Time { return *clock },
	})
	return m, be, repo
}

func TestEnsureUpUnknownPR(t *testing.T) {
	now := time.Now()
	m, _, _ := newTestManager(t, 100, &now)
	if _, err := m.EnsureUp(context.Background(), 7); err != ErrUnknownPR {
		t.Fatalf("want ErrUnknownPR, got %v", err)
	}
}

func TestLazySpawn(t *testing.T) {
	now := time.Now()
	m, be, _ := newTestManager(t, 100, &now)
	ctx := context.Background()

	if err := m.Notify(ctx, 5, "feat/x", "sha1", "opened"); err != nil {
		t.Fatal(err)
	}
	if e, _ := m.Get(5); e.Status != env.StatusPending {
		t.Fatalf("want pending, got %s", e.Status)
	}

	e, err := m.EnsureUp(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != env.StatusRunning {
		t.Fatalf("want running, got %s", e.Status)
	}
	if len(be.up) != 1 || be.up[0] != "demo-pr-5" {
		t.Fatalf("up calls = %v", be.up)
	}

	// Verify the override file was written into the worktree.
	if _, err := os.Stat(filepath.Join(e.Worktree, overrideFileName)); err != nil {
		t.Errorf("override not written: %v", err)
	}
}

func TestLRUEviction(t *testing.T) {
	now := time.Now()
	m, be, _ := newTestManager(t, 1, &now) // only one running at a time
	ctx := context.Background()

	_ = m.Notify(ctx, 1, "b1", "s1", "opened")
	if _, err := m.EnsureUp(ctx, 1); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute) // pr1 accessed earlier than pr2

	_ = m.Notify(ctx, 2, "b2", "s2", "opened")
	if _, err := m.EnsureUp(ctx, 2); err != nil {
		t.Fatal(err)
	}

	if e1, _ := m.Get(1); e1.Status != env.StatusStopped {
		t.Fatalf("pr1 should be evicted/stopped, got %s", e1.Status)
	}
	if e2, _ := m.Get(2); e2.Status != env.StatusRunning {
		t.Fatalf("pr2 should be running, got %s", e2.Status)
	}
	if len(be.stop) != 1 || be.stop[0] != "demo-pr-1" {
		t.Fatalf("expected pr1 stopped, stop=%v", be.stop)
	}
}

func TestReapIdle(t *testing.T) {
	now := time.Now()
	m, be, _ := newTestManager(t, 100, &now)
	ctx := context.Background()

	_ = m.Notify(ctx, 9, "b", "s", "opened")
	if _, err := m.EnsureUp(ctx, 9); err != nil {
		t.Fatal(err)
	}

	now = now.Add(31 * time.Minute) // past idle_timeout
	if err := m.ReapIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if e, _ := m.Get(9); e.Status != env.StatusStopped {
		t.Fatalf("want stopped after reap, got %s", e.Status)
	}
	if len(be.stop) != 1 {
		t.Fatalf("expected one stop, got %v", be.stop)
	}
}

func TestAutoPullHotReload(t *testing.T) {
	now := time.Now()
	m, be, repo := newTestManager(t, 100, &now)
	ctx := context.Background()

	_ = m.Notify(ctx, 3, "b", "old", "opened")
	if _, err := m.EnsureUp(ctx, 3); err != nil {
		t.Fatal(err)
	}

	// Code-only change → hot reload in place, NO rebuild (still one Up).
	repo.changed = []string{"src/app.tsx", "lib/foo.ex"}
	if err := m.Notify(ctx, 3, "b", "new", "synchronize"); err != nil {
		t.Fatal(err)
	}
	if len(be.up) != 1 {
		t.Fatalf("hot reload must not rebuild; ups = %v", be.up)
	}
	if e, _ := m.Get(3); e.SHA != "new" || e.Status != env.StatusRunning {
		t.Fatalf("want sha=new running, got sha=%s status=%s", e.SHA, e.Status)
	}
}

func TestAutoPullRebuildOnDeps(t *testing.T) {
	now := time.Now()
	m, be, repo := newTestManager(t, 100, &now)
	ctx := context.Background()

	_ = m.Notify(ctx, 4, "b", "old", "opened")
	if _, err := m.EnsureUp(ctx, 4); err != nil {
		t.Fatal(err)
	}

	// A dependency manifest changed → full rebuild (second Up).
	repo.changed = []string{"src/app.tsx", "package.json"}
	if err := m.Notify(ctx, 4, "b", "new", "synchronize"); err != nil {
		t.Fatal(err)
	}
	if len(be.up) != 2 {
		t.Fatalf("dep change must rebuild; ups = %v", be.up)
	}
}

func TestAutoPullResetVolumesOnRebuild(t *testing.T) {
	now := time.Now()
	m, be, repo := newTestManager(t, 100, &now)
	m.cfg.ResetVolumesOnRebuild = []string{"web-node-modules"}
	ctx := context.Background()

	_ = m.Notify(ctx, 5, "b", "old", "opened")
	if _, err := m.EnsureUp(ctx, 5); err != nil {
		t.Fatal(err)
	}

	// Code-only change → hot reload, volumes must NOT be reset.
	repo.changed = []string{"src/app.tsx"}
	if err := m.Notify(ctx, 5, "b", "mid", "synchronize"); err != nil {
		t.Fatal(err)
	}
	if len(be.rmVolumes) != 0 {
		t.Fatalf("hot reload must not reset volumes; got %v", be.rmVolumes)
	}

	// Dependency change → rebuild, the configured volume is dropped so the new
	// image re-seeds it.
	repo.changed = []string{"package.json"}
	if err := m.Notify(ctx, 5, "b", "new", "synchronize"); err != nil {
		t.Fatal(err)
	}
	if len(be.rmVolumes) != 1 || be.rmVolumes[0] != "demo-pr-5_web-node-modules" {
		t.Fatalf("rebuild must reset the configured volume; got %v", be.rmVolumes)
	}
}

func TestReloadTriggerOnHotReload(t *testing.T) {
	now := time.Now()
	cfg, err := config.Parse([]byte(`
repo: github.com/me/app
expose:
  - { service: web, subdomain: "pr-{n}", port: 80 }
reload_trigger:
  - { service: api, port: 4000, path: "/" }
`))
	if err != nil {
		t.Fatal(err)
	}
	be := newFakeBackend()
	repo := &fakeRepo{dir: t.TempDir()}
	m := New(Options{
		Project: "kosmi", Config: cfg, BaseDomain: "preview.example.com",
		DataDir: t.TempDir(), Store: store.NewMemory(), Repo: repo, Backend: be,
		Now: func() time.Time { return now },
	})
	var pinged []string
	m.reload = func(_ context.Context, url string) error { pinged = append(pinged, url); return nil }
	ctx := context.Background()

	_ = m.Notify(ctx, 7, "b", "old", "opened")
	if _, err := m.EnsureUp(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if len(pinged) != 0 {
		t.Fatalf("no reload ping expected on initial spawn, got %v", pinged)
	}

	repo.changed = []string{"lib/poker.ex"} // code-only → hot reload
	if err := m.Notify(ctx, 7, "b", "new", "synchronize"); err != nil {
		t.Fatal(err)
	}
	want := "http://kosmi-pr-7-api:4000/"
	if len(pinged) != 1 || pinged[0] != want {
		t.Fatalf("reload ping = %v, want [%s]", pinged, want)
	}
}

func TestOnUpdateHooks(t *testing.T) {
	now := time.Now()
	cfg, err := config.Parse([]byte(`
repo: github.com/me/app
expose:
  - { service: web, subdomain: "pr-{n}", port: 80 }
on_update:
  - { service: api, run: "migrate" }
`))
	if err != nil {
		t.Fatal(err)
	}
	be := newFakeBackend()
	repo := &fakeRepo{dir: t.TempDir()}
	m := New(Options{
		Project: "demo", Config: cfg, BaseDomain: "preview.example.com",
		DataDir: t.TempDir(), Store: store.NewMemory(), Repo: repo, Backend: be,
		Now: func() time.Time { return now },
	})
	ctx := context.Background()

	_ = m.Notify(ctx, 1, "b", "old", "opened")
	if _, err := m.EnsureUp(ctx, 1); err != nil {
		t.Fatal(err)
	}
	repo.changed = []string{"src/x"} // hot-reload path still runs hooks
	if err := m.Notify(ctx, 1, "b", "new", "synchronize"); err != nil {
		t.Fatal(err)
	}
	if len(be.exec) != 1 || be.exec[0] != "demo-pr-1/api" {
		t.Fatalf("expected on_update exec, got %v", be.exec)
	}
}

func TestTeardown(t *testing.T) {
	now := time.Now()
	m, be, repo := newTestManager(t, 100, &now)
	ctx := context.Background()

	_ = m.Notify(ctx, 4, "b", "s", "opened")
	if _, err := m.EnsureUp(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if err := m.Notify(ctx, 4, "b", "s", "closed"); err != nil {
		t.Fatal(err)
	}
	if len(be.down) != 1 || be.down[0] != "demo-pr-4" {
		t.Fatalf("expected down, got %v", be.down)
	}
	if len(repo.removed) != 1 || repo.removed[0] != 4 {
		t.Fatalf("expected worktree removed, got %v", repo.removed)
	}
	if _, ok := m.Get(4); ok {
		t.Fatal("env should be deleted")
	}
}

// liteTestConfig is a config with a full stack gated behind the "fullstack"
// profile and a lite variant for frontend-only PRs that points elsewhere.
const liteTestConfig = `
repo: github.com/me/app
expose:
  - { service: web, subdomain: "pr-{n}", port: 80 }
profiles: [fullstack]
health:
  web: { tcp: 80, timeout: 60s }
  api: { tcp: 4000, timeout: 60s }
on_update:
  - { service: api, run: "migrate" }
lite:
  when_changed_only: [apps/frontend/]
  unless_changed: [apps/frontend/src/__generated__/]
  profiles: []
  health:
    web: { tcp: 80, timeout: 60s }
  on_update:
    - { service: web, run: "codegen" }
  inject:
    web:
      files:
        - { dest: cfg.json, content: '{"host":"shared"}' }
`

func composeProfiles(spec engine.Spec) string {
	for _, kv := range spec.Env {
		if v, ok := strings.CutPrefix(kv, "COMPOSE_PROFILES="); ok {
			return v
		}
	}
	return "<unset>"
}

func newLiteManager(t *testing.T, now time.Time) (*Manager, *fakeBackend, *fakeRepo, *[]string) {
	t.Helper()
	cfg, err := config.Parse([]byte(liteTestConfig))
	if err != nil {
		t.Fatal(err)
	}
	be := newFakeBackend()
	repo := &fakeRepo{dir: t.TempDir()}
	m := New(Options{
		Project: "demo", Config: cfg, BaseDomain: "preview.example.com",
		DataDir: t.TempDir(), Store: store.NewMemory(), Repo: repo, Backend: be,
		Now: func() time.Time { return now },
	})
	probed := &[]string{}
	m.ready = func(_ context.Context, alias string, _ config.HealthCheck) error {
		*probed = append(*probed, alias)
		return nil
	}
	return m, be, repo, probed
}

func TestLiteVariantSpawn(t *testing.T) {
	now := time.Now()
	m, be, repo, probed := newLiteManager(t, now)
	ctx := context.Background()

	repo.baseChanged = []string{"apps/frontend/src/App.tsx"} // frontend-only → lite
	_ = m.Notify(ctx, 1, "b", "sha1", "opened")
	if _, err := m.EnsureUp(ctx, 1); err != nil {
		t.Fatal(err)
	}

	e, _, _ := m.store.Get("demo", 1)
	if !e.Lite {
		t.Fatal("expected lite variant for a frontend-only diff")
	}
	if got := composeProfiles(be.upSpecs[len(be.upSpecs)-1]); got != "" {
		t.Fatalf("lite COMPOSE_PROFILES = %q, want empty (no profiles)", got)
	}
	if len(*probed) != 1 || (*probed)[0] != "demo-pr-1-web" {
		t.Fatalf("lite health probed %v, want only [demo-pr-1-web]", *probed)
	}
	if _, err := os.Stat(filepath.Join(repo.dir, "pr-1", "cfg.json")); err != nil {
		t.Fatalf("expected lite inject file cfg.json: %v", err)
	}
}

func TestFullVariantWhenDiffEscapesLite(t *testing.T) {
	now := time.Now()
	m, be, repo, probed := newLiteManager(t, now)
	ctx := context.Background()

	// Touches a backend path too → not lite.
	repo.baseChanged = []string{"apps/frontend/src/App.tsx", "apps/backend/lib/x.ex"}
	_ = m.Notify(ctx, 2, "b", "sha1", "opened")
	if _, err := m.EnsureUp(ctx, 2); err != nil {
		t.Fatal(err)
	}

	e, _, _ := m.store.Get("demo", 2)
	if e.Lite {
		t.Fatal("expected full variant when the diff touches backend paths")
	}
	if got := composeProfiles(be.upSpecs[len(be.upSpecs)-1]); got != "fullstack" {
		t.Fatalf("full COMPOSE_PROFILES = %q, want fullstack", got)
	}
	if len(*probed) != 2 { // both web and api health gates
		t.Fatalf("full health probed %v, want web+api", *probed)
	}
}

func TestPushFlipFullToLiteRebuilds(t *testing.T) {
	now := time.Now()
	m, be, repo, _ := newLiteManager(t, now)
	ctx := context.Background()

	// Start full (backend touched).
	repo.baseChanged = []string{"apps/backend/lib/x.ex"}
	_ = m.Notify(ctx, 3, "b", "sha1", "opened")
	if _, err := m.EnsureUp(ctx, 3); err != nil {
		t.Fatal(err)
	}
	ups := len(be.up)

	// Push now confined to frontend → flips to lite, which must rebuild (not a
	// hot reload) because the service set changes.
	repo.changed = []string{"apps/frontend/src/App.tsx"}
	repo.baseChanged = []string{"apps/frontend/src/App.tsx"}
	if err := m.Notify(ctx, 3, "b", "sha2", "synchronize"); err != nil {
		t.Fatal(err)
	}
	if len(be.up) != ups+1 {
		t.Fatalf("expected a rebuild (Up) on lite/full flip, ups went %d -> %d", ups, len(be.up))
	}
	e, _, _ := m.store.Get("demo", 3)
	if !e.Lite {
		t.Fatal("expected env to be lite after the flip")
	}
}

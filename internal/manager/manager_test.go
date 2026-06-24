package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hauxir/suprcow/internal/config"
	"github.com/hauxir/suprcow/internal/engine"
	"github.com/hauxir/suprcow/internal/env"
	"github.com/hauxir/suprcow/internal/store"
)

type fakeRepo struct {
	dir     string
	removed []int
	changed []string // returned by ChangedFiles
}

func (f *fakeRepo) Checkout(_ context.Context, pr int, _ string) (string, error) {
	wt := filepath.Join(f.dir, fmt.Sprintf("pr-%d", pr))
	return wt, os.MkdirAll(wt, 0o755)
}

func (f *fakeRepo) ChangedFiles(_ context.Context, _ int, _, _ string) ([]string, error) {
	return f.changed, nil
}

func (f *fakeRepo) Remove(_ context.Context, pr int) error {
	f.removed = append(f.removed, pr)
	return nil
}

type fakeBackend struct {
	up, stop, down []string
	exec           []string
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

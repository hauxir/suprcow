package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hauxir/suprcow/internal/env"
)

func testStore(t *testing.T, s Store) {
	t.Helper()

	if _, ok, err := s.Get("demo", 1); err != nil || ok {
		t.Fatalf("empty Get: ok=%v err=%v", ok, err)
	}

	e := &env.Environment{
		Project:    "demo",
		PR:         123,
		Branch:     "feat/x",
		SHA:        "abc",
		Status:     env.StatusRunning,
		LastAccess: time.Now().Truncate(time.Second),
	}
	if err := s.Put(e); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok, err := s.Get("demo", 123)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Branch != "feat/x" || got.Status != env.StatusRunning {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List: n=%d err=%v", len(list), err)
	}

	if err := s.Delete("demo", 123); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := s.Get("demo", 123); ok {
		t.Fatal("expected deleted")
	}
}

func TestMemory(t *testing.T) { testStore(t, NewMemory()) }

func TestBolt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenBolt(path)
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}
	defer s.Close()
	testStore(t, s)
}

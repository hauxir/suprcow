package store

import (
	"sync"

	"github.com/hauxir/suprcow/internal/env"
)

// Memory is an in-memory Store for tests and ephemeral runs.
type Memory struct {
	mu sync.RWMutex
	m  map[string]env.Environment
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{m: make(map[string]env.Environment)}
}

func (s *Memory) Get(project string, pr int) (*env.Environment, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.m[env.Key(project, pr)]
	if !ok {
		return nil, false, nil
	}
	cp := e
	return &cp, true, nil
}

func (s *Memory) Put(e *env.Environment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[e.Key()] = *e
	return nil
}

func (s *Memory) Delete(project string, pr int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, env.Key(project, pr))
	return nil
}

func (s *Memory) List() ([]*env.Environment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*env.Environment, 0, len(s.m))
	for _, e := range s.m {
		cp := e
		out = append(out, &cp)
	}
	return out, nil
}

func (s *Memory) Close() error { return nil }

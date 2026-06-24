// Package store persists Environment records so the daemon survives restarts
// and can reconcile against Docker on boot.
package store

import "github.com/hauxir/suprcow/internal/env"

// Store is the persistence interface for environments. Implementations must be
// safe for concurrent use.
type Store interface {
	// Get returns the environment for (project, pr), or ok=false if absent.
	Get(project string, pr int) (*env.Environment, bool, error)
	// Put inserts or replaces an environment.
	Put(e *env.Environment) error
	// Delete removes an environment record (used on PR teardown).
	Delete(project string, pr int) error
	// List returns all known environments.
	List() ([]*env.Environment, error)
	// Close releases resources.
	Close() error
}

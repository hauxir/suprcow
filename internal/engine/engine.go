// Package engine abstracts the container backend that runs a stack. v1 ships a
// Docker Compose implementation; the Backend interface leaves room for other
// backends (Nomad, k8s) without touching the router or lifecycle layers.
package engine

import "context"

// RunState is the coarse running state of a stack's containers.
type RunState string

const (
	// StateAbsent means no containers exist for the project.
	StateAbsent RunState = "absent"
	// StateStopped means containers exist but none are running.
	StateStopped RunState = "stopped"
	// StateRunning means at least one container is running.
	StateRunning RunState = "running"
)

// Spec describes how to bring a stack up.
type Spec struct {
	// Project is the compose project name (docker compose -p).
	Project string
	// WorkingDir is the directory the compose files are resolved against.
	WorkingDir string
	// ComposeFiles are passed as -f, in order (base first, overrides last).
	ComposeFiles []string
	// EnvFiles are passed as --env-file, in order.
	EnvFiles []string
	// Env is extra KEY=VAL pairs for the compose process (used for ${VAR}
	// interpolation in the compose files).
	Env []string
}

// Backend runs and tears down stacks.
type Backend interface {
	// Up builds and starts the stack (idempotent; safe to re-run for restarts).
	Up(ctx context.Context, spec Spec) error
	// Stop stops the stack's containers but keeps volumes (idle/LRU eviction).
	Stop(ctx context.Context, project string) error
	// Down removes containers AND volumes (teardown on PR close).
	Down(ctx context.Context, project string) error
	// RemoveVolumes deletes the given named volumes (scoped <project>_<name>)
	// after detaching the stack's containers, leaving every other volume intact.
	// Used on rebuild so a subsequent Up re-seeds them from the freshly built
	// image (Docker only seeds an EMPTY named volume).
	RemoveVolumes(ctx context.Context, project string, names []string) error
	// State reports the coarse running state of the project's containers.
	State(ctx context.Context, project string) (RunState, error)
	// Exec runs a command in a running service container (e.g. a migration
	// after an auto-pull).
	Exec(ctx context.Context, project, service string, command []string) error
}

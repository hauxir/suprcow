// Package env defines the core domain model: an Environment is one PR's
// isolated stack, tracked through its lifecycle from "known" to "running" to
// "torn down".
package env

import (
	"fmt"
	"time"
)

// Status is the lifecycle state of an environment.
type Status string

const (
	// StatusPending means a webhook told us about the PR but no stack exists yet
	// (lazy: we build on the first request to its URL).
	StatusPending Status = "pending"
	// StatusStopped means the stack was built and its volumes exist, but its
	// containers are stopped (idle-reaped or LRU-evicted). It warm-restarts fast.
	StatusStopped Status = "stopped"
	// StatusStarting means a spawn/restart is in progress.
	StatusStarting Status = "starting"
	// StatusRunning means containers are up and health gates passed.
	StatusRunning Status = "running"
	// StatusError means the last lifecycle action failed; see Message.
	StatusError Status = "error"
)

// Environment is one PR's isolated stack.
type Environment struct {
	// Project is the logical project key (e.g. "demo").
	Project string `json:"project"`
	// PR is the pull request number.
	PR int `json:"pr"`
	// Branch is the PR head branch.
	Branch string `json:"branch"`
	// SHA is the commit currently checked out (updated on auto-pull).
	SHA string `json:"sha"`

	// Status is the current lifecycle state.
	Status Status `json:"status"`
	// Message holds the last error or status detail (for the waiting page / UI).
	Message string `json:"message,omitempty"`

	// Worktree is the absolute path to this PR's git checkout.
	Worktree string `json:"worktree,omitempty"`

	// Lite records whether this env last spawned as the reduced "lite" variant
	// (see config.Lite). Persisted so health gates, inject, update hooks, and the
	// active compose profiles pick the right variant on warm restarts without
	// recomputing the diff. Recomputed from scratch on every (re)spawn.
	Lite bool `json:"lite,omitempty"`

	// LastAccess is when traffic last hit this env (drives idle reaping + LRU).
	LastAccess time.Time `json:"last_access"`
	// CreatedAt is when the env was first recorded.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the env record last changed.
	UpdatedAt time.Time `json:"updated_at"`
}

// ComposeProject is the docker compose -p project name for this env. It is
// unique per (project, PR) so stacks never collide.
func (e *Environment) ComposeProject() string {
	return fmt.Sprintf("%s-pr-%d", e.Project, e.PR)
}

// IsLive reports whether the env is running or mid-spawn.
func (e *Environment) IsLive() bool {
	return e.Status == StatusRunning || e.Status == StatusStarting
}

// Key uniquely identifies an environment within the daemon.
func Key(project string, pr int) string {
	return fmt.Sprintf("%s/%d", project, pr)
}

// Key returns this environment's unique key.
func (e *Environment) Key() string { return Key(e.Project, e.PR) }

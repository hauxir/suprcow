// Package shell wraps external command execution behind a small interface so
// the git and engine layers can be unit-tested without real git/docker.
package shell

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Runner executes external commands.
type Runner interface {
	// Run executes name+args in dir (cwd if empty) with extra env appended to
	// the process environment, returning combined stdout.
	Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error)
}

// Exec is the real Runner backed by os/exec.
type Exec struct{}

// Run executes the command and returns its stdout, wrapping failures with
// stderr for diagnosability.
func (Exec) Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %v: %w: %s", name, args, err, stderr.String())
	}
	return stdout.String(), nil
}

package engine

import (
	"context"
	"strings"

	"github.com/hauxir/suprcow/internal/shell"
)

// Compose is a Backend that shells out to `docker compose`.
type Compose struct {
	run shell.Runner
}

// NewCompose returns a Compose backend (use shell.Exec{} in production).
func NewCompose(run shell.Runner) *Compose {
	return &Compose{run: run}
}

// composeProjectLabel is the label Docker Compose sets on every container it
// creates, used to query stack state in a version-stable way.
const composeProjectLabel = "com.docker.compose.project"

func (c *Compose) baseArgs(spec Spec) []string {
	args := []string{"compose", "-p", spec.Project}
	for _, f := range spec.ComposeFiles {
		args = append(args, "-f", f)
	}
	for _, f := range spec.EnvFiles {
		args = append(args, "--env-file", f)
	}
	return args
}

func (c *Compose) Up(ctx context.Context, spec Spec) error {
	args := append(c.baseArgs(spec), "up", "-d", "--build", "--remove-orphans")
	_, err := c.run.Run(ctx, spec.WorkingDir, spec.Env, "docker", args...)
	return err
}

func (c *Compose) Stop(ctx context.Context, project string) error {
	_, err := c.run.Run(ctx, "", nil, "docker", "compose", "-p", project, "stop")
	return err
}

func (c *Compose) Down(ctx context.Context, project string) error {
	_, err := c.run.Run(ctx, "", nil, "docker", "compose", "-p", project, "down", "-v", "--remove-orphans")
	return err
}

func (c *Compose) Exec(ctx context.Context, project, service string, command []string) error {
	args := append([]string{"compose", "-p", project, "exec", "-T", service}, command...)
	_, err := c.run.Run(ctx, "", nil, "docker", args...)
	return err
}

// State queries container state by the compose project label rather than
// `compose ps` JSON, which keeps it stable across Compose versions.
func (c *Compose) State(ctx context.Context, project string) (RunState, error) {
	filter := "label=" + composeProjectLabel + "=" + project
	running, err := c.run.Run(ctx, "", nil, "docker", "ps", "-q", "--filter", filter)
	if err != nil {
		return StateAbsent, err
	}
	if strings.TrimSpace(running) != "" {
		return StateRunning, nil
	}
	all, err := c.run.Run(ctx, "", nil, "docker", "ps", "-aq", "--filter", filter)
	if err != nil {
		return StateAbsent, err
	}
	if strings.TrimSpace(all) != "" {
		return StateStopped, nil
	}
	return StateAbsent, nil
}

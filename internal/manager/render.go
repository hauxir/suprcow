package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hauxir/suprcow/internal/config"
	"gopkg.in/yaml.v3"
)

// overrideFileName is the compose override suprcow generates per stack. It is
// layered after the user's compose file (-f base -f override) so we never
// modify the user's file: we only add per-service env and attach exposed
// services to the shared network with a stable alias the daemon can reach.
const overrideFileName = "docker-compose.suprcow.yml"

// serviceAlias is the DNS name the daemon uses to reach a service on the shared
// network, e.g. "demo-pr-123-api".
func serviceAlias(project string, pr int, service string) string {
	return fmt.Sprintf("%s-pr-%d-%s", project, pr, service)
}

// buildOverride constructs the compose override document for a PR environment.
func (m *Manager) buildOverride(rc *config.RenderContext, pr int) (map[string]any, error) {
	services := map[string]any{}

	ensure := func(name string) map[string]any {
		if existing, ok := services[name].(map[string]any); ok {
			return existing
		}
		s := map[string]any{}
		services[name] = s
		return s
	}

	// Every service the daemon must reach (exposed defaults + route targets)
	// gets attached to the shared network with a stable alias.
	for _, name := range m.cfg.AliasServices() {
		svc := ensure(name)
		svc["networks"] = map[string]any{
			"default": nil,
			m.sharedNetwork: map[string]any{
				"aliases": []string{serviceAlias(m.project, pr, name)},
			},
		}
	}

	// Injected env (with template vars resolved) is added per service.
	for name, inj := range m.cfg.Inject {
		if len(inj.Env) == 0 {
			continue
		}
		resolved, err := rc.ResolveEnv(inj.Env)
		if err != nil {
			return nil, fmt.Errorf("inject env for %s: %w", name, err)
		}
		ensure(name)["environment"] = resolved
	}

	doc := map[string]any{
		"services": services,
		"networks": map[string]any{
			m.sharedNetwork: map[string]any{
				"external": true,
				"name":     m.sharedNetwork,
			},
		},
	}
	return doc, nil
}

// writeOverride renders the override file into the worktree and returns its path.
func (m *Manager) writeOverride(rc *config.RenderContext, pr int, worktree string) (string, error) {
	doc, err := m.buildOverride(rc, pr)
	if err != nil {
		return "", err
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	path := filepath.Join(worktree, overrideFileName)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return "", fmt.Errorf("write override: %w", err)
	}
	return path, nil
}

// writeInjectFiles renders configured inject files into the worktree, with
// template vars resolved. Destinations are constrained to the worktree.
func (m *Manager) writeInjectFiles(rc *config.RenderContext, worktree string) error {
	for svc, inj := range m.cfg.Inject {
		for _, f := range inj.Files {
			content, err := rc.Resolve(f.Content)
			if err != nil {
				return fmt.Errorf("inject file %s (%s): %w", f.Dest, svc, err)
			}
			dest, err := safeJoin(worktree, f.Dest)
			if err != nil {
				return fmt.Errorf("inject file %s (%s): %w", f.Dest, svc, err)
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
				return fmt.Errorf("write inject file %s: %w", dest, err)
			}
		}
	}
	return nil
}

// safeJoin joins rel onto base, rejecting paths that escape base (e.g. "../").
func safeJoin(base, rel string) (string, error) {
	joined := filepath.Join(base, rel)
	rp, err := filepath.Rel(base, joined)
	if err != nil || rp == ".." || strings.HasPrefix(rp, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the checkout", rel)
	}
	return joined, nil
}

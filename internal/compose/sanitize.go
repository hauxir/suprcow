// Package compose validates a user-supplied (PR-authored) compose file before
// suprcow runs it with access to the host Docker socket. A preview stack only
// needs ordinary app primitives (images, project-relative bind mounts, named
// volumes, networks); anything that can escape the container to the host —
// privileged mode, added capabilities, devices, host namespaces, or host
// bind-mounts (e.g. `/` or the docker socket) — is rejected so a malicious PR
// can't root the host the daemon runs on.
package compose

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// blockedKeys are service fields whose mere presence (non-empty) grants
// elevated host access and is never needed by a preview stack.
var blockedKeys = []string{
	"privileged",
	"cap_add",
	"devices",
	"device_cgroup_rules",
	"security_opt",
	"userns_mode",
	"cgroup_parent",
}

// namespaceKeys are blocked only when they share a host (or another container's)
// namespace.
var namespaceKeys = []string{"pid", "ipc", "uts", "cgroup", "network_mode"}

// Sanitize parses a compose file and returns an error describing the first
// host-escape risk it finds, or nil if the file is safe to run.
func Sanitize(data []byte) error {
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse compose: %w", err)
	}
	for name, svc := range doc.Services {
		if err := checkService(name, svc); err != nil {
			return err
		}
	}
	return nil
}

func checkService(name string, svc map[string]any) error {
	for _, k := range blockedKeys {
		if v, ok := svc[k]; ok && !isEmpty(v) {
			return fmt.Errorf("service %q: %q is not allowed in a preview (host-escape risk)", name, k)
		}
	}
	for _, k := range namespaceKeys {
		if s, ok := svc[k].(string); ok {
			if strings.HasPrefix(s, "host") || strings.HasPrefix(s, "container:") {
				return fmt.Errorf("service %q: %s: %q is not allowed in a preview (host-escape risk)", name, k, s)
			}
		}
	}
	if v, ok := svc["volumes"]; ok {
		if vols, ok := v.([]any); ok {
			for _, item := range vols {
				if err := checkVolume(name, item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// checkVolume rejects bind mounts of host paths (absolute, home, or escaping the
// project dir). Project-relative paths and named volumes are allowed.
func checkVolume(svc string, item any) error {
	var source string
	switch m := item.(type) {
	case string:
		parts := strings.SplitN(m, ":", 2)
		if len(parts) < 2 {
			return nil // anonymous volume, no host source
		}
		source = parts[0]
	case map[string]any:
		typ, _ := m["type"].(string)
		if typ != "" && typ != "bind" {
			return nil // volume / tmpfs / npipe — not a host bind
		}
		source, _ = m["source"].(string)
	default:
		return nil
	}
	return checkBindSource(svc, source)
}

func checkBindSource(svc, src string) error {
	if src == "" {
		return nil
	}
	// Named volume: no path separators → managed by Docker, safe.
	if !strings.ContainsAny(src, "/.~") {
		return nil
	}
	// Project-relative paths are fine, as long as they don't escape upward.
	if src == "." || strings.HasPrefix(src, "./") {
		if strings.Contains(src, "..") {
			return fmt.Errorf("service %q: bind mount %q escapes the project directory", svc, src)
		}
		return nil
	}
	// Absolute paths, ~, ../, and env-expanded sources are host paths → blocked.
	return fmt.Errorf("service %q: host bind mount %q is not allowed in a preview "+
		"(only project-relative paths and named volumes)", svc, src)
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

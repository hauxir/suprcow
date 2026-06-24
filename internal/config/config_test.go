package config

import (
	"testing"
	"time"
)

const sample = `
repo: github.com/me/myapp
compose: docker-compose.yml
env_file: .preview.env

expose:
  - service: web
    subdomain: "pr-{n}"
    port: 5173
  - service: api
    subdomain: "api-pr-{n}"
    port: 4000

inject:
  web:
    env:
      VITE_API_URL: "${PREVIEW_URL(api)}"
      PR: "pr ${PR_NUMBER}"
    files:
      - dest: src/config.json
        content: '{ "apiHost": "${PREVIEW_HOST(api)}" }'

health:
  api: { http: "/health", timeout: 180s }
  web: { tcp: 5173, timeout: 120s }

idle_timeout: 30m
max_running: 5
`

func TestParseSample(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Repo != "github.com/me/myapp" {
		t.Errorf("repo = %q", c.Repo)
	}
	if len(c.Expose) != 2 {
		t.Fatalf("expose count = %d", len(c.Expose))
	}
	if c.MaxRunning != 5 {
		t.Errorf("max_running = %d", c.MaxRunning)
	}
	if c.IdleTimeout.Duration() != 30*time.Minute {
		t.Errorf("idle_timeout = %v", c.IdleTimeout.Duration())
	}
	if got := c.Health["api"].Timeout.Duration(); got != 180*time.Second {
		t.Errorf("api health timeout = %v", got)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(`
repo: github.com/me/app
expose:
  - service: web
    subdomain: "pr-{n}"
    port: 3000
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Compose != DefaultComposeFile {
		t.Errorf("compose default = %q", c.Compose)
	}
	if c.MaxRunning != DefaultMaxRunning {
		t.Errorf("max_running default = %d", c.MaxRunning)
	}
	if c.IdleTimeout.Duration() != DefaultIdleTimeout {
		t.Errorf("idle_timeout default = %v", c.IdleTimeout.Duration())
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]string{
		"missing repo": `
expose:
  - { service: web, subdomain: "pr-{n}", port: 80 }
`,
		"no expose": `
repo: github.com/me/app
`,
		"subdomain without token": `
repo: github.com/me/app
expose:
  - { service: web, subdomain: "web", port: 80 }
`,
		"duplicate service": `
repo: github.com/me/app
expose:
  - { service: web, subdomain: "pr-{n}", port: 80 }
  - { service: web, subdomain: "alt-{n}", port: 81 }
`,
		"duplicate subdomain": `
repo: github.com/me/app
expose:
  - { service: web, subdomain: "pr-{n}", port: 80 }
  - { service: api, subdomain: "pr-{n}", port: 81 }
`,
		"bad port": `
repo: github.com/me/app
expose:
  - { service: web, subdomain: "pr-{n}", port: 0 }
`,
		"health both http and tcp": `
repo: github.com/me/app
expose:
  - { service: web, subdomain: "pr-{n}", port: 80 }
health:
  web: { http: "/", tcp: 80, timeout: 10s }
`,
		"unknown field": `
repo: github.com/me/app
expoze:
  - { service: web, subdomain: "pr-{n}", port: 80 }
`,
	}
	for name, yml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(yml)); err == nil {
				t.Fatalf("expected error for %q, got nil", name)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rc := c.RenderContext(123, "feat/x", "abc123", "preview.example.com")

	if h, _ := rc.Host("api"); h != "api-pr-123.preview.example.com" {
		t.Errorf("Host(api) = %q", h)
	}

	got, err := rc.Resolve("${PREVIEW_URL(api)}")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "https://api-pr-123.preview.example.com" {
		t.Errorf("resolved = %q", got)
	}

	env, err := rc.ResolveEnv(c.Inject["web"].Env)
	if err != nil {
		t.Fatalf("ResolveEnv: %v", err)
	}
	if env["VITE_API_URL"] != "https://api-pr-123.preview.example.com" {
		t.Errorf("VITE_API_URL = %q", env["VITE_API_URL"])
	}
	if env["PR"] != "pr 123" {
		t.Errorf("PR = %q", env["PR"])
	}
}

func TestResolveUnknownVarErrors(t *testing.T) {
	c, _ := Parse([]byte(sample))
	rc := c.RenderContext(1, "b", "s", "d.example.com")
	if _, err := rc.Resolve("${NOPE}"); err == nil {
		t.Error("expected error for unknown variable")
	}
	if _, err := rc.Resolve("${PREVIEW_HOST(ghost)}"); err == nil {
		t.Error("expected error for non-exposed service")
	}
}

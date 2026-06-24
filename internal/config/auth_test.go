package config

import "testing"

func TestAuthEnabledByDefault(t *testing.T) {
	// No auth block at all → gate is on with GitHub/collaborators defaults.
	c, err := Parse([]byte(`
repo: github.com/me/app
expose:
  - { service: web, subdomain: "pr-{n}", port: 80 }
`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.AuthEnabled() {
		t.Fatal("auth should be enabled by default")
	}
	if c.Auth.Provider != "github" || c.Auth.Allow != "collaborators" {
		t.Fatalf("defaults = provider %q allow %q", c.Auth.Provider, c.Auth.Allow)
	}
}

func TestAuthOptOut(t *testing.T) {
	c, err := Parse([]byte(`
repo: github.com/me/app
expose:
  - { service: web, subdomain: "pr-{n}", port: 80 }
auth:
  disabled: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.AuthEnabled() {
		t.Fatal("auth should be disabled when opted out")
	}
}

func TestAuthInvalidAllow(t *testing.T) {
	_, err := Parse([]byte(`
repo: github.com/me/app
expose:
  - { service: web, subdomain: "pr-{n}", port: 80 }
auth:
  allow: everyone
`))
	if err == nil {
		t.Fatal("expected error for invalid auth.allow")
	}
}

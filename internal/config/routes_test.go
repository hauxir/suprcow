package config

import "testing"

const sameOrigin = `
repo: github.com/me/app
expose:
  - service: web
    subdomain: "pr-{n}"
    port: 443
    routes:
      - { path: "/gql-ws", service: api, port: 4000 }
      - { method: POST, path: "/", service: api, port: 4000 }
      - { path_prefix: "/webhooks/", service: api, port: 4000 }
`

func TestRouteResolve(t *testing.T) {
	c, err := Parse([]byte(sameOrigin))
	if err != nil {
		t.Fatal(err)
	}
	e := c.Expose[0]

	tests := []struct {
		method, path string
		wantSvc      string
		wantPort     int
	}{
		{"GET", "/", "web", 443},                   // SPA root
		{"POST", "/", "api", 4000},                 // GraphQL HTTP
		{"GET", "/gql-ws", "api", 4000},            // subscriptions WS
		{"GET", "/assets/core.js", "web", 443},     // static asset
		{"POST", "/webhooks/stripe", "api", 4000},  // prefix match
		{"GET", "/webhooks/anything", "api", 4000}, // prefix, any method
	}
	for _, tc := range tests {
		svc, port := e.Resolve(tc.method, tc.path)
		if svc != tc.wantSvc || port != tc.wantPort {
			t.Errorf("Resolve(%s %s) = (%s,%d), want (%s,%d)",
				tc.method, tc.path, svc, port, tc.wantSvc, tc.wantPort)
		}
	}
}

func TestAliasServicesIncludesRouteTargets(t *testing.T) {
	c, err := Parse([]byte(sameOrigin))
	if err != nil {
		t.Fatal(err)
	}
	got := c.AliasServices()
	want := []string{"web", "api"}
	if len(got) != len(want) {
		t.Fatalf("AliasServices = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AliasServices = %v, want %v", got, want)
		}
	}
}

func TestRouteValidation(t *testing.T) {
	bad := map[string]string{
		"route missing service": `
repo: github.com/me/app
expose:
  - service: web
    subdomain: "pr-{n}"
    port: 443
    routes:
      - { path: "/gql", port: 4000 }
`,
		"route missing matcher": `
repo: github.com/me/app
expose:
  - service: web
    subdomain: "pr-{n}"
    port: 443
    routes:
      - { service: api, port: 4000 }
`,
		"route bad port": `
repo: github.com/me/app
expose:
  - service: web
    subdomain: "pr-{n}"
    port: 443
    routes:
      - { path: "/gql", service: api, port: 0 }
`,
	}
	for name, yml := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(yml)); err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}

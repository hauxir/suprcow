// Package config defines the per-project preview.yml schema that tells suprcow
// how to spin up an isolated environment for a pull request: which compose
// services to expose at which subdomains, how services discover each other's
// external URLs, readiness gates, and lifecycle limits.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults applied by Load when a field is omitted.
const (
	DefaultComposeFile = "docker-compose.yml"
	DefaultMaxRunning  = 10
	DefaultIdleTimeout = 30 * time.Minute
)

// Config is a parsed preview.yml. It is intentionally stack-agnostic: it never
// names a language or framework, only compose services and how to wire them.
type Config struct {
	// Repo is the git remote suprcow clones/fetches PR branches from,
	// e.g. "github.com/me/myapp".
	Repo string `yaml:"repo"`
	// Compose is the path (within the repo) to the compose file used as-is.
	Compose string `yaml:"compose"`
	// EnvFile is an optional path to a dotenv file injected into every stack.
	EnvFile string `yaml:"env_file"`

	// Expose lists the services reachable from the outside and their subdomains.
	Expose []ExposeRule `yaml:"expose"`
	// Inject wires per-service environment/config so services can discover each
	// other's *external* preview URLs. Keyed by compose service name.
	Inject map[string]Injection `yaml:"inject"`
	// Health gates readiness before suprcow proxies traffic to a service.
	Health map[string]HealthCheck `yaml:"health"`

	// IdleTimeout is how long a stack may sit without traffic before suprcow
	// stops it (volumes are kept so it warm-restarts on the next request).
	IdleTimeout Duration `yaml:"idle_timeout"`
	// MaxRunning caps how many stacks may run at once; exceeding it evicts the
	// least-recently-accessed running stack first.
	MaxRunning int `yaml:"max_running"`

	// Auth optionally gates preview access behind GitHub repo/org access.
	Auth *Auth `yaml:"auth"`

	// RebuildOn lists changed paths that force a full rebuild+restart on push
	// instead of an in-place hot reload (dependency manifests, Dockerfiles, the
	// compose file). Matched by basename, exact path, or directory prefix.
	// Defaults to common lockfiles/manifests when empty.
	RebuildOn []string `yaml:"rebuild_on"`
	// ResetVolumesOnRebuild lists named volumes (by their key in the compose
	// file) to delete and re-seed from the image whenever a rebuild_on change
	// triggers an image rebuild. Use it for volumes that carry content baked
	// into the image (e.g. a prebuilt node_modules): Docker only seeds an EMPTY
	// named volume, so without this a rebuilt image's content never reaches an
	// existing PR's already-populated volume. All other volumes (databases,
	// build caches) are left untouched, so persistent state survives a rebuild.
	ResetVolumesOnRebuild []string `yaml:"reset_volumes_on_rebuild"`
	// OnUpdate runs commands inside service containers after a push updates a
	// running env (e.g. database migrations).
	OnUpdate []UpdateHook `yaml:"on_update"`
	// ReloadTrigger lists HTTP endpoints suprcow GETs after a hot-reload push, to
	// nudge a dev server that only recompiles on request (e.g. Phoenix's
	// code_reloader). Without it, a WebSocket-only backend never sees the change.
	ReloadTrigger []ReloadHTTP `yaml:"reload_trigger"`
	// CommentOnPR controls posting/updating a preview-URL comment on the PR.
	// Enabled by default; needs the GitHub App's Pull requests: Write permission.
	// nil = enabled.
	CommentOnPR *bool `yaml:"comment_on_pr"`
}

// CommentEnabled reports whether suprcow should comment the preview URL on PRs
// (default true).
func (c *Config) CommentEnabled() bool {
	return c.CommentOnPR == nil || *c.CommentOnPR
}

// ReloadHTTP is an endpoint pinged after a hot-reload to fire a request-driven
// recompile (Phoenix code_reloader and the like).
type ReloadHTTP struct {
	Service string `yaml:"service"`
	Port    int    `yaml:"port"`
	Path    string `yaml:"path"`
}

// UpdateHook is a command run in a service container after an auto-pull.
type UpdateHook struct {
	// Service is the compose service to run the command in.
	Service string `yaml:"service"`
	// Run is the shell command, e.g. "npm run migrate".
	Run string `yaml:"run"`
}

// DefaultRebuildOn is the built-in set of files whose changes force a rebuild
// (they can't be hot-reloaded). Used when RebuildOn is empty.
var DefaultRebuildOn = []string{
	"package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml",
	"go.mod", "go.sum",
	"requirements.txt", "pyproject.toml", "poetry.lock", "uv.lock",
	"Gemfile", "Gemfile.lock",
	"composer.json", "composer.lock",
	"Cargo.toml", "Cargo.lock",
}

// Auth configures the access gate. It is ENABLED BY DEFAULT (GitHub, gated by
// repo access) — set Disabled to opt out and serve previews openly. Credentials
// (client id/secret, session key) are supplied via environment variables, never
// the config file.
type Auth struct {
	// Disabled opts out of the access gate, leaving previews open to anyone.
	Disabled bool `yaml:"disabled"`
	// Provider is the identity provider; only "github" (a GitHub App) is
	// supported, which also delivers webhooks and clones private repos.
	Provider string `yaml:"provider"`
	// Repo is the "owner/name" whose access governs previews; defaults to the
	// top-level repo.
	Repo string `yaml:"repo"`
	// Allow is the authorization rule: "collaborators" (default) grants anyone
	// who can access the repo; "org-members" grants members of Org.
	Allow string `yaml:"allow"`
	// Org is the GitHub org for the org-members rule; defaults to the repo owner.
	Org string `yaml:"org"`
	// CookieDomain scopes the session cookie so one login covers every PR
	// subdomain, e.g. ".preview.example.com"; defaults to "."+base domain.
	CookieDomain string `yaml:"cookie_domain"`
}

// ExposeRule maps one compose service to an externally reachable subdomain.
type ExposeRule struct {
	// Service is the compose service this subdomain routes to by default.
	Service string `yaml:"service"`
	// Subdomain is a pattern containing "{n}" (the PR number), e.g. "pr-{n}"
	// or "api-pr-{n}". The base domain is appended at runtime.
	Subdomain string `yaml:"subdomain"`
	// Port is the container port suprcow proxies to by default.
	Port int `yaml:"port"`
	// Routes optionally fold other services into THIS host by path/method, so a
	// frontend and its API can share one origin (no cross-origin auth/CORS).
	// Routes are evaluated in order; the first match wins, else the default
	// Service/Port handles the request.
	Routes []RouteRule `yaml:"routes"`
}

// RouteRule sends requests matching a method and/or path to another service on
// the same subdomain. At least one of Method, Path, or PathPrefix must be set.
type RouteRule struct {
	// Method matches the HTTP method (case-insensitive); empty matches any.
	Method string `yaml:"method"`
	// Path matches the exact request path; empty matches any.
	Path string `yaml:"path"`
	// PathPrefix matches a request path prefix; empty matches any.
	PathPrefix string `yaml:"path_prefix"`
	// Service is the target compose service.
	Service string `yaml:"service"`
	// Port is the target container port.
	Port int `yaml:"port"`
}

// Resolve returns the target service and port for a request's method and path,
// honoring the rule's Routes (first match wins) and falling back to the default.
func (e ExposeRule) Resolve(method, path string) (service string, port int) {
	for _, r := range e.Routes {
		if r.matches(method, path) {
			return r.Service, r.Port
		}
	}
	return e.Service, e.Port
}

func (r RouteRule) matches(method, path string) bool {
	if r.Method == "" && r.Path == "" && r.PathPrefix == "" {
		return false
	}
	if r.Method != "" && !strings.EqualFold(r.Method, method) {
		return false
	}
	if r.Path != "" && r.Path != path {
		return false
	}
	if r.PathPrefix != "" && !strings.HasPrefix(path, r.PathPrefix) {
		return false
	}
	return true
}

// Injection describes how to feed a service its environment-specific wiring.
type Injection struct {
	// Env is a map of environment variables (values may use template vars).
	Env map[string]string `yaml:"env"`
	// Files are rendered into the service's checkout before the stack starts.
	Files []InjectFile `yaml:"files"`
}

// InjectFile is a file rendered (with template vars resolved) into a path
// relative to the repo checkout, e.g. a frontend config that needs the API URL.
type InjectFile struct {
	Dest    string `yaml:"dest"`
	Content string `yaml:"content"`
}

// HealthCheck is a readiness gate. Exactly one of HTTP or TCP should be set.
type HealthCheck struct {
	// HTTP is a path polled until it returns 2xx, e.g. "/health".
	HTTP string `yaml:"http"`
	// TCP is a port polled until it accepts a connection.
	TCP int `yaml:"tcp"`
	// Timeout bounds how long to wait for readiness.
	Timeout Duration `yaml:"timeout"`
}

// Duration is a time.Duration that unmarshals from a Go duration string
// ("30m", "180s") in YAML.
type Duration time.Duration

// UnmarshalYAML parses a duration string such as "30m".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the underlying time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Load reads and validates a preview.yml from path, applying defaults.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse validates preview.yml content from memory, applying defaults.
func Parse(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // reject unknown keys so typos are caught early
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Compose == "" {
		c.Compose = DefaultComposeFile
	}
	if c.MaxRunning == 0 {
		c.MaxRunning = DefaultMaxRunning
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = Duration(DefaultIdleTimeout)
	}
	if len(c.RebuildOn) == 0 {
		c.RebuildOn = DefaultRebuildOn
	}
	for i := range c.ReloadTrigger {
		if c.ReloadTrigger[i].Path == "" {
			c.ReloadTrigger[i].Path = "/"
		}
	}
	// Auth is on by default: an absent block means "enabled with defaults".
	if c.Auth == nil {
		c.Auth = &Auth{}
	}
	if c.Auth.Provider == "" {
		c.Auth.Provider = "github"
	}
	if c.Auth.Allow == "" {
		c.Auth.Allow = "collaborators"
	}
}

// AuthEnabled reports whether the access gate should run (the default).
func (c *Config) AuthEnabled() bool {
	return c.Auth != nil && !c.Auth.Disabled
}

// Validate checks the config for internal consistency. It cannot verify that
// referenced services exist in the compose file (that needs the compose file),
// but it catches every error suprcow can detect from preview.yml alone.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Repo) == "" {
		return fmt.Errorf("repo is required")
	}
	if len(c.Expose) == 0 {
		return fmt.Errorf("at least one expose rule is required")
	}
	if c.MaxRunning < 1 {
		return fmt.Errorf("max_running must be >= 1, got %d", c.MaxRunning)
	}
	if c.IdleTimeout.Duration() <= 0 {
		return fmt.Errorf("idle_timeout must be positive")
	}

	seenSub := map[string]bool{}
	seenSvc := map[string]bool{}
	for i, e := range c.Expose {
		if e.Service == "" {
			return fmt.Errorf("expose[%d]: service is required", i)
		}
		if seenSvc[e.Service] {
			return fmt.Errorf("expose[%d]: service %q exposed more than once", i, e.Service)
		}
		seenSvc[e.Service] = true

		if e.Subdomain == "" {
			return fmt.Errorf("expose[%d] (%s): subdomain is required", i, e.Service)
		}
		if !strings.Contains(e.Subdomain, prToken) {
			return fmt.Errorf("expose[%d] (%s): subdomain %q must contain %q so each PR gets a unique host", i, e.Service, e.Subdomain, prToken)
		}
		if seenSub[e.Subdomain] {
			return fmt.Errorf("expose[%d] (%s): subdomain %q used more than once", i, e.Service, e.Subdomain)
		}
		seenSub[e.Subdomain] = true

		if e.Port < 1 || e.Port > 65535 {
			return fmt.Errorf("expose[%d] (%s): port must be 1-65535, got %d", i, e.Service, e.Port)
		}
		for j, r := range e.Routes {
			if r.Service == "" {
				return fmt.Errorf("expose[%d] (%s) routes[%d]: service is required", i, e.Service, j)
			}
			if r.Port < 1 || r.Port > 65535 {
				return fmt.Errorf("expose[%d] (%s) routes[%d] (%s): port must be 1-65535, got %d", i, e.Service, j, r.Service, r.Port)
			}
			if r.Method == "" && r.Path == "" && r.PathPrefix == "" {
				return fmt.Errorf("expose[%d] (%s) routes[%d] (%s): set at least one of method, path, or path_prefix", i, e.Service, j, r.Service)
			}
		}
	}

	for svc, h := range c.Health {
		if (h.HTTP == "") == (h.TCP == 0) {
			return fmt.Errorf("health[%s]: set exactly one of http or tcp", svc)
		}
		if h.TCP < 0 || h.TCP > 65535 {
			return fmt.Errorf("health[%s]: tcp must be 1-65535, got %d", svc, h.TCP)
		}
	}

	for svc, inj := range c.Inject {
		for j, f := range inj.Files {
			if f.Dest == "" {
				return fmt.Errorf("inject[%s].files[%d]: dest is required", svc, j)
			}
		}
	}

	for i, h := range c.OnUpdate {
		if h.Service == "" || h.Run == "" {
			return fmt.Errorf("on_update[%d]: service and run are required", i)
		}
	}

	for i, r := range c.ReloadTrigger {
		if r.Service == "" {
			return fmt.Errorf("reload_trigger[%d]: service is required", i)
		}
		if r.Port < 1 || r.Port > 65535 {
			return fmt.Errorf("reload_trigger[%d] (%s): port must be 1-65535, got %d", i, r.Service, r.Port)
		}
	}

	if c.Auth != nil {
		switch c.Auth.Provider {
		case "", "github":
		default:
			return fmt.Errorf("auth.provider %q is not supported (only \"github\")", c.Auth.Provider)
		}
		switch c.Auth.Allow {
		case "", "collaborators", "org-members":
		default:
			return fmt.Errorf("auth.allow %q is invalid (use \"collaborators\" or \"org-members\")", c.Auth.Allow)
		}
	}
	return nil
}

// ExposeFor returns the expose rule for a service, or false if not exposed.
func (c *Config) ExposeFor(service string) (ExposeRule, bool) {
	for _, e := range c.Expose {
		if e.Service == service {
			return e, true
		}
	}
	return ExposeRule{}, false
}

// AliasServices returns every service the daemon must be able to reach on the
// shared network: the default service of each expose rule plus any route
// targets folded into the same host. Deduplicated, order-stable.
func (c *Config) AliasServices() []string {
	seen := map[string]bool{}
	var out []string
	add := func(svc string) {
		if svc != "" && !seen[svc] {
			seen[svc] = true
			out = append(out, svc)
		}
	}
	for _, e := range c.Expose {
		add(e.Service)
		for _, r := range e.Routes {
			add(r.Service)
		}
	}
	// Reload-trigger targets must be reachable by the daemon too.
	for _, t := range c.ReloadTrigger {
		add(t.Service)
	}
	return out
}

package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hauxir/suprcow/internal/auth"
	"github.com/hauxir/suprcow/internal/config"
	"github.com/hauxir/suprcow/internal/engine"
	"github.com/hauxir/suprcow/internal/ghapp"
	"github.com/hauxir/suprcow/internal/git"
	"github.com/hauxir/suprcow/internal/manager"
	"github.com/hauxir/suprcow/internal/server"
	"github.com/hauxir/suprcow/internal/shell"
	"github.com/hauxir/suprcow/internal/store"
)

// reapInterval is how often the idle reaper runs.
const reapInterval = time.Minute

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var (
		cfgPath    = fs.String("config", "preview.yml", "path to the project preview.yml")
		baseDomain = fs.String("base-domain", "", "wildcard base domain, e.g. preview.example.com (required)")
		addr       = fs.String("addr", ":8080", "address to listen on")
		dataDir    = fs.String("data-dir", "/var/lib/suprcow", "directory for state + checkouts")
		project    = fs.String("project", "", "logical project name (default: repo basename)")
		repoURL    = fs.String("repo-url", "", "git clone URL (default: derived from config repo)")
		network    = fs.String("network", "suprcow", "shared docker network the daemon and stacks share")
		secretEnv  = fs.String("webhook-secret-env", "SUPRCOW_WEBHOOK_SECRET", "env var holding the webhook HMAC secret")
		authHost   = fs.String("auth-host", "", "control host for the OAuth flow (default: suprcow.<base-domain>)")
	)
	var envFiles multiFlag
	fs.Var(&envFiles, "env-file", "extra env file passed to compose (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *baseDomain == "" {
		fmt.Fprintln(os.Stderr, "serve: --base-domain is required")
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return 1
	}

	proj := *project
	if proj == "" {
		proj = path.Base(strings.TrimSuffix(cfg.Repo, ".git"))
	}
	cloneURL := *repoURL
	if cloneURL == "" {
		cloneURL = deriveCloneURL(cfg.Repo)
	}

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "serve: data dir: %v\n", err)
		return 1
	}

	st, err := store.OpenBolt(filepath.Join(*dataDir, "suprcow.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return 1
	}
	defer st.Close()

	runner := shell.Exec{}
	repo := git.New(cloneURL, filepath.Join(*dataDir, "repos", proj), runner)

	// GitHub App: powers private-repo cloning (installation tokens) and PR
	// comments. Absent App credentials → nil (fine for public repos).
	app, err := buildGitHubApp(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return 1
	}
	var commenter func(context.Context, int, string) error
	if app != nil {
		owner, name, err := splitOwnerRepo(cfg.Repo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "serve: %v\n", err)
			return 1
		}
		repo.AuthHeader = func(ctx context.Context) (string, error) {
			return app.GitAuthHeader(ctx, owner, name)
		}
		if cfg.CommentEnabled() {
			commenter = func(ctx context.Context, pr int, body string) error {
				return app.UpsertComment(ctx, owner, name, pr, body)
			}
		}
	}

	var allEnvFiles []string
	if cfg.EnvFile != "" {
		allEnvFiles = append(allEnvFiles, cfg.EnvFile)
	}
	allEnvFiles = append(allEnvFiles, envFiles...)

	mgr := manager.New(manager.Options{
		Project:       proj,
		Config:        cfg,
		BaseDomain:    *baseDomain,
		DataDir:       *dataDir,
		SharedNetwork: *network,
		EnvFiles:      allEnvFiles,
		Store:         st,
		Repo:          repo,
		Backend:       engine.NewCompose(runner),
	})

	gate, err := buildAuth(cfg, *baseDomain, *authHost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return 1
	}
	if gate == nil {
		log.Printf("WARNING: access gate DISABLED — previews are open to anyone who can reach them")
	} else {
		log.Printf("access gate: GitHub, allow=%s (previews require repo access)", cfg.Auth.Allow)
	}

	srv, err := server.New(server.Options{
		Config:        cfg,
		Manager:       mgr,
		BaseDomain:    *baseDomain,
		WebhookSecret: os.Getenv(*secretEnv),
		Auth:          gate,
		Comment:       commenter,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return 1
	}

	// Idle reaper.
	go func() {
		t := time.NewTicker(reapInterval)
		defer t.Stop()
		for range t.C {
			if err := mgr.ReapIdle(context.Background()); err != nil {
				log.Printf("reap: %v", err)
			}
		}
	}()

	log.Printf("suprcow %s serving project %q on %s (base domain *.%s)", version, proj, *addr, *baseDomain)
	log.Printf("webhook endpoint: %s", server.HookPath)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return 1
	}
	return 0
}

// buildAuth constructs the GitHub access gate from config + environment. Auth
// is on by default; it returns (nil, nil) only when explicitly disabled, and
// errors (refusing to serve unprotected) when enabled but missing credentials.
func buildAuth(cfg *config.Config, baseDomain, authHost string) (*auth.GitHub, error) {
	if !cfg.AuthEnabled() {
		return nil, nil
	}
	if os.Getenv("SUPRCOW_GITHUB_CLIENT_ID") == "" {
		return nil, fmt.Errorf("access gate is enabled by default but SUPRCOW_GITHUB_CLIENT_ID is unset — " +
			"provide GitHub OAuth credentials (SUPRCOW_GITHUB_CLIENT_ID/SECRET + SUPRCOW_SESSION_KEY), " +
			"or opt out explicitly with `auth: { disabled: true }` in preview.yml")
	}
	repo := cfg.Auth.Repo
	if repo == "" {
		repo = cfg.Repo
	}
	cookieDomain := cfg.Auth.CookieDomain
	if cookieDomain == "" {
		cookieDomain = "." + baseDomain
	}
	if authHost == "" {
		authHost = "suprcow." + baseDomain
	}
	return auth.NewGitHub(auth.Options{
		Repo:         repo,
		Allow:        cfg.Auth.Allow,
		Org:          cfg.Auth.Org,
		CookieDomain: cookieDomain,
		AuthBaseURL:  "https://" + authHost,
		BaseDomain:   baseDomain,
		ClientID:     os.Getenv("SUPRCOW_GITHUB_CLIENT_ID"),
		ClientSecret: os.Getenv("SUPRCOW_GITHUB_CLIENT_SECRET"),
		SessionKey:   []byte(os.Getenv("SUPRCOW_SESSION_KEY")),
	})
}

// buildGitHubApp constructs the GitHub App identity (cloning + PR comments) from
// env, or (nil, nil) when no App credentials are set (fine for public repos).
func buildGitHubApp(cfg *config.Config) (*ghapp.App, error) {
	idStr := os.Getenv("SUPRCOW_GITHUB_APP_ID")
	if idStr == "" {
		return nil, nil
	}
	appID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("SUPRCOW_GITHUB_APP_ID: %w", err)
	}
	key, err := appPrivateKey()
	if err != nil {
		return nil, err
	}
	return ghapp.NewApp(appID, key)
}

// appPrivateKey reads the GitHub App private key from a file (preferred) or env.
func appPrivateKey() ([]byte, error) {
	if path := os.Getenv("SUPRCOW_GITHUB_APP_PRIVATE_KEY_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read app private key: %w", err)
		}
		return b, nil
	}
	if key := os.Getenv("SUPRCOW_GITHUB_APP_PRIVATE_KEY"); key != "" {
		// Accept raw PEM, or base64-encoded PEM (single-line, friendly for
		// secret stores / dotenv that can't hold multi-line values).
		if strings.Contains(key, "BEGIN") {
			return []byte(key), nil
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
		if err != nil {
			return nil, fmt.Errorf("SUPRCOW_GITHUB_APP_PRIVATE_KEY is neither PEM nor base64: %w", err)
		}
		return decoded, nil
	}
	return nil, fmt.Errorf("SUPRCOW_GITHUB_APP_ID is set but no private key (SUPRCOW_GITHUB_APP_PRIVATE_KEY[_FILE])")
}

func splitOwnerRepo(repo string) (owner, name string, err error) {
	s := strings.TrimSuffix(strings.TrimSpace(repo), ".git")
	if i := strings.Index(s, "github.com/"); i >= 0 {
		s = s[i+len("github.com/"):]
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] == "" || parts[len(parts)-1] == "" {
		return "", "", fmt.Errorf("cannot parse owner/name from repo %q", repo)
	}
	return parts[len(parts)-2], parts[len(parts)-1], nil
}

// deriveCloneURL turns a config repo reference into a clone URL.
func deriveCloneURL(repo string) string {
	if strings.Contains(repo, "://") || strings.HasPrefix(repo, "git@") {
		return repo
	}
	return "https://" + strings.TrimSuffix(repo, ".git") + ".git"
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

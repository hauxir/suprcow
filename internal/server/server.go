// Package server is the daemon's HTTP front: it receives git webhooks and
// reverse-proxies preview traffic, lazily spawning a PR's stack on the first
// request and showing a waiting page until it is ready.
package server

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/hauxir/suprcow/internal/auth"
	"github.com/hauxir/suprcow/internal/config"
	"github.com/hauxir/suprcow/internal/env"
	"github.com/hauxir/suprcow/internal/manager"
)

// HookPath is the endpoint GitHub webhooks POST to.
const HookPath = "/_suprcow/hooks/github"

// Options configures a Server.
type Options struct {
	Config        *config.Config
	Manager       *manager.Manager
	BaseDomain    string
	WebhookSecret string
	// Auth optionally gates preview traffic; nil leaves previews open.
	Auth *auth.GitHub
	// Comment, if set, posts/updates a comment on a PR (nil disables commenting).
	Comment func(ctx context.Context, pr int, body string) error
}

// Server routes webhooks and preview traffic.
type Server struct {
	cfg           *config.Config
	mgr           *manager.Manager
	matcher       *config.Matcher
	baseDomain    string
	webhookSecret []byte
	auth          *auth.GitHub
	comment       func(ctx context.Context, pr int, body string) error
}

// New builds a Server, compiling the host matcher from the config.
func New(o Options) (*Server, error) {
	matcher, err := o.Config.NewMatcher()
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:           o.Config,
		mgr:           o.Manager,
		matcher:       matcher,
		baseDomain:    o.BaseDomain,
		webhookSecret: []byte(o.WebhookSecret),
		auth:          o.Auth,
		comment:       o.Comment,
	}, nil
}

// previewURL is the primary preview URL for a PR (the first exposed service).
func (s *Server) previewURL(pr int) string {
	if len(s.cfg.Expose) == 0 {
		return ""
	}
	host, err := s.cfg.RenderContext(pr, "", "", s.baseDomain).Host(s.cfg.Expose[0].Service)
	if err != nil {
		return ""
	}
	return "https://" + host + "/"
}

// maybeComment posts/updates suprcow's PR comment for lifecycle events.
func (s *Server) maybeComment(ctx context.Context, pr int, action string) {
	if s.comment == nil {
		return
	}
	var body string
	switch action {
	case "opened", "reopened":
		body = "🐮 **Preview:** " + s.previewURL(pr) +
			"\n\nSpins up on first visit and idles back down when unused."
	case "closed", "merged":
		body = "🐮 Preview torn down (PR closed)."
	default:
		return
	}
	if err := s.comment(ctx, pr, body); err != nil {
		log.Printf("pr comment pr=%d action=%s: %v", pr, action, err)
	}
}

// Handler returns the daemon's HTTP handler. The webhook and OAuth endpoints
// are always ungated; preview traffic is wrapped by the auth gate when one is
// configured, so unauthenticated requests can't trigger a spawn.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(HookPath, s.handleGitHub)
	if s.auth != nil {
		mux.Handle(auth.AuthPathPrefix, s.auth.Handlers())
		mux.Handle("/", s.auth.Middleware(http.HandlerFunc(s.handleProxy)))
	} else {
		mux.HandleFunc("/", s.handleProxy)
	}
	return mux
}

// handleProxy resolves the request host to a PR + service, ensures the stack is
// up (lazily, in the background), and proxies once ready.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	host := stripPort(r.Host)
	sub, ok := strings.CutSuffix(host, "."+s.baseDomain)
	if !ok || sub == "" {
		http.NotFound(w, r)
		return
	}
	rule, pr, ok := s.matcher.Match(sub)
	if !ok {
		http.NotFound(w, r)
		return
	}

	e, exists := s.mgr.Get(pr)
	if !exists {
		s.renderWaiting(w, pr, "unknown PR — no webhook received yet", http.StatusNotFound)
		return
	}

	switch e.Status {
	case env.StatusRunning:
		s.mgr.Touch(pr)
		service, port := rule.Resolve(r.Method, r.URL.Path)
		s.proxyTo(w, r, pr, service, port)
	case env.StatusStarting:
		s.renderWaiting(w, pr, "starting…", http.StatusServiceUnavailable)
	case env.StatusError:
		s.trigger(pr) // retry
		s.renderWaiting(w, pr, "previous start failed; retrying: "+e.Message, http.StatusServiceUnavailable)
	default: // pending, stopped
		s.trigger(pr)
		s.renderWaiting(w, pr, "spinning up your environment…", http.StatusServiceUnavailable)
	}
}

// proxyTo reverse-proxies to a service's container on the shared network. If the
// upstream is unreachable (e.g. the stack drifted/stopped), it triggers a
// respawn and shows the waiting page.
func (s *Server) proxyTo(w http.ResponseWriter, r *http.Request, pr int, service string, port int) {
	target := &url.URL{Scheme: "http", Host: s.mgr.ServiceTarget(pr, service, port)}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		log.Printf("proxy pr=%d service=%s: %v", pr, service, err)
		s.trigger(pr)
		s.renderWaiting(w, pr, "reconnecting…", http.StatusServiceUnavailable)
	}
	rp.ServeHTTP(w, r)
}

// trigger kicks off a background spawn/restart unless one is already underway.
func (s *Server) trigger(pr int) {
	if e, ok := s.mgr.Get(pr); ok && e.Status == env.StatusStarting {
		return
	}
	go func() {
		if _, err := s.mgr.EnsureUp(context.Background(), pr); err != nil {
			log.Printf("ensure up pr=%d: %v", pr, err)
		}
	}()
}

func stripPort(host string) string {
	if i := strings.LastIndexByte(host, ':'); i != -1 {
		// Guard against IPv6 literals without a port.
		if !strings.Contains(host[i:], "]") {
			return host[:i]
		}
	}
	return host
}

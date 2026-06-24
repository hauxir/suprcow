// Package auth gates preview access behind GitHub: a user may reach a PR's
// environment only if they can access the repo on GitHub. Because suprcow is
// already GitHub-driven (it verifies PR webhooks), GitHub is the natural source
// of truth for authorization — no separate identity provider or allowlist.
//
// The gate runs as middleware in front of the proxy, so unauthenticated
// requests are rejected BEFORE they can trigger a lazy spawn. OAuth happens on
// a fixed control host and the session is a signed cookie scoped to the parent
// domain, so one login covers every PR subdomain.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	sessionCookie = "suprcow_session"
	sessionTTL    = 8 * time.Hour
	stateTTL      = 10 * time.Minute

	// AuthPathPrefix is the (ungated) route prefix for the OAuth endpoints.
	AuthPathPrefix = "/_suprcow/auth/"
)

// Options configures the GitHub gate.
type Options struct {
	Repo         string // "owner/name" or a github URL
	Allow        string // "collaborators" (default) or "org-members"
	Org          string // org for org-members mode; defaults to the repo owner
	CookieDomain string // e.g. ".preview.example.com"
	AuthBaseURL  string // public URL of the control host, e.g. https://suprcow.preview.example.com
	BaseDomain   string // wildcard root, for the open-redirect guard
	ClientID     string
	ClientSecret string
	SessionKey   []byte // HMAC key for signing cookies/state
}

// GitHub is a GitHub-backed access gate.
type GitHub struct {
	owner, name  string
	allow        string
	org          string
	cookieDomain string
	authBaseURL  string
	baseDomain   string
	clientID     string
	clientSecret string
	key          []byte
	http         *http.Client
}

// NewGitHub builds a gate. It returns (nil, nil) when ClientID is empty, so
// callers can treat "no credentials" as "auth disabled".
func NewGitHub(o Options) (*GitHub, error) {
	if o.ClientID == "" {
		return nil, nil
	}
	owner, name, err := splitRepo(o.Repo)
	if err != nil {
		return nil, err
	}
	if len(o.SessionKey) == 0 {
		return nil, fmt.Errorf("auth: a session key is required (set SUPRCOW_SESSION_KEY)")
	}
	allow := o.Allow
	if allow == "" {
		allow = "collaborators"
	}
	org := o.Org
	if org == "" {
		org = owner
	}
	return &GitHub{
		owner: owner, name: name,
		allow:        allow,
		org:          org,
		cookieDomain: o.CookieDomain,
		authBaseURL:  strings.TrimRight(o.AuthBaseURL, "/"),
		baseDomain:   o.BaseDomain,
		clientID:     o.ClientID,
		clientSecret: o.ClientSecret,
		key:          o.SessionKey,
		http:         &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Middleware rejects unauthenticated/unauthorized requests before they reach
// next (which would otherwise trigger a spawn).
func (g *GitHub) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := g.readSession(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		login := g.authBaseURL + AuthPathPrefix + "login?return=" + url.QueryEscape(currentURL(r))
		http.Redirect(w, r, login, http.StatusFound)
	})
}

// Handlers serves the OAuth endpoints under AuthPathPrefix.
func (g *GitHub) Handlers() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(AuthPathPrefix+"login", g.handleLogin)
	mux.HandleFunc(AuthPathPrefix+"callback", g.handleCallback)
	mux.HandleFunc(AuthPathPrefix+"logout", g.handleLogout)
	return mux
}

func (g *GitHub) handleLogin(w http.ResponseWriter, r *http.Request) {
	ret := r.URL.Query().Get("return")
	if !g.safeReturn(ret) {
		ret = g.authBaseURL + "/"
	}
	state := g.sign(mustJSON(oauthState{Return: ret, Exp: time.Now().Add(stateTTL).Unix(), Nonce: nonce()}))
	// GitHub App user-to-server OAuth: no scopes — access is governed by the
	// App's installation and the user's own repo access.
	q := url.Values{
		"client_id":    {g.clientID},
		"redirect_uri": {g.authBaseURL + AuthPathPrefix + "callback"},
		"state":        {state},
	}
	http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}

func (g *GitHub) handleCallback(w http.ResponseWriter, r *http.Request) {
	payload, ok := g.verify(r.URL.Query().Get("state"))
	if !ok {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	var st oauthState
	if err := json.Unmarshal(payload, &st); err != nil || time.Now().Unix() > st.Exp || !g.safeReturn(st.Return) {
		http.Error(w, "expired or invalid state", http.StatusBadRequest)
		return
	}

	token, err := g.exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "oauth exchange failed", http.StatusBadGateway)
		return
	}
	login, err := g.currentUser(r.Context(), token)
	if err != nil {
		http.Error(w, "could not read GitHub user", http.StatusBadGateway)
		return
	}
	authorized, err := g.hasAccess(r.Context(), token, login)
	if err != nil {
		http.Error(w, "access check failed", http.StatusBadGateway)
		return
	}
	if !authorized {
		g.renderForbidden(w, login)
		return
	}

	g.setSession(w, login)
	http.Redirect(w, r, st.Return, http.StatusFound)
}

func (g *GitHub) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", Domain: g.cookieDomain,
		MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "logged out\n")
}

// exchange swaps an OAuth code for an access token.
func (g *GitHub) exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {g.clientID},
		"client_secret": {g.clientSecret},
		"code":          {code},
		"redirect_uri":  {g.authBaseURL + AuthPathPrefix + "callback"},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("no access token (%s)", out.Error)
	}
	return out.AccessToken, nil
}

func (g *GitHub) currentUser(ctx context.Context, token string) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	if _, err := g.api(ctx, token, http.MethodGet, "https://api.github.com/user", &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", fmt.Errorf("empty login")
	}
	return u.Login, nil
}

// hasAccess checks repo access (collaborators) or org membership (org-members)
// using the authenticated user's own token.
func (g *GitHub) hasAccess(ctx context.Context, token, login string) (bool, error) {
	if g.allow == "org-members" {
		var m struct {
			State string `json:"state"`
		}
		status, err := g.api(ctx, token, http.MethodGet,
			"https://api.github.com/user/memberships/orgs/"+g.org, &m)
		if err != nil {
			return false, err
		}
		return status == http.StatusOK && m.State == "active", nil
	}
	// collaborators: visibility of the repo implies at least read access.
	status, err := g.api(ctx, token, http.MethodGet,
		"https://api.github.com/repos/"+g.owner+"/"+g.name, nil)
	if err != nil {
		return false, err
	}
	return status == http.StatusOK, nil
}

// api performs a GitHub API request, optionally decoding JSON into out, and
// returns the HTTP status. A 404 (no access) is not an error.
func (g *GitHub) api(ctx context.Context, token, method, urlStr string, out any) (int, error) {
	req, _ := http.NewRequestWithContext(ctx, method, urlStr, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode, nil
}

// --- session + state signing ---

type sessionData struct {
	Login string `json:"l"`
	Exp   int64  `json:"e"`
}

type oauthState struct {
	Return string `json:"r"`
	Exp    int64  `json:"e"`
	Nonce  string `json:"n"`
}

func (g *GitHub) setSession(w http.ResponseWriter, login string) {
	val := g.sign(mustJSON(sessionData{Login: login, Exp: time.Now().Add(sessionTTL).Unix()}))
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: val, Path: "/", Domain: g.cookieDomain,
		Expires: time.Now().Add(sessionTTL), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

func (g *GitHub) readSession(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	payload, ok := g.verify(c.Value)
	if !ok {
		return "", false
	}
	var s sessionData
	if err := json.Unmarshal(payload, &s); err != nil || time.Now().Unix() > s.Exp {
		return "", false
	}
	return s.Login, true
}

func (g *GitHub) sign(payload []byte) string {
	mac := hmac.New(sha256.New, g.key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (g *GitHub) verify(token string) ([]byte, bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, g.key)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, false
	}
	return payload, true
}

// safeReturn guards against open redirects: the return URL must be https and
// its host must be the base domain or a subdomain of it.
func (g *GitHub) safeReturn(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return false
	}
	host := u.Hostname()
	return host == g.baseDomain || strings.HasSuffix(host, "."+g.baseDomain)
}

func (g *GitHub) renderForbidden(w http.ResponseWriter, login string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>suprcow — access denied</title>
<body style="font-family:system-ui;background:#0c0d10;color:#e7e9ee;display:flex;min-height:100vh;margin:0;align-items:center;justify-content:center;text-align:center">
<div><div style="font-size:3rem">🐮🚫</div>
<h1 style="font-size:1.25rem">Access denied</h1>
<p style="color:#9aa0ad"><strong>%s</strong> doesn't have access to <code>%s/%s</code> on GitHub.</p>
<p style="color:#9aa0ad">Ask for repo access, then <a style="color:#7c93ff" href="%slogin">try again</a>.</p></div></body>`,
		htmlEscape(login), htmlEscape(g.owner), htmlEscape(g.name), AuthPathPrefix)
}

// --- helpers ---

func currentURL(r *http.Request) string {
	// Previews are always served over https (Caddy terminates TLS upstream).
	return "https://" + r.Host + r.URL.RequestURI()
}

func splitRepo(s string) (owner, name string, err error) {
	s = strings.TrimSuffix(strings.TrimSpace(s), ".git")
	if i := strings.Index(s, "github.com/"); i >= 0 {
		s = s[i+len("github.com/"):]
	}
	s = strings.Trim(s, "/")
	parts := strings.Split(s, "/")
	if len(parts) < 2 || parts[len(parts)-2] == "" || parts[len(parts)-1] == "" {
		return "", "", fmt.Errorf("auth: cannot parse owner/name from repo %q", s)
	}
	return parts[len(parts)-2], parts[len(parts)-1], nil
}

func nonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

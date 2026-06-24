// Package ghapp implements the GitHub App identity: signing the App JWT and
// minting short-lived installation access tokens. Those tokens are used as git
// credentials to clone private repos and as Bearer tokens for App-scoped API
// calls — least privilege, auto-expiring, no long-lived deploy key or PAT.
package ghapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// tokenSkew refreshes installation tokens a little before they actually expire.
const tokenSkew = 2 * time.Minute

// App is a GitHub App identity.
type App struct {
	appID int64
	key   *rsa.PrivateKey
	http  *http.Client
	now   func() time.Time

	mu     sync.Mutex
	tokens map[string]cachedToken // keyed by owner/repo
	// fetch is the token fetcher; a seam overridden in tests.
	fetch func(ctx context.Context, owner, repo string) (string, time.Time, error)
}

type cachedToken struct {
	token string
	exp   time.Time
}

// NewApp parses the App's PEM private key (PKCS#1 or PKCS#8) and returns an App.
func NewApp(appID int64, pemKey []byte) (*App, error) {
	if appID == 0 {
		return nil, fmt.Errorf("ghapp: app id is required")
	}
	key, err := parseKey(pemKey)
	if err != nil {
		return nil, err
	}
	a := &App{
		appID:  appID,
		key:    key,
		http:   &http.Client{Timeout: 10 * time.Second},
		now:    time.Now,
		tokens: map[string]cachedToken{},
	}
	a.fetch = a.fetchInstallationToken
	return a, nil
}

func parseKey(pemKey []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemKey)
	if block == nil {
		return nil, fmt.Errorf("ghapp: invalid PEM private key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ghapp: parse private key: %w", err)
	}
	rsaKey, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("ghapp: private key is not RSA")
	}
	return rsaKey, nil
}

// Token returns a valid installation token for owner/repo, minting/refreshing
// one as needed. Safe for concurrent use.
func (a *App) Token(ctx context.Context, owner, repo string) (string, error) {
	key := owner + "/" + repo
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.tokens[key]; ok && a.now().Before(t.exp.Add(-tokenSkew)) {
		return t.token, nil
	}
	tok, exp, err := a.fetch(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	a.tokens[key] = cachedToken{token: tok, exp: exp}
	return tok, nil
}

// GitAuthHeader returns the value for git's http.extraHeader so clones/fetches
// authenticate as the App installation (token kept out of argv via env).
func (a *App) GitAuthHeader(ctx context.Context, owner, repo string) (string, error) {
	tok, err := a.Token(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	return "Authorization: Basic " + basic, nil
}

// appJWT signs a short-lived RS256 JWT proving App identity.
func (a *App) appJWT() (string, error) {
	now := a.now()
	header := b64json(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims := b64json(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.appID,
	})
	signingInput := header + "." + claims
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (a *App) fetchInstallationToken(ctx context.Context, owner, repo string) (string, time.Time, error) {
	jwt, err := a.appJWT()
	if err != nil {
		return "", time.Time{}, err
	}

	var inst struct {
		ID int64 `json:"id"`
	}
	if err := a.do(ctx, jwt, http.MethodGet,
		fmt.Sprintf("https://api.github.com/repos/%s/%s/installation", owner, repo), &inst); err != nil {
		return "", time.Time{}, fmt.Errorf("get installation: %w", err)
	}
	if inst.ID == 0 {
		return "", time.Time{}, fmt.Errorf("app not installed on %s/%s", owner, repo)
	}

	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.do(ctx, jwt, http.MethodPost,
		fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", inst.ID), &tok); err != nil {
		return "", time.Time{}, fmt.Errorf("create installation token: %w", err)
	}
	if tok.Token == "" {
		return "", time.Time{}, fmt.Errorf("empty installation token")
	}
	return tok.Token, tok.ExpiresAt, nil
}

func (a *App) do(ctx context.Context, jwt, method, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("github %s %s: %d: %s", method, url, resp.StatusCode, body)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func b64json(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

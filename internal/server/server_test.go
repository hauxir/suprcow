package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hauxir/suprcow/internal/config"
	"github.com/hauxir/suprcow/internal/env"
	"github.com/hauxir/suprcow/internal/manager"
	"github.com/hauxir/suprcow/internal/store"
)

const secret = "s3cr3t"

func testServer(t *testing.T) (*Server, *manager.Manager) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
repo: github.com/me/app
expose:
  - { service: web, subdomain: "pr-{n}", port: 80 }
`))
	if err != nil {
		t.Fatal(err)
	}
	mgr := manager.New(manager.Options{
		Project:    "app",
		Config:     cfg,
		BaseDomain: "preview.test",
		Store:      store.NewMemory(),
	})
	srv, err := New(Options{Config: cfg, Manager: mgr, BaseDomain: "preview.test", WebhookSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	return srv, mgr
}

func TestStripPort(t *testing.T) {
	cases := map[string]string{
		"pr-1.preview.test":      "pr-1.preview.test",
		"pr-1.preview.test:8080": "pr-1.preview.test",
		"[::1]":                  "[::1]",
		"[::1]:80":               "[::1]",
	}
	for in, want := range cases {
		if got := stripPort(in); got != want {
			t.Errorf("stripPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRouteUnknownHost(t *testing.T) {
	srv, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "http://pr-1.elsewhere.com/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRouteUnknownPR(t *testing.T) {
	srv, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "http://pr-9.preview.test/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no webhook") {
		t.Errorf("body missing hint: %s", rec.Body.String())
	}
}

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookBadSignature(t *testing.T) {
	srv, _ := testServer(t)
	req := httptest.NewRequest(http.MethodPost, "http://box"+HookPath, strings.NewReader(`{}`))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestWebhookPing(t *testing.T) {
	srv, _ := testServer(t)
	body := []byte(`{"zen":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "http://box"+HookPath, strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", sign(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestWebhookOpenedComments(t *testing.T) {
	cfg, err := config.Parse([]byte(`
repo: github.com/me/app
expose:
  - { service: web, subdomain: "pr-{n}", port: 80 }
`))
	if err != nil {
		t.Fatal(err)
	}
	mgr := manager.New(manager.Options{Project: "app", Config: cfg, BaseDomain: "preview.test", Store: store.NewMemory()})

	var mu sync.Mutex
	var gotPR int
	var gotBody string
	srv, err := New(Options{
		Config: cfg, Manager: mgr, BaseDomain: "preview.test", WebhookSecret: secret,
		Comment: func(_ context.Context, pr int, body string) error {
			mu.Lock()
			gotPR, gotBody = pr, body
			mu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"action":"opened","number":42,"pull_request":{"head":{"ref":"x","sha":"s"}}}`)
	req := httptest.NewRequest(http.MethodPost, "http://box"+HookPath, strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", sign(body))
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		b, pr := gotBody, gotPR
		mu.Unlock()
		if b != "" {
			if pr != 42 || !strings.Contains(b, "https://pr-42.preview.test/") {
				t.Fatalf("comment pr=%d body=%q", pr, b)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("preview comment was not posted")
}

func TestWebhookOpenedRecordsPending(t *testing.T) {
	srv, mgr := testServer(t)
	body := []byte(`{"action":"opened","number":42,"pull_request":{"head":{"ref":"feat/x","sha":"abc"}}}`)
	req := httptest.NewRequest(http.MethodPost, "http://box"+HookPath, strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", sign(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Notify runs in the background; poll for the pending record.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e, ok := mgr.Get(42); ok && e.Status == env.StatusPending && e.SHA == "abc" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("PR 42 was not recorded as pending")
}

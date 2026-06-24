package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testGate(t *testing.T) *GitHub {
	t.Helper()
	g, err := NewGitHub(Options{
		Repo:         "github.com/acme/web",
		CookieDomain: ".preview.example.com",
		AuthBaseURL:  "https://suprcow.preview.example.com",
		BaseDomain:   "preview.example.com",
		ClientID:     "cid",
		ClientSecret: "secret",
		SessionKey:   []byte("test-key-please-change"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if g == nil {
		t.Fatal("expected gate")
	}
	return g
}

func TestDisabledWithoutClientID(t *testing.T) {
	g, err := NewGitHub(Options{Repo: "a/b", SessionKey: []byte("k")})
	if err != nil || g != nil {
		t.Fatalf("want (nil,nil) when no client id, got (%v,%v)", g, err)
	}
}

func TestSplitRepo(t *testing.T) {
	cases := map[string][2]string{
		"github.com/owner/name":         {"owner", "name"},
		"https://github.com/owner/name": {"owner", "name"},
		"owner/name":                    {"owner", "name"},
		"github.com/owner/name.git":     {"owner", "name"},
	}
	for in, want := range cases {
		o, n, err := splitRepo(in)
		if err != nil || o != want[0] || n != want[1] {
			t.Errorf("splitRepo(%q) = (%q,%q,%v)", in, o, n, err)
		}
	}
	if _, _, err := splitRepo("nope"); err == nil {
		t.Error("expected error for unparseable repo")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	g := testGate(t)
	rec := httptest.NewRecorder()
	g.setSession(rec, "octocat")

	req := httptest.NewRequest(http.MethodGet, "https://pr-1.preview.example.com/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	login, ok := g.readSession(req)
	if !ok || login != "octocat" {
		t.Fatalf("session round-trip = (%q,%v)", login, ok)
	}
}

func TestSessionTamperRejected(t *testing.T) {
	g := testGate(t)
	req := httptest.NewRequest(http.MethodGet, "https://pr-1.preview.example.com/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "forged.payload"})
	if _, ok := g.readSession(req); ok {
		t.Fatal("forged cookie accepted")
	}
}

func TestMiddlewareRedirectsUnauthenticated(t *testing.T) {
	g := testGate(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next must not run for unauthenticated request")
		_ = w
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://pr-7.preview.example.com/foo", nil)
	g.Middleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://suprcow.preview.example.com"+AuthPathPrefix+"login") {
		t.Fatalf("redirect = %q", loc)
	}
	if !strings.Contains(loc, "return=https%3A%2F%2Fpr-7.preview.example.com%2Ffoo") {
		t.Fatalf("return not preserved: %q", loc)
	}
}

func TestMiddlewarePassesAuthenticated(t *testing.T) {
	g := testGate(t)
	ran := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { ran = true })

	auth := httptest.NewRecorder()
	g.setSession(auth, "octocat")
	req := httptest.NewRequest(http.MethodGet, "https://pr-7.preview.example.com/", nil)
	for _, c := range auth.Result().Cookies() {
		req.AddCookie(c)
	}
	g.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)
	if !ran {
		t.Fatal("authenticated request did not reach next")
	}
}

func TestSafeReturn(t *testing.T) {
	g := testGate(t)
	ok := []string{
		"https://pr-1.preview.example.com/",
		"https://preview.example.com/x",
		"https://api-pr-9.preview.example.com/gql-ws",
	}
	bad := []string{
		"https://evil.com/",
		"http://pr-1.preview.example.com/", // not https
		"https://preview.example.com.evil.com/",
		"not a url",
		"",
	}
	for _, u := range ok {
		if !g.safeReturn(u) {
			t.Errorf("safeReturn(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if g.safeReturn(u) {
			t.Errorf("safeReturn(%q) = true, want false", u)
		}
	}
}

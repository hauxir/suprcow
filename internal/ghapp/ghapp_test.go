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
	"strings"
	"testing"
	"time"
)

func testKeyPEM(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return pemBytes, key
}

func TestAppJWT(t *testing.T) {
	pemBytes, key := testKeyPEM(t)
	app, err := NewApp(424242, pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	app.now = func() time.Time { return time.Unix(1_700_000_000, 0) }

	tok, err := app.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}

	// Signature must verify against the public key.
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("signature verify: %v", err)
	}

	// Claims must carry the app id as issuer and a sane expiry window.
	claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}
	if iss, _ := claims["iss"].(float64); int64(iss) != 424242 {
		t.Errorf("iss = %v", claims["iss"])
	}
	if exp, _ := claims["exp"].(float64); int64(exp) <= 1_700_000_000 {
		t.Errorf("exp not in the future: %v", claims["exp"])
	}
}

func TestParseKeyRejectsGarbage(t *testing.T) {
	if _, err := NewApp(1, []byte("not a pem")); err == nil {
		t.Fatal("expected error for invalid key")
	}
}

func TestTokenCaching(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	calls := 0
	app := &App{
		now:    func() time.Time { return clock },
		tokens: map[string]cachedToken{},
	}
	app.fetch = func(_ context.Context, owner, repo string) (string, time.Time, error) {
		calls++
		return fmt.Sprintf("tok-%d", calls), clock.Add(time.Hour), nil
	}

	ctx := context.Background()
	t1, _ := app.Token(ctx, "o", "r")
	t2, _ := app.Token(ctx, "o", "r")
	if t1 != "tok-1" || t2 != "tok-1" || calls != 1 {
		t.Fatalf("expected cached single fetch, got t1=%s t2=%s calls=%d", t1, t2, calls)
	}

	// Advance past expiry (minus skew) → refetch.
	clock = clock.Add(59 * time.Minute)
	t3, _ := app.Token(ctx, "o", "r")
	if t3 != "tok-2" || calls != 2 {
		t.Fatalf("expected refetch after expiry, got t3=%s calls=%d", t3, calls)
	}

	// A different repo is cached separately.
	if _, _ = app.Token(ctx, "o", "other"); calls != 3 {
		t.Fatalf("expected separate fetch per repo, calls=%d", calls)
	}
}

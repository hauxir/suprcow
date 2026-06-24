package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

// ghPullRequestEvent is the subset of GitHub's pull_request webhook payload we need.
type ghPullRequestEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Merged bool `json:"merged"`
		Head   struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
}

// handleGitHub validates and processes a GitHub pull_request webhook.
func (s *Server) handleGitHub(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if !s.verifySignature(r, body) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
		w.WriteHeader(http.StatusOK)
		return
	case "pull_request":
		// handled below
	default:
		w.WriteHeader(http.StatusOK) // ignore other events
		return
	}

	var ev ghPullRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if ev.Number == 0 {
		http.Error(w, "missing PR number", http.StatusBadRequest)
		return
	}

	// Run the lifecycle action in the background so GitHub gets a fast 200.
	action := ev.Action
	branch, sha := ev.PullRequest.Head.Ref, ev.PullRequest.Head.SHA
	pr := ev.Number
	go func() {
		if err := s.mgr.Notify(context.Background(), pr, branch, sha, action); err != nil {
			log.Printf("webhook: notify pr=%d action=%s: %v", pr, action, err)
		}
	}()

	fmt.Fprintf(w, "ok: pr=%d action=%s\n", pr, action)
}

// verifySignature checks the X-Hub-Signature-256 HMAC. If no secret is
// configured, verification is skipped (useful for local testing only).
func (s *Server) verifySignature(r *http.Request, body []byte) bool {
	if len(s.webhookSecret) == 0 {
		return true
	}
	got := r.Header.Get("X-Hub-Signature-256")
	const prefix = "sha256="
	if len(got) <= len(prefix) {
		return false
	}
	mac := hmac.New(sha256.New, s.webhookSecret)
	mac.Write(body)
	want := prefix + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(got), []byte(want))
}

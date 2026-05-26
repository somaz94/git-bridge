package consumer

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"git-bridge/internal/mirror"
)

// MirrorRetrier is the subset of mirror.Service the retry handler depends on.
// Kept narrow so tests can substitute a mock.
type MirrorRetrier interface {
	Retry(ctx context.Context, repoName, direction string, meta mirror.EventMeta) error
}

// RetryRequest is the JSON body accepted by POST /retry/mirror.
type RetryRequest struct {
	Repo      string `json:"repo"`
	Direction string `json:"direction"`
	Ref       string `json:"ref,omitempty"`
}

// Retry handles POST /retry/mirror — the manual retry endpoint.
// When apiToken is empty the endpoint is disabled (Handler returns 404).
type Retry struct {
	ctx       context.Context
	mirrorSvc MirrorRetrier
	apiToken  string
}

// NewRetry constructs a Retry handler. An empty apiToken yields a disabled
// handler that responds with 404 to every request.
func NewRetry(ctx context.Context, mirrorSvc MirrorRetrier, apiToken string) *Retry {
	return &Retry{ctx: ctx, mirrorSvc: mirrorSvc, apiToken: apiToken}
}

// Handler serves POST /retry/mirror. It performs Bearer token verification
// (constant-time), validates the request, then runs mirror.Retry in a
// background goroutine so the caller gets an immediate 200 response.
func (r *Retry) Handler(rw http.ResponseWriter, req *http.Request) {
	// Empty token → endpoint disabled. Mirrors the convention of webhook
	// secrets (empty = skip verify) but inverted: retry must always require auth.
	if r.apiToken == "" {
		http.NotFound(rw, req)
		return
	}
	if req.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Require the literal "Bearer " prefix per API spec, then constant-time
	// compare the rest against the configured token. The prefix check is
	// upfront (not constant-time) but reveals no secret bits — only the
	// presence/absence of the well-known scheme.
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		slog.Warn("retry api: missing or malformed authorization header")
		http.Error(rw, "unauthorized", http.StatusUnauthorized)
		return
	}
	presented := strings.TrimPrefix(auth, "Bearer ")
	if subtle.ConstantTimeCompare([]byte(presented), []byte(r.apiToken)) != 1 {
		slog.Warn("retry api: invalid token")
		http.Error(rw, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, maxBodySize))
	if err != nil {
		slog.Error("retry api: read body failed", "error", err)
		http.Error(rw, "bad request", http.StatusBadRequest)
		return
	}

	var rr RetryRequest
	if err := json.Unmarshal(body, &rr); err != nil {
		slog.Error("retry api: parse failed", "error", err)
		http.Error(rw, "bad request: invalid json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(rr.Repo) == "" {
		http.Error(rw, "bad request: repo required", http.StatusBadRequest)
		return
	}
	if rr.Direction == "" {
		rr.Direction = "auto"
	}
	if !isValidRetryDirection(rr.Direction) {
		http.Error(rw, "bad request: invalid direction", http.StatusBadRequest)
		return
	}

	logger := slog.With("source", "retry-api", "repo", rr.Repo,
		"direction", rr.Direction, "ref", rr.Ref)
	logger.Info("received retry request")

	meta := mirror.EventMeta{Ref: rr.Ref, Source: "retry-api"}
	go func() {
		if err := r.mirrorSvc.Retry(r.ctx, rr.Repo, rr.Direction, meta); err != nil {
			logger.Error("retry sync failed", "error", err)
		}
	}()

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(map[string]string{
		"status":    "accepted",
		"repo":      rr.Repo,
		"direction": rr.Direction,
		"ref":       rr.Ref,
		"queued_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// isValidRetryDirection reports whether d is one of the accepted direction
// strings. Comparison is case-insensitive.
func isValidRetryDirection(d string) bool {
	switch strings.ToLower(d) {
	case "source-to-target", "target-to-source", "auto":
		return true
	}
	return false
}

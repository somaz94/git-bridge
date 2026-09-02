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
	"git-bridge/internal/task"
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
	// Source lets a scheduled caller identify itself. Only "cron" is accepted;
	// anything else is rejected rather than recorded, so the history's trigger
	// column stays a closed vocabulary instead of free text from whoever holds
	// the token. Omitted means a human or ad-hoc caller — recorded as retry-api.
	Source string `json:"source,omitempty"`
	// Force applies a rewind the push guard would otherwise withhold, for the
	// case where someone reset a branch on purpose and means it.
	//
	// It is gated twice below: a force must name a ref, so one call can move at
	// most one ref, and cron may not set it, so the hourly reconcile can never
	// carry a bypass across every repository at once. Both gates exist because
	// the damage a force can do scales with how many refs it covers.
	Force bool `json:"force,omitempty"`
	// Dest is the destination tip the caller looked at, and is required with
	// Force. It becomes the push's lease, so a force only ever overwrites the
	// commit the caller actually decided about — anything that arrived since is
	// refused rather than adopted as the expected value.
	Dest string `json:"dest,omitempty"`
	// Actor attributes the call, so a console button can say who pressed it.
	Actor string `json:"actor,omitempty"`
}

// resolveRetrySource maps the optional source hint to the recorded trigger.
//
// The reconcile CronJob and a hand-run curl hit the same route, and only the
// caller knows which it is. Trusting the hint is safe because the route is
// already behind the API token; the whitelist is what keeps the vocabulary
// closed. ok=false means the caller sent something outside it.
func resolveRetrySource(hint string) (source string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(hint)) {
	case "":
		return mirror.SourceRetryAPI, true
	case mirror.SourceCron:
		return mirror.SourceCron, true
	}
	return "", false
}

// Retry handles POST /retry/mirror — the manual retry endpoint.
// When apiToken is empty the endpoint is disabled (Handler returns 404).
type Retry struct {
	// tasks owns the syncs this handler starts, so shutdown can wait for one
	// that is already running instead of killing its git command.
	tasks     *task.Group
	mirrorSvc MirrorRetrier
	apiToken  string
}

// NewRetry constructs a Retry handler. An empty apiToken yields a disabled
// handler that responds with 404 to every request. Syncs it starts run under
// tasks.
func NewRetry(tasks *task.Group, mirrorSvc MirrorRetrier, apiToken string) *Retry {
	return &Retry{tasks: tasks, mirrorSvc: mirrorSvc, apiToken: apiToken}
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
	if !IsValidRetryDirection(rr.Direction) {
		http.Error(rw, "bad request: invalid direction", http.StatusBadRequest)
		return
	}

	source, ok := resolveRetrySource(rr.Source)
	if !ok {
		http.Error(rw, "bad request: invalid source", http.StatusBadRequest)
		return
	}
	// A force with no ref would push every ref past the guard in one call. That
	// is the shape of the 2026-08-10 loss — one event, every ref rewritten —
	// and there is no reason to want it: a deliberate rewind is about a branch
	// someone has in mind.
	if rr.Force && strings.TrimSpace(rr.Ref) == "" {
		http.Error(rw, "bad request: force requires ref", http.StatusBadRequest)
		return
	}
	// A force without the tip it is overwriting is a force against whatever the
	// destination happens to hold when the push finally runs — which, after a
	// fetch that can take a minute, is not necessarily what the caller decided
	// about. The tip is in the alert that sent them here.
	if rr.Force && strings.TrimSpace(rr.Dest) == "" {
		http.Error(rw, "bad request: force requires dest (the destination tip you are overwriting)", http.StatusBadRequest)
		return
	}
	// The reconcile CronJob runs unattended against every repository, so a
	// bypass there would be one nobody chose. Force is for a person.
	if rr.Force && source == mirror.SourceCron {
		http.Error(rw, "bad request: cron may not force", http.StatusBadRequest)
		return
	}

	logger := slog.With("source", source, "repo", rr.Repo,
		"direction", rr.Direction, "ref", rr.Ref, "force", rr.Force)
	logger.Info("received retry request")

	meta := mirror.EventMeta{
		Ref: rr.Ref, Source: source,
		Force: rr.Force, ForceLease: strings.TrimSpace(rr.Dest), Actor: rr.Actor,
	}
	r.tasks.Go(func(ctx context.Context) {
		if err := r.mirrorSvc.Retry(ctx, rr.Repo, rr.Direction, meta); err != nil {
			logger.Error("retry sync failed", "error", err)
		}
	})

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

// IsValidRetryDirection reports whether d is one of the accepted direction
// strings. Comparison is case-insensitive. Exported because the console offers
// the same retry without a token and must accept exactly the same values —
// duplicating the list there would let the two drift apart.
func IsValidRetryDirection(d string) bool {
	switch strings.ToLower(d) {
	case "source-to-target", "target-to-source", "auto":
		return true
	}
	return false
}

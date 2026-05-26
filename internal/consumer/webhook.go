package consumer

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"git-bridge/internal/mirror"
)

// maxBodySize is the maximum allowed webhook request body size (1MB).
const maxBodySize = 1 << 20

// Mirrorer is the interface for mirror sync operations.
type Mirrorer interface {
	SyncByTarget(ctx context.Context, providerName, repoPath string, meta mirror.EventMeta) error
}

// GitLabPushEvent represents a GitLab push webhook payload.
type GitLabPushEvent struct {
	EventName  string `json:"event_name"`
	UserName   string `json:"user_name"`
	Repository struct {
		Name string `json:"name"`
	} `json:"repository"`
	Project struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	Ref string `json:"ref"`
}

// GitHubPushEvent represents a GitHub push webhook payload.
type GitHubPushEvent struct {
	Ref    string `json:"ref"`
	Pusher struct {
		Name string `json:"name"`
	} `json:"pusher"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
	Repository struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// Webhook handles HTTP webhook events from GitLab and GitHub.
type Webhook struct {
	ctx          context.Context
	mirrorSvc    Mirrorer
	gitlabSecret string
	githubSecret string
}

// NewWebhook creates a new webhook consumer.
func NewWebhook(ctx context.Context, mirrorSvc Mirrorer, gitlabSecret, githubSecret string) *Webhook {
	return &Webhook{
		ctx:          ctx,
		mirrorSvc:    mirrorSvc,
		gitlabSecret: gitlabSecret,
		githubSecret: githubSecret,
	}
}

// GitLabHandler handles POST /webhook/gitlab
func (w *Webhook) GitLabHandler(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Verify secret token
	if w.gitlabSecret != "" {
		token := r.Header.Get("X-Gitlab-Token")
		if token != w.gitlabSecret {
			slog.Warn("gitlab webhook: invalid token")
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		slog.Error("gitlab webhook: read body failed", "error", err)
		http.Error(rw, "bad request", http.StatusBadRequest)
		return
	}

	var event GitLabPushEvent
	if err := json.Unmarshal(body, &event); err != nil {
		slog.Error("gitlab webhook: parse failed", "error", err)
		http.Error(rw, "bad request", http.StatusBadRequest)
		return
	}

	repoPath := event.Project.PathWithNamespace
	logger := slog.With("provider", "gitlab", "repo", repoPath, "ref", event.Ref, "pusher", event.UserName)
	logger.Info("received gitlab push event")

	meta := mirror.EventMeta{
		Ref: event.Ref,
	}
	go func() {
		if err := w.mirrorSvc.SyncByTarget(w.ctx, "gitlab", repoPath, meta); err != nil {
			logger.Error("mirror sync failed", "error", err)
		}
	}()

	rw.WriteHeader(http.StatusOK)
	rw.Write([]byte(`{"status":"accepted"}`))
}

// GitHubHandler handles POST /webhook/github
func (w *Webhook) GitHubHandler(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		slog.Error("github webhook: read body failed", "error", err)
		http.Error(rw, "bad request", http.StatusBadRequest)
		return
	}

	// Verify HMAC-SHA256 signature
	if w.githubSecret != "" {
		signature := r.Header.Get("X-Hub-Signature-256")
		if !verifyGitHubSignature(body, w.githubSecret, signature) {
			slog.Warn("github webhook: invalid signature")
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var event GitHubPushEvent
	if err := json.Unmarshal(body, &event); err != nil {
		slog.Error("github webhook: parse failed", "error", err)
		http.Error(rw, "bad request", http.StatusBadRequest)
		return
	}

	repoPath := event.Repository.FullName
	pusher := event.Pusher.Name
	if pusher == "" {
		pusher = event.Sender.Login
	}
	logger := slog.With("provider", "github", "repo", repoPath, "ref", event.Ref, "pusher", pusher)
	logger.Info("received github push event")

	meta := mirror.EventMeta{
		Ref: event.Ref,
	}
	go func() {
		if err := w.mirrorSvc.SyncByTarget(w.ctx, "github", repoPath, meta); err != nil {
			logger.Error("mirror sync failed", "error", err)
		}
	}()

	rw.WriteHeader(http.StatusOK)
	rw.Write([]byte(`{"status":"accepted"}`))
}

// verifyGitHubSignature validates the X-Hub-Signature-256 header using HMAC-SHA256.
func verifyGitHubSignature(payload []byte, secret, signature string) bool {
	expected := fmt.Sprintf("sha256=%s", hmacSHA256(payload, secret))
	return hmac.Equal([]byte(expected), []byte(signature))
}

func hmacSHA256(data []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

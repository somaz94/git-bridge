package consumer

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"git-bridge/internal/config"
	"git-bridge/internal/mirror"
	"git-bridge/internal/task"
)

const (
	headerGitLabToken     = "X-Gitlab-Token"
	headerGitHubSignature = "X-Hub-Signature-256"
	githubSigPrefix       = "sha256="
	// zeroSHA is what GitLab and GitHub put in after when a push payload reports
	// that a ref was deleted.
	zeroSHA = "0000000000000000000000000000000000000000"
)

// Mirrorer is the interface for mirror sync operations.
type Mirrorer interface {
	SyncByTarget(ctx context.Context, providerName, repoPath string, meta mirror.EventMeta) error
	SyncDeleteByTarget(ctx context.Context, providerName, repoPath, refType, refName string) error
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
		// WebURL is the only clue to which GitLab instance sent the event
		// (e.g. "http://gitlab.example.com/group/repo"). When two instances of the
		// same type are configured, it is what narrows the provider down by host.
		WebURL string `json:"web_url"`
	} `json:"project"`
	Ref   string `json:"ref"`
	After string `json:"after"`
}

// GitHubPushEvent represents a GitHub push webhook payload.
type GitHubPushEvent struct {
	Ref    string `json:"ref"`
	After  string `json:"after"`
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
	Deleted bool `json:"deleted"`
}

// Webhook handles HTTP webhook events from GitLab and GitHub.
type Webhook struct {
	// tasks owns the syncs this handler starts. The handler answers the hook
	// immediately and the sync outlives the request, so shutdown needs a way to
	// wait for it rather than killing the git command it is running.
	tasks        *task.Group
	mirrorSvc    Mirrorer
	gitlabSecret string
	githubSecret string
	// hosts is the index that turns the payload's instance host into a provider
	// name. When nil, events dispatch by type alone, with no narrowing, as before.
	hosts config.HostResolver
	// maxBodySize caps an incoming request body, in bytes. Set through
	// WithMaxBodySizeMB; zero means defaultMaxBodySizeMB.
	maxBodySize int64
}

// WebhookOption tunes a Webhook after its dependencies are supplied.
//
// The knobs live here rather than in the constructor signature because they are
// settings, not dependencies: a caller that has no opinion should not have to
// state one, and the existing five arguments are already the limit of what reads
// clearly positionally.
type WebhookOption func(*Webhook)

// WithMaxBodySizeMB caps incoming webhook request bodies at mb megabytes.
// Values below 1 are ignored so a zeroed config cannot silently reject every
// event; config validation is what rejects a bad value out loud.
func WithMaxBodySizeMB(mb int) WebhookOption {
	return func(w *Webhook) {
		if mb >= 1 {
			w.maxBodySize = int64(mb) << 20
		}
	}
}

// NewWebhook creates a new webhook consumer. Syncs it starts run under tasks.
//
// hosts is the base_url host → provider name index (config.Config.HostResolver);
// when it is nil or empty, every event dispatches by type alone exactly as before.
func NewWebhook(tasks *task.Group, mirrorSvc Mirrorer, gitlabSecret, githubSecret string, hosts config.HostResolver, opts ...WebhookOption) *Webhook {
	w := &Webhook{
		tasks:        tasks,
		mirrorSvc:    mirrorSvc,
		gitlabSecret: gitlabSecret,
		githubSecret: githubSecret,
		hosts:        hosts,
		maxBodySize:  config.DefaultWebhookMaxBodySizeMB << 20,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// pushEvent abstracts pulling the repo path, ref and pusher out of a per-provider
// push payload, and deciding whether the push is a deletion.
type pushEvent interface {
	target() (repoPath, ref, pusher string)
	// isDelete reports whether this push is a ref deletion (after == zeroSHA, etc.).
	isDelete() bool
	// instanceURL returns the URL of the instance that sent the event, and whether
	// this provider is the kind that can be narrowed by host. routable=false means
	// no host lookup is attempted at all.
	instanceURL() (rawURL string, routable bool)
}

func (e *GitLabPushEvent) target() (repoPath, ref, pusher string) {
	return e.Project.PathWithNamespace, e.Ref, e.UserName
}

// GitLab sends zeroSHA in after on a ref delete, for both push and tag_push.
func (e *GitLabPushEvent) isDelete() bool {
	return e.After == zeroSHA
}

// GitLab can run several self-hosted instances, so narrowing by host is worth it.
// An older instance that sends no web_url yields an empty string, and the caller
// falls back.
func (e *GitLabPushEvent) instanceURL() (string, bool) {
	return e.Project.WebURL, true
}

func (e *GitHubPushEvent) target() (repoPath, ref, pusher string) {
	pusher = e.Pusher.Name
	if pusher == "" {
		pusher = e.Sender.Login
	}
	return e.Repository.FullName, e.Ref, pusher
}

// GitHub sends a deleted:true flag, and after is zeroSHA too (both are accepted).
func (e *GitHubPushEvent) isDelete() bool {
	return e.Deleted || e.After == zeroSHA
}

// The GitHub provider only ever deals with the single github.com instance, so
// there is nothing to narrow. Once several GitHub Enterprise instances get
// attached, switch this to routable=true and use repository.html_url.
func (e *GitHubPushEvent) instanceURL() (string, bool) {
	return "", false
}

// readLimitedBody reads the request body up to w.maxBodySize and writes an error
// response on failure. When ok=false the caller must return immediately.
//
// 🔴 http.MaxBytesReader, not io.LimitReader. LimitReader stops at the cap and
// reports a clean EOF, so io.ReadAll returns a *truncated* body with err=nil and
// the failure only surfaces downstream as "unexpected end of JSON input" — an
// error that says nothing about size and points at the sender's payload rather
// than at this limit. That misdirection is what made the 2026-09-15 drop take a
// second round of digging; the incident is written up on
// config.WebhookConfig.MaxBodySizeMB. MaxBytesReader fails loudly and lets us
// answer 413, which is both true and actionable.
func (w *Webhook) readLimitedBody(rw http.ResponseWriter, r *http.Request, logPrefix string) (body []byte, ok bool) {
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, w.maxBodySize))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			slog.Error(logPrefix+": body too large", "limit_bytes", w.maxBodySize)
			http.Error(rw, "payload too large", http.StatusRequestEntityTooLarge)
			return nil, false
		}
		slog.Error(logPrefix+": read body failed", "error", err)
		http.Error(rw, "bad request", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

// dispatchPushEvent is the shared webhook path: parse the body into event → pull
// out repo/ref/pusher → log → run SyncByTarget asynchronously → answer 200.
func (w *Webhook) dispatchPushEvent(rw http.ResponseWriter, provider string, body []byte, event pushEvent) {
	if err := json.Unmarshal(body, event); err != nil {
		slog.Error(provider+" webhook: parse failed", "error", err)
		http.Error(rw, "bad request", http.StatusBadRequest)
		return
	}

	repoPath, ref, pusher := event.target()
	logger := slog.With("provider", provider, "repo", repoPath, "ref", ref, "pusher", pusher)
	logger.Info("received " + provider + " push event")

	// The route only tells us the type. Narrowing all the way to a provider name by
	// the payload's instance host keeps the directions apart even when two instances
	// of the same type hold the same path. When it cannot be narrowed, the type
	// string is passed through and the old behaviour is kept.
	dispatchKey := provider
	if rawURL, routable := event.instanceURL(); routable {
		switch name := w.hosts.Resolve(rawURL); {
		case name != "":
			dispatchKey = name
			logger = logger.With("provider_name", name)
		case rawURL == "":
			logger.Warn("webhook payload carries no instance URL, dispatching by provider type")
		default:
			logger.Warn("webhook instance URL matches no provider base_url, dispatching by provider type",
				"instance_url", rawURL)
		}
	}

	meta := mirror.EventMeta{Ref: ref, Source: mirror.SourceWebhook}
	if event.isDelete() {
		// Ref delete event: propagate the delete on the target (gitlab/github) back
		// to the source (codecommit). Every ref that is not a tag counts as a branch,
		// so an unknown ref kind falls through to branch, fullRefName prefixes it with
		// refs/heads/ → RefTip="" → a harmless no-op.
		refType := "branch"
		if meta.IsTag() {
			refType = "tag"
		}
		refName := meta.RefName()
		w.tasks.Go(func(ctx context.Context) {
			if err := w.mirrorSvc.SyncDeleteByTarget(ctx, dispatchKey, repoPath, refType, refName); err != nil {
				logger.Error("mirror delete sync failed", "error", err)
			}
		})
	} else {
		w.tasks.Go(func(ctx context.Context) {
			if err := w.mirrorSvc.SyncByTarget(ctx, dispatchKey, repoPath, meta); err != nil {
				logger.Error("mirror sync failed", "error", err)
			}
		})
	}

	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte(`{"status":"accepted"}`))
}

// GitLabHandler handles POST /webhook/gitlab
func (w *Webhook) GitLabHandler(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Secret token check — GitLab's token is a header, so a bad one is rejected
	// before the body is read.
	if w.gitlabSecret != "" {
		if r.Header.Get(headerGitLabToken) != w.gitlabSecret {
			slog.Warn("gitlab webhook: invalid token")
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	body, ok := w.readLimitedBody(rw, r, "gitlab webhook")
	if !ok {
		return
	}
	w.dispatchPushEvent(rw, "gitlab", body, &GitLabPushEvent{})
}

// GitHubHandler handles POST /webhook/github
func (w *Webhook) GitHubHandler(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, ok := w.readLimitedBody(rw, r, "github webhook")
	if !ok {
		return
	}

	// HMAC-SHA256 signature check — it needs the body, so it runs after the read.
	if w.githubSecret != "" {
		signature := r.Header.Get(headerGitHubSignature)
		if !verifyGitHubSignature(body, w.githubSecret, signature) {
			slog.Warn("github webhook: invalid signature")
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	w.dispatchPushEvent(rw, "github", body, &GitHubPushEvent{})
}

// verifyGitHubSignature validates the X-Hub-Signature-256 header using HMAC-SHA256.
func verifyGitHubSignature(payload []byte, secret, signature string) bool {
	expected := githubSigPrefix + hmacSHA256(payload, secret)
	return hmac.Equal([]byte(expected), []byte(signature))
}

func hmacSHA256(data []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

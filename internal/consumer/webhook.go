package consumer

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"git-bridge/internal/mirror"
)

// maxBodySize is the maximum allowed webhook request body size (1MB).
const maxBodySize = 1 << 20

const (
	headerGitLabToken     = "X-Gitlab-Token"
	headerGitHubSignature = "X-Hub-Signature-256"
	githubSigPrefix       = "sha256="
	// zeroSHA는 GitLab/GitHub push 페이로드가 ref 삭제를 알릴 때 after에 보내는 값.
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

// pushEvent는 provider별 push 페이로드에서 repo 경로/ref/pusher 추출과 삭제 여부
// 판정을 추상화한다.
type pushEvent interface {
	target() (repoPath, ref, pusher string)
	// isDelete는 이 push가 ref 삭제 이벤트인지 반환한다(after == zeroSHA 등).
	isDelete() bool
}

func (e *GitLabPushEvent) target() (repoPath, ref, pusher string) {
	return e.Project.PathWithNamespace, e.Ref, e.UserName
}

// GitLab push/tag_push 모두 ref 삭제 시 after에 zeroSHA를 보낸다.
func (e *GitLabPushEvent) isDelete() bool {
	return e.After == zeroSHA
}

func (e *GitHubPushEvent) target() (repoPath, ref, pusher string) {
	pusher = e.Pusher.Name
	if pusher == "" {
		pusher = e.Sender.Login
	}
	return e.Repository.FullName, e.Ref, pusher
}

// GitHub은 deleted:true 플래그를 보내며, after도 zeroSHA가 된다(둘 다 허용).
func (e *GitHubPushEvent) isDelete() bool {
	return e.Deleted || e.After == zeroSHA
}

// readLimitedBody는 요청 본문을 maxBodySize까지 읽고, 실패 시 400을 쓴다.
// ok=false면 호출부는 즉시 return해야 한다.
func readLimitedBody(rw http.ResponseWriter, r *http.Request, logPrefix string) (body []byte, ok bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		slog.Error(logPrefix+": read body failed", "error", err)
		http.Error(rw, "bad request", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

// dispatchPushEvent는 webhook 공통 처리: 본문을 event로 파싱 → repo/ref/pusher 추출 →
// 로그 → 비동기 SyncByTarget 실행 → 200 응답.
func (w *Webhook) dispatchPushEvent(rw http.ResponseWriter, provider string, body []byte, event pushEvent) {
	if err := json.Unmarshal(body, event); err != nil {
		slog.Error(provider+" webhook: parse failed", "error", err)
		http.Error(rw, "bad request", http.StatusBadRequest)
		return
	}

	repoPath, ref, pusher := event.target()
	logger := slog.With("provider", provider, "repo", repoPath, "ref", ref, "pusher", pusher)
	logger.Info("received " + provider + " push event")

	meta := mirror.EventMeta{Ref: ref}
	if event.isDelete() {
		// ref 삭제 이벤트: target(gitlab/github) 삭제를 source(codecommit)로 전파한다.
		// tag가 아닌 모든 ref는 branch로 본다. 미지의 ref 종류는 branch로 떨어져
		// fullRefName이 refs/heads/를 붙이고 → RefExists=false → 무해한 no-op이 된다.
		refType := "branch"
		if meta.IsTag() {
			refType = "tag"
		}
		refName := meta.RefName()
		go func() {
			if err := w.mirrorSvc.SyncDeleteByTarget(w.ctx, provider, repoPath, refType, refName); err != nil {
				logger.Error("mirror delete sync failed", "error", err)
			}
		}()
	} else {
		go func() {
			if err := w.mirrorSvc.SyncByTarget(w.ctx, provider, repoPath, meta); err != nil {
				logger.Error("mirror sync failed", "error", err)
			}
		}()
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

	// secret token 검증 — GitLab은 헤더 토큰이라 본문을 읽기 전에 거부한다.
	if w.gitlabSecret != "" {
		if r.Header.Get(headerGitLabToken) != w.gitlabSecret {
			slog.Warn("gitlab webhook: invalid token")
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	body, ok := readLimitedBody(rw, r, "gitlab webhook")
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

	body, ok := readLimitedBody(rw, r, "github webhook")
	if !ok {
		return
	}

	// HMAC-SHA256 서명 검증 — 본문이 필요하므로 읽은 뒤에 검증한다.
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

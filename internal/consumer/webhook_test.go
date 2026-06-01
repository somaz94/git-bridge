package consumer

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git-bridge/internal/mirror"
)

// mockMirrorer is a no-op mock for testing webhook handlers.
type mockMirrorer struct{}

func (m *mockMirrorer) SyncByTarget(_ context.Context, providerName, repoPath string, _ mirror.EventMeta) error {
	return nil
}

func (m *mockMirrorer) SyncDeleteByTarget(_ context.Context, providerName, repoPath, refType, refName string) error {
	return nil
}

// signPayload generates a GitHub-style HMAC-SHA256 signature for the given payload.
func signPayload(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return fmt.Sprintf("sha256=%s", hex.EncodeToString(mac.Sum(nil)))
}

func TestGitLabHandler_ValidPush(t *testing.T) {
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "", "")

	payload := GitLabPushEvent{
		EventName: "push",
		Ref:       "refs/heads/main",
	}
	payload.Project.PathWithNamespace = "team/test-repo"
	payload.Repository.Name = "test-repo"

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader(body))
	w := httptest.NewRecorder()

	wh.GitLabHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestGitLabHandler_InvalidToken(t *testing.T) {
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "correct-secret", "")

	payload := GitLabPushEvent{}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "wrong-secret")
	w := httptest.NewRecorder()

	wh.GitLabHandler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestGitLabHandler_ValidToken(t *testing.T) {
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "my-secret", "")

	payload := GitLabPushEvent{Ref: "refs/heads/main"}
	payload.Project.PathWithNamespace = "team/test-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "my-secret")
	w := httptest.NewRecorder()

	wh.GitLabHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestGitLabHandler_MethodNotAllowed(t *testing.T) {
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "", "")

	req := httptest.NewRequest(http.MethodGet, "/webhook/gitlab", nil)
	w := httptest.NewRecorder()

	wh.GitLabHandler(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestGitLabHandler_InvalidBody(t *testing.T) {
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "", "")

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()

	wh.GitLabHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubHandler_ValidPush(t *testing.T) {
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "", "")

	payload := GitHubPushEvent{Ref: "refs/heads/main"}
	payload.Repository.Name = "test-repo"
	payload.Repository.FullName = "org/test-repo"

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	w := httptest.NewRecorder()

	wh.GitHubHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestGitHubHandler_ValidHMAC(t *testing.T) {
	secret := "my-secret"
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "", secret)

	payload := GitHubPushEvent{Ref: "refs/heads/main"}
	payload.Repository.Name = "test-repo"
	payload.Repository.FullName = "org/test-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", signPayload(body, secret))
	w := httptest.NewRecorder()

	wh.GitHubHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestGitHubHandler_InvalidHMAC(t *testing.T) {
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "", "correct-secret")

	payload := GitHubPushEvent{}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	w := httptest.NewRecorder()

	wh.GitHubHandler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestGitHubHandler_MissingSignature(t *testing.T) {
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "", "my-secret")

	payload := GitHubPushEvent{}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	// No X-Hub-Signature-256 header
	w := httptest.NewRecorder()

	wh.GitHubHandler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestGitHubHandler_MethodNotAllowed(t *testing.T) {
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "", "")

	req := httptest.NewRequest(http.MethodGet, "/webhook/github", nil)
	w := httptest.NewRecorder()

	wh.GitHubHandler(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestGitHubHandler_InvalidBody(t *testing.T) {
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "", "")

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader([]byte("{invalid")))
	w := httptest.NewRecorder()

	wh.GitHubHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubHandler_NoSecretSkipsVerification(t *testing.T) {
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "", "")

	payload := GitHubPushEvent{Ref: "refs/heads/main"}
	payload.Repository.Name = "test-repo"
	payload.Repository.FullName = "org/test-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	w := httptest.NewRecorder()

	wh.GitHubHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (skip verification when secret empty)", w.Code)
	}
}

func TestGitLabHandler_NoSecretSkipsVerification(t *testing.T) {
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "", "")

	payload := GitLabPushEvent{Ref: "refs/heads/main"}
	payload.Project.PathWithNamespace = "team/test-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader(body))
	w := httptest.NewRecorder()

	wh.GitLabHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (skip verification when secret empty)", w.Code)
	}
}

// --- goroutine coverage: verify SyncByTarget is called ---

type trackingMirrorer struct {
	called    chan string
	meta      chan mirror.EventMeta
	deleted   chan string // captures "provider/repoPath/refType/refName" on SyncDeleteByTarget
	err       error
	deleteErr error
}

func newTrackingMirrorer(err error) *trackingMirrorer {
	return &trackingMirrorer{
		called:  make(chan string, 1),
		meta:    make(chan mirror.EventMeta, 1),
		deleted: make(chan string, 1),
		err:     err,
	}
}

func (m *trackingMirrorer) SyncByTarget(_ context.Context, providerName, repoPath string, meta mirror.EventMeta) error {
	m.called <- providerName + "/" + repoPath
	m.meta <- meta
	return m.err
}

func (m *trackingMirrorer) SyncDeleteByTarget(_ context.Context, providerName, repoPath, refType, refName string) error {
	m.deleted <- providerName + "/" + repoPath + "/" + refType + "/" + refName
	return m.deleteErr
}

func TestGitLabHandler_SyncByTargetCalled(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(context.Background(), mock, "", "")

	payload := GitLabPushEvent{Ref: "refs/heads/main"}
	payload.Project.PathWithNamespace = "team/test-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader(body))
	w := httptest.NewRecorder()
	wh.GitLabHandler(w, req)

	select {
	case got := <-mock.called:
		if got != "gitlab/team/test-repo" {
			t.Errorf("unexpected call: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SyncByTarget was not called")
	}
}

func TestGitHubHandler_SyncByTargetCalled(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(context.Background(), mock, "", "")

	payload := GitHubPushEvent{Ref: "refs/heads/main"}
	payload.Repository.FullName = "org/test-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	w := httptest.NewRecorder()
	wh.GitHubHandler(w, req)

	select {
	case got := <-mock.called:
		if got != "github/org/test-repo" {
			t.Errorf("unexpected call: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SyncByTarget was not called")
	}
}

func TestGitLabHandler_SyncByTargetError(t *testing.T) {
	mock := newTrackingMirrorer(fmt.Errorf("sync failed"))
	wh := NewWebhook(context.Background(), mock, "", "")

	payload := GitLabPushEvent{Ref: "refs/heads/main"}
	payload.Project.PathWithNamespace = "team/err-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader(body))
	w := httptest.NewRecorder()
	wh.GitLabHandler(w, req)

	// Response should still be 200 (async)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	select {
	case <-mock.called:
	case <-time.After(2 * time.Second):
		t.Fatal("SyncByTarget was not called")
	}
}

func TestGitHubHandler_SyncByTargetError(t *testing.T) {
	mock := newTrackingMirrorer(fmt.Errorf("sync failed"))
	wh := NewWebhook(context.Background(), mock, "", "")

	payload := GitHubPushEvent{Ref: "refs/heads/main"}
	payload.Repository.FullName = "org/err-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	w := httptest.NewRecorder()
	wh.GitHubHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	select {
	case <-mock.called:
	case <-time.After(2 * time.Second):
		t.Fatal("SyncByTarget was not called")
	}
}

// errReader always returns an error on Read.
type errReader struct{}

func (e *errReader) Read(_ []byte) (int, error) {
	return 0, fmt.Errorf("read error")
}

func TestGitLabHandler_ReadBodyError(t *testing.T) {
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "", "")

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", &errReader{})
	w := httptest.NewRecorder()

	wh.GitLabHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubHandler_ReadBodyError(t *testing.T) {
	wh := NewWebhook(context.Background(), &mockMirrorer{}, "", "")

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", &errReader{})
	w := httptest.NewRecorder()

	wh.GitHubHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestVerifyGitHubSignature(t *testing.T) {
	secret := "test-secret"
	payload := []byte(`{"ref":"refs/heads/main"}`)

	validSig := signPayload(payload, secret)
	if !verifyGitHubSignature(payload, secret, validSig) {
		t.Error("valid signature should pass verification")
	}

	if verifyGitHubSignature(payload, secret, "sha256=wrong") {
		t.Error("invalid signature should fail verification")
	}

	if verifyGitHubSignature(payload, secret, "") {
		t.Error("empty signature should fail verification")
	}

	if verifyGitHubSignature(payload, "wrong-secret", validSig) {
		t.Error("wrong secret should fail verification")
	}
}

// --- EventMeta propagation tests ---

func TestGitLabHandler_PassesRefMeta(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(context.Background(), mock, "", "")

	payload := GitLabPushEvent{
		EventName: "push",
		UserName:  "somaz",
		Ref:       "refs/heads/feature/login",
	}
	payload.Project.PathWithNamespace = "team/test-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader(body))
	w := httptest.NewRecorder()
	wh.GitLabHandler(w, req)

	select {
	case meta := <-mock.meta:
		if meta.Ref != "refs/heads/feature/login" {
			t.Errorf("expected ref 'refs/heads/feature/login', got %q", meta.Ref)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SyncByTarget was not called")
	}
}

func TestGitHubHandler_PassesRefMeta(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(context.Background(), mock, "", "")

	payload := GitHubPushEvent{Ref: "refs/tags/v1.0.0"}
	payload.Pusher.Name = "somaz"
	payload.Repository.FullName = "org/test-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	w := httptest.NewRecorder()
	wh.GitHubHandler(w, req)

	select {
	case meta := <-mock.meta:
		if meta.Ref != "refs/tags/v1.0.0" {
			t.Errorf("expected ref 'refs/tags/v1.0.0', got %q", meta.Ref)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SyncByTarget was not called")
	}
}

// --- delete-event dispatch tests ---

// GitLab after==zeroSHA 브랜치 삭제 → SyncDeleteByTarget(branch).
func TestGitLabHandler_BranchDeleteDispatchesDelete(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(context.Background(), mock, "", "")

	payload := GitLabPushEvent{Ref: "refs/heads/old-feature", After: zeroSHA}
	payload.Project.PathWithNamespace = "team/test-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader(body))
	w := httptest.NewRecorder()
	wh.GitLabHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	select {
	case got := <-mock.deleted:
		if got != "gitlab/team/test-repo/branch/old-feature" {
			t.Errorf("unexpected delete call: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SyncDeleteByTarget was not called")
	}
}

// GitLab tag_push after==zeroSHA 태그 삭제 → SyncDeleteByTarget(tag).
func TestGitLabHandler_TagDeleteDispatchesDelete(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(context.Background(), mock, "", "")

	payload := GitLabPushEvent{Ref: "refs/tags/v0.9.0", After: zeroSHA}
	payload.Project.PathWithNamespace = "team/test-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader(body))
	w := httptest.NewRecorder()
	wh.GitLabHandler(w, req)

	select {
	case got := <-mock.deleted:
		if got != "gitlab/team/test-repo/tag/v0.9.0" {
			t.Errorf("unexpected delete call: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SyncDeleteByTarget was not called")
	}
}

// GitHub deleted:true → SyncDeleteByTarget(branch).
func TestGitHubHandler_DeletedFlagDispatchesDelete(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(context.Background(), mock, "", "")

	payload := GitHubPushEvent{Ref: "refs/heads/stale", Deleted: true}
	payload.Repository.FullName = "org/test-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	w := httptest.NewRecorder()
	wh.GitHubHandler(w, req)

	select {
	case got := <-mock.deleted:
		if got != "github/org/test-repo/branch/stale" {
			t.Errorf("unexpected delete call: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SyncDeleteByTarget was not called")
	}
}

// GitHub after==zeroSHA(deleted 플래그 없이)도 삭제로 처리한다.
func TestGitHubHandler_AfterZeroDispatchesDelete(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(context.Background(), mock, "", "")

	payload := GitHubPushEvent{Ref: "refs/tags/v0.1.0", After: zeroSHA}
	payload.Repository.FullName = "org/test-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	w := httptest.NewRecorder()
	wh.GitHubHandler(w, req)

	select {
	case got := <-mock.deleted:
		if got != "github/org/test-repo/tag/v0.1.0" {
			t.Errorf("unexpected delete call: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SyncDeleteByTarget was not called")
	}
}

// 회귀 가드: 일반 push(after != zeroSHA)는 여전히 SyncByTarget으로 가야 한다.
func TestGitLabHandler_NormalPushDoesNotDelete(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(context.Background(), mock, "", "")

	payload := GitLabPushEvent{Ref: "refs/heads/main", After: "abc1234567890abc1234567890abc1234567890a"}
	payload.Project.PathWithNamespace = "team/test-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader(body))
	w := httptest.NewRecorder()
	wh.GitLabHandler(w, req)

	select {
	case got := <-mock.called:
		if got != "gitlab/team/test-repo" {
			t.Errorf("unexpected sync call: %q", got)
		}
	case <-mock.deleted:
		t.Fatal("normal push must not dispatch a delete")
	case <-time.After(2 * time.Second):
		t.Fatal("SyncByTarget was not called")
	}
}

// 삭제 비동기 에러는 로그만 남기고 응답은 200(accepted).
func TestGitLabHandler_DeleteErrorStillReturns200(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	mock.deleteErr = fmt.Errorf("delete sync failed")
	wh := NewWebhook(context.Background(), mock, "", "")

	payload := GitLabPushEvent{Ref: "refs/heads/old", After: zeroSHA}
	payload.Project.PathWithNamespace = "team/err-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader(body))
	w := httptest.NewRecorder()
	wh.GitLabHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	select {
	case <-mock.deleted:
	case <-time.After(2 * time.Second):
		t.Fatal("SyncDeleteByTarget was not called")
	}
}

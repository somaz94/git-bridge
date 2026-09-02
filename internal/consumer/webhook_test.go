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

	"git-bridge/internal/config"
	"git-bridge/internal/mirror"
	"git-bridge/internal/task"
	"strings"
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
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "", "", nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "correct-secret", "", nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "my-secret", "", nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "", "", nil)

	req := httptest.NewRequest(http.MethodGet, "/webhook/gitlab", nil)
	w := httptest.NewRecorder()

	wh.GitLabHandler(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestGitLabHandler_InvalidBody(t *testing.T) {
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "", "", nil)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()

	wh.GitLabHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubHandler_ValidPush(t *testing.T) {
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "", "", nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "", secret, nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "", "correct-secret", nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "", "my-secret", nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "", "", nil)

	req := httptest.NewRequest(http.MethodGet, "/webhook/github", nil)
	w := httptest.NewRecorder()

	wh.GitHubHandler(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestGitHubHandler_InvalidBody(t *testing.T) {
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "", "", nil)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader([]byte("{invalid")))
	w := httptest.NewRecorder()

	wh.GitHubHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubHandler_NoSecretSkipsVerification(t *testing.T) {
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "", "", nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "", "", nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "", nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "", nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "", nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "", nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "", "", nil)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", &errReader{})
	w := httptest.NewRecorder()

	wh.GitLabHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubHandler_ReadBodyError(t *testing.T) {
	wh := NewWebhook(task.NewGroup(context.Background()), &mockMirrorer{}, "", "", nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "", nil)

	payload := GitLabPushEvent{
		EventName: "push",
		UserName:  "alice",
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
	wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "", nil)

	payload := GitHubPushEvent{Ref: "refs/tags/v1.0.0"}
	payload.Pusher.Name = "alice"
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

// GitLab branch delete with after==zeroSHA → SyncDeleteByTarget(branch).
func TestGitLabHandler_BranchDeleteDispatchesDelete(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "", nil)

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

// GitLab tag_push tag delete with after==zeroSHA → SyncDeleteByTarget(tag).
func TestGitLabHandler_TagDeleteDispatchesDelete(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "", nil)

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
	wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "", nil)

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

// GitHub after==zeroSHA (with no deleted flag) counts as a delete too.
func TestGitHubHandler_AfterZeroDispatchesDelete(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "", nil)

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

// Regression guard: a normal push (after != zeroSHA) must still go to SyncByTarget.
func TestGitLabHandler_NormalPushDoesNotDelete(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "", nil)

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

// An async delete error is only logged; the response is still 200 (accepted).
func TestGitLabHandler_DeleteErrorStillReturns200(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	mock.deleteErr = fmt.Errorf("delete sync failed")
	wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "", nil)

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

// blockingMirrorer holds a push sync open so a test can watch shutdown while
// one is genuinely running.
type blockingMirrorer struct {
	started  chan struct{}
	release  chan struct{}
	ctxAlive chan bool
}

func newBlockingMirrorer() *blockingMirrorer {
	return &blockingMirrorer{
		started:  make(chan struct{}, 1),
		release:  make(chan struct{}),
		ctxAlive: make(chan bool, 1),
	}
}

func (b *blockingMirrorer) SyncByTarget(ctx context.Context, _, _ string, _ mirror.EventMeta) error {
	b.started <- struct{}{}
	<-b.release
	b.ctxAlive <- ctx.Err() == nil
	return nil
}

func (b *blockingMirrorer) SyncDeleteByTarget(ctx context.Context, _, _, _, _ string) error {
	b.started <- struct{}{}
	<-b.release
	b.ctxAlive <- ctx.Err() == nil
	return nil
}

// A webhook is answered immediately and the sync runs on. Shutdown has to be
// able to wait for it: an untracked goroutine here is what let SIGTERM kill a
// fetch mid-run and strand a pack .keep marker.
func TestWebhookSyncIsWaitableAfterTheResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"push", `{"project":{"path_with_namespace":"team/repo"},"ref":"refs/heads/main","after":"abc123"}`},
		{"delete", `{"project":{"path_with_namespace":"team/repo"},"ref":"refs/heads/gone","after":"` + zeroSHA + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newBlockingMirrorer()
			tasks := task.NewGroup(context.Background())
			w := NewWebhook(tasks, m, "", "", nil)

			rec := httptest.NewRecorder()
			w.GitLabHandler(rec, httptest.NewRequest(http.MethodPost, "/webhook/gitlab",
				strings.NewReader(tc.body)))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			<-m.started

			drained := make(chan struct{})
			go func() { defer close(drained); tasks.Wait() }()
			select {
			case <-drained:
				t.Fatal("Wait() returned while the sync was still running")
			case <-time.After(50 * time.Millisecond):
			}

			close(m.release)
			if alive := <-m.ctxAlive; !alive {
				t.Error("sync context was cancelled before the sync finished")
			}
			<-drained
		})
	}
}

// --- narrowing the provider by instance host ---

// TestGitLabHandler_DispatchKeyByInstanceHost pins down how the payload's
// project.web_url host decides the dispatch key.
//
// Half of it is regression cover. With no web_url (an older instance), or a host
// that matches no base_url, the type string "gitlab" must go through exactly as
// before narrowing existed — every repository already running takes that path.
func TestGitLabHandler_DispatchKeyByInstanceHost(t *testing.T) {
	hosts := config.HostResolver{
		"gitlab.example.com":     "gitlab-main",
		"gitlab-old.example.com": "gitlab-old",
	}

	tests := []struct {
		name    string
		webURL  string
		hosts   config.HostResolver
		wantKey string
	}{
		{
			name:    "host narrows to the provider name",
			webURL:  "https://gitlab-old.example.com/team/test-repo",
			hosts:   hosts,
			wantKey: "gitlab-old",
		},
		{
			name:    "the other instance narrows the other way",
			webURL:  "https://gitlab.example.com/team/test-repo",
			hosts:   hosts,
			wantKey: "gitlab-main",
		},
		{
			name:    "port and case are normalized",
			webURL:  "HTTP://GitLab-Old.Example.com/team/test-repo",
			hosts:   hosts,
			wantKey: "gitlab-old",
		},
		{
			// An instance that sends no web_url, like GitLab 13.x.
			name:    "missing web_url falls back to the provider type",
			webURL:  "",
			hosts:   hosts,
			wantKey: "gitlab",
		},
		{
			name:    "unknown host falls back to the provider type",
			webURL:  "https://gitlab-somewhere-else.example.com/team/test-repo",
			hosts:   hosts,
			wantKey: "gitlab",
		},
		{
			// A deployment with no index at all (no base_url in the config).
			name:    "nil resolver falls back to the provider type",
			webURL:  "https://gitlab-old.example.com/team/test-repo",
			hosts:   nil,
			wantKey: "gitlab",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock := newTrackingMirrorer(nil)
			wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "", tc.hosts)

			payload := GitLabPushEvent{Ref: "refs/heads/main"}
			payload.Project.PathWithNamespace = "team/test-repo"
			payload.Project.WebURL = tc.webURL
			body, _ := json.Marshal(payload)

			req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader(body))
			wh.GitLabHandler(httptest.NewRecorder(), req)

			select {
			case got := <-mock.called:
				if want := tc.wantKey + "/team/test-repo"; got != want {
					t.Errorf("dispatched as %q, want %q", got, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("SyncByTarget was not called")
			}
		})
	}
}

// TestGitLabHandler_DeleteDispatchKeyByInstanceHost checks a delete event goes
// through under the same key. push and delete take different call paths, so both
// are pinned down.
func TestGitLabHandler_DeleteDispatchKeyByInstanceHost(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "",
		config.HostResolver{"gitlab-old.example.com": "gitlab-old"})

	payload := GitLabPushEvent{Ref: "refs/heads/feature", After: zeroSHA}
	payload.Project.PathWithNamespace = "team/test-repo"
	payload.Project.WebURL = "https://gitlab-old.example.com/team/test-repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewReader(body))
	wh.GitLabHandler(httptest.NewRecorder(), req)

	select {
	case got := <-mock.deleted:
		if want := "gitlab-old/team/test-repo/branch/feature"; got != want {
			t.Errorf("dispatched as %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SyncDeleteByTarget was not called")
	}
}

// TestGitHubHandler_IgnoresHostNarrowing — GitHub is a single instance, so it is
// never narrowed. Even with github.com in the index, the type goes through as is.
func TestGitHubHandler_IgnoresHostNarrowing(t *testing.T) {
	mock := newTrackingMirrorer(nil)
	wh := NewWebhook(task.NewGroup(context.Background()), mock, "", "",
		config.HostResolver{"github.com": "github-main"})

	payload := GitHubPushEvent{Ref: "refs/heads/main"}
	payload.Repository.FullName = "org/repo"
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	wh.GitHubHandler(httptest.NewRecorder(), req)

	select {
	case got := <-mock.called:
		if want := "github/org/repo"; got != want {
			t.Errorf("dispatched as %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SyncByTarget was not called")
	}
}

// TestGitLabPushEvent_ParsesWebURL checks that a real GitLab payload parses under
// these field names. A wrong tag makes every test above pass down the fallback
// path, which is hard to notice.
func TestGitLabPushEvent_ParsesWebURL(t *testing.T) {
	// Excerpted from a push payload sent by the gitlab.example.com instance.
	const raw = `{"object_kind":"push","event_name":"push","ref":"refs/heads/main",
	  "project":{"path_with_namespace":"team/test-repo",
	  "web_url":"http://gitlab.example.com/team/test-repo"}}`

	var e GitLabPushEvent
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.Project.WebURL != "http://gitlab.example.com/team/test-repo" {
		t.Errorf("WebURL = %q, want the payload's project.web_url", e.Project.WebURL)
	}
	url, routable := e.instanceURL()
	if !routable || url != e.Project.WebURL {
		t.Errorf("instanceURL() = (%q, %v), want the web_url and routable=true", url, routable)
	}
}

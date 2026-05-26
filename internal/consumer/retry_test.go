package consumer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git-bridge/internal/mirror"
)

// mockRetrier records the parameters of Retry calls and returns a fixed error.
type mockRetrier struct {
	called    chan struct{}
	gotRepo   string
	gotDir    string
	gotMeta   mirror.EventMeta
	returnErr error
}

func newMockRetrier(err error) *mockRetrier {
	return &mockRetrier{called: make(chan struct{}, 1), returnErr: err}
}

func (m *mockRetrier) Retry(_ context.Context, repoName, direction string, meta mirror.EventMeta) error {
	m.gotRepo = repoName
	m.gotDir = direction
	m.gotMeta = meta
	m.called <- struct{}{}
	return m.returnErr
}

func newRetryRequest(t *testing.T, token string, body any) *http.Request {
	t.Helper()
	var rdr *bytes.Reader
	switch v := body.(type) {
	case []byte:
		rdr = bytes.NewReader(v)
	case string:
		rdr = bytes.NewReader([]byte(v))
	default:
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(http.MethodPost, "/retry/mirror", rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestRetryHandler_TokenUnset_Returns404(t *testing.T) {
	h := NewRetry(context.Background(), newMockRetrier(nil), "")
	req := newRetryRequest(t, "anything", RetryRequest{Repo: "x", Direction: "auto"})
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestRetryHandler_InvalidToken_Returns401(t *testing.T) {
	h := NewRetry(context.Background(), newMockRetrier(nil), "correct-token")
	req := newRetryRequest(t, "wrong-token", RetryRequest{Repo: "x", Direction: "auto"})
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRetryHandler_MalformedAuthHeader_Returns401(t *testing.T) {
	// "Authorization: <token>" (no "Bearer " scheme) must be rejected,
	// even when the bare value happens to match the configured token.
	h := NewRetry(context.Background(), newMockRetrier(nil), "correct-token")
	req := newRetryRequest(t, "", RetryRequest{Repo: "x", Direction: "auto"})
	req.Header.Set("Authorization", "correct-token")
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRetryHandler_MissingAuthHeader_Returns401(t *testing.T) {
	h := NewRetry(context.Background(), newMockRetrier(nil), "correct-token")
	req := newRetryRequest(t, "", RetryRequest{Repo: "x", Direction: "auto"})
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRetryHandler_MethodNotPost_Returns405(t *testing.T) {
	h := NewRetry(context.Background(), newMockRetrier(nil), "tok")
	req := httptest.NewRequest(http.MethodGet, "/retry/mirror", nil)
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestRetryHandler_InvalidJSON_Returns400(t *testing.T) {
	h := NewRetry(context.Background(), newMockRetrier(nil), "tok")
	req := newRetryRequest(t, "tok", "{not json")
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestRetryHandler_EmptyRepo_Returns400(t *testing.T) {
	h := NewRetry(context.Background(), newMockRetrier(nil), "tok")
	req := newRetryRequest(t, "tok", RetryRequest{Repo: "  ", Direction: "auto"})
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestRetryHandler_InvalidDirection_Returns400(t *testing.T) {
	h := NewRetry(context.Background(), newMockRetrier(nil), "tok")
	req := newRetryRequest(t, "tok", RetryRequest{Repo: "x", Direction: "sideways"})
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestRetryHandler_DirectionOmitted_DefaultsToAuto(t *testing.T) {
	mr := newMockRetrier(nil)
	h := NewRetry(context.Background(), mr, "tok")
	req := newRetryRequest(t, "tok", RetryRequest{Repo: "x"}) // direction omitted
	w := httptest.NewRecorder()

	h.Handler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	select {
	case <-mr.called:
		if mr.gotDir != "auto" {
			t.Errorf("direction = %q, want %q", mr.gotDir, "auto")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Retry was not called")
	}
}

func TestRetryHandler_Success_CallsRetry(t *testing.T) {
	mr := newMockRetrier(nil)
	h := NewRetry(context.Background(), mr, "tok")
	req := newRetryRequest(t, "tok", RetryRequest{
		Repo:      "my-repo",
		Direction: "target-to-source",
		Ref:       "refs/tags/Build-2231",
	})
	w := httptest.NewRecorder()

	h.Handler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	select {
	case <-mr.called:
		if mr.gotRepo != "my-repo" || mr.gotDir != "target-to-source" {
			t.Errorf("unexpected call: repo=%q dir=%q", mr.gotRepo, mr.gotDir)
		}
		if mr.gotMeta.Ref != "refs/tags/Build-2231" {
			t.Errorf("ref = %q, want %q", mr.gotMeta.Ref, "refs/tags/Build-2231")
		}
		if mr.gotMeta.Source != "retry-api" {
			t.Errorf("source = %q, want %q", mr.gotMeta.Source, "retry-api")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Retry was not called")
	}

	// Response body should contain queued_at and the echoed fields.
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "accepted" {
		t.Errorf("status field = %q, want accepted", resp["status"])
	}
	if resp["repo"] != "my-repo" {
		t.Errorf("repo field = %q, want my-repo", resp["repo"])
	}
}

func TestRetryHandler_SyncError_StillReturns200(t *testing.T) {
	mr := newMockRetrier(fmt.Errorf("retry boom"))
	h := NewRetry(context.Background(), mr, "tok")
	req := newRetryRequest(t, "tok", RetryRequest{Repo: "x", Direction: "auto"})
	w := httptest.NewRecorder()

	h.Handler(w, req)

	// The handler is async — the goroutine error is logged, not surfaced to the caller.
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	select {
	case <-mr.called:
	case <-time.After(2 * time.Second):
		t.Fatal("Retry was not called")
	}
}

func TestRetryHandler_ReadBodyError_Returns400(t *testing.T) {
	h := NewRetry(context.Background(), newMockRetrier(nil), "tok")
	req := httptest.NewRequest(http.MethodPost, "/retry/mirror", &errReader{})
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestIsValidRetryDirection(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"source-to-target", true},
		{"target-to-source", true},
		{"auto", true},
		{"Source-To-Target", true},
		{"AUTO", true},
		{"", false},
		{"sideways", false},
		{"both", false},
	}
	for _, tt := range tests {
		if got := isValidRetryDirection(tt.in); got != tt.want {
			t.Errorf("isValidRetryDirection(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

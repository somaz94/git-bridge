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
	"git-bridge/internal/task"
	"strings"
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
	h := NewRetry(task.NewGroup(context.Background()), newMockRetrier(nil), "")
	req := newRetryRequest(t, "anything", RetryRequest{Repo: "x", Direction: "auto"})
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestRetryHandler_InvalidToken_Returns401(t *testing.T) {
	h := NewRetry(task.NewGroup(context.Background()), newMockRetrier(nil), "correct-token")
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
	h := NewRetry(task.NewGroup(context.Background()), newMockRetrier(nil), "correct-token")
	req := newRetryRequest(t, "", RetryRequest{Repo: "x", Direction: "auto"})
	req.Header.Set("Authorization", "correct-token")
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRetryHandler_MissingAuthHeader_Returns401(t *testing.T) {
	h := NewRetry(task.NewGroup(context.Background()), newMockRetrier(nil), "correct-token")
	req := newRetryRequest(t, "", RetryRequest{Repo: "x", Direction: "auto"})
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRetryHandler_MethodNotPost_Returns405(t *testing.T) {
	h := NewRetry(task.NewGroup(context.Background()), newMockRetrier(nil), "tok")
	req := httptest.NewRequest(http.MethodGet, "/retry/mirror", nil)
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestRetryHandler_InvalidJSON_Returns400(t *testing.T) {
	h := NewRetry(task.NewGroup(context.Background()), newMockRetrier(nil), "tok")
	req := newRetryRequest(t, "tok", "{not json")
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestRetryHandler_EmptyRepo_Returns400(t *testing.T) {
	h := NewRetry(task.NewGroup(context.Background()), newMockRetrier(nil), "tok")
	req := newRetryRequest(t, "tok", RetryRequest{Repo: "  ", Direction: "auto"})
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestRetryHandler_InvalidDirection_Returns400(t *testing.T) {
	h := NewRetry(task.NewGroup(context.Background()), newMockRetrier(nil), "tok")
	req := newRetryRequest(t, "tok", RetryRequest{Repo: "x", Direction: "sideways"})
	w := httptest.NewRecorder()

	h.Handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestRetryHandler_DirectionOmitted_DefaultsToAuto(t *testing.T) {
	mr := newMockRetrier(nil)
	h := NewRetry(task.NewGroup(context.Background()), mr, "tok")
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
	h := NewRetry(task.NewGroup(context.Background()), mr, "tok")
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
	h := NewRetry(task.NewGroup(context.Background()), mr, "tok")
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
	h := NewRetry(task.NewGroup(context.Background()), newMockRetrier(nil), "tok")
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
		if got := IsValidRetryDirection(tt.in); got != tt.want {
			t.Errorf("IsValidRetryDirection(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// blockingRetrier holds a sync open until released, so a test can observe what
// shutdown does while one is genuinely in flight.
type blockingRetrier struct {
	started  chan struct{}
	release  chan struct{}
	ctxAlive chan bool
}

func newBlockingRetrier() *blockingRetrier {
	return &blockingRetrier{
		started:  make(chan struct{}, 1),
		release:  make(chan struct{}),
		ctxAlive: make(chan bool, 1),
	}
}

func (b *blockingRetrier) Retry(ctx context.Context, _, _ string, _ mirror.EventMeta) error {
	b.started <- struct{}{}
	<-b.release
	// Report whether the work context survived long enough to finish. This is
	// the property that matters: a cancelled context here means git was killed
	// mid-command.
	b.ctxAlive <- ctx.Err() == nil
	return nil
}

// The handler answers "accepted" and lets the sync run on. Shutdown must be
// able to wait for that sync — before the task group existed the goroutine was
// untracked, so a SIGTERM landing here killed the git command mid-fetch and
// left an abandoned pack .keep marker behind.
func TestRetryHandlerSyncIsWaitableAndOutlivesTheRequest(t *testing.T) {
	retrier := newBlockingRetrier()
	tasks := task.NewGroup(context.Background())
	h := NewRetry(tasks, retrier, "tok")

	req := httptest.NewRequest(http.MethodPost, "/retry/mirror",
		strings.NewReader(`{"repo":"my-repo","direction":"auto"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.Handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	<-retrier.started

	// The response is already written, yet the group still counts the sync.
	drained := make(chan struct{})
	go func() { defer close(drained); tasks.Wait() }()
	select {
	case <-drained:
		t.Fatal("Wait() returned while the sync was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(retrier.release)
	if alive := <-retrier.ctxAlive; !alive {
		t.Error("sync context was cancelled before the sync finished")
	}
	<-drained
}

// The cron and a hand-run curl share this route, so the caller declares itself.
// The vocabulary has to stay closed: whoever holds the token must not be able
// to write arbitrary text into the history's trigger column.
func TestResolveRetrySource(t *testing.T) {
	cases := map[string]struct {
		hint   string
		want   string
		wantOK bool
	}{
		"omitted means a human or ad-hoc caller": {"", mirror.SourceRetryAPI, true},
		"cron identifies the reconcile job":      {"cron", mirror.SourceCron, true},
		"case and padding are forgiven":          {"  CRON ", mirror.SourceCron, true},
		"webhook is not a caller identity":       {"webhook", "", false},
		"console cannot be claimed remotely":     {"console", "", false},
		"arbitrary text is rejected":             {"anything", "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := resolveRetrySource(tc.hint)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("resolveRetrySource(%q) = (%q, %v), want (%q, %v)",
					tc.hint, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// A rejected source must not start a sync — otherwise the mirror runs while the
// caller is told the request was bad.
func TestRetryHandlerRejectsAnUnknownSource(t *testing.T) {
	retrier := newMockRetrier(nil)
	h := NewRetry(task.NewGroup(context.Background()), retrier, "tok")

	req := httptest.NewRequest(http.MethodPost, "/retry/mirror",
		strings.NewReader(`{"repo":"my-repo","source":"webhook"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.Handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	select {
	case <-retrier.called:
		t.Error("a sync was started for a rejected source")
	case <-time.After(50 * time.Millisecond):
	}
}

// The cron's events have to be distinguishable in the history, which is the
// whole reason the hint exists.
func TestRetryHandlerRecordsTheCronTrigger(t *testing.T) {
	retrier := newMockRetrier(nil)
	h := NewRetry(task.NewGroup(context.Background()), retrier, "tok")

	req := httptest.NewRequest(http.MethodPost, "/retry/mirror",
		strings.NewReader(`{"repo":"my-repo","direction":"auto","source":"cron"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.Handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	<-retrier.called
	if got := retrier.gotMeta.Source; got != mirror.SourceCron {
		t.Errorf("recorded source = %q, want %q", got, mirror.SourceCron)
	}
}

// --- force ---

// postRetry runs one authorised retry request and returns the recorder.
func postRetry(t *testing.T, retrier *mockRetrier, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewRetry(task.NewGroup(context.Background()), retrier, "tok")
	req := httptest.NewRequest(http.MethodPost, "/retry/mirror", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.Handler(rec, req)
	return rec
}

// A force with no ref would carry the bypass across every ref in the repository
// in one call — the shape of the 2026-08-10 loss. One force, one ref.
func TestRetryHandler_ForceWithoutRef_Returns400(t *testing.T) {
	retrier := newMockRetrier(nil)
	rec := postRetry(t, retrier, `{"repo":"my-repo","direction":"auto","force":true}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	select {
	case <-retrier.called:
		t.Error("a sync was started for a rejected force request")
	case <-time.After(50 * time.Millisecond):
	}
}

// The reconcile CronJob runs unattended over every repository, so a bypass
// there is one nobody chose. Force is for a person.
func TestRetryHandler_ForceFromCron_Returns400(t *testing.T) {
	retrier := newMockRetrier(nil)
	rec := postRetry(t, retrier,
		`{"repo":"my-repo","direction":"auto","ref":"refs/heads/main","dest":"deadbeef","source":"cron","force":true}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	select {
	case <-retrier.called:
		t.Error("cron must not be able to force")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRetryHandler_ForceWithRef_ReachesTheSync(t *testing.T) {
	retrier := newMockRetrier(nil)
	rec := postRetry(t, retrier,
		`{"repo":"my-repo","direction":"auto","ref":"refs/heads/main","dest":"deadbeef","force":true,"actor":"alice"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	<-retrier.called
	if !retrier.gotMeta.Force {
		t.Error("force did not reach the sync")
	}
	if retrier.gotMeta.Actor != "alice" {
		t.Errorf("actor = %q, want alice", retrier.gotMeta.Actor)
	}
	if retrier.gotMeta.ForceLease != "deadbeef" {
		t.Errorf("force lease = %q, want the dest the caller named", retrier.gotMeta.ForceLease)
	}
}

// A force without the tip it is overwriting is a force against whatever the
// destination happens to hold when the push finally runs, which after a
// minute-long fetch is not necessarily what the caller decided about.
func TestRetryHandler_ForceWithoutDest_Returns400(t *testing.T) {
	retrier := newMockRetrier(nil)
	rec := postRetry(t, retrier,
		`{"repo":"my-repo","direction":"auto","ref":"refs/heads/main","force":true}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	select {
	case <-retrier.called:
		t.Error("a sync was started for a force with no expected tip")
	case <-time.After(50 * time.Millisecond):
	}
}

// The guard is only useful if it is on by default; an omitted field must never
// read as permission.
func TestRetryHandler_OmittedForceIsNotForced(t *testing.T) {
	retrier := newMockRetrier(nil)
	rec := postRetry(t, retrier, `{"repo":"my-repo","direction":"auto","ref":"refs/heads/main"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	<-retrier.called
	if retrier.gotMeta.Force {
		t.Error("an omitted force must not be treated as a force")
	}
}

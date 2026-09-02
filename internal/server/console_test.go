package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"git-bridge/internal/history"
	"git-bridge/internal/mirror"
	"git-bridge/internal/task"
)

// fakeReader serves canned events and remembers what the handler asked for.
type fakeReader struct {
	events  []history.Event
	repos   []string
	sources []string
	gotQ    history.Query
}

func (f *fakeReader) Recent(q history.Query) []history.Event {
	f.gotQ = q
	return f.events
}

func (f *fakeReader) Repos() []string { return f.repos }

func (f *fakeReader) Sources() []string { return f.sources }

// fakeRetrier records retry requests instead of mirroring anything.
type fakeRetrier struct {
	mu        sync.Mutex
	calls     int
	repo      string
	direction string
	err       error
	// dirs maps a destination endpoint to the direction that writes it, and
	// dirErr is what an endpoint outside the map answers — the two halves of
	// what the real service resolves from the config.
	dirs   map[string]string
	dirErr error
}

func (f *fakeRetrier) Retry(_ context.Context, repo, direction string, _ mirror.EventMeta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.repo = repo
	f.direction = direction
	return f.err
}

func (f *fakeRetrier) DirectionTo(_, toEndpoint string) (string, error) {
	if dir, ok := f.dirs[toEndpoint]; ok {
		return dir, nil
	}
	if f.dirErr != nil {
		return "", f.dirErr
	}
	return "", errors.New("not a side of this repo")
}

func (f *fakeRetrier) snapshot() (int, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.repo, f.direction
}

func sampleEvents() []history.Event {
	return []history.Event{
		{
			TS: time.Date(2026, 7, 28, 4, 12, 33, 0, time.UTC), Repo: "git-bridge-test",
			Action: history.ActionMirror, Source: "webhook",
			From: "gitlab/team/test-repo", To: "codecommit/git-bridge-test",
			Ref: "refs/tags/v1.0.0", Result: history.ResultSkip,
			Reason: history.ReasonAlreadyUpToDate, DurationMS: 812,
		},
		{
			TS: time.Date(2026, 7, 28, 4, 10, 1, 0, time.UTC), Repo: "demo-repo",
			Action: history.ActionDelete, Source: "sqs",
			From: "codecommit/demo-repo", To: "gitlab/team/demo-repo",
			Ref: "refs/heads/tmp", Result: history.ResultFail,
			Reason: history.ReasonDeleteRef, DurationMS: 4820, Err: "delete ref: exit 1",
		},
	}
}

// This is the whole guard. The console handlers live on their own mux bound to
// its own listener, so nothing a client can put in a request — no header, no
// Host, no forwarded port — can reach them from the public listener. If someone
// ever registers a console path on the public mux, this fails.
//
// 404 and not 403: a public caller should not be able to learn the console
// exists at all.
func TestPublicMuxDoesNotServeAnyConsolePath(t *testing.T) {
	mux := NewMux(nil, nil)

	for _, path := range consolePaths {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Errorf("public mux %s = %d, want %d", path, rec.Code, http.StatusNotFound)
			}
			if strings.Contains(rec.Body.String(), "Git Bridge") {
				t.Errorf("public mux %s served console content: %q", path, rec.Body.String())
			}
		})
	}
}

// The console page is registered at "/", which in Go's mux matches everything
// unclaimed, so an unknown path must be rejected explicitly rather than
// silently rendering the console.
func TestConsoleMuxRejectsUnknownPaths(t *testing.T) {
	mux := NewConsoleMux(&fakeReader{}, nil, task.NewGroup(context.Background()), "")

	for _, path := range []string{"/nope", "/console", "/console/api", "/health"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("console mux %s = %d, want %d", path, rec.Code, http.StatusNotFound)
		}
	}
}

func TestConsoleMuxServesThePage(t *testing.T) {
	mux := NewConsoleMux(&fakeReader{}, nil, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodGet, consolePagePath, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "Recent mirror activity") {
		t.Error("page body does not look like the console")
	}
}

func TestHistoryEndpointReturnsEventsNewestFirst(t *testing.T) {
	reader := &fakeReader{events: sampleEvents()}
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodGet, consoleHistoryPath, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	var body struct {
		Events []history.Event `json:"events"`
		Count  int             `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Count != 2 || len(body.Events) != 2 {
		t.Fatalf("count = %d, events = %d, want 2 and 2", body.Count, len(body.Events))
	}
	// Every field the console renders has to survive the round trip, in
	// particular action and reason — they are what distinguish a sync from a
	// delete and one skip from another.
	got := body.Events[0]
	if got.Repo != "git-bridge-test" || got.Action != history.ActionMirror {
		t.Errorf("first event = %+v", got)
	}
	if got.Result != history.ResultSkip || got.Reason != history.ReasonAlreadyUpToDate {
		t.Errorf("outcome = %q/%q, want %q/%q", got.Result, got.Reason, history.ResultSkip, history.ReasonAlreadyUpToDate)
	}
	if got.DurationMS != 812 || got.Ref != "refs/tags/v1.0.0" {
		t.Errorf("duration/ref = %d/%q", got.DurationMS, got.Ref)
	}
	if body.Events[1].Err != "delete ref: exit 1" {
		t.Errorf("err = %q, want the failure detail", body.Events[1].Err)
	}
}

func TestHistoryEndpointDefaultsTheLimit(t *testing.T) {
	reader := &fakeReader{}
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodGet, consoleHistoryPath, nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if reader.gotQ.Limit != defaultHistoryLimit {
		t.Errorf("limit = %d, want %d", reader.gotQ.Limit, defaultHistoryLimit)
	}
	if reader.gotQ.FailuresOnly {
		t.Error("failuresOnly = true, want false by default")
	}
}

func TestHistoryEndpointPassesTheRequestedLimit(t *testing.T) {
	reader := &fakeReader{}
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodGet, consoleHistoryPath+"?limit=25", nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if reader.gotQ.Limit != 25 {
		t.Errorf("limit = %d, want 25", reader.gotQ.Limit)
	}
}

// A hand-crafted request must not be able to ask for an unbounded response.
func TestHistoryEndpointCapsTheLimit(t *testing.T) {
	reader := &fakeReader{}
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("%s?limit=%d", consoleHistoryPath, maxHistoryLimit*10), nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if reader.gotQ.Limit != maxHistoryLimit {
		t.Errorf("limit = %d, want it capped at %d", reader.gotQ.Limit, maxHistoryLimit)
	}
}

func TestHistoryEndpointRejectsABadLimit(t *testing.T) {
	mux := NewConsoleMux(&fakeReader{}, nil, task.NewGroup(context.Background()), "")

	for _, raw := range []string{"abc", "0", "-5", "1.5"} {
		req := httptest.NewRequest(http.MethodGet, consoleHistoryPath+"?limit="+raw, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("limit=%s → %d, want %d", raw, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestHistoryEndpointForwardsTheFailuresFilter(t *testing.T) {
	reader := &fakeReader{}
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodGet, consoleHistoryPath+"?failures=true", nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if !reader.gotQ.FailuresOnly {
		t.Error("failuresOnly = false, want true")
	}
}

func TestHistoryEndpointIgnoresANonTrueFailuresValue(t *testing.T) {
	reader := &fakeReader{}
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodGet, consoleHistoryPath+"?failures=yes", nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if reader.gotQ.FailuresOnly {
		t.Error("failuresOnly = true, want false for a value other than \"true\"")
	}
}

func TestHistoryEndpointRejectsNonGET(t *testing.T) {
	mux := NewConsoleMux(&fakeReader{}, nil, task.NewGroup(context.Background()), "")

	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		req := httptest.NewRequest(method, consoleHistoryPath, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s → %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Errorf("%s Allow = %q, want GET", method, allow)
		}
	}
}

// An empty history must encode as [] rather than null, so the page can iterate
// the result without a special case.
func TestHistoryEndpointEncodesNoEventsAsAnEmptyArray(t *testing.T) {
	mux := NewConsoleMux(history.NewNoop(), nil, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodGet, consoleHistoryPath, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `"events":[]`) {
		t.Errorf("body = %s, want an empty events array", rec.Body.String())
	}
}

// The portal serves this page under /apps/git-bridge/ and strips that prefix
// before forwarding. A root-relative fetch would leave the app and hit the
// portal instead — and it would work locally, only breaking once deployed.
func TestConsolePageUsesRelativeURLsOnly(t *testing.T) {
	page := string(consoleHTML)

	if !strings.Contains(page, `fetch("console/api/history`) {
		t.Error("page does not fetch the history with a relative URL")
	}
	for _, bad := range []string{`fetch("/`, `fetch('/`, "fetch(`/"} {
		if strings.Contains(page, bad) {
			t.Errorf("page contains an absolute fetch (%s); it would hit the portal, not this app", bad)
		}
	}
}

// The page and the handler have to agree on the path; nothing else checks it.
func TestConsolePageFetchesThePathTheServerRegisters(t *testing.T) {
	page := string(consoleHTML)
	relative := strings.TrimPrefix(consoleHistoryPath, "/")

	if !strings.Contains(page, relative) {
		t.Errorf("page never references %q, the path the server serves", relative)
	}
}

func TestRunConsoleShutsDownWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunConsole(ctx, 0, &fakeReader{}, nil, task.NewGroup(context.Background()), "")
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunConsole did not return after the context was cancelled")
	}
}

func TestRunConsoleReturnsWhenThePortIsTaken(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunConsole(context.Background(), port, &fakeReader{}, nil, task.NewGroup(context.Background()), "")
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunConsole did not return when the port was already bound")
	}
}

func TestHistoryEndpointForwardsTheRepoFilter(t *testing.T) {
	reader := &fakeReader{}
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodGet, consoleHistoryPath+"?repo=demo-repo", nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if reader.gotQ.Repo != "demo-repo" {
		t.Errorf("Repo = %q, want demo-repo", reader.gotQ.Repo)
	}
}

func TestHistoryEndpointTrimsTheRepoFilter(t *testing.T) {
	reader := &fakeReader{}
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodGet, consoleHistoryPath+"?repo=%20%20", nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if reader.gotQ.Repo != "" {
		t.Errorf("Repo = %q, want empty so a blank filter matches everything", reader.gotQ.Repo)
	}
}

// The dropdown is built from this list, so it must come from the unfiltered
// tail — otherwise selecting one repo would remove every other option and the
// reader could not get back.
func TestHistoryEndpointReturnsTheRepoList(t *testing.T) {
	reader := &fakeReader{repos: []string{"git-bridge-test", "demo-repo"}}
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodGet, consoleHistoryPath+"?repo=demo-repo", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var body struct {
		Repos []string `json:"repos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Repos) != 2 || body.Repos[0] != "git-bridge-test" {
		t.Errorf("repos = %v, want both repos regardless of the filter", body.Repos)
	}
}

func TestHistoryEndpointEncodesNoReposAsAnEmptyArray(t *testing.T) {
	mux := NewConsoleMux(history.NewNoop(), nil, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodGet, consoleHistoryPath, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), `"repos":[]`) {
		t.Errorf("body = %s, want an empty repos array", rec.Body.String())
	}
}

func TestRetryEndpointTriggersTheMirror(t *testing.T) {
	retrier := &fakeRetrier{}
	mux := NewConsoleMux(&fakeReader{}, retrier, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodPost, consoleRetryPath,
		strings.NewReader(`{"repo":"git-bridge-test","direction":"source-to-target"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	// The handler answers before the sync finishes, so wait for the goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls, _, _ := retrier.snapshot(); calls > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	calls, repo, direction := retrier.snapshot()
	if calls != 1 || repo != "git-bridge-test" || direction != "source-to-target" {
		t.Errorf("Retry called %d times with (%q, %q)", calls, repo, direction)
	}
}

func TestRetryEndpointDefaultsDirectionToAuto(t *testing.T) {
	retrier := &fakeRetrier{}
	mux := NewConsoleMux(&fakeReader{}, retrier, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodPost, consoleRetryPath, strings.NewReader(`{"repo":"r"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls, _, _ := retrier.snapshot(); calls > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, direction := retrier.snapshot(); direction != "auto" {
		t.Errorf("direction = %q, want auto", direction)
	}
}

// The regression this guards: the button used to post direction "auto", which
// on a bidirectional repo always resolves to target-to-source — so clicking it
// on a failed source-to-target row re-ran the leg that had not failed.
func TestRetryEndpointResolvesDirectionFromDestination(t *testing.T) {
	for _, tc := range []struct{ to, want string }{
		{"gitlab-main/team/test-repo", "source-to-target"},
		{"codecommit-eu/git-bridge-test", "target-to-source"},
	} {
		t.Run(tc.to, func(t *testing.T) {
			// A retrier per subtest: the assertion below waits for the recorded
			// direction to become tc.want, so a shared one would let the first
			// case's value stand in for the second's and pass on nothing.
			retrier := &fakeRetrier{dirs: map[string]string{
				"gitlab-main/team/test-repo":    "source-to-target",
				"codecommit-eu/git-bridge-test": "target-to-source",
			}}
			mux := NewConsoleMux(&fakeReader{}, retrier, task.NewGroup(context.Background()), "")
			body := `{"repo":"git-bridge-test","to":"` + tc.to + `"}`
			req := httptest.NewRequest(http.MethodPost, consoleRetryPath, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
			}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if _, _, dir := retrier.snapshot(); dir == tc.want {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			_, _, dir := retrier.snapshot()
			t.Errorf("direction = %q, want %q", dir, tc.want)
		})
	}
}

// An explicit direction is a caller that means a particular way round, so it
// beats the row's destination rather than being overruled by it.
func TestRetryEndpointPrefersExplicitDirectionOverDestination(t *testing.T) {
	retrier := &fakeRetrier{dirs: map[string]string{"gitlab-main/x": "source-to-target"}}
	mux := NewConsoleMux(&fakeReader{}, retrier, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodPost, consoleRetryPath,
		strings.NewReader(`{"repo":"r","to":"gitlab-main/x","direction":"target-to-source"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls, _, _ := retrier.snapshot(); calls > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, dir := retrier.snapshot(); dir != "target-to-source" {
		t.Errorf("direction = %q, want target-to-source", dir)
	}
}

// A destination the configuration no longer names is a state problem, not a
// typo — and nothing may be re-run on a guess about which way round it was.
func TestRetryEndpointRefusesUnresolvableDestination(t *testing.T) {
	retrier := &fakeRetrier{dirErr: errors.New("not a side of this repo")}
	mux := NewConsoleMux(&fakeReader{}, retrier, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodPost, consoleRetryPath,
		strings.NewReader(`{"repo":"r","to":"gone/elsewhere"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	if calls, _, _ := retrier.snapshot(); calls != 0 {
		t.Errorf("Retry was called %d times for an unresolvable destination", calls)
	}
}

func TestRetryEndpointRejectsBadRequests(t *testing.T) {
	cases := map[string]string{
		"empty repo":    `{"repo":"  "}`,
		"missing repo":  `{"direction":"auto"}`,
		"bad direction": `{"repo":"r","direction":"sideways"}`,
		"unknown field": `{"repo":"r","nope":1}`,
		"not json":      `{`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			retrier := &fakeRetrier{}
			mux := NewConsoleMux(&fakeReader{}, retrier, task.NewGroup(context.Background()), "")

			req := httptest.NewRequest(http.MethodPost, consoleRetryPath, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if calls, _, _ := retrier.snapshot(); calls != 0 {
				t.Errorf("Retry was called %d times for an invalid request", calls)
			}
		})
	}
}

// The retry route carries the same content-type gate as restore and force. It
// writes to whichever side "to" names, so a cross-site form that can reach it
// picks the direction as well as the repo.
func TestRetryEndpointRejectsANonJSONContentType(t *testing.T) {
	retrier := &fakeRetrier{}
	mux := NewConsoleMux(&fakeReader{}, retrier, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodPost, consoleRetryPath,
		strings.NewReader(`{"repo":"r","direction":"auto"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnsupportedMediaType)
	}
	if calls, _, _ := retrier.snapshot(); calls != 0 {
		t.Errorf("Retry was called %d times for a form-encodable request", calls)
	}
}

func TestRetryEndpointRejectsNonPOST(t *testing.T) {
	mux := NewConsoleMux(&fakeReader{}, &fakeRetrier{}, task.NewGroup(context.Background()), "")

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(method, consoleRetryPath, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s → %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}

// A nil retrier means retry is not wired up; the route must then look absent
// rather than answer with an error that implies it exists.
func TestRetryEndpointIs404WhenDisabled(t *testing.T) {
	mux := NewConsoleMux(&fakeReader{}, nil, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodPost, consoleRetryPath, strings.NewReader(`{"repo":"r"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// The whole point of retrying through the console is that the browser never
// holds the retry API token.
func TestConsolePageDoesNotSendAnAuthorizationHeader(t *testing.T) {
	page := string(consoleHTML)

	for _, bad := range []string{"Authorization", "Bearer", "RETRY_API_TOKEN"} {
		if strings.Contains(page, bad) {
			t.Errorf("page mentions %q — the retry token must stay in the pod", bad)
		}
	}
	if !strings.Contains(page, `fetch("console/api/retry"`) {
		t.Error("page does not post the retry with a relative URL")
	}
}

// blockingConsoleRetrier holds a console-triggered sync open so the test can
// check shutdown behaviour while one is in flight.
type blockingConsoleRetrier struct {
	started  chan struct{}
	release  chan struct{}
	ctxAlive chan bool
}

func (b *blockingConsoleRetrier) DirectionTo(_, _ string) (string, error) {
	return "source-to-target", nil
}

func (b *blockingConsoleRetrier) Retry(ctx context.Context, _, _ string, _ mirror.EventMeta) error {
	b.started <- struct{}{}
	<-b.release
	b.ctxAlive <- ctx.Err() == nil
	return nil
}

// The console retry used to run on context.Background(): never cancelled, but
// also never counted, so shutdown walked away from it mid-git-command. It now
// belongs to the task group like every other detached sync.
func TestConsoleRetrySyncIsWaitable(t *testing.T) {
	r := &blockingConsoleRetrier{
		started:  make(chan struct{}, 1),
		release:  make(chan struct{}),
		ctxAlive: make(chan bool, 1),
	}
	tasks := task.NewGroup(context.Background())
	mux := NewConsoleMux(&fakeReader{}, r, tasks, "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, consoleRetryPath,
		strings.NewReader(`{"repo":"my-repo","direction":"auto"}`))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	<-r.started

	drained := make(chan struct{})
	go func() { defer close(drained); tasks.Wait() }()
	select {
	case <-drained:
		t.Fatal("Wait() returned while the console retry was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(r.release)
	if alive := <-r.ctxAlive; !alive {
		t.Error("retry context was cancelled before the sync finished")
	}
	<-drained
}

func TestHistoryEndpointForwardsTheSourceFilter(t *testing.T) {
	reader := &fakeReader{}
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "")

	req := httptest.NewRequest(http.MethodGet, consoleHistoryPath+"?source=%20cron%20", nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if reader.gotQ.Source != "cron" {
		t.Errorf("Source = %q, want cron (trimmed)", reader.gotQ.Source)
	}
}

// The dropdown is built from this list, and it comes from the unfiltered tail
// so selecting one trigger never removes the others from the menu.
func TestHistoryEndpointReturnsTheTriggerList(t *testing.T) {
	reader := &fakeReader{sources: []string{"cron", "webhook"}}
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, consoleHistoryPath+"?source=cron", nil))

	var body struct {
		Sources []string `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sources) != 2 || body.Sources[0] != "cron" || body.Sources[1] != "webhook" {
		t.Errorf("sources = %v, want [cron webhook]", body.Sources)
	}
}

// A reader with no history must still produce an array, so the page can build
// the dropdown without a null check.
func TestHistoryEndpointEncodesAnEmptyTriggerList(t *testing.T) {
	mux := NewConsoleMux(&fakeReader{}, nil, task.NewGroup(context.Background()), "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, consoleHistoryPath, nil))

	if !strings.Contains(rec.Body.String(), `"sources":[]`) {
		t.Errorf("body = %s, want an empty sources array", rec.Body.String())
	}
}

// The page asks for the filter; a direct API caller does not get it silently.
func TestHistoryEndpointHidesIdleReconcilesOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{"opt-in", "?hide_routine=true", mirror.SourceCron},
		{"absent", "", ""},
		{"any other value is not opt-in", "?hide_routine=1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeReader{}
			mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "")
			mux.ServeHTTP(httptest.NewRecorder(),
				httptest.NewRequest(http.MethodGet, consoleHistoryPath+tc.query, nil))

			if reader.gotQ.RoutineSource != tc.want {
				t.Errorf("RoutineSource = %q, want %q", reader.gotQ.RoutineSource, tc.want)
			}
		})
	}
}

// --- identity endpoint tests ---

func TestMeHandler_ReportsThePortalIdentity(t *testing.T) {
	mux := NewConsoleMux(history.NewNoop(), nil, nil, "")

	req := httptest.NewRequest(http.MethodGet, consoleMePath, nil)
	req.Header.Set("X-Auth-User", "alice")
	req.Header.Set("X-Auth-Name", "Alice Kim")
	req.Header.Set("X-Auth-Email", "alice@example.com")
	req.Header.Set("X-Auth-Groups", "platform, sre ,")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		User   string   `json:"user"`
		Name   string   `json:"name"`
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if got.User != "alice" || got.Name != "Alice Kim" || got.Email != "alice@example.com" {
		t.Errorf("identity = %+v, want the header values", got)
	}
	// Blank entries from a trailing comma must not become empty group names.
	if len(got.Groups) != 2 || got.Groups[0] != "platform" || got.Groups[1] != "sre" {
		t.Errorf("groups = %q, want [platform sre]", got.Groups)
	}
}

// Without the portal — a port-forward while debugging — there is no identity to
// report. The page must get a well-formed empty answer rather than an error, so
// it can hide the label instead of failing to load.
func TestMeHandler_WithoutPortalHeadersReturnsEmptyIdentity(t *testing.T) {
	mux := NewConsoleMux(history.NewNoop(), nil, nil, "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, consoleMePath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"user", "name", "email"} {
		if got[k] != "" {
			t.Errorf("%s = %v, want empty", k, got[k])
		}
	}
	// An array, not null, so the page can iterate without a special case.
	if g, ok := got["groups"].([]any); !ok || len(g) != 0 {
		t.Errorf("groups = %v, want an empty array", got["groups"])
	}
}

func TestMeHandler_RejectsNonGET(t *testing.T) {
	mux := NewConsoleMux(history.NewNoop(), nil, nil, "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, consoleMePath, nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// The identity route must be as unreachable from the public listener as every
// other console route — otherwise the headers it echoes become caller-supplied.
func TestMePathIsNotServedOnThePublicMux(t *testing.T) {
	found := false
	for _, p := range consolePaths {
		if p == consoleMePath {
			found = true
		}
	}
	if !found {
		t.Fatal("consoleMePath must be listed in consolePaths so the public-mux 404 guard covers it")
	}
}

// The docs live on the public port while the portal proxies only the console
// port, so the page cannot build this link itself — it has to be told.
func TestMeHandler_CarriesTheConfiguredAPIDocsURL(t *testing.T) {
	mux := NewConsoleMux(history.NewNoop(), nil, nil, "https://git-bridge.example.com/api-docs")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, consoleMePath, nil))

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["api_docs_url"] != "https://git-bridge.example.com/api-docs" {
		t.Errorf("api_docs_url = %v, want the configured URL", got["api_docs_url"])
	}
}

// An unconfigured link must come back empty rather than absent, so the page's
// one check (`if (docs)`) hides it instead of rendering a dead link.
func TestMeHandler_EmptyAPIDocsURLIsStillPresentAndEmpty(t *testing.T) {
	mux := NewConsoleMux(history.NewNoop(), nil, nil, "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, consoleMePath, nil))

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	v, ok := got["api_docs_url"]
	if !ok {
		t.Fatal("api_docs_url must always be present so the page has one code path")
	}
	if v != "" {
		t.Errorf("api_docs_url = %v, want empty", v)
	}
}

// --- restore route ---

type fakeRestorer struct {
	mu    sync.Mutex
	calls int
	repo  string
	to    string
	rtype string
	rname string
	sha   string
	actor string
	err   error
}

func (f *fakeRestorer) RestoreRef(_ context.Context, repo, to, refType, refName, sha, actor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.repo, f.to, f.rtype, f.rname, f.sha, f.actor = repo, to, refType, refName, sha, actor
	return f.err
}

func postRestore(t *testing.T, mux *http.ServeMux, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/console/api/restore", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// deleteReader vouches for one recorded delete, which is what the restore
// route requires before it will put anything back.
func deleteReader(repo, to, ref, sha string) *fakeReader {
	return &fakeReader{events: []history.Event{{
		Repo: repo, Action: history.ActionDelete, Result: history.ResultOK,
		To: to, Ref: ref, DeletedTip: sha,
	}}}
}

// Without a restorer the route must not exist at all, matching how a nil
// retrier disables retry — a button that 404s is worse than no button.
func TestRestore_RouteIs404WithoutARestorer(t *testing.T) {
	mux := NewConsoleMux(&fakeReader{}, nil, task.NewGroup(context.Background()), "")
	w := postRestore(t, mux, `{"repo":"r","to":"gitlab/x","ref":"refs/heads/b","sha":"`+strings.Repeat("a", 40)+`"}`, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestRestore_HappyPathPassesTheParsedRefAndActor(t *testing.T) {
	r := &fakeRestorer{}
	sha := strings.Repeat("a", 40)
	reader := deleteReader("my-repo", "gitlab/team/my-repo", "refs/heads/feature-x", sha)
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "", WithRestorer(r))

	w := postRestore(t, mux,
		`{"repo":"my-repo","to":"gitlab/team/my-repo","ref":"refs/heads/feature-x","sha":"`+sha+`"}`,
		map[string]string{headerAuthUser: "alice"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if r.calls != 1 {
		t.Fatalf("RestoreRef called %d times, want 1", r.calls)
	}
	if r.rtype != "branch" || r.rname != "feature-x" {
		t.Errorf("parsed ref = %s/%s, want branch/feature-x", r.rtype, r.rname)
	}
	if r.actor != "alice" {
		t.Errorf("actor = %q, want alice from the portal header", r.actor)
	}
}

func TestRestore_ParsesTagRefs(t *testing.T) {
	r := &fakeRestorer{}
	sha := strings.Repeat("b", 40)
	reader := deleteReader("my-repo", "gitlab/team/my-repo", "refs/tags/v1.0.0", sha)
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "", WithRestorer(r))
	postRestore(t, mux,
		`{"repo":"my-repo","to":"gitlab/team/my-repo","ref":"refs/tags/v1.0.0","sha":"`+sha+`"}`, nil)
	if r.rtype != "tag" || r.rname != "v1.0.0" {
		t.Errorf("parsed ref = %s/%s, want tag/v1.0.0", r.rtype, r.rname)
	}
}

// The refusal is the interesting outcome, so it has to reach the caller as a
// real status rather than an "accepted" they discover was wrong later.
func TestRestore_RefusalIsReportedSynchronously(t *testing.T) {
	r := &fakeRestorer{err: fmt.Errorf("refs/heads/x: %w — it is at deadbeef", mirror.ErrRefExists)}
	sha := strings.Repeat("c", 40)
	reader := deleteReader("my-repo", "gitlab/team/my-repo", "refs/heads/x", sha)
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "", WithRestorer(r))

	w := postRestore(t, mux,
		`{"repo":"my-repo","to":"gitlab/team/my-repo","ref":"refs/heads/x","sha":"`+sha+`"}`, nil)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), "already exists") {
		t.Errorf("body = %s, want it to carry the reason", w.Body.String())
	}
}

// An abbreviated SHA cannot be pushed to a remote, so it is rejected before it
// ever reaches git rather than failing obscurely later.
func TestRestore_RejectsBadInput(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for name, body := range map[string]string{
		"short sha":     `{"repo":"r","to":"gitlab/x","ref":"refs/heads/b","sha":"abc1234"}`,
		"non-hex sha":   `{"repo":"r","to":"gitlab/x","ref":"refs/heads/b","sha":"` + strings.Repeat("z", 40) + `"}`,
		"bare ref":      `{"repo":"r","to":"gitlab/x","ref":"main","sha":"` + sha + `"}`,
		"empty refname": `{"repo":"r","to":"gitlab/x","ref":"refs/heads/","sha":"` + sha + `"}`,
		"no repo":       `{"repo":"","to":"gitlab/x","ref":"refs/heads/b","sha":"` + sha + `"}`,
		"no to":         `{"repo":"r","to":"","ref":"refs/heads/b","sha":"` + sha + `"}`,
		"unknown field": `{"repo":"r","to":"gitlab/x","ref":"refs/heads/b","sha":"` + sha + `","force":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := &fakeRestorer{}
			reader := deleteReader("r", "gitlab/x", "refs/heads/b", sha)
			mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "", WithRestorer(r))
			if w := postRestore(t, mux, body, nil); w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			if r.calls != 0 {
				t.Errorf("must not reach the restorer on bad input, got %d calls", r.calls)
			}
		})
	}
}

func TestRestore_RejectsNonPost(t *testing.T) {
	mux := NewConsoleMux(&fakeReader{}, nil, task.NewGroup(context.Background()), "", WithRestorer(&fakeRestorer{}))
	req := httptest.NewRequest(http.MethodGet, "/console/api/restore", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

// The page hides the button when the route is not wired in, so it has to be
// able to ask.
func TestMe_ReportsWhetherRestoreIsAvailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []ConsoleOption
		want bool
	}{
		{"wired in", []ConsoleOption{WithRestorer(&fakeRestorer{})}, true},
		{"absent", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := NewConsoleMux(&fakeReader{}, nil, task.NewGroup(context.Background()), "", tc.opts...)
			req := httptest.NewRequest(http.MethodGet, "/console/api/me", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["restore_enabled"] != tc.want {
				t.Errorf("restore_enabled = %v, want %v", body["restore_enabled"], tc.want)
			}
		})
	}
}

// The invariant that a restore only undoes a real delete lives here, not in the
// browser. Without it the route creates any ref from any cached commit.
func TestRestore_RefusesWhenNoDeleteMatches(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for name, reader := range map[string]*fakeReader{
		"no events at all": {},
		"different sha":    deleteReader("my-repo", "gitlab/team/my-repo", "refs/heads/x", strings.Repeat("9", 40)),
		"different ref":    deleteReader("my-repo", "gitlab/team/my-repo", "refs/heads/other", sha),
		"different dest":   deleteReader("my-repo", "codecommit/my-repo", "refs/heads/x", sha),
	} {
		t.Run(name, func(t *testing.T) {
			r := &fakeRestorer{}
			mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "", WithRestorer(r))
			w := postRestore(t, mux,
				`{"repo":"my-repo","to":"gitlab/team/my-repo","ref":"refs/heads/x","sha":"`+sha+`"}`, nil)

			if w.Code != http.StatusConflict {
				t.Errorf("status = %d, want 409", w.Code)
			}
			if r.calls != 0 {
				t.Errorf("must not reach the restorer, got %d calls", r.calls)
			}
		})
	}
}

// A cross-site form cannot set a JSON content type without a preflight, so
// requiring it is what keeps this write off the end of a link.
func TestRestore_RequiresAJSONContentType(t *testing.T) {
	sha := strings.Repeat("a", 40)
	r := &fakeRestorer{}
	mux := NewConsoleMux(deleteReader("my-repo", "gitlab/team/my-repo", "refs/heads/x", sha),
		nil, task.NewGroup(context.Background()), "", WithRestorer(r))

	req := httptest.NewRequest(http.MethodPost, "/console/api/restore",
		strings.NewReader(`{"repo":"my-repo","to":"gitlab/team/my-repo","ref":"refs/heads/x","sha":"`+sha+`"}`))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", w.Code)
	}
	if r.calls != 0 {
		t.Errorf("must not reach the restorer, got %d calls", r.calls)
	}
}

// A rejected restore is the one most worth having a name against, and an
// earlier version answered 409 here without even a log line. The actor must be
// resolved before any gate runs.
func TestRestore_ResolvesTheActorBeforeRejecting(t *testing.T) {
	sha := strings.Repeat("a", 40)
	r := &fakeRestorer{}
	// A reader with no matching delete: the request is rejected at the gate.
	mux := NewConsoleMux(&fakeReader{}, nil, task.NewGroup(context.Background()), "", WithRestorer(r))

	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	w := postRestore(t, mux,
		`{"repo":"my-repo","to":"gitlab/team/my-repo","ref":"refs/heads/x","sha":"`+sha+`"}`,
		map[string]string{headerAuthUser: "alice"})

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	logged := buf.String()
	if !strings.Contains(logged, "alice") {
		t.Errorf("the rejection is not attributable — no actor in:\n%s", logged)
	}
	if !strings.Contains(logged, "refused") {
		t.Errorf("the rejection was not logged at all:\n%s", logged)
	}
}

// A refusal and a breakage must not arrive as the same status: the first means
// the guard worked and the operator should look at the destination, the second
// means the service could not do its job.
func TestRestore_MapsEachOutcomeToItsOwnStatus(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		err    error
		status int
		reason string
	}{
		{mirror.ErrRefExists, http.StatusConflict, "ref-exists"},
		{mirror.ErrObjectGone, http.StatusGone, "object-gone"},
		{mirror.ErrDirectionNotAllowed, http.StatusForbidden, "direction"},
		{mirror.ErrRefOverridden, http.StatusForbidden, "ref-override"},
		{mirror.ErrUnknownSide, http.StatusBadRequest, "unknown-side"},
		{mirror.ErrUnknownRepo, http.StatusBadRequest, "unknown-repo"},
		{mirror.ErrRepoBusy, http.StatusServiceUnavailable, "repo-busy"},
		{errors.New("git exploded"), http.StatusInternalServerError, "restore-failed"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			r := &fakeRestorer{err: fmt.Errorf("wrapped: %w", tc.err)}
			reader := deleteReader("my-repo", "gitlab/team/my-repo", "refs/heads/x", sha)
			mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "", WithRestorer(r))

			w := postRestore(t, mux,
				`{"repo":"my-repo","to":"gitlab/team/my-repo","ref":"refs/heads/x","sha":"`+sha+`"}`, nil)

			if w.Code != tc.status {
				t.Errorf("status = %d, want %d", w.Code, tc.status)
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["reason"] != tc.reason {
				t.Errorf("reason = %q, want %q", body["reason"], tc.reason)
			}
		})
	}
}

// An upper-case object name is the same object; rejecting it on presentation
// alone would only bite someone pasting from a tool that upcases hex.
func TestRestore_AcceptsAnUppercaseSHA(t *testing.T) {
	lower := strings.Repeat("a", 39) + "b"
	r := &fakeRestorer{}
	reader := deleteReader("my-repo", "gitlab/team/my-repo", "refs/heads/x", lower)
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "", WithRestorer(r))

	w := postRestore(t, mux,
		`{"repo":"my-repo","to":"gitlab/team/my-repo","ref":"refs/heads/x","sha":"`+strings.ToUpper(lower)+`"}`, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if r.sha != lower {
		t.Errorf("sha passed on = %q, want it normalised to %q", r.sha, lower)
	}
}

// --- force route ---

type fakeForcer struct {
	calls                      int
	repo, to, ref, dest, actor string
	err                        error
	done                       chan struct{}
}

func newFakeForcer(err error) *fakeForcer {
	return &fakeForcer{err: err, done: make(chan struct{}, 1)}
}

func (f *fakeForcer) ForcePush(_ context.Context, repo, toEndpoint, ref, dest, actor string) error {
	f.calls++
	f.repo, f.to, f.ref, f.dest, f.actor = repo, toEndpoint, ref, dest, actor
	f.done <- struct{}{}
	return f.err
}

func postForce(t *testing.T, mux *http.ServeMux, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/console/api/force", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// holdReader vouches for one recorded hold, which is what the force route
// cross-checks a request against.
func holdReader(repo, to, ref, dest string) *fakeReader {
	return &fakeReader{events: []history.Event{{
		Repo: repo, Action: history.ActionMirror, Result: history.ResultSkip,
		Reason: history.ReasonDestinationAhead, To: to,
		Held: []history.HeldRef{{Ref: ref, Reason: "destination-ahead", Dest: dest}},
	}}}
}

// Without a forcer the route must not exist at all, matching restore and retry.
func TestForce_RouteIs404WithoutAForcer(t *testing.T) {
	mux := NewConsoleMux(&fakeReader{}, nil, task.NewGroup(context.Background()), "")
	w := postForce(t, mux, `{"repo":"r","to":"codecommit/r","ref":"refs/heads/b","dest":"x"}`, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestForce_HappyPathPassesTheRefAndActor(t *testing.T) {
	f := newFakeForcer(nil)
	dest := strings.Repeat("a", 40)
	reader := holdReader("my-repo", "codecommit/my-repo", "refs/heads/version/4.3.0", dest)
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "", WithForcer(f))

	w := postForce(t, mux,
		`{"repo":"my-repo","to":"codecommit/my-repo","ref":"refs/heads/version/4.3.0","dest":"`+dest+`"}`,
		map[string]string{headerAuthUser: "alice"})

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
	}
	<-f.done
	if f.repo != "my-repo" || f.to != "codecommit/my-repo" || f.ref != "refs/heads/version/4.3.0" {
		t.Errorf("unexpected call: repo=%q to=%q ref=%q", f.repo, f.to, f.ref)
	}
	if f.actor != "alice" {
		t.Errorf("actor = %q, want alice from the portal header", f.actor)
	}
}

// Without this check the route is a general "force any ref onto either side"
// API for anyone who can reach the console. The button may only finish
// something the mirror already declined and recorded.
func TestForce_RefusesARefTheGuardNeverWithheld(t *testing.T) {
	f := newFakeForcer(nil)
	reader := holdReader("my-repo", "codecommit/my-repo", "refs/heads/version/4.3.0", strings.Repeat("a", 40))
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "", WithForcer(f))

	w := postForce(t, mux,
		`{"repo":"my-repo","to":"codecommit/my-repo","ref":"refs/heads/some-other-branch","dest":"`+strings.Repeat("a", 40)+`"}`, nil)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	if f.calls != 0 {
		t.Errorf("ForcePush ran for a ref that was never withheld")
	}
}

// A hold recorded against one destination must not authorise a force at the
// other one — that would push the rewind the wrong way.
func TestForce_RefusesAMismatchedDestination(t *testing.T) {
	f := newFakeForcer(nil)
	reader := holdReader("my-repo", "codecommit/my-repo", "refs/heads/b", strings.Repeat("a", 40))
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "", WithForcer(f))

	w := postForce(t, mux,
		`{"repo":"my-repo","to":"gitlab/team/my-repo","ref":"refs/heads/b","dest":"`+strings.Repeat("a", 40)+`"}`, nil)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	if f.calls != 0 {
		t.Errorf("ForcePush ran against a destination no hold was recorded for")
	}
}

func TestForce_RejectsIncompleteInput(t *testing.T) {
	f := newFakeForcer(nil)
	reader := holdReader("my-repo", "codecommit/my-repo", "refs/heads/b", strings.Repeat("a", 40))
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "", WithForcer(f))

	for name, body := range map[string]string{
		"no repo": `{"to":"codecommit/my-repo","ref":"refs/heads/b","dest":"x"}`,
		"no to":   `{"repo":"my-repo","ref":"refs/heads/b","dest":"x"}`,
		"no ref":  `{"repo":"my-repo","to":"codecommit/my-repo","dest":"x"}`,
		"no dest": `{"repo":"my-repo","to":"codecommit/my-repo","ref":"refs/heads/b"}`,
		"garbage": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			if w := postForce(t, mux, body, nil); w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
		})
	}
	if f.calls != 0 {
		t.Errorf("ForcePush ran on a rejected request")
	}
}

// The page asks whether the route is wired in rather than assuming, so a
// deployment without a mirror service shows no button instead of one that 404s.
func TestMe_ReportsWhetherForceIsEnabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []ConsoleOption
		want bool
	}{
		{"wired in", []ConsoleOption{WithForcer(newFakeForcer(nil))}, true},
		{"absent", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := NewConsoleMux(&fakeReader{}, nil, task.NewGroup(context.Background()), "", tc.opts...)
			req := httptest.NewRequest(http.MethodGet, "/console/api/me", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["force_enabled"] != tc.want {
				t.Errorf("force_enabled = %v, want %v", body["force_enabled"], tc.want)
			}
		})
	}
}

// The tip is part of the match. Without it a hold sitting in the visible tail
// would authorise a force forever, including one pressed long after the
// destination moved on to something else.
func TestForce_RefusesAStaleTip(t *testing.T) {
	f := newFakeForcer(nil)
	reader := holdReader("my-repo", "codecommit/my-repo", "refs/heads/b", strings.Repeat("a", 40))
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "", WithForcer(f))

	w := postForce(t, mux,
		`{"repo":"my-repo","to":"codecommit/my-repo","ref":"refs/heads/b","dest":"`+strings.Repeat("b", 40)+`"}`, nil)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	if f.calls != 0 {
		t.Errorf("ForcePush ran against a tip no hold was recorded for")
	}
}

// The tip the operator was shown has to reach the mirror service, because that
// is what becomes the push's lease.
func TestForce_PassesTheObservedTipThrough(t *testing.T) {
	f := newFakeForcer(nil)
	dest := strings.Repeat("a", 40)
	reader := holdReader("my-repo", "codecommit/my-repo", "refs/heads/b", dest)
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "", WithForcer(f))

	postForce(t, mux, `{"repo":"my-repo","to":"codecommit/my-repo","ref":"refs/heads/b","dest":"`+dest+`"}`, nil)
	<-f.done
	if f.dest != dest {
		t.Errorf("dest = %q, want the tip the operator was shown %q", f.dest, dest)
	}
}

// A cross-site form cannot send a JSON content type without a preflight, so
// requiring it keeps the most destructive of the three writing routes off the
// end of a link someone was tricked into opening. Restore already does this.
func TestForce_RequiresAJSONContentType(t *testing.T) {
	f := newFakeForcer(nil)
	dest := strings.Repeat("a", 40)
	reader := holdReader("my-repo", "codecommit/my-repo", "refs/heads/b", dest)
	mux := NewConsoleMux(reader, nil, task.NewGroup(context.Background()), "", WithForcer(f))

	body := `{"repo":"my-repo","to":"codecommit/my-repo","ref":"refs/heads/b","dest":"` + dest + `"}`
	req := httptest.NewRequest(http.MethodPost, "/console/api/force", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", w.Code)
	}
	if f.calls != 0 {
		t.Errorf("ForcePush ran for a request that could have come from a cross-site form")
	}
}

// TestGates_FindRowsBuriedUnderRoutineReconciles pins the invariant both gate
// helpers state in their doc comments: they read the same tail the console page
// renders.
//
// The page loads with "Hide idle reconciles" on, so it shows a window with the
// no-op reconciles filtered out. A gate reading the same Limit without that
// filter gets a window the hourly reconcile fills up, leaving a far shorter
// span. The page then draws a restore / force-push button on a row that
// answers the click with a 409 — which is what happened in production.
func TestGates_FindRowsBuriedUnderRoutineReconciles(t *testing.T) {
	w, err := history.New(t.TempDir())
	if err != nil {
		t.Fatalf("history.New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	const (
		repo = "buried-repo"
		to   = "codecommit/buried-repo"
		ref  = "refs/heads/buried"
		tip  = "a7d5a1ff02b84b97609c056c13cfba173166bb3d"
	)

	// The rows worth acting on go in first, so everything after buries them.
	w.Record(history.Event{
		Repo: repo, Action: history.ActionDelete, Source: "webhook",
		To: to, Ref: ref, Result: history.ResultOK, DeletedTip: tip,
	})
	w.Record(history.Event{
		Repo: repo, Action: history.ActionMirror, Source: "webhook",
		To: to, Result: history.ResultSkip, Reason: "destination-ahead",
		Held: []history.HeldRef{{Ref: ref, Dest: tip}},
	})

	// More idle reconciles than the gate's window holds. Unfiltered, these
	// alone would push both rows above out of every bounded read.
	for range maxHistoryLimit + 50 {
		w.Record(history.Event{
			Repo: repo, Action: history.ActionMirror, Source: mirror.SourceCron,
			To: to, Result: history.ResultSkip, Reason: history.ReasonAlreadyUpToDate,
		})
	}

	if !matchingDeleteExists(w, repo, to, ref, tip) {
		t.Error("restore gate lost the delete row under routine reconciles; the page still draws its button")
	}
	if !matchingHoldExists(w, repo, to, ref, tip) {
		t.Error("force gate lost the held row under routine reconciles; the page still draws its button")
	}
}

// TestGateQuery_MatchesThePageFilter keeps the two from drifting apart again:
// the page sets hide_routine, so the gate has to ask for the same thing.
func TestGateQuery_MatchesThePageFilter(t *testing.T) {
	q := gateQuery("some-repo")
	if q.RoutineSource != mirror.SourceCron {
		t.Errorf("gate query RoutineSource = %q, want %q — the console page filters idle reconciles and the gate must read the same tail", q.RoutineSource, mirror.SourceCron)
	}
	if q.Limit != maxHistoryLimit {
		t.Errorf("gate query Limit = %d, want %d", q.Limit, maxHistoryLimit)
	}
	if q.Repo != "some-repo" {
		t.Errorf("gate query Repo = %q, want %q", q.Repo, "some-repo")
	}
}

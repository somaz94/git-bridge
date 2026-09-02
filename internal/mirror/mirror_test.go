package mirror

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"git-bridge/internal/askpass"
	"git-bridge/internal/config"
	"git-bridge/internal/history"
	"git-bridge/internal/notify"
	"git-bridge/internal/provider"
)

// defaultMockRefTip is the tip RefTip reports when a test does not care which
// SHA a present ref points at. Full 40 characters on purpose: a delete records
// this value so the objects can be fetched back, and the abbreviated form is
// not enough for that (see the core.abbrev note on runPush).
const defaultMockRefTip = "1111111111111111111111111111111111111111"

// localRemote wraps a local directory as a remote with no credentials. The tests that
// drive real git use it when they hand a temporary repository over as a remote — the
// credentials are empty, so the GIT_ASKPASS path is never taken.
func localRemote(dir string) provider.Remote { return provider.Remote{URL: dir} }

// mockGitRunner records calls and returns configurable errors.
type mockGitRunner struct {
	cloneCalls         []cloneCall
	fetchCalls         []fetchCall
	pushCalls          []pushCall
	deleteRefCalls     []deleteRefCall
	cloneErr           error
	fetchErr           error
	pushErr            error
	pushChanged        bool                // when true, PushMirror reports changes were pushed
	pushForced         []history.ForcedRef // refs PushMirror reports as overwritten non-fast-forward
	pushRejected       []string            // refs PushMirror reports the destination refused
	deleteRefErr       error
	commitAuthor       string // return value for CommitAuthor
	commitAuthorErr    error
	listRefs           []string // return value for ListRefs
	listRefsErr        error
	listRefsCalls      int
	gcCalls            []string // dirs GCAuto was invoked on
	sanitizeCalls      []string // dirs SanitizeCache was invoked on
	sanitizeScrubbed   bool     // when true, SanitizeCache reports it removed credentials
	sanitizeErr        error
	gcStats            GCStats
	gcErr              error
	objectMissing      bool // when true, HasObject reports the commit is gone (gc'd)
	ensureBareDirErr   error
	ensureBareDirCalls []string
	fetchObjectErr     error
	createRefErr       error
	hasObjectCalls     []string
	fetchObjectCalls   []string
	createRefCalls     []createRefCall

	refMissing  bool   // when true, RefTip reports the ref is absent (idempotent skip)
	refTip      string // tip RefTip returns; empty falls back to defaultMockRefTip
	refTipErr   error
	refTipCalls []refTipCall

	// Push-guard inputs. localTips/remoteTips are the two sides planPush
	// compares; nil localTips derives one tip per ref from mockRefList, and nil
	// remoteTips means the destination has nothing yet.
	localTips        map[string]string
	localTipsErr     error
	remoteTips       map[string]string
	remoteTipsErr    error
	ancestors        map[string]bool // "<ancestor>><descendant>" → true
	listRefTipsCalls int
	remoteRefsCalls  []string
	isAncestorCalls  []string

	// pushCtxDeadline records the deadline of the ctx PushMirror received. It is used to
	// verify that doMirror applies the git-op timeout after acquiring the mutex (the push
	// ctx must carry a deadline even when the parent ctx has none). refTipCtxHasDeadline
	// is the symmetric check for the doDeleteRef path (that RefTip is called with the
	// post-lock ctx).
	pushCtxDeadline      time.Time
	pushCtxHasDeadline   bool
	refTipCtxHasDeadline bool
}

type refTipCall struct {
	URL     string
	RefType string
	RefName string
}

type createRefCall struct {
	URL     string
	SHA     string
	FullRef string
}

type cloneCall struct {
	URL string
	Dir string
}

type fetchCall struct {
	URL string
	Dir string
}

type pushCall struct {
	Dir   string
	URL   string
	Specs []PushSpec
}

// pushedRefs pulls the ref names out of a recorded push so a test can assert on
// scope without restating the lease of every ref.
func pushedRefs(specs []PushSpec) []string {
	refs := make([]string, 0, len(specs))
	for _, s := range specs {
		refs = append(refs, s.Ref)
	}
	return refs
}

type deleteRefCall struct {
	URL     string
	RefType string
	RefName string
}

func (m *mockGitRunner) CloneMirror(_ context.Context, rem provider.Remote, dir string) error {
	m.cloneCalls = append(m.cloneCalls, cloneCall{URL: rem.URL, Dir: dir})
	return m.cloneErr
}

func (m *mockGitRunner) FetchMirror(_ context.Context, rem provider.Remote, dir string) error {
	m.fetchCalls = append(m.fetchCalls, fetchCall{URL: rem.URL, Dir: dir})
	return m.fetchErr
}

// SanitizeCache only counts the cache-scrub calls. The real scrubbing is checked by the
// defaultGitRunner tests, which build an actual repository.
func (m *mockGitRunner) SanitizeCache(_ context.Context, dir string, _ provider.Remote) (bool, error) {
	m.sanitizeCalls = append(m.sanitizeCalls, dir)
	return m.sanitizeScrubbed, m.sanitizeErr
}

func (m *mockGitRunner) PushMirror(ctx context.Context, dir string, rem provider.Remote, specs []PushSpec) (PushResult, error) {
	if d, ok := ctx.Deadline(); ok {
		m.pushCtxDeadline = d
		m.pushCtxHasDeadline = true
	}
	m.pushCalls = append(m.pushCalls, pushCall{Dir: dir, URL: rem.URL, Specs: specs})
	if m.pushErr != nil {
		return PushResult{}, m.pushErr
	}
	return PushResult{Changed: m.pushChanged, Forced: m.pushForced, Rejected: m.pushRejected}, nil
}

// defaultMockLocalTip is the SHA an unconfigured mock reports for every local
// ref. It only has to be stable and distinct from defaultMockRefTip.
const defaultMockLocalTip = "2222222222222222222222222222222222222222"

// mockRefList is the local ref set both ListRefs and ListRefTips answer from.
//
// ListRefTips deliberately does not call ListRefs: listRefsCalls is asserted by
// tests that check an event narrowed to one ref, and routing a second caller
// through the counter would make those assertions count something else.
func (m *mockGitRunner) mockRefList() []string {
	if m.listRefs == nil && m.listRefsErr == nil {
		return defaultMockRefs
	}
	return m.listRefs
}

func (m *mockGitRunner) ListRefTips(_ context.Context, _ string) (map[string]string, error) {
	m.listRefTipsCalls++
	if m.localTipsErr != nil {
		return nil, m.localTipsErr
	}
	if m.localTips != nil {
		return m.localTips, nil
	}
	tips := make(map[string]string)
	for _, ref := range m.mockRefList() {
		tips[ref] = defaultMockLocalTip
	}
	return tips, nil
}

// RemoteRefs reports an empty destination unless a test says otherwise, so
// every ref reads as new and the push goes through. That keeps the guard out of
// the way of tests about something else, and forces a test that cares about the
// guard to state the destination it wants.
func (m *mockGitRunner) RemoteRefs(_ context.Context, rem provider.Remote) (map[string]string, error) {
	m.remoteRefsCalls = append(m.remoteRefsCalls, rem.URL)
	if m.remoteTipsErr != nil {
		return nil, m.remoteTipsErr
	}
	return m.remoteTips, nil
}

func (m *mockGitRunner) IsAncestor(_ context.Context, _, ancestor, descendant string) bool {
	key := ancestor + ">" + descendant
	m.isAncestorCalls = append(m.isAncestorCalls, key)
	return m.ancestors[key]
}

// defaultMockRefs is what an unconfigured mock mirror reports it contains.
//
// A scoped push looks the triggered ref up in this list, and in production the
// fetch that runs immediately before it has just put that ref there — so
// "present" is the realistic default, and an empty mirror is not. A test that
// wants the absent-ref path (push nothing rather than push everything) says so
// by setting listRefs explicitly — as must any test naming a ref that is not in
// this list, or its scoped push is skipped as absent and the test reads as a
// mysterious "no push happened".
var defaultMockRefs = []string{
	"refs/heads/main",
	"refs/heads/master-b",
	"refs/tags/v1.0.0",
	"refs/tags/build-1",
}

func (m *mockGitRunner) ListRefs(_ context.Context, _ string) ([]string, error) {
	m.listRefsCalls++
	if m.listRefs == nil && m.listRefsErr == nil {
		return defaultMockRefs, nil
	}
	return m.listRefs, m.listRefsErr
}

func (m *mockGitRunner) DeleteRef(_ context.Context, _ string, rem provider.Remote, refType, refName string) error {
	m.deleteRefCalls = append(m.deleteRefCalls, deleteRefCall{URL: rem.URL, RefType: refType, RefName: refName})
	return m.deleteRefErr
}

func (m *mockGitRunner) RefTip(ctx context.Context, rem provider.Remote, refType, refName string) (string, error) {
	if _, ok := ctx.Deadline(); ok {
		m.refTipCtxHasDeadline = true
	}
	m.refTipCalls = append(m.refTipCalls, refTipCall{URL: rem.URL, RefType: refType, RefName: refName})
	if m.refTipErr != nil {
		return "", m.refTipErr
	}
	if m.refMissing {
		return "", nil
	}
	if m.refTip != "" {
		return m.refTip, nil
	}
	return defaultMockRefTip, nil
}

func (m *mockGitRunner) EnsureBareDir(_ context.Context, dir string) error {
	m.ensureBareDirCalls = append(m.ensureBareDirCalls, dir)
	return m.ensureBareDirErr
}

func (m *mockGitRunner) HasObject(_ context.Context, dir, sha string) bool {
	m.hasObjectCalls = append(m.hasObjectCalls, sha)
	return !m.objectMissing
}

func (m *mockGitRunner) FetchObject(_ context.Context, dir string, _ provider.Remote, sha string) error {
	m.fetchObjectCalls = append(m.fetchObjectCalls, sha)
	return m.fetchObjectErr
}

func (m *mockGitRunner) CreateRef(_ context.Context, dir string, rem provider.Remote, sha, fullRef string) error {
	m.createRefCalls = append(m.createRefCalls, createRefCall{URL: rem.URL, SHA: sha, FullRef: fullRef})
	return m.createRefErr
}

func (m *mockGitRunner) CommitAuthor(_ context.Context, _, _ string) (string, error) {
	return m.commitAuthor, m.commitAuthorErr
}

func (m *mockGitRunner) GCAuto(_ context.Context, dir string) (GCStats, error) {
	m.gcCalls = append(m.gcCalls, dir)
	return m.gcStats, m.gcErr
}

// mockNotifier records sent notifications.
type mockNotifier struct {
	messages []notify.Message
}

func (m *mockNotifier) Send(msg notify.Message) {
	m.messages = append(m.messages, msg)
}

// newTestService creates a Service with mock git runner and notifier.
func newTestService(repos []config.RepoConfig, providers map[string]provider.Provider, notif notify.Notifier, git *mockGitRunner) *Service {
	return &Service{
		configs:        repos,
		providers:      providers,
		notifier:       notif,
		workDir:        "/tmp/git-bridge-test",
		git:            git,
		timeoutSeconds: 300,
		repoLocks:      make(map[string]*sync.Mutex),
	}
}

func makeProviders() map[string]provider.Provider {
	return map[string]provider.Provider{
		"codecommit-eu": NewCodeCommit(config.ProviderConfig{
			Type:   "codecommit",
			Region: "ap-northeast-2",
			Credentials: map[string]string{
				"git_username": "user",
				"git_password": "pass",
			},
		}),
		"gitlab-main": NewGitLab(config.ProviderConfig{
			Type:    "gitlab",
			BaseURL: "https://gitlab.example.com",
			Credentials: map[string]string{
				"token": "glpat-test",
			},
		}),
		"github-main": NewGitHub(config.ProviderConfig{
			Type: "github",
			Credentials: map[string]string{
				"token": "ghp-test",
			},
		}),
	}
}

func defaultRepos() []config.RepoConfig {
	return []config.RepoConfig{
		{
			Name:       "my-repo",
			Source:     "codecommit-eu",
			Target:     "gitlab-main",
			SourcePath: "my-repo",
			TargetPath: "team/my-repo",
			Direction:  "source-to-target",
		},
		{
			Name:       "bidi-repo",
			Source:     "codecommit-eu",
			Target:     "gitlab-main",
			SourcePath: "bidi-repo",
			TargetPath: "team/bidi-repo",
			Direction:  "bidirectional",
		},
		{
			Name:       "reverse-repo",
			Source:     "gitlab-main",
			Target:     "github-main",
			SourcePath: "team/reverse-repo",
			TargetPath: "org/reverse-repo",
			Direction:  "target-to-source",
		},
	}
}

// --- Sync tests ---

func TestSync_SourceToTarget(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.Sync(context.Background(), "my-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(git.cloneCalls) != 1 {
		t.Fatalf("expected 1 clone call, got %d", len(git.cloneCalls))
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("expected 1 push call, got %d", len(git.pushCalls))
	}

	// Should notify success
	if len(notif.messages) != 1 || notif.messages[0].Level != "success" {
		t.Errorf("expected success notification, got %+v", notif.messages)
	}
}

func TestSync_Bidirectional(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.Sync(context.Background(), "bidi-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(git.cloneCalls) != 1 {
		t.Fatalf("expected 1 clone call, got %d", len(git.cloneCalls))
	}
}

func TestSync_DirectionNotAllowed(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	// reverse-repo is target-to-source only, so Sync (source-side trigger) should fail
	err := svc.Sync(context.Background(), "team/reverse-repo", EventMeta{})
	if err == nil {
		t.Fatal("expected error for disallowed direction")
	}
	if len(git.cloneCalls) != 0 {
		t.Error("should not have called clone")
	}
}

func TestSync_RepoNotConfigured(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.Sync(context.Background(), "nonexistent-repo", EventMeta{})
	if err == nil {
		t.Fatal("expected error for unconfigured repo")
	}
}

func TestSync_CloneError(t *testing.T) {
	git := &mockGitRunner{cloneErr: fmt.Errorf("clone failed")}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.Sync(context.Background(), "my-repo", EventMeta{})
	if err == nil {
		t.Fatal("expected error")
	}

	// Should notify error
	if len(notif.messages) != 1 || notif.messages[0].Level != "error" {
		t.Errorf("expected error notification, got %+v", notif.messages)
	}
}

func TestSync_PushError(t *testing.T) {
	git := &mockGitRunner{pushErr: fmt.Errorf("push failed")}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.Sync(context.Background(), "my-repo", EventMeta{})
	if err == nil {
		t.Fatal("expected error")
	}

	if len(notif.messages) != 1 || notif.messages[0].Level != "error" {
		t.Errorf("expected error notification, got %+v", notif.messages)
	}
}

// --- SyncByTarget tests ---

func TestSyncByTarget_TargetMatch(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	// bidi-repo: target is gitlab, target_path is team/bidi-repo, direction bidirectional
	err := svc.SyncByTarget(context.Background(), "gitlab", "team/bidi-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(git.cloneCalls) != 1 {
		t.Fatalf("expected 1 clone call, got %d", len(git.cloneCalls))
	}
}

func TestSyncByTarget_SourceMatch(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	// my-repo: source is codecommit, source_path is my-repo, direction source-to-target
	// SyncByTarget with source provider match should trigger source-to-target
	err := svc.SyncByTarget(context.Background(), "codecommit", "my-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(git.cloneCalls) != 1 {
		t.Fatalf("expected 1 clone call, got %d", len(git.cloneCalls))
	}
}

func TestSyncByTarget_DirectionNotAllowed(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	// my-repo is source-to-target only; target-side webhook (gitlab) should not allow target-to-source
	err := svc.SyncByTarget(context.Background(), "gitlab", "team/my-repo", EventMeta{})
	if err == nil {
		t.Fatal("expected error for disallowed direction")
	}
}

func TestSyncByTarget_NoMatch(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncByTarget(context.Background(), "gitlab", "unknown/repo", EventMeta{})
	if err == nil {
		t.Fatal("expected error for no matching repo")
	}
}

func TestSyncByTarget_CloneError(t *testing.T) {
	git := &mockGitRunner{cloneErr: fmt.Errorf("clone boom")}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncByTarget(context.Background(), "gitlab", "team/bidi-repo", EventMeta{})
	if err == nil {
		t.Fatal("expected error")
	}

	if len(notif.messages) != 1 || notif.messages[0].Level != "error" {
		t.Errorf("expected error notification, got %+v", notif.messages)
	}
}

// --- doMirror tests ---

func TestDoMirror_ProviderNotFound(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)

	repoCfg := config.RepoConfig{Name: "test"}

	// Source provider not found
	err := svc.doMirror(context.Background(), repoCfg, "nonexistent", "repo", "gitlab-main", "team/repo", EventMeta{})
	if err == nil {
		t.Fatal("expected error for missing source provider")
	}

	// Target provider not found
	err = svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "repo", "nonexistent", "team/repo", EventMeta{})
	if err == nil {
		t.Fatal("expected error for missing target provider")
	}
}

func TestDoMirror_SuccessNotification(t *testing.T) {
	git := &mockGitRunner{pushChanged: true, commitAuthor: "alice"}
	notif := &mockNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)

	meta := EventMeta{Ref: "refs/heads/main"}
	repoCfg := config.RepoConfig{Name: "test-repo"}
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "my-repo", "gitlab-main", "team/my-repo", meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(notif.messages) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notif.messages))
	}
	if notif.messages[0].Level != "success" {
		t.Errorf("expected success notification, got %q", notif.messages[0].Level)
	}
	if notif.messages[0].Title != "Mirror Sync: test-repo" {
		t.Errorf("unexpected title: %q", notif.messages[0].Title)
	}
	if !strings.Contains(notif.messages[0].Body, "Pushed by: alice") {
		t.Errorf("expected notification body to contain commit author, got %q", notif.messages[0].Body)
	}
	if !strings.Contains(notif.messages[0].Body, "Branch: main") {
		t.Errorf("expected notification body to contain branch info, got %q", notif.messages[0].Body)
	}
}

func TestDoMirror_SuccessNotification_WithTag(t *testing.T) {
	git := &mockGitRunner{pushChanged: true, commitAuthor: "alice"}
	notif := &mockNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)

	meta := EventMeta{Ref: "refs/tags/v1.0.0"}
	repoCfg := config.RepoConfig{Name: "test-repo"}
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "my-repo", "gitlab-main", "team/my-repo", meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(notif.messages) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notif.messages))
	}
	if !strings.Contains(notif.messages[0].Body, "Tag: v1.0.0") {
		t.Errorf("expected notification body to contain tag info, got %q", notif.messages[0].Body)
	}
}

// --- New() constructor tests ---

func TestNew_DefaultWorkDir(t *testing.T) {
	t.Setenv("WORK_DIR", "")
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"codecommit-eu": {
				Type:   "codecommit",
				Region: "us-east-1",
				Credentials: map[string]string{
					"git_username": "u",
					"git_password": "p",
				},
			},
		},
		Repos: []config.RepoConfig{
			{Name: "r", Source: "codecommit-eu", Target: "codecommit-eu", SourcePath: "a", TargetPath: "b", Direction: "bidirectional"},
		},
	}

	svc, err := New(cfg, notify.NewNoop(), history.NewNoop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if svc.workDir != "/tmp/git-bridge" {
		t.Errorf("expected default workDir, got %q", svc.workDir)
	}
	if svc.git == nil {
		t.Error("git runner should not be nil")
	}
}

func TestNew_CustomWorkDir(t *testing.T) {
	t.Setenv("WORK_DIR", "/custom/dir")
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{},
		Repos:     nil,
	}

	svc, err := New(cfg, notify.NewNoop(), history.NewNoop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if svc.workDir != "/custom/dir" {
		t.Errorf("expected /custom/dir, got %q", svc.workDir)
	}
}

func TestNew_InvalidProvider(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"bad": {Type: "unsupported"},
		},
		Repos: nil,
	}

	_, err := New(cfg, notify.NewNoop(), history.NewNoop())
	if err == nil {
		t.Fatal("expected error for unsupported provider type")
	}
}

func TestSyncByTarget_TargetProviderNotInMap(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	// Only codecommit in providers; target "missing" not in map
	providers := map[string]provider.Provider{
		"codecommit-eu": NewCodeCommit(config.ProviderConfig{
			Type: "codecommit", Region: "us-east-1",
			Credentials: map[string]string{"git_username": "u", "git_password": "p"},
		}),
	}
	repos := []config.RepoConfig{
		{Name: "r", Source: "codecommit-eu", Target: "missing", SourcePath: "r", TargetPath: "t/r", Direction: "source-to-target"},
	}
	svc := newTestService(repos, providers, notif, git)

	// Target provider "missing" not in map → skip target match
	// Source provider "codecommit-eu" matches → doMirror → fails because target "missing" not found
	err := svc.SyncByTarget(context.Background(), "codecommit", "r", EventMeta{})
	if err == nil {
		t.Fatal("expected error because target provider missing from providers map")
	}
}

func TestSyncByTarget_SourceProviderNotInMap(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	// Only gitlab in providers; source "missing" is not there
	providers := map[string]provider.Provider{
		"gitlab-main": NewGitLab(config.ProviderConfig{
			Type: "gitlab", BaseURL: "https://gl.test",
			Credentials: map[string]string{"token": "t"},
		}),
	}
	repos := []config.RepoConfig{
		{Name: "r", Source: "missing", Target: "gitlab-main", SourcePath: "r", TargetPath: "t/r", Direction: "bidirectional"},
	}
	svc := newTestService(repos, providers, notif, git)

	// Target matches (gitlab-main, t/r) → doMirror from "gitlab-main" to "missing" → fails
	err := svc.SyncByTarget(context.Background(), "gitlab", "t/r", EventMeta{})
	if err == nil {
		t.Fatal("expected error because source provider missing from providers map")
	}
}

func TestSyncByTarget_SourceDirectionNotAllowed(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	// reverse-repo: source=gitlab, target=github, direction=target-to-source
	// SyncByTarget with source match (gitlab, team/reverse-repo) should fail because
	// direction is target-to-source, not source-to-target
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncByTarget(context.Background(), "gitlab", "team/reverse-repo", EventMeta{})
	if err == nil {
		t.Fatal("expected error for source-side direction not allowed")
	}
}

func TestSyncByTarget_TargetToSource_Success(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	// reverse-repo: target=github, target_path=org/reverse-repo, direction=target-to-source
	err := svc.SyncByTarget(context.Background(), "github", "org/reverse-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.cloneCalls) != 1 {
		t.Errorf("expected 1 clone, got %d", len(git.cloneCalls))
	}
}

// --- SyncDelete tests ---

func TestSyncDelete_Success(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncDelete(context.Background(), "my-repo", "branch", "feature-branch")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(git.deleteRefCalls) != 1 {
		t.Fatalf("expected 1 deleteRef call, got %d", len(git.deleteRefCalls))
	}
	dc := git.deleteRefCalls[0]
	if dc.RefType != "branch" || dc.RefName != "feature-branch" {
		t.Errorf("unexpected deleteRef call: %+v", dc)
	}
	if len(notif.messages) != 1 || notif.messages[0].Level != "success" {
		t.Errorf("expected success notification, got %+v", notif.messages)
	}
	// A delete notification has to show the direction (Route), symmetric with a push's Mirror Sync.
	if !strings.Contains(notif.messages[0].Body, "Route: codecommit-eu/my-repo → gitlab-main/team/my-repo") {
		t.Errorf("expected delete notification to show Route direction, got %q", notif.messages[0].Body)
	}
}

func TestSyncDelete_Tag(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncDelete(context.Background(), "my-repo", "tag", "v1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dc := git.deleteRefCalls[0]
	if dc.RefType != "tag" || dc.RefName != "v1.0.0" {
		t.Errorf("unexpected deleteRef call: %+v", dc)
	}
}

func TestSyncDelete_RepoNotConfigured(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncDelete(context.Background(), "nonexistent", "branch", "main")
	if err == nil {
		t.Fatal("expected error for unconfigured repo")
	}
}

func TestSyncDelete_DirectionNotAllowed(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	repos := []config.RepoConfig{
		{Name: "rev", Source: "gitlab-main", Target: "github-main", SourcePath: "team/rev", TargetPath: "org/rev", Direction: "target-to-source"},
	}
	svc := newTestService(repos, makeProviders(), notif, git)

	err := svc.SyncDelete(context.Background(), "team/rev", "branch", "old-branch")
	if err == nil {
		t.Fatal("expected error for disallowed direction")
	}
}

func TestSyncDelete_ProviderNotFound(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	repos := []config.RepoConfig{
		{Name: "r", Source: "codecommit-eu", Target: "missing", SourcePath: "r", TargetPath: "t/r", Direction: "source-to-target"},
	}
	svc := newTestService(repos, makeProviders(), notif, git)

	err := svc.SyncDelete(context.Background(), "r", "branch", "main")
	if err == nil {
		t.Fatal("expected error for missing provider")
	}
}

func TestSyncDelete_DeleteRefError(t *testing.T) {
	git := &mockGitRunner{deleteRefErr: fmt.Errorf("delete failed")}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncDelete(context.Background(), "my-repo", "branch", "old-branch")
	if err == nil {
		t.Fatal("expected error")
	}
	if len(notif.messages) != 1 || notif.messages[0].Level != "error" {
		t.Errorf("expected error notification, got %+v", notif.messages)
	}
}

// --- defaultGitRunner integration tests ---

func TestDefaultGitRunner_CloneMirror(t *testing.T) {
	// Create a source bare repo
	srcDir := t.TempDir()
	runGit(t, srcDir, "init", "--bare")

	// Clone mirror from local bare repo
	runner := &defaultGitRunner{}
	destDir := t.TempDir() + "/mirror.git"

	err := runner.CloneMirror(context.Background(), localRemote(srcDir), destDir)
	if err != nil {
		t.Fatalf("CloneMirror failed: %v", err)
	}
}

// localSpecs builds a lease-free spec per local ref — what planPush produces
// for a destination that reports none of them. Unlike specsAgainst it never
// contacts the destination, so it also works for a target that cannot be read.
func localSpecs(t *testing.T, runner *defaultGitRunner, mirrorDir string) []PushSpec {
	t.Helper()
	local, err := runner.ListRefTips(context.Background(), mirrorDir)
	if err != nil {
		t.Fatalf("ListRefTips failed: %v", err)
	}
	specs := make([]PushSpec, 0, len(local))
	for ref := range local {
		specs = append(specs, PushSpec{Ref: ref})
	}
	return specs
}

// specsAgainst builds the specs planPush would produce for a destination that
// needs no ref withheld: every local ref, leased to whatever the destination
// currently reports for it. Re-reading the destination each time is the point —
// a spec built for an empty destination carries the "must not exist" lease, and
// reusing it after the refs are there would be rejected rather than up-to-date.
func specsAgainst(t *testing.T, runner *defaultGitRunner, mirrorDir, tgtURL string) []PushSpec {
	t.Helper()
	local, err := runner.ListRefTips(context.Background(), mirrorDir)
	if err != nil {
		t.Fatalf("ListRefTips failed: %v", err)
	}
	remote, err := runner.RemoteRefs(context.Background(), localRemote(tgtURL))
	if err != nil {
		t.Fatalf("RemoteRefs failed: %v", err)
	}
	specs := make([]PushSpec, 0, len(local))
	for ref := range local {
		specs = append(specs, PushSpec{Ref: ref, Lease: remote[ref]})
	}
	return specs
}

func TestDefaultGitRunner_PushMirror(t *testing.T) {
	// Create source repo with a commit
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/file.txt", "hello")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "init")

	// Create target bare repo
	tgtDir := t.TempDir()
	runGit(t, tgtDir, "init", "--bare")

	// Clone mirror from source, then push to target
	runner := &defaultGitRunner{}
	mirrorDir := t.TempDir() + "/mirror.git"

	if err := runner.CloneMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("CloneMirror failed: %v", err)
	}
	res, err := runner.PushMirror(context.Background(), mirrorDir, localRemote(tgtDir), specsAgainst(t, runner, mirrorDir, tgtDir))
	if err != nil {
		t.Fatalf("PushMirror failed: %v", err)
	}
	if !res.Changed {
		t.Error("expected changed=true for first push")
	}
	// A first push creates refs ('*'), which is not an overwrite.
	if len(res.Forced) != 0 {
		t.Errorf("expected no forced refs on initial push, got %v", res.Forced)
	}
	if len(res.Rejected) != 0 {
		t.Errorf("expected no rejected refs on initial push, got %v", res.Rejected)
	}

	// Push again — should be up-to-date
	res2, err := runner.PushMirror(context.Background(), mirrorDir, localRemote(tgtDir), specsAgainst(t, runner, mirrorDir, tgtDir))
	if err != nil {
		t.Fatalf("PushMirror second push failed: %v", err)
	}
	if res2.Changed {
		t.Error("expected changed=false for second push (up-to-date)")
	}
}

// TestDefaultGitRunner_PushMirror_Scoped verifies with real git that a scoped push given
// an explicit refspec moves only that ref, and that ListRefs returns the local refs.
func TestDefaultGitRunner_PushMirror_Scoped(t *testing.T) {
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/file.txt", "hello")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "init")
	runGit(t, srcDir, "branch", "feature-x")

	tgtDir := t.TempDir()
	runGit(t, tgtDir, "init", "--bare")

	runner := &defaultGitRunner{}
	mirrorDir := t.TempDir() + "/mirror.git"
	if err := runner.CloneMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("CloneMirror failed: %v", err)
	}

	// ListRefs has to return both branches (the main-family one + feature-x).
	refs, err := runner.ListRefs(context.Background(), mirrorDir)
	if err != nil {
		t.Fatalf("ListRefs failed: %v", err)
	}
	hasFeature := false
	for _, r := range refs {
		if r == "refs/heads/feature-x" {
			hasFeature = true
		}
	}
	if !hasFeature {
		t.Errorf("ListRefs missing refs/heads/feature-x, got %v", refs)
	}

	// Scoped push of feature-x only → only feature-x may exist on the target.
	res, err := runner.PushMirror(context.Background(), mirrorDir, localRemote(tgtDir),
		[]PushSpec{{Ref: "refs/heads/feature-x"}})
	if err != nil {
		t.Fatalf("scoped PushMirror failed: %v", err)
	}
	if !res.Changed {
		t.Error("expected changed=true for scoped push")
	}
	tgtRefs, err := runner.ListRefs(context.Background(), tgtDir)
	if err != nil {
		t.Fatalf("ListRefs(target) failed: %v", err)
	}
	for _, r := range tgtRefs {
		if r != "refs/heads/feature-x" {
			t.Errorf("scoped push leaked ref %q to target (expected only feature-x)", r)
		}
	}
	if len(tgtRefs) != 1 {
		t.Errorf("expected exactly 1 ref on target, got %v", tgtRefs)
	}
}

func TestDefaultGitRunner_DeleteRef(t *testing.T) {
	// Create a repo with a branch
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/file.txt", "hello")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "init")
	runGit(t, srcDir, "checkout", "-b", "feature-branch")
	runGit(t, srcDir, "checkout", "master")

	// Clone to bare (target)
	tgtDir := t.TempDir() + "/target.git"
	runGit(t, "", "clone", "--bare", srcDir, tgtDir)

	// Delete the feature-branch from target
	runner := &defaultGitRunner{}
	workDir := t.TempDir() + "/delete-work.git"

	err := runner.DeleteRef(context.Background(), workDir, localRemote(tgtDir), "branch", "feature-branch")
	if err != nil {
		t.Fatalf("DeleteRef failed: %v", err)
	}
}

func TestDefaultGitRunner_RefTip(t *testing.T) {
	// Create a repo with a branch and a tag
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/file.txt", "hello")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "init")
	runGit(t, srcDir, "checkout", "-b", "feature-branch")
	runGit(t, srcDir, "checkout", "master")
	runGit(t, srcDir, "tag", "v1.0.0")

	tgtDir := t.TempDir() + "/target.git"
	runGit(t, "", "clone", "--bare", srcDir, tgtDir)

	runner := &defaultGitRunner{}
	ctx := context.Background()

	// Present branch → full 40-character tip. The length is the point: a delete
	// records this value so the objects can be fetched back, and a remote
	// rejects an abbreviated object name.
	fullSHA := regexp.MustCompile(`^[0-9a-f]{40}$`)
	branchTip, err := runner.RefTip(ctx, localRemote(tgtDir), "branch", "feature-branch")
	if err != nil {
		t.Fatalf("RefTip(present branch) failed: %v", err)
	}
	if !fullSHA.MatchString(branchTip) {
		t.Errorf("expected a full 40-character object name, got %q", branchTip)
	}

	// Present tag → tip too. Every ref here points at the same commit, so the
	// tag must resolve to exactly what the branch did.
	tagTip, err := runner.RefTip(ctx, localRemote(tgtDir), "tag", "v1.0.0")
	if err != nil {
		t.Fatalf("RefTip(present tag) failed: %v", err)
	}
	if tagTip != branchTip {
		t.Errorf("tag and branch point at the same commit; got tag=%q branch=%q", tagTip, branchTip)
	}

	// Absent branch → "", no error
	tip, err := runner.RefTip(ctx, localRemote(tgtDir), "branch", "no-such-branch")
	if err != nil {
		t.Fatalf("RefTip(absent) returned error: %v", err)
	}
	if tip != "" {
		t.Errorf("expected no-such-branch to be absent, got %q", tip)
	}

	// Prefix non-match: "feature" must not match "feature-branch".
	tip, err = runner.RefTip(ctx, localRemote(tgtDir), "branch", "feature")
	if err != nil {
		t.Fatalf("RefTip(prefix) returned error: %v", err)
	}
	if tip != "" {
		t.Errorf("prefix 'feature' must not match 'feature-branch', got %q", tip)
	}
}

func TestDefaultGitRunner_RefTip_InvalidURL(t *testing.T) {
	runner := &defaultGitRunner{}
	_, err := runner.RefTip(context.Background(), localRemote("http://invalid.invalid.invalid/repo.git"), "branch", "main")
	if err == nil {
		t.Fatal("expected error for invalid ls-remote URL")
	}
}

// runGitOut is runGit for the cases that need to read what git printed.
func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
	return string(out)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	var cmd *exec.Cmd
	if dir == "" {
		cmd = exec.Command("git", args...)
	} else {
		cmd = exec.Command("git", append([]string{"-C", dir}, args...)...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// gitOut runs git and returns its trimmed stdout, for the cases that assert on
// a SHA rather than on a side effect.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mkBareHEAD creates dir and a HEAD file to imitate an existing mirror (a bare git dir).
func mkBareHEAD(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/HEAD", []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- defaultGitRunner error path tests ---

func TestDefaultGitRunner_CloneMirror_InvalidURL(t *testing.T) {
	runner := &defaultGitRunner{}
	destDir := t.TempDir() + "/mirror.git"

	err := runner.CloneMirror(context.Background(), localRemote("http://invalid.invalid.invalid/repo.git"), destDir)
	if err == nil {
		t.Fatal("expected error for invalid clone URL")
	}
}

func TestDefaultGitRunner_PushMirror_InvalidURL(t *testing.T) {
	// Create a valid mirror repo first
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/file.txt", "hello")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "init")

	runner := &defaultGitRunner{}
	mirrorDir := t.TempDir() + "/mirror.git"
	if err := runner.CloneMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("CloneMirror failed: %v", err)
	}

	// Push to invalid URL should fail
	_, err := runner.PushMirror(context.Background(), mirrorDir, localRemote("http://invalid.invalid.invalid/repo.git"),
		localSpecs(t, runner, mirrorDir))
	if err == nil {
		t.Fatal("expected error for invalid push URL")
	}
}

func TestDefaultGitRunner_DeleteRef_InvalidURL(t *testing.T) {
	runner := &defaultGitRunner{}
	workDir := t.TempDir() + "/delete-work.git"

	err := runner.DeleteRef(context.Background(), workDir, localRemote("http://invalid.invalid.invalid/repo.git"), "branch", "main")
	if err == nil {
		t.Fatal("expected error for invalid delete URL")
	}
}

func TestDefaultGitRunner_CloneMirror_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	runner := &defaultGitRunner{}
	destDir := t.TempDir() + "/mirror.git"

	err := runner.CloneMirror(ctx, localRemote("http://example.com/repo.git"), destDir)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestDefaultGitRunner_PushMirror_TagsFailure(t *testing.T) {
	// Create source with a commit
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/file.txt", "hello")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "init")

	// Clone mirror
	runner := &defaultGitRunner{}
	mirrorDir := t.TempDir() + "/mirror.git"
	if err := runner.CloneMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// Push branches succeeds to valid target, then we remove the target to fail tags push
	tgtDir := t.TempDir()
	runGit(t, tgtDir, "init", "--bare")

	// This should succeed (both branches and tags)
	if _, err := runner.PushMirror(context.Background(), mirrorDir, localRemote(tgtDir), nil); err != nil {
		t.Fatalf("PushMirror should succeed: %v", err)
	}
}

// --- direction helper tests ---

func TestAllowsSourceToTarget(t *testing.T) {
	tests := []struct {
		dir  string
		want bool
	}{
		{"source-to-target", true},
		{"Source-To-Target", true},
		{"bidirectional", true},
		{"Bidirectional", true},
		{"target-to-source", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := allowsSourceToTarget(tt.dir); got != tt.want {
			t.Errorf("allowsSourceToTarget(%q) = %v, want %v", tt.dir, got, tt.want)
		}
	}
}

func TestAllowsTargetToSource(t *testing.T) {
	tests := []struct {
		dir  string
		want bool
	}{
		{"target-to-source", true},
		{"Target-To-Source", true},
		{"bidirectional", true},
		{"source-to-target", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := allowsTargetToSource(tt.dir); got != tt.want {
			t.Errorf("allowsTargetToSource(%q) = %v, want %v", tt.dir, got, tt.want)
		}
	}
}

// --- parsePush tests ---

func TestParsePushChanged(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"empty output", "", false},
		{"whitespace only", "  \n  ", false},
		{"up-to-date ref", "To /tmp/target.git\n=\trefs/heads/main:refs/heads/main\t[up to date]\nDone", false},
		{"new branch", "To /tmp/target.git\n*\trefs/heads/main:refs/heads/main\t[new branch]\nDone", true},
		{"forced update", "To /tmp/target.git\n+\trefs/heads/main:refs/heads/main\tabc123...def456\nDone", true},
		{"normal update", "To /tmp/target.git\n \trefs/heads/main:refs/heads/main\tabc123..def456\nDone", true},
		{"mixed up-to-date and changed", "To /tmp/target.git\n=\trefs/heads/main:refs/heads/main\t[up to date]\n+\trefs/heads/dev:refs/heads/dev\tabc123...def456\nDone", true},
		{"all up-to-date", "To /tmp/target.git\n=\trefs/heads/main:refs/heads/main\t[up to date]\n=\trefs/heads/dev:refs/heads/dev\t[up to date]\nDone", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parsePush(tt.output).Changed; got != tt.want {
				t.Errorf("parsePush(%q).Changed = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

// TestParsePushForced covers the half that used to be discarded: which refs were
// overwritten, and the tip each one replaced.
func TestParsePushForced(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   []history.ForcedRef
	}{
		{
			// A fast-forward uses ".." and must never be reported as a rewind —
			// this is the case that separates routine mirroring from data loss.
			name:   "fast-forward is not forced",
			output: "To /tmp/t.git\n \trefs/heads/main:refs/heads/main\tabc123..def456\nDone",
		},
		{
			name:   "new ref is not forced",
			output: "To /tmp/t.git\n*\trefs/heads/main:refs/heads/main\t[new branch]\nDone",
		},
		{
			name:   "forced branch yields old and new tip",
			output: "To /tmp/t.git\n+\trefs/heads/main:refs/heads/main\tabc123...def456\nDone",
			want:   []history.ForcedRef{{Ref: "refs/heads/main", Old: "abc123", New: "def456"}},
		},
		{
			// git appends a parenthesised reason to the summary; it must not end
			// up glued to the SHA that recovery depends on.
			name:   "trailing reason is stripped from the new tip",
			output: "To /tmp/t.git\n+\trefs/heads/main:refs/heads/main\tabc123...def456 (forced update)\nDone",
			want:   []history.ForcedRef{{Ref: "refs/heads/main", Old: "abc123", New: "def456"}},
		},
		{
			name:   "destination ref is taken from the right of the colon",
			output: "To /tmp/t.git\n+\trefs/heads/local:refs/heads/remote\tabc123...def456\nDone",
			want:   []history.ForcedRef{{Ref: "refs/heads/remote", Old: "abc123", New: "def456"}},
		},
		{
			name:   "tags are reported too",
			output: "To /tmp/t.git\n+\trefs/tags/v1:refs/tags/v1\t111aaa...222bbb\nDone",
			want:   []history.ForcedRef{{Ref: "refs/tags/v1", Old: "111aaa", New: "222bbb"}},
		},
		{
			name: "several forced refs in one push",
			output: "To /tmp/t.git\n" +
				"=\trefs/heads/idle:refs/heads/idle\t[up to date]\n" +
				"+\trefs/heads/a:refs/heads/a\t1a...2a\n" +
				" \trefs/heads/ff:refs/heads/ff\t9x..9y\n" +
				"+\trefs/heads/b:refs/heads/b\t1b...2b\nDone",
			want: []history.ForcedRef{
				{Ref: "refs/heads/a", Old: "1a", New: "2a"},
				{Ref: "refs/heads/b", Old: "1b", New: "2b"},
			},
		},
		{
			// A record without the old tip is worthless — it claims a rewind
			// while withholding the only field that makes recovery possible — so
			// an unparseable line is dropped rather than recorded blank.
			name:   "unparseable summary is not recorded",
			output: "To /tmp/t.git\n+\trefs/heads/main:refs/heads/main\t(forced update)\nDone",
		},
		{
			name:   "missing summary field is not recorded",
			output: "To /tmp/t.git\n+\trefs/heads/main:refs/heads/main\nDone",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePush(tt.output).Forced
			if len(got) != len(tt.want) {
				t.Fatalf("parsePush(%q).Forced = %+v, want %+v", tt.output, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("forced[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
			// Anything flagged forced also moved.
			if len(tt.want) > 0 && !parsePush(tt.output).Changed {
				t.Error("a forced ref implies Changed=true")
			}
		})
	}
}

// TestDefaultGitRunner_PushMirror_ForcedUpdate drives the parser from real git
// output: rewrite the source tip so the mirror's next push cannot fast-forward,
// then check the old tip is reported and actually is the commit that was on the
// target beforehand.
func TestDefaultGitRunner_PushMirror_ForcedUpdate(t *testing.T) {
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/a.txt", "one")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "first")
	runGit(t, srcDir, "branch", "-M", "main")

	tgtDir := t.TempDir()
	runGit(t, tgtDir, "init", "--bare")

	runner := &defaultGitRunner{}
	mirrorDir := t.TempDir() + "/mirror.git"
	if err := runner.CloneMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("CloneMirror failed: %v", err)
	}
	if _, err := runner.PushMirror(context.Background(), mirrorDir, localRemote(tgtDir), localSpecs(t, runner, mirrorDir)); err != nil {
		t.Fatalf("initial PushMirror failed: %v", err)
	}
	tipBefore := gitOut(t, mirrorDir, "rev-parse", "refs/heads/main")

	// Replace the tip with an unrelated commit — the shape of a rebase, an
	// amend, or a stale mirror overwriting a push it never fetched.
	runGit(t, srcDir, "checkout", "--orphan", "rewritten")
	writeFile(t, srcDir+"/b.txt", "two")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "rewritten")
	runGit(t, srcDir, "branch", "-M", "main")

	if err := runner.FetchMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("FetchMirror failed: %v", err)
	}
	res, err := runner.PushMirror(context.Background(), mirrorDir, localRemote(tgtDir), specsAgainst(t, runner, mirrorDir, tgtDir))
	if err != nil {
		t.Fatalf("second PushMirror failed: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected the rewriting push to report a change")
	}
	if len(res.Forced) == 0 {
		t.Fatal("expected the rewriting push to report a forced update")
	}
	var main *history.ForcedRef
	for i := range res.Forced {
		if res.Forced[i].Ref == "refs/heads/main" {
			main = &res.Forced[i]
		}
	}
	if main == nil {
		t.Fatalf("refs/heads/main missing from forced refs %+v", res.Forced)
	}
	// The recorded old tip must be the commit the target actually held, or the
	// history says a rewind happened and points at the wrong commit to recover.
	//
	// Full length is asserted, not just a prefix match: git abbreviates the
	// porcelain summary to 7 characters by default, and a remote will not serve
	// a fetch for an abbreviated object name. A short SHA here would still pass
	// a prefix check while making the recorded tip unfetchable — which is the
	// whole reason runPush pins core.abbrev.
	if main.Old != tipBefore {
		t.Errorf("forced old tip = %q, want the full previous tip %q", main.Old, tipBefore)
	}
	if len(main.New) != len(tipBefore) {
		t.Errorf("forced new tip %q is not a full object name", main.New)
	}
	if main.New == tipBefore {
		t.Errorf("forced new tip %q should differ from the previous tip", main.New)
	}
}

// --- no-change skip notification tests ---

func TestDoMirror_NoChange_SkipsNotification(t *testing.T) {
	git := &mockGitRunner{pushChanged: false}
	notif := &mockNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)

	repoCfg := config.RepoConfig{Name: "test-repo"}
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "my-repo", "gitlab-main", "team/my-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(notif.messages) != 0 {
		t.Errorf("expected no notifications when nothing changed, got %d: %+v", len(notif.messages), notif.messages)
	}
}

func TestSync_NoChange_SkipsNotification(t *testing.T) {
	git := &mockGitRunner{pushChanged: false}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.Sync(context.Background(), "my-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(notif.messages) != 0 {
		t.Errorf("expected no notifications when nothing changed, got %d", len(notif.messages))
	}
}

func TestSyncByTarget_NoChange_SkipsNotification(t *testing.T) {
	git := &mockGitRunner{pushChanged: false}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncByTarget(context.Background(), "gitlab", "team/bidi-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(notif.messages) != 0 {
		t.Errorf("expected no notifications when nothing changed, got %d", len(notif.messages))
	}
}

func TestSyncByTarget_WithChanges_SendsNotification(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncByTarget(context.Background(), "gitlab", "team/bidi-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(notif.messages) != 1 || notif.messages[0].Level != "success" {
		t.Errorf("expected 1 success notification, got %+v", notif.messages)
	}
}

// --- repoLock tests ---

func TestRepoLock_ReturnsSameMutex(t *testing.T) {
	svc := newTestService(nil, nil, &mockNotifier{}, &mockGitRunner{})
	mu1 := svc.repoLock("repo-a")
	mu2 := svc.repoLock("repo-a")
	if mu1 != mu2 {
		t.Error("expected same mutex for same repo")
	}
	mu3 := svc.repoLock("repo-b")
	if mu1 == mu3 {
		t.Error("expected different mutex for different repo")
	}
}

// --- incremental fetch tests ---

func TestDoMirror_IncrementalFetch(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)
	svc.workDir = t.TempDir()

	// Create a fake bare git dir to simulate existing mirror
	mirrorDir := svc.workDir + "/test-repo-codecommit-eu.git"
	mkBareHEAD(t, mirrorDir)

	repoCfg := config.RepoConfig{Name: "test-repo"}
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "my-repo", "gitlab-main", "team/my-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should fetch, not clone
	if len(git.fetchCalls) != 1 {
		t.Errorf("expected 1 fetch call, got %d", len(git.fetchCalls))
	}
	if len(git.cloneCalls) != 0 {
		t.Errorf("expected 0 clone calls, got %d", len(git.cloneCalls))
	}
}

func TestDoMirror_FetchFallbackToClone(t *testing.T) {
	git := &mockGitRunner{fetchErr: fmt.Errorf("fetch failed"), pushChanged: true}
	notif := &mockNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)
	svc.workDir = t.TempDir()

	// Create a fake bare git dir
	mirrorDir := svc.workDir + "/test-repo-codecommit-eu.git"
	mkBareHEAD(t, mirrorDir)

	repoCfg := config.RepoConfig{Name: "test-repo"}
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "my-repo", "gitlab-main", "team/my-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should fetch first, then fallback to clone
	if len(git.fetchCalls) != 1 {
		t.Errorf("expected 1 fetch call, got %d", len(git.fetchCalls))
	}
	if len(git.cloneCalls) != 1 {
		t.Errorf("expected 1 clone call (fallback), got %d", len(git.cloneCalls))
	}
}

func TestDoMirror_FetchAndFallbackCloneBothFail(t *testing.T) {
	git := &mockGitRunner{fetchErr: fmt.Errorf("fetch failed"), cloneErr: fmt.Errorf("clone failed")}
	notif := &mockNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)
	svc.workDir = t.TempDir()

	// Create a fake bare git dir to trigger fetch path
	mirrorDir := svc.workDir + "/test-repo-codecommit-eu.git"
	mkBareHEAD(t, mirrorDir)

	repoCfg := config.RepoConfig{Name: "test-repo"}
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "my-repo", "gitlab-main", "team/my-repo", EventMeta{})
	if err == nil {
		t.Fatal("expected error when both fetch and fallback clone fail")
	}

	if len(notif.messages) != 1 || notif.messages[0].Level != "error" {
		t.Errorf("expected error notification, got %+v", notif.messages)
	}
}

func TestDoMirror_InitialClone(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)
	svc.workDir = t.TempDir()

	// No existing mirror dir — should do full clone
	repoCfg := config.RepoConfig{Name: "test-repo"}
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "my-repo", "gitlab-main", "team/my-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(git.cloneCalls) != 1 {
		t.Errorf("expected 1 clone call, got %d", len(git.cloneCalls))
	}
	if len(git.fetchCalls) != 0 {
		t.Errorf("expected 0 fetch calls, got %d", len(git.fetchCalls))
	}
}

func TestDefaultGitRunner_CommitAuthor(t *testing.T) {
	// Create a repo with a commit by a known author.
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "alice@example.com")
	runGit(t, srcDir, "config", "user.name", "Alice")
	writeFile(t, srcDir+"/file.txt", "hello")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "init")

	// Mirror-clone so we have a bare repo to query against (matches doMirror layout).
	mirrorDir := t.TempDir() + "/mirror.git"
	runner := &defaultGitRunner{}
	if err := runner.CloneMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("CloneMirror failed: %v", err)
	}

	// HEAD on a freshly-cloned bare mirror should resolve to the latest commit
	// authored by Alice.
	author, err := runner.CommitAuthor(context.Background(), mirrorDir, "HEAD")
	if err != nil {
		t.Fatalf("CommitAuthor failed: %v", err)
	}
	if author != "Alice" {
		t.Errorf("author = %q, want Alice", author)
	}
}

func TestDefaultGitRunner_CommitAuthor_BadRef(t *testing.T) {
	// Invalid ref → error path (covers the failure branch).
	srcDir := t.TempDir()
	runGit(t, srcDir, "init", "--bare")

	runner := &defaultGitRunner{}
	_, err := runner.CommitAuthor(context.Background(), srcDir, "refs/heads/does-not-exist")
	if err == nil {
		t.Fatal("expected error for nonexistent ref")
	}
}

func TestDefaultGitRunner_FetchMirror(t *testing.T) {
	// Create source with a commit
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/file.txt", "hello")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "init")

	// Clone mirror
	runner := &defaultGitRunner{}
	mirrorDir := t.TempDir() + "/mirror.git"
	if err := runner.CloneMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("CloneMirror failed: %v", err)
	}

	// Add a new commit to source
	writeFile(t, srcDir+"/file2.txt", "world")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "second")

	// Fetch should pick up the new commit
	if err := runner.FetchMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("FetchMirror failed: %v", err)
	}
}

func TestIsGitDir(t *testing.T) {
	// Not a git dir
	if isGitDir("/nonexistent") {
		t.Error("expected false for nonexistent dir")
	}

	// Dir without HEAD
	tmpDir := t.TempDir()
	if isGitDir(tmpDir) {
		t.Error("expected false for dir without HEAD")
	}

	// Valid bare git dir
	writeFile(t, tmpDir+"/HEAD", "ref: refs/heads/main\n")
	if !isGitDir(tmpDir) {
		t.Error("expected true for dir with HEAD file")
	}
}

// --- LastCommitInfo tests ---

func TestDefaultGitRunner_FetchMirror_InvalidURL(t *testing.T) {
	// Create a valid mirror dir first
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/file.txt", "hello")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "init")

	runner := &defaultGitRunner{}
	mirrorDir := t.TempDir() + "/mirror.git"
	if err := runner.CloneMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	err := runner.FetchMirror(context.Background(), localRemote("http://invalid.invalid.invalid/repo.git"), mirrorDir)
	if err == nil {
		t.Fatal("expected error for invalid fetch URL")
	}
}

func TestDefaultGitRunner_DeleteRef_MkdirFail(t *testing.T) {
	runner := &defaultGitRunner{}
	// Use a path under a file (not a directory) to trigger MkdirAll failure
	tmpFile := t.TempDir() + "/file"
	writeFile(t, tmpFile, "not a dir")

	err := runner.DeleteRef(context.Background(), tmpFile+"/sub", localRemote("http://example.com/repo.git"), "branch", "main")
	if err == nil {
		t.Fatal("expected error when workdir creation fails")
	}
}

func TestDefaultGitRunner_PushMirror_TagsError(t *testing.T) {
	// Create source with a commit and a tag
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/file.txt", "hello")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "init")
	runGit(t, srcDir, "tag", "v1.0.0")

	runner := &defaultGitRunner{}
	mirrorDir := t.TempDir() + "/mirror.git"
	if err := runner.CloneMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// Push branches to valid target, but cancel context before tags
	ctx, cancel := context.WithCancel(context.Background())
	// We can't easily cancel between branches and tags, so just test normal path
	defer cancel()

	tgtDir := t.TempDir()
	runGit(t, tgtDir, "init", "--bare")
	res, err := runner.PushMirror(ctx, mirrorDir, localRemote(tgtDir), localSpecs(t, runner, mirrorDir))
	if err != nil {
		t.Fatalf("PushMirror failed: %v", err)
	}
	if !res.Changed {
		t.Error("expected changed=true")
	}
}

func TestDefaultGitRunner_CloneMirror_CleanupExistingDir(t *testing.T) {
	srcDir := t.TempDir()
	runGit(t, srcDir, "init", "--bare")

	runner := &defaultGitRunner{}
	destDir := t.TempDir() + "/mirror.git"
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, destDir+"/stale-file", "old data")

	err := runner.CloneMirror(context.Background(), localRemote(srcDir), destDir)
	if err != nil {
		t.Fatalf("CloneMirror failed: %v", err)
	}

	// stale file should be removed
	if _, err := os.Stat(destDir + "/stale-file"); err == nil {
		t.Error("expected stale file to be removed")
	}
}

// TestNewGitCmd_ProcessGroupAndCancel verifies that newGitCmd sets the process group, the
// cancel hook and WaitDelay, so that on a timeout even the orphan children git forked get
// cleaned up.
func TestNewGitCmd_ProcessGroupAndCancel(t *testing.T) {
	cmd := newGitCmd(context.Background(), "version")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Error("expected SysProcAttr.Setpgid=true (new process group)")
	}
	if cmd.Cancel == nil {
		t.Fatal("expected Cancel hook to be set")
	}
	if cmd.WaitDelay != gitWaitDelay {
		t.Errorf("expected WaitDelay=%v, got %v", gitWaitDelay, cmd.WaitDelay)
	}
	// Before the process starts, Process is nil, so Cancel has to return nil safely.
	if err := cmd.Cancel(); err != nil {
		t.Errorf("Cancel with nil Process should return nil, got %v", err)
	}
}

// TestDefaultGitRunner_CloneMirror_PreservesCacheOnFailure verifies that a failed clone
// leaves the existing cache (dir) intact rather than destroying it (guarding the atomic
// swap against regression). It used to call os.RemoveAll(dir) before the clone, wiping the
// cache first, so when the fallback clone failed the cache was left gone and empty, forcing
// a full clone every time — a vicious circle (the 2026-06 demo-repo incident).
func TestDefaultGitRunner_CloneMirror_PreservesCacheOnFailure(t *testing.T) {
	srcDir := t.TempDir()
	runGit(t, srcDir, "init", "--bare")

	runner := &defaultGitRunner{}
	destDir := t.TempDir() + "/mirror.git"

	// Build a healthy mirror first to stand in as the "existing cache".
	if err := runner.CloneMirror(context.Background(), localRemote(srcDir), destDir); err != nil {
		t.Fatalf("initial clone failed: %v", err)
	}
	// Plant a marker that identifies this cache.
	writeFile(t, destDir+"/CACHE_MARKER", "precious")

	// Attempt a fallback clone from an invalid URL → it has to fail.
	err := runner.CloneMirror(context.Background(), localRemote("http://invalid.invalid.invalid/repo.git"), destDir)
	if err == nil {
		t.Fatal("expected clone failure for invalid URL")
	}

	// Was the existing cache (the marker) preserved — if it is gone, the atomic swap regressed.
	if _, statErr := os.Stat(destDir + "/CACHE_MARKER"); statErr != nil {
		t.Errorf("existing cache destroyed by a failed clone (atomic-swap regression): %v", statErr)
	}
	// The temporary directory must have been cleaned up after the failure.
	if _, statErr := os.Stat(destDir + ".tmp"); statErr == nil {
		t.Error("expected temp dir to be cleaned up after failed clone")
	}
}

func TestDoMirror_SuccessNotification_EmptyMeta(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)

	repoCfg := config.RepoConfig{Name: "test-repo"}
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "my-repo", "gitlab-main", "team/my-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(notif.messages) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notif.messages))
	}
	body := notif.messages[0].Body
	if strings.Contains(body, "Pushed by:") {
		t.Errorf("should not contain Pushed by when ref is empty, got %q", body)
	}
	if strings.Contains(body, "Branch:") {
		t.Errorf("should not contain Branch when ref is empty, got %q", body)
	}
	if strings.Contains(body, "Tag:") {
		t.Errorf("should not contain Tag when ref is empty, got %q", body)
	}
}

func TestDoMirror_SuccessNotification_NoRef(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)

	repoCfg := config.RepoConfig{Name: "test-repo"}
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "my-repo", "gitlab-main", "team/my-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(notif.messages) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notif.messages))
	}
	body := notif.messages[0].Body
	if strings.Contains(body, "Branch:") {
		t.Errorf("should not contain Branch when ref is empty, got %q", body)
	}
	if strings.Contains(body, "Tag:") {
		t.Errorf("should not contain Tag when ref is empty, got %q", body)
	}
	if strings.Contains(body, "Pushed by:") {
		t.Errorf("should not contain Pushed by when ref is empty, got %q", body)
	}
}

func TestDoMirror_SuccessNotification_CommitAuthorError(t *testing.T) {
	git := &mockGitRunner{pushChanged: true, commitAuthorErr: fmt.Errorf("ref not found")}
	notif := &mockNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)

	meta := EventMeta{Ref: "refs/heads/main"}
	repoCfg := config.RepoConfig{Name: "test-repo"}
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "my-repo", "gitlab-main", "team/my-repo", meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body := notif.messages[0].Body
	if strings.Contains(body, "Pushed by:") {
		t.Errorf("should not contain Pushed by when CommitAuthor fails, got %q", body)
	}
	if !strings.Contains(body, "Branch: main") {
		t.Errorf("expected Branch: main, got %q", body)
	}
}

func TestDoMirror_SuccessNotification_BranchOnly(t *testing.T) {
	git := &mockGitRunner{
		pushChanged:  true,
		commitAuthor: "developer",
		listRefs:     []string{"refs/heads/develop"},
	}
	notif := &mockNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)

	meta := EventMeta{Ref: "refs/heads/develop"}
	repoCfg := config.RepoConfig{Name: "test-repo"}
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "my-repo", "gitlab-main", "team/my-repo", meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body := notif.messages[0].Body
	if !strings.Contains(body, "Branch: develop") {
		t.Errorf("expected Branch: develop, got %q", body)
	}
	if strings.Contains(body, "Tag:") {
		t.Errorf("should not contain Tag for branch push, got %q", body)
	}
}

func TestDoMirror_SuccessNotification_TagOnly(t *testing.T) {
	git := &mockGitRunner{
		pushChanged:  true,
		commitAuthor: "tagger",
		listRefs:     []string{"refs/tags/v2.0.0"},
	}
	notif := &mockNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)

	meta := EventMeta{Ref: "refs/tags/v2.0.0"}
	repoCfg := config.RepoConfig{Name: "test-repo"}
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "my-repo", "gitlab-main", "team/my-repo", meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body := notif.messages[0].Body
	if !strings.Contains(body, "Tag: v2.0.0") {
		t.Errorf("expected Tag: v2.0.0, got %q", body)
	}
	if strings.Contains(body, "Branch:") {
		t.Errorf("should not contain Branch for tag push, got %q", body)
	}
}

func TestEventMeta_RefName(t *testing.T) {
	tests := []struct {
		ref  string
		want string
	}{
		{"refs/heads/main", "main"},
		{"refs/heads/feature/foo", "feature/foo"},
		{"refs/tags/v1.0.0", "v1.0.0"},
		{"other", "other"},
		{"", ""},
	}
	for _, tt := range tests {
		m := EventMeta{Ref: tt.ref}
		if got := m.RefName(); got != tt.want {
			t.Errorf("EventMeta{Ref: %q}.RefName() = %q, want %q", tt.ref, got, tt.want)
		}
	}
}

func TestEventMeta_IsTag(t *testing.T) {
	if !(EventMeta{Ref: "refs/tags/v1.0"}).IsTag() {
		t.Error("expected IsTag() true for refs/tags/v1.0")
	}
	if (EventMeta{Ref: "refs/heads/main"}).IsTag() {
		t.Error("expected IsTag() false for refs/heads/main")
	}
}

// --- Per-repo Slack webhook URL override tests ---

// urlCapturingNotifier records the WebhookURL field of every message sent.
type urlCapturingNotifier struct {
	urls []string
}

func (n *urlCapturingNotifier) Send(msg notify.Message) {
	n.urls = append(n.urls, msg.WebhookURL)
}

func TestDoMirror_PropagatesRepoSlackWebhookURL(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	notif := &urlCapturingNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)

	repoCfg := config.RepoConfig{Name: "test-repo", SlackWebhookURL: "https://hooks.slack.test/TESTURL"}
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "src", "gitlab-main", "team/dst", EventMeta{Ref: "refs/heads/main"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(notif.urls) != 1 || notif.urls[0] != "https://hooks.slack.test/TESTURL" {
		t.Errorf("expected webhook URL to be propagated from RepoConfig, got %v", notif.urls)
	}
}

func TestDoMirror_EmptySlackWebhookURL_LeavesOverrideEmpty(t *testing.T) {
	// When RepoConfig.SlackWebhookURL is empty, the Message.WebhookURL field stays
	// empty — Slack.Send then falls back to the notifier's default URL.
	git := &mockGitRunner{pushChanged: true}
	notif := &urlCapturingNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)

	repoCfg := config.RepoConfig{Name: "test-repo"} // no SlackWebhookURL
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "src", "gitlab-main", "team/dst", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(notif.urls) != 1 || notif.urls[0] != "" {
		t.Errorf("expected empty webhook URL when RepoConfig has none, got %v", notif.urls)
	}
}

func TestDoMirror_FailureNotification_PropagatesSlackWebhookURL(t *testing.T) {
	git := &mockGitRunner{cloneErr: fmt.Errorf("clone failed")}
	notif := &urlCapturingNotifier{}
	svc := newTestService(nil, makeProviders(), notif, git)

	repoCfg := config.RepoConfig{Name: "test-repo", SlackWebhookURL: "https://hooks.slack.test/FAILURL"}
	_ = svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "src", "gitlab-main", "team/dst", EventMeta{})

	if len(notif.urls) != 1 || notif.urls[0] != "https://hooks.slack.test/FAILURL" {
		t.Errorf("failure notification should carry the override URL, got %v", notif.urls)
	}
}

// --- Retry tests ---

func TestRetry_SourceToTarget_Explicit(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.Retry(context.Background(), "my-repo", "source-to-target", EventMeta{Source: "retry-api"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.cloneCalls) != 1 {
		t.Errorf("expected 1 clone call, got %d", len(git.cloneCalls))
	}
	if len(notif.messages) != 1 || notif.messages[0].Level != "success" {
		t.Fatalf("expected success notification, got %+v", notif.messages)
	}
	if !strings.Contains(notif.messages[0].Body, "Source: retry-api") {
		t.Errorf("expected body to contain 'Source: retry-api', got %q", notif.messages[0].Body)
	}
}

func TestRetry_TargetToSource_Explicit(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.Retry(context.Background(), "reverse-repo", "target-to-source", EventMeta{Source: "retry-api"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.cloneCalls) != 1 {
		t.Errorf("expected 1 clone call, got %d", len(git.cloneCalls))
	}
}

func TestRetry_Auto_Bidirectional_FallsBackToTargetToSource(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	// bidi-repo: source=codecommit-eu, target=gitlab-main, direction=bidirectional
	err := svc.Retry(context.Background(), "bidi-repo", "auto", EventMeta{Source: "retry-api"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// auto on bidirectional should pick target-to-source, i.e. clone from gitlab.
	if len(git.cloneCalls) != 1 {
		t.Fatalf("expected 1 clone call, got %d", len(git.cloneCalls))
	}
	cloneURL := git.cloneCalls[0].URL
	if !strings.Contains(cloneURL, "gitlab") {
		t.Errorf("expected clone URL to come from gitlab (target), got %q", cloneURL)
	}
}

func TestRetry_Auto_UsesRepoRetryDirectionOverride(t *testing.T) {
	// bidi-repo with retry_direction="source-to-target" (operator override) →
	// auto must pick source-to-target, NOT the built-in target-to-source fallback.
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	repos := defaultRepos()
	for i := range repos {
		if repos[i].Name == "bidi-repo" {
			repos[i].RetryDirection = "source-to-target"
		}
	}
	svc := newTestService(repos, makeProviders(), notif, git)

	err := svc.Retry(context.Background(), "bidi-repo", "auto", EventMeta{Source: "retry-api"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cloneURL := git.cloneCalls[0].URL
	// Source provider is codecommit, target is gitlab.example.com — under
	// source-to-target the clone must come from codecommit (no gitlab in URL).
	if strings.Contains(cloneURL, "gitlab.example.com") {
		t.Errorf("retry_direction override should clone from source (codecommit), got %q", cloneURL)
	}
}

func TestRetry_Auto_RetryDirectionOverridesFallback(t *testing.T) {
	// Even when retry_direction equals the built-in fallback for that repo
	// (target-to-source), the override path must still be taken (gives operator
	// confidence that the configured value drives behavior).
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	repos := defaultRepos()
	for i := range repos {
		if repos[i].Name == "bidi-repo" {
			repos[i].RetryDirection = "target-to-source"
		}
	}
	svc := newTestService(repos, makeProviders(), notif, git)

	err := svc.Retry(context.Background(), "bidi-repo", "auto", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cloneURL := git.cloneCalls[0].URL
	if !strings.Contains(cloneURL, "gitlab") {
		t.Errorf("expected target-to-source (clone from gitlab), got %q", cloneURL)
	}
}

func TestRetry_ExplicitDirection_IgnoresRepoRetryDirection(t *testing.T) {
	// Explicit direction in the API call wins over repo's retry_direction.
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	repos := defaultRepos()
	for i := range repos {
		if repos[i].Name == "bidi-repo" {
			repos[i].RetryDirection = "source-to-target"
		}
	}
	svc := newTestService(repos, makeProviders(), notif, git)

	// API call requests target-to-source — should override repo's pin.
	err := svc.Retry(context.Background(), "bidi-repo", "target-to-source", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cloneURL := git.cloneCalls[0].URL
	if !strings.Contains(cloneURL, "gitlab") {
		t.Errorf("explicit direction should win over retry_direction, expected clone from gitlab, got %q", cloneURL)
	}
}

func TestRetry_Auto_OneWay_UsesAllowedDirection(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	// my-repo: source-to-target only — auto must resolve to source-to-target.
	err := svc.Retry(context.Background(), "my-repo", "auto", EventMeta{Source: "retry-api"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cloneURL := git.cloneCalls[0].URL
	// source provider is codecommit-eu — its URL should appear in the clone call.
	if strings.Contains(cloneURL, "gitlab.example.com") {
		t.Errorf("auto on one-way source-to-target should clone from source (codecommit), got %q", cloneURL)
	}
}

func TestRetry_EmptyDirection_DefaultsToAuto(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	// empty direction should behave the same as "auto" — for my-repo, that's source-to-target.
	err := svc.Retry(context.Background(), "my-repo", "", EventMeta{Source: "retry-api"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.cloneCalls) != 1 {
		t.Fatalf("expected 1 clone call, got %d", len(git.cloneCalls))
	}
}

func TestRetry_ConflictDirection_OneWayRepo(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	// my-repo is source-to-target only; requesting target-to-source must fail.
	err := svc.Retry(context.Background(), "my-repo", "target-to-source", EventMeta{Source: "retry-api"})
	if err == nil {
		t.Fatal("expected error for conflicting direction")
	}
	if len(git.cloneCalls) != 0 {
		t.Errorf("expected no clone calls, got %d", len(git.cloneCalls))
	}
}

func TestRetry_UnknownRepo(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.Retry(context.Background(), "no-such-repo", "auto", EventMeta{})
	if err == nil {
		t.Fatal("expected error for unknown repo")
	}
}

func TestRetry_InvalidDirectionAfterAuto(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	// "sideways" passes the lookup but fails the switch — directionAllowed returns false first.
	err := svc.Retry(context.Background(), "bidi-repo", "sideways", EventMeta{})
	if err == nil {
		t.Fatal("expected error for invalid direction string")
	}
}

func TestResolveAutoDirection(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"bidirectional", "target-to-source"},
		{"Bidirectional", "target-to-source"},
		{"source-to-target", "source-to-target"},
		{"target-to-source", "target-to-source"},
		{"unknown", "target-to-source"}, // safe default
		{"", "target-to-source"},
	}
	for _, tt := range tests {
		if got := resolveAutoDirection(tt.in); got != tt.want {
			t.Errorf("resolveAutoDirection(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDirectionAllowed(t *testing.T) {
	tests := []struct {
		cfgDir   string
		retryDir string
		want     bool
	}{
		{"bidirectional", "source-to-target", true},
		{"bidirectional", "target-to-source", true},
		{"source-to-target", "source-to-target", true},
		{"source-to-target", "target-to-source", false},
		{"target-to-source", "target-to-source", true},
		{"target-to-source", "source-to-target", false},
		{"Source-To-Target", "source-to-target", true},
	}
	for _, tt := range tests {
		if got := directionAllowed(tt.cfgDir, tt.retryDir); got != tt.want {
			t.Errorf("directionAllowed(%q, %q) = %v, want %v", tt.cfgDir, tt.retryDir, got, tt.want)
		}
	}
}

func TestAppendSource(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		source string
		want   string
	}{
		{"empty source", "Action: push", "", "Action: push"},
		{"webhook source", "Action: push", "webhook", "Action: push"},
		{"retry-api source", "Action: push", "retry-api", "Action: push\nSource: retry-api"},
		{"sqs source", "Action: clone", "sqs", "Action: clone\nSource: sqs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendSource(tt.body, EventMeta{Source: tt.source})
			if got != tt.want {
				t.Errorf("appendSource() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRetry_FailureNotification_IncludesSource(t *testing.T) {
	git := &mockGitRunner{cloneErr: fmt.Errorf("network down")}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.Retry(context.Background(), "my-repo", "auto", EventMeta{Source: "retry-api"})
	if err == nil {
		t.Fatal("expected error")
	}
	if len(notif.messages) != 1 || notif.messages[0].Level != "error" {
		t.Fatalf("expected error notification, got %+v", notif.messages)
	}
	if !strings.Contains(notif.messages[0].Body, "Source: retry-api") {
		t.Errorf("failure body should include 'Source: retry-api', got %q", notif.messages[0].Body)
	}
}

// Helper wrappers to use provider constructors from test package
func NewCodeCommit(cfg config.ProviderConfig) provider.Provider {
	p, _ := provider.New("cc", cfg)
	return p
}

func NewGitLab(cfg config.ProviderConfig) provider.Provider {
	p, _ := provider.New("gl", cfg)
	return p
}

func NewGitHub(cfg config.ProviderConfig) provider.Provider {
	p, _ := provider.New("gh", cfg)
	return p
}

// --- ref_overrides (Phase A: per-ref direction) tests ---

// bidiOverrideRepo is a bidirectional repo fixture carrying a ref_override.
// source=codecommit-eu, target=gitlab-main, and branch-a is allowed only gitlab→codecommit (G→C).
func bidiOverrideRepo() config.RepoConfig {
	return config.RepoConfig{
		Name:       "example-bidi",
		Source:     "codecommit-eu",
		Target:     "gitlab-main",
		SourcePath: "example-bidi",
		TargetPath: "server/example-bidi",
		Direction:  "bidirectional",
		RefOverrides: []config.RefOverride{
			{Pattern: "branch-a", From: "gitlab-main", To: "codecommit-eu"},
		},
	}
}

// A reverse-direction (C→G) branch-a event has to be skipped silently (no clone/push, nil returned).
func TestDoMirror_RefOverride_SkipsReverseDirection(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService([]config.RepoConfig{bidiOverrideRepo()}, makeProviders(), notif, git)

	// Sync = source-to-target = codecommit→gitlab = C→G (the override allows G→C only)
	err := svc.Sync(context.Background(), "example-bidi", EventMeta{Ref: "refs/heads/branch-a"})
	if err != nil {
		t.Fatalf("expected nil (terminal skip), got %v", err)
	}
	if len(git.cloneCalls) != 0 || len(git.pushCalls) != 0 {
		t.Errorf("reverse-direction event should skip: clones=%d pushes=%d", len(git.cloneCalls), len(git.pushCalls))
	}
	if len(notif.messages) != 0 {
		t.Errorf("skip should not notify, got %+v", notif.messages)
	}
}

// A branch-a event in the allowed direction (G→C) proceeds, and the push has to be scoped to the trigger ref.
func TestDoMirror_RefOverride_AllowsForwardDirectionScoped(t *testing.T) {
	git := &mockGitRunner{pushChanged: true, listRefs: []string{"refs/heads/branch-a"}}
	notif := &mockNotifier{}
	svc := newTestService([]config.RepoConfig{bidiOverrideRepo()}, makeProviders(), notif, git)

	// SyncByTarget(gitlab,...) = target-to-source = gitlab→codecommit = G→C (allowed)
	err := svc.SyncByTarget(context.Background(), "gitlab", "server/example-bidi", EventMeta{Ref: "refs/heads/branch-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("expected 1 push, got %d", len(git.pushCalls))
	}
	want := []string{"refs/heads/branch-a"}
	if got := pushedRefs(git.pushCalls[0].Specs); !reflect.DeepEqual(got, want) {
		t.Errorf("scoped ref mismatch: got %v want %v", got, want)
	}
}

// A ref that matches no override proceeds in either direction and is scoped to that single ref.
func TestDoMirror_RefOverride_NonMatchingRefProceeds(t *testing.T) {
	git := &mockGitRunner{pushChanged: true, listRefs: []string{"refs/heads/branch-b"}}
	notif := &mockNotifier{}
	svc := newTestService([]config.RepoConfig{bidiOverrideRepo()}, makeProviders(), notif, git)

	// C→G branch-b — no override matches, so it proceeds normally
	err := svc.Sync(context.Background(), "example-bidi", EventMeta{Ref: "refs/heads/branch-b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("expected 1 push, got %d", len(git.pushCalls))
	}
	want := []string{"refs/heads/branch-b"}
	if got := pushedRefs(git.pushCalls[0].Specs); !reflect.DeepEqual(got, want) {
		t.Errorf("scoped ref mismatch: got %v want %v", got, want)
	}
}

// When the trigger ref is not present locally (a retry of a branch that is gone / a prune race),
// it is a no-op skip rather than an error.
func TestDoMirror_RefOverride_AbsentRefSkipsWithoutError(t *testing.T) {
	// An override repo whose ListRefs does not carry the trigger ref (branch-a) → no push, no error
	git := &mockGitRunner{pushChanged: true, listRefs: []string{"refs/heads/other"}}
	notif := &mockNotifier{}
	svc := newTestService([]config.RepoConfig{bidiOverrideRepo()}, makeProviders(), notif, git)

	// G→C (the allowed direction), but branch-a is not present locally
	err := svc.SyncByTarget(context.Background(), "gitlab", "server/example-bidi", EventMeta{Ref: "refs/heads/branch-a"})
	if err != nil {
		t.Fatalf("absent ref should be a no-op (nil), got %v", err)
	}
	if len(git.pushCalls) != 0 {
		t.Errorf("absent ref should not push, got %d push calls", len(git.pushCalls))
	}
	if len(notif.messages) != 0 {
		t.Errorf("absent ref should not notify (no false-alarm), got %+v", notif.messages)
	}
}

// A C→G full sync (no meta.Ref) enumerates through ListRefs and then has to exclude the ref the
// override forbids (branch-a).
func TestDoMirror_FullSync_ExcludesOverriddenRef(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{"refs/heads/branch-a", "refs/heads/branch-b", "refs/tags/v1.0.0"},
	}
	notif := &mockNotifier{}
	svc := newTestService([]config.RepoConfig{bidiOverrideRepo()}, makeProviders(), notif, git)

	// C→G full sync: branch-a excluded, everything else included
	err := svc.Sync(context.Background(), "example-bidi", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if git.listRefsCalls != 1 {
		t.Errorf("expected ListRefs called once, got %d", git.listRefsCalls)
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("expected 1 push, got %d", len(git.pushCalls))
	}
	want := []string{"refs/heads/branch-b", "refs/tags/v1.0.0"}
	if got := pushedRefs(git.pushCalls[0].Specs); !reflect.DeepEqual(got, want) {
		t.Errorf("full-sync ref mismatch: got %v want %v", got, want)
	}
}

// A ListRefs failure is now fail-closed.
//
// It used to fall back to an --all push, but --all cannot carry a per-ref lease, so the
// rewind guard drops out entirely. Skipping one sync is repaired by the next event or by the
// hourly reconcile, but commits lost to an unguarded push are not repaired.
func TestDoMirror_FullSync_ListRefsError_FailsClosed(t *testing.T) {
	git := &mockGitRunner{pushChanged: true, listRefsErr: fmt.Errorf("for-each-ref boom")}
	notif := &mockNotifier{}
	svc := newTestService([]config.RepoConfig{bidiOverrideRepo()}, makeProviders(), notif, git)

	err := svc.Sync(context.Background(), "example-bidi", EventMeta{})
	if err == nil {
		t.Fatal("expected an error when refs cannot be enumerated, got nil")
	}
	if len(git.pushCalls) != 0 {
		t.Errorf("must not push without an enumerated, guarded ref list, got %+v", git.pushCalls)
	}
}

// When a full sync has every ref for this direction excluded by an override, it is a no-op (nil) with no push.
func TestDoMirror_FullSync_AllExcluded_Skips(t *testing.T) {
	git := &mockGitRunner{listRefs: []string{"refs/heads/branch-a"}}
	notif := &mockNotifier{}
	svc := newTestService([]config.RepoConfig{bidiOverrideRepo()}, makeProviders(), notif, git)

	// A C→G full sync where only branch-a exists → everything excluded → no push
	err := svc.Sync(context.Background(), "example-bidi", EventMeta{})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if len(git.pushCalls) != 0 {
		t.Errorf("expected no push, got %d", len(git.pushCalls))
	}
}

// A full sync on a repo with no overrides now enumerates the refs too.
//
// --all was convenient while it lasted, but it leaves nowhere to attach a per-ref lease.
// Enumerating is what gives the hourly reconcile the same rewind guard the event path has —
// and the reconcile is more exposed, not less, because it pushes every branch rather than one.
func TestDoMirror_FullSync_NoOverrides_EnumeratesEveryRef(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.Sync(context.Background(), "bidi-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if git.listRefsCalls != 1 {
		t.Errorf("expected ListRefs called once, got %d", git.listRefsCalls)
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("expected 1 push, got %d", len(git.pushCalls))
	}
	if got := pushedRefs(git.pushCalls[0].Specs); !reflect.DeepEqual(got, defaultMockRefs) {
		t.Errorf("full sync should push every local ref: got %v want %v", got, defaultMockRefs)
	}
}

// A reverse-direction (C→G) branch-a delete event has to be skipped (protecting the branch on the authoritative side).
func TestSyncDelete_RefOverride_SkipsReverseDirection(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService([]config.RepoConfig{bidiOverrideRepo()}, makeProviders(), notif, git)

	// SyncDelete is source→target = codecommit→gitlab = C→G (the override allows G→C only)
	err := svc.SyncDelete(context.Background(), "example-bidi", "branch", "branch-a")
	if err != nil {
		t.Fatalf("expected nil (terminal skip), got %v", err)
	}
	if len(git.deleteRefCalls) != 0 {
		t.Errorf("reverse-direction delete should skip, got %d delete calls", len(git.deleteRefCalls))
	}
	if len(notif.messages) != 0 {
		t.Errorf("skip should not notify, got %+v", notif.messages)
	}
}

// Deleting a ref that matches no override proceeds normally.
func TestSyncDelete_RefOverride_NonMatchingProceeds(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService([]config.RepoConfig{bidiOverrideRepo()}, makeProviders(), notif, git)

	err := svc.SyncDelete(context.Background(), "example-bidi", "branch", "branch-b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.deleteRefCalls) != 1 {
		t.Errorf("non-matching delete should proceed, got %d delete calls", len(git.deleteRefCalls))
	}
}

// --- doDeleteRef idempotency (RefTip) tests ---

// Deleting a ref that is already gone has to end as a successful no-op (no deleteRef, no
// notification → the delete loop terminates).
func TestSyncDelete_AbsentRefIdempotentNoop(t *testing.T) {
	git := &mockGitRunner{refMissing: true}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncDelete(context.Background(), "my-repo", "branch", "feature-branch")
	if err != nil {
		t.Fatalf("expected nil for absent ref (idempotent), got %v", err)
	}
	if len(git.refTipCalls) != 1 {
		t.Fatalf("expected 1 RefTip call, got %d", len(git.refTipCalls))
	}
	if len(git.deleteRefCalls) != 0 {
		t.Errorf("absent ref must not call DeleteRef, got %d", len(git.deleteRefCalls))
	}
	if len(notif.messages) != 0 {
		t.Errorf("idempotent no-op must not notify, got %+v", notif.messages)
	}
}

// A RefTip failure is fail-closed: return the error and send an error notification (which
// surfaces an auth or network problem).
func TestSyncDelete_RefTipErrorFailsClosed(t *testing.T) {
	git := &mockGitRunner{refTipErr: fmt.Errorf("ls-remote auth failed")}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncDelete(context.Background(), "my-repo", "branch", "feature-branch")
	if err == nil {
		t.Fatal("expected error (fail-closed) on RefTip failure")
	}
	if len(git.deleteRefCalls) != 0 {
		t.Errorf("must not delete when RefTip fails, got %d", len(git.deleteRefCalls))
	}
	if len(notif.messages) != 1 || notif.messages[0].Level != "error" {
		t.Errorf("expected one error notification, got %+v", notif.messages)
	}
}

// --- SyncDeleteByTarget tests ---

// A delete webhook from the target (gitlab) side deletes the ref on the source (codecommit) side.
func TestSyncDeleteByTarget_TargetMatchDeletesSource(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	// bidi-repo: source=codecommit-eu, target=gitlab-main, targetPath=team/bidi-repo
	err := svc.SyncDeleteByTarget(context.Background(), "gitlab", "team/bidi-repo", "branch", "feature-x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.deleteRefCalls) != 1 {
		t.Fatalf("expected 1 delete, got %d", len(git.deleteRefCalls))
	}
	// The delete has to land on the source (codecommit) side.
	if !strings.Contains(git.deleteRefCalls[0].URL, "git-codecommit") {
		t.Errorf("expected delete on codecommit source, got URL %q", git.deleteRefCalls[0].URL)
	}
	if len(notif.messages) != 1 || notif.messages[0].Level != "success" {
		t.Errorf("expected success notification, got %+v", notif.messages)
	}
	// Route has to show the target→source direction (gitlab→codecommit).
	if !strings.Contains(notif.messages[0].Body, "Route: gitlab-main/team/bidi-repo → codecommit-eu/bidi-repo") {
		t.Errorf("expected target→source Route in delete notification, got %q", notif.messages[0].Body)
	}
}

// A delete webhook from the source (codecommit) side deletes the ref on the target (gitlab) side.
func TestSyncDeleteByTarget_SourceMatchDeletesTarget(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncDeleteByTarget(context.Background(), "codecommit", "bidi-repo", "branch", "feature-y")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.deleteRefCalls) != 1 {
		t.Fatalf("expected 1 delete, got %d", len(git.deleteRefCalls))
	}
	// The delete has to land on the target (gitlab) side, not on codecommit.
	if strings.Contains(git.deleteRefCalls[0].URL, "git-codecommit") {
		t.Errorf("expected delete on gitlab target, got codecommit URL %q", git.deleteRefCalls[0].URL)
	}
}

// A direction that is not allowed is an error (my-repo is source-to-target, so a delete
// originating from the target is refused).
func TestSyncDeleteByTarget_DirectionNotAllowed(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncDeleteByTarget(context.Background(), "gitlab", "team/my-repo", "branch", "x")
	if err == nil {
		t.Fatal("expected error for disallowed target-to-source delete")
	}
	if len(git.deleteRefCalls) != 0 {
		t.Errorf("must not delete on disallowed direction, got %d", len(git.deleteRefCalls))
	}
}

// A reverse-direction (C→G) branch-a delete is skipped by the override (protecting the
// branch on the authoritative side).
func TestSyncDeleteByTarget_RefOverrideSkipsReverseDirection(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService([]config.RepoConfig{bidiOverrideRepo()}, makeProviders(), notif, git)

	// A branch-a delete originating from codecommit (source) → source→target (C→G), and the
	// override allows G→C only → skip.
	err := svc.SyncDeleteByTarget(context.Background(), "codecommit", "example-bidi", "branch", "branch-a")
	if err != nil {
		t.Fatalf("expected nil (terminal skip), got %v", err)
	}
	if len(git.deleteRefCalls) != 0 {
		t.Errorf("reverse-direction delete should skip, got %d", len(git.deleteRefCalls))
	}
}

// A branch-a delete in the allowed direction (G→C) proceeds.
func TestSyncDeleteByTarget_RefOverrideAllowedDirectionProceeds(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService([]config.RepoConfig{bidiOverrideRepo()}, makeProviders(), notif, git)

	// A branch-a delete originating from gitlab (target) → target→source (G→C), which the
	// override allows → proceed.
	err := svc.SyncDeleteByTarget(context.Background(), "gitlab", "server/example-bidi", "branch", "branch-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.deleteRefCalls) != 1 {
		t.Errorf("allowed-direction delete should proceed, got %d", len(git.deleteRefCalls))
	}
}

// Deleting a ref that matches no override (branch-b) proceeds normally.
func TestSyncDeleteByTarget_RefOverrideNonMatchingProceeds(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService([]config.RepoConfig{bidiOverrideRepo()}, makeProviders(), notif, git)

	err := svc.SyncDeleteByTarget(context.Background(), "codecommit", "example-bidi", "branch", "branch-b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.deleteRefCalls) != 1 {
		t.Errorf("non-matching delete should proceed, got %d", len(git.deleteRefCalls))
	}
}

// On the SyncDeleteByTarget path too, a ref that is already gone has to be an idempotent
// no-op (the guard at the webhook entry point).
func TestSyncDeleteByTarget_AbsentRefIdempotentNoop(t *testing.T) {
	git := &mockGitRunner{refMissing: true}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncDeleteByTarget(context.Background(), "gitlab", "team/bidi-repo", "branch", "gone")
	if err != nil {
		t.Fatalf("expected nil for absent ref (idempotent), got %v", err)
	}
	if len(git.deleteRefCalls) != 0 {
		t.Errorf("absent ref must not call DeleteRef, got %d", len(git.deleteRefCalls))
	}
	if len(notif.messages) != 0 {
		t.Errorf("idempotent no-op must not notify, got %+v", notif.messages)
	}
}

// No matching repo is an error.
func TestSyncDeleteByTarget_NoMatch(t *testing.T) {
	git := &mockGitRunner{}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	err := svc.SyncDeleteByTarget(context.Background(), "gitlab", "nonexistent/repo", "branch", "main")
	if err == nil {
		t.Fatal("expected error for no matching repo")
	}
}

// Regression guard: even with no ref_overrides, a single-ref event pushes only that ref.
//
// This assertion used to be its own inverse — a no-override repo pushed --all
// even for a single-ref event — and that is exactly what destroyed a commit on
// 2026-08-10. A demo-repo event for version/4.2.0 pushed every ref, including
// master-b, force-writing it from a source that had not yet seen the commit
// pushed there 49 seconds earlier. Narrowing to the triggered ref is what makes
// one ref's event unable to touch another ref.
func TestDoMirror_NoOverrides_SingleRefEvent_PushesOnlyThatRef(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{"refs/heads/main", "refs/heads/master-b", "refs/tags/v1"},
	}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	// my-repo has no ref_overrides and meta.Ref is set → the push is scoped to that ref.
	err := svc.Sync(context.Background(), "my-repo", EventMeta{Ref: "refs/heads/main"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"refs/heads/main"}
	got := pushedRefs(git.pushCalls[0].Specs)
	if len(git.pushCalls) != 1 || !reflect.DeepEqual(got, want) {
		t.Fatalf("expected a push scoped to the triggered ref %v, got %+v", want, git.pushCalls)
	}
	// The point of the change: no other ref may appear in the pushed list.
	for _, ref := range got {
		if strings.Contains(ref, "master-b") {
			t.Errorf("an event for main must never push master-b, got %q", ref)
		}
	}
}

// The incident shape, end to end: an event naming one branch must leave every
// other branch untouched, on a repo that declares no ref_overrides.
func TestDoMirror_NoOverrides_EventForOneBranchCannotRewriteAnother(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{"refs/heads/version/4.2.0", "refs/heads/master-b"},
	}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: "refs/heads/version/4.2.0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("expected exactly one push, got %d", len(git.pushCalls))
	}
	want := []string{"refs/heads/version/4.2.0"}
	if got := pushedRefs(git.pushCalls[0].Specs); !reflect.DeepEqual(got, want) {
		t.Errorf("pushed refs = %v, want only the triggered ref %v", got, want)
	}
}

// The safety net must not have been narrowed with it: the reconcile cron sends
// no ref, and that still has to push everything, or refs whose own event was
// missed would never be reconciled at all.
func TestDoMirror_NoOverrides_CronFullSyncStillPushesEverything(t *testing.T) {
	git := &mockGitRunner{pushChanged: true, listRefs: []string{"refs/heads/main"}}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	err := svc.Retry(context.Background(), "bidi-repo", "target-to-source", EventMeta{Source: SourceCron})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"refs/heads/main"}
	if len(git.pushCalls) != 1 || !reflect.DeepEqual(pushedRefs(git.pushCalls[0].Specs), want) {
		t.Errorf("a ref-less reconcile must still push every ref, got %+v", git.pushCalls)
	}
	if git.listRefsCalls != 1 {
		t.Errorf("a ref-less sync enumerates once so each ref gets its own lease, got %d", git.listRefsCalls)
	}
}

// A tag event narrows the same way a branch event does.
func TestDoMirror_NoOverrides_TagEventPushesOnlyThatTag(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{"refs/heads/master-b", "refs/tags/Build-2312"},
	}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: "refs/tags/Build-2312"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"refs/tags/Build-2312"}
	if len(git.pushCalls) != 1 || !reflect.DeepEqual(pushedRefs(git.pushCalls[0].Specs), want) {
		t.Errorf("pushed refs = %+v, want only the triggered tag %v", git.pushCalls, want)
	}
}

// ListRefs failure now refuses the push instead of falling back to --all.
//
// The old fallback traded a stalled sync for an unguarded one, which is the
// wrong way round: a skipped sync is repaired by the next event or the hourly
// reconcile, and a commit overwritten without a lease is not repaired at all.
func TestDoMirror_NoOverrides_ListRefsFailureRefusesToPush(t *testing.T) {
	git := &mockGitRunner{pushChanged: true, listRefsErr: errors.New("boom")}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: "refs/heads/main"})
	if err == nil {
		t.Fatal("expected an error when refs cannot be enumerated, got nil")
	}
	if len(git.pushCalls) != 0 {
		t.Errorf("expected no push at all, got %+v", git.pushCalls)
	}
}

// --- post-mutex-acquisition timeout / timeout diagnostics ---

// TestDoMirror_AppliesTimeoutAfterLock verifies that doMirror builds a git-op ctx from
// timeout_seconds after acquiring the mutex. Even on the SQS Sync path, whose parent ctx has
// no deadline, the push ctx has to carry one (the timeout runs from the lock onward, not
// from the moment the event entered the queue).
func TestDoMirror_AppliesTimeoutAfterLock(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	svc := newTestService(nil, makeProviders(), &mockNotifier{}, git)
	svc.timeoutSeconds = 120

	repoCfg := config.RepoConfig{Name: "t"}
	err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "my-repo", "gitlab-main", "team/my-repo", EventMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !git.pushCtxHasDeadline {
		t.Fatal("push ctx should carry a deadline derived from timeoutSeconds (applied after lock)")
	}
	// deadline ≈ now + 120s. The parent (Background) has no deadline, so this deadline can
	// only have come from doMirror's withGitTimeout. Checked against a generous window.
	remaining := time.Until(git.pushCtxDeadline)
	if remaining <= 60*time.Second || remaining > 120*time.Second {
		t.Errorf("expected a fresh ~120s budget, got remaining=%v", remaining)
	}
}

// TestRunPush_DeadlineExceededAnnotatesError verifies that when git dies because the ctx
// deadline was exceeded, runPush appends "timed out (deadline exceeded)" to the error. That
// wording is what lets a notification tell a timeout kill apart from an OOM (a bare
// "signal: killed").
func TestRunPush_DeadlineExceededAnnotatesError(t *testing.T) {
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/file.txt", "hello")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "init")

	mirrorDir := t.TempDir() + "/mirror.git"
	if err := (&defaultGitRunner{}).CloneMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	tgtDir := t.TempDir()
	runGit(t, tgtDir, "init", "--bare")

	// An already-expired deadline → exec.CommandContext kills the child immediately.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := runPush(ctx, mirrorDir, provider.Remote{}, "push branches", "--all", tgtDir)
	if err == nil {
		t.Fatal("expected error from expired deadline")
	}
	if !strings.Contains(err.Error(), "timed out (deadline exceeded)") {
		t.Errorf("expected timeout annotation in error, got %q", err.Error())
	}
}

// TestRunPush_NonTimeoutErrorNotAnnotated verifies that a failure which is not a timeout (a
// bad destination) does not get the "timed out" wording attached (guarding against a false
// positive).
func TestRunPush_NonTimeoutErrorNotAnnotated(t *testing.T) {
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/file.txt", "hello")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "init")

	mirrorDir := t.TempDir() + "/mirror.git"
	if err := (&defaultGitRunner{}).CloneMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// Push to a local path that does not exist → git exits non-zero, and not from a timeout.
	_, err := runPush(context.Background(), mirrorDir, provider.Remote{}, "push branches", "--all", t.TempDir()+"/does-not-exist.git")
	if err == nil {
		t.Fatal("expected error pushing to a nonexistent target")
	}
	if strings.Contains(err.Error(), "timed out (deadline exceeded)") {
		t.Errorf("non-timeout error must not carry timeout annotation, got %q", err.Error())
	}
}

// TestDoDeleteRef_AppliesTimeoutAfterLock is the mirror image of the doMirror case: it
// verifies that doDeleteRef also applies the git-op timeout after acquiring the mutex. The
// parent ctx (Background) has no deadline, so the deadline RefTip receives can only come
// from withGitTimeout.
func TestDoDeleteRef_AppliesTimeoutAfterLock(t *testing.T) {
	git := &mockGitRunner{}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	err := svc.SyncDelete(context.Background(), "my-repo", "branch", "feature-branch")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !git.refTipCtxHasDeadline {
		t.Error("RefTip ctx should carry a deadline derived from timeoutSeconds (applied after lock)")
	}
}

// TestRunPush_ParentCancelNotAnnotated verifies that when git dies from a parent cancel (a
// shutdown, say — not DeadlineExceeded) the "timed out" label is not attached — it pins down
// the promise runPush's comment makes, that a timeout kill is told apart from a shutdown or
// an external kill.
func TestRunPush_ParentCancelNotAnnotated(t *testing.T) {
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/file.txt", "hello")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "init")

	mirrorDir := t.TempDir() + "/mirror.git"
	if err := (&defaultGitRunner{}).CloneMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	tgtDir := t.TempDir()
	runGit(t, tgtDir, "init", "--bare")

	// An already-canceled ctx, not an expired deadline → ctx.Err() is context.Canceled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runPush(ctx, mirrorDir, provider.Remote{}, "push branches", "--all", tgtDir)
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
	if strings.Contains(err.Error(), "timed out (deadline exceeded)") {
		t.Errorf("parent-cancel must not be labeled as timeout, got %q", err.Error())
	}
}

// --- restore git primitives (real git, not the mock) ---

// CreateRef must create only. The first version used a plain non-force push,
// which rejects non-fast-forward updates but happily MOVES a ref whose tip is
// an ancestor of what is being pushed — so a branch someone re-created at an
// older commit after the delete would have been silently advanced onto the
// restored tip. The mock cannot see the refspec, so this is the only place that
// invariant is actually checked.
func TestDefaultGitRunner_CreateRef(t *testing.T) {
	runner := &defaultGitRunner{}
	ctx := context.Background()

	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/f.txt", "a")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "A")
	writeFile(t, srcDir+"/f.txt", "b")
	runGit(t, srcDir, "commit", "-am", "B")

	tip := strings.TrimSpace(runGitOut(t, srcDir, "rev-parse", "HEAD"))
	parent := strings.TrimSpace(runGitOut(t, srcDir, "rev-parse", "HEAD~1"))

	remote := t.TempDir() + "/remote.git"
	runGit(t, "", "init", "--bare", remote)

	t.Run("creates an absent ref", func(t *testing.T) {
		if err := runner.CreateRef(ctx, srcDir, localRemote(remote), tip, "refs/heads/restored"); err != nil {
			t.Fatalf("CreateRef on an absent ref failed: %v", err)
		}
		got, err := runner.RefTip(ctx, localRemote(remote), "branch", "restored")
		if err != nil || got != tip {
			t.Errorf("remote tip = %q (err %v), want %q", got, err, tip)
		}
	})

	// A ref sitting at exactly the value being restored is not a conflict —
	// the destination is already in the state the restore wants, and git makes
	// it a no-op. doRestoreRef short-circuits this earlier anyway.
	t.Run("is a no-op when the ref is already exactly there", func(t *testing.T) {
		if err := runner.CreateRef(ctx, srcDir, localRemote(remote), tip, "refs/heads/restored"); err != nil {
			t.Errorf("re-creating a ref at its current value should be harmless, got %v", err)
		}
	})

	// The case a plain non-force push allowed, and the reason this test exists:
	// the remote holds an ANCESTOR, so a normal push would fast-forward it and
	// silently move a branch somebody re-created after the delete.
	t.Run("refuses a fast-forward over an existing ref", func(t *testing.T) {
		runGit(t, srcDir, "push", remote, parent+":refs/heads/ff")

		if err := runner.CreateRef(ctx, srcDir, localRemote(remote), tip, "refs/heads/ff"); err == nil {
			t.Error("CreateRef advanced an existing ref by fast-forward, want a refusal")
		}
		if got, _ := runner.RefTip(ctx, localRemote(remote), "branch", "ff"); got != parent {
			t.Errorf("remote ref moved to %q, want it left at %q", got, parent)
		}
	})

	t.Run("refuses a divergent existing ref", func(t *testing.T) {
		runGit(t, srcDir, "checkout", "-q", "-b", "other", parent)
		writeFile(t, srcDir+"/f.txt", "c")
		runGit(t, srcDir, "commit", "-qam", "C")
		divergent := strings.TrimSpace(runGitOut(t, srcDir, "rev-parse", "HEAD"))
		runGit(t, srcDir, "push", remote, divergent+":refs/heads/div")

		if err := runner.CreateRef(ctx, srcDir, localRemote(remote), tip, "refs/heads/div"); err == nil {
			t.Error("CreateRef overwrote a divergent ref, want a refusal")
		}
		if got, _ := runner.RefTip(ctx, localRemote(remote), "branch", "div"); got != divergent {
			t.Errorf("remote ref moved to %q, want it left at %q", got, divergent)
		}
	})
}

func TestDefaultGitRunner_HasObjectAndEnsureBareDir(t *testing.T) {
	runner := &defaultGitRunner{}
	ctx := context.Background()

	dir := t.TempDir() + "/fresh.git"
	if err := runner.EnsureBareDir(ctx, dir); err != nil {
		t.Fatalf("EnsureBareDir: %v", err)
	}
	// Idempotent: the live mirror cache is an existing repo and must survive it.
	if err := runner.EnsureBareDir(ctx, dir); err != nil {
		t.Fatalf("EnsureBareDir is not idempotent: %v", err)
	}
	if runner.HasObject(ctx, dir, "1111111111111111111111111111111111111111") {
		t.Error("HasObject reported a commit an empty repo cannot have")
	}

	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/f.txt", "a")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "A")
	sha := strings.TrimSpace(runGitOut(t, srcDir, "rev-parse", "HEAD"))

	if !runner.HasObject(ctx, srcDir, sha) {
		t.Errorf("HasObject(%s) = false in the repo that just made it", sha)
	}
	// The restore fallback: pull one unreachable-by-name object across.
	if err := runner.FetchObject(ctx, dir, localRemote(srcDir), sha); err != nil {
		t.Fatalf("FetchObject: %v", err)
	}
	if !runner.HasObject(ctx, dir, sha) {
		t.Error("FetchObject reported success but the object is not there")
	}
}

// --- mirrorDirFor tests ---

// Two providers of the same type (gitlab↔gitlab) have to use a different cache per
// direction. Naming the directory after the type makes both directions share one, so they
// fetch --prune over each other and keep deleting and resurrecting the other side's refs —
// this test blocks that regression.
func TestMirrorDirFor_SameTypeProvidersGetSeparateDirs(t *testing.T) {
	providers := map[string]provider.Provider{
		"gitlab-main": NewGitLab(config.ProviderConfig{Type: "gitlab", BaseURL: "https://gitlab.example.com"}),
		"gitlab-old":  NewGitLab(config.ProviderConfig{Type: "gitlab", BaseURL: "https://gitlab-old.example.com"}),
	}
	svc := newTestService(nil, providers, &mockNotifier{}, &mockGitRunner{})
	svc.workDir = t.TempDir()

	main := svc.mirrorDirFor("repo", "gitlab-main")
	old := svc.mirrorDirFor("repo", "gitlab-old")

	if main == old {
		t.Fatalf("both directions resolved to the same mirror cache: %s", main)
	}
	if want := svc.workDir + "/repo-gitlab-main.git"; main != want {
		t.Errorf("gitlab-main dir = %q, want %q", main, want)
	}
	if want := svc.workDir + "/repo-gitlab-old.git"; old != want {
		t.Errorf("gitlab-old dir = %q, want %q", old, want)
	}
}

// When an older type-named cache is present, it is renamed to the new name and carried over.
// Without that migration, every direction runs a full clone again right after a deploy and
// the old directory stays sitting on the PVC.
func TestMirrorDirFor_MigratesLegacyTypeNamedCache(t *testing.T) {
	svc := newTestService(nil, makeProviders(), &mockNotifier{}, &mockGitRunner{})
	svc.workDir = t.TempDir()

	legacy := svc.workDir + "/repo-codecommit.git"
	mkBareHEAD(t, legacy)
	// A marker that shows the same directory was moved rather than a new one created.
	if err := os.WriteFile(legacy+"/marker", []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := svc.mirrorDirFor("repo", "codecommit-eu")

	want := svc.workDir + "/repo-codecommit-eu.git"
	if got != want {
		t.Fatalf("mirrorDirFor = %q, want %q", got, want)
	}
	if isGitDir(legacy) {
		t.Error("legacy cache is still in place; it should have been renamed")
	}
	if !isGitDir(want) {
		t.Fatal("migrated cache is not a git dir")
	}
	if _, err := os.Stat(want + "/marker"); err != nil {
		t.Errorf("cache contents did not survive the migration: %v", err)
	}
}

// When the new-name cache already exists, the legacy one is left alone — the migration
// happens once, and an old directory that happens to still be lying around must not
// overwrite the current cache.
func TestMirrorDirFor_ExistingNewNameWins(t *testing.T) {
	svc := newTestService(nil, makeProviders(), &mockNotifier{}, &mockGitRunner{})
	svc.workDir = t.TempDir()

	current := svc.workDir + "/repo-codecommit-eu.git"
	legacy := svc.workDir + "/repo-codecommit.git"
	mkBareHEAD(t, current)
	mkBareHEAD(t, legacy)
	if err := os.WriteFile(current+"/marker", []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := svc.mirrorDirFor("repo", "codecommit-eu"); got != current {
		t.Fatalf("mirrorDirFor = %q, want %q", got, current)
	}
	if b, err := os.ReadFile(current + "/marker"); err != nil || string(b) != "current" {
		t.Errorf("current cache was overwritten: b=%q err=%v", b, err)
	}
	if !isGitDir(legacy) {
		t.Error("legacy cache should have been left alone")
	}
}

// With no cache at all, only the new path is returned (the caller clones).
func TestMirrorDirFor_NoCacheReturnsNewPath(t *testing.T) {
	svc := newTestService(nil, makeProviders(), &mockNotifier{}, &mockGitRunner{})
	svc.workDir = t.TempDir()

	got := svc.mirrorDirFor("repo", "gitlab-main")

	if want := svc.workDir + "/repo-gitlab-main.git"; got != want {
		t.Fatalf("mirrorDirFor = %q, want %q", got, want)
	}
	if isGitDir(got) {
		t.Error("no cache should have been created")
	}
}

// For an unknown provider name there is no way to know what to migrate from, so only the
// path is returned and the legacy directory is left in place. (The caller fails on the very
// next line when it cannot find the provider.)
func TestMirrorDirFor_UnknownProviderLeavesLegacyAlone(t *testing.T) {
	svc := newTestService(nil, makeProviders(), &mockNotifier{}, &mockGitRunner{})
	svc.workDir = t.TempDir()

	legacy := svc.workDir + "/repo-codecommit.git"
	mkBareHEAD(t, legacy)

	got := svc.mirrorDirFor("repo", "nope")

	if want := svc.workDir + "/repo-nope.git"; got != want {
		t.Fatalf("mirrorDirFor = %q, want %q", got, want)
	}
	if !isGitDir(legacy) {
		t.Error("legacy cache should have been left alone for an unknown provider")
	}
}

// Whether doMirror carries the legacy cache over and goes straight to an incremental fetch
// with no full clone. The whole point of the migration is avoiding a re-clone, so this is
// checked on the call path rather than at the helper level.
func TestDoMirror_MigratesLegacyCacheAndFetchesIncrementally(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	svc := newTestService(nil, makeProviders(), &mockNotifier{}, git)
	svc.workDir = t.TempDir()

	legacy := svc.workDir + "/test-repo-codecommit.git"
	mkBareHEAD(t, legacy)

	repoCfg := config.RepoConfig{Name: "test-repo"}
	if err := svc.doMirror(context.Background(), repoCfg, "codecommit-eu", "my-repo", "gitlab-main", "team/my-repo", EventMeta{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(git.cloneCalls) != 0 {
		t.Errorf("expected 0 clone calls after migration, got %d", len(git.cloneCalls))
	}
	if len(git.fetchCalls) != 1 {
		t.Fatalf("expected 1 fetch call, got %d", len(git.fetchCalls))
	}
	want := svc.workDir + "/test-repo-codecommit-eu.git"
	if git.fetchCalls[0].Dir != want {
		t.Errorf("fetched into %q, want %q", git.fetchCalls[0].Dir, want)
	}
	if isGitDir(legacy) {
		t.Error("legacy cache is still in place; it should have been renamed")
	}
}

// A failed rename must not stop the sync — the migration is only an optimization for reusing
// the cache, so when it fails, cloning under the new name is all that is needed. Here a plain
// file is put where the new path goes, which makes the rename fail with ENOTDIR.
func TestMirrorDirFor_RenameFailureFallsBackToNewPath(t *testing.T) {
	svc := newTestService(nil, makeProviders(), &mockNotifier{}, &mockGitRunner{})
	svc.workDir = t.TempDir()

	legacy := svc.workDir + "/repo-codecommit.git"
	mkBareHEAD(t, legacy)
	blocker := svc.workDir + "/repo-codecommit-eu.git"
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := svc.mirrorDirFor("repo", "codecommit-eu")

	if got != blocker {
		t.Fatalf("mirrorDirFor = %q, want %q", got, blocker)
	}
	if !isGitDir(legacy) {
		t.Error("legacy cache should be untouched when the rename fails")
	}
}

// --- per-instance dispatch (narrowing by provider name) ---

// twoInstanceRepos is a configuration where two instances of the same type (gitlab) use the
// same path. It is the real shape of the attempt to move team/test-repo onto a bidirectional
// mirror: (type, path) does not tell them apart, only the provider name does.
func twoInstanceRepos() []config.RepoConfig {
	return []config.RepoConfig{
		{
			Name:       "old-to-main",
			Source:     "gitlab-old",
			Target:     "gitlab-main",
			SourcePath: "team/test-repo",
			TargetPath: "team/test-repo",
			Direction:  "bidirectional",
		},
	}
}

func twoInstanceProviders() map[string]provider.Provider {
	return map[string]provider.Provider{
		"gitlab-main": NewGitLab(config.ProviderConfig{
			Type:        "gitlab",
			BaseURL:     "https://gitlab.example.com",
			Credentials: map[string]string{"token": "glpat-main"},
		}),
		"gitlab-old": NewGitLab(config.ProviderConfig{
			Type:        "gitlab",
			BaseURL:     "https://gitlab-old.example.com",
			Credentials: map[string]string{"token": "glpat-old"},
		}),
	}
}

// TestSyncByTarget_SameTypeSamePath_SplitsByProviderName is the core of Phase 1. Even when
// two instances send events for the same path, narrowing by provider name has to split the
// mirror direction into opposite ones.
func TestSyncByTarget_SameTypeSamePath_SplitsByProviderName(t *testing.T) {
	tests := []struct {
		name        string
		providerKey string
		wantPushTo  string // the host pushed to — the evidence of the direction
	}{
		// An event from the target (gitlab-main) has to push to the source (gitlab-old).
		{name: "event from target instance", providerKey: "gitlab-main", wantPushTo: "gitlab-old.example.com"},
		// An event from the source (gitlab-old) has to push to the target (gitlab-main).
		{name: "event from source instance", providerKey: "gitlab-old", wantPushTo: "gitlab.example.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			git := &mockGitRunner{}
			svc := newTestService(twoInstanceRepos(), twoInstanceProviders(), &mockNotifier{}, git)

			if err := svc.SyncByTarget(context.Background(), tc.providerKey, "team/test-repo", EventMeta{}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(git.pushCalls) != 1 {
				t.Fatalf("expected 1 push call, got %d", len(git.pushCalls))
			}
			if !strings.Contains(git.pushCalls[0].URL, tc.wantPushTo) {
				t.Errorf("pushed to %q, want a URL on %q — the direction is inverted", git.pushCalls[0].URL, tc.wantPushTo)
			}
		})
	}
}

// TestSyncDeleteByTarget_SameTypeSamePath_SplitsByProviderName checks that delete propagation
// splits the same way. push and delete have separate matching code, so both have to be pinned.
func TestSyncDeleteByTarget_SameTypeSamePath_SplitsByProviderName(t *testing.T) {
	tests := []struct {
		name         string
		providerKey  string
		wantDeleteOn string
	}{
		{name: "event from target instance", providerKey: "gitlab-main", wantDeleteOn: "gitlab-old.example.com"},
		{name: "event from source instance", providerKey: "gitlab-old", wantDeleteOn: "gitlab.example.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			git := &mockGitRunner{}
			svc := newTestService(twoInstanceRepos(), twoInstanceProviders(), &mockNotifier{}, git)

			if err := svc.SyncDeleteByTarget(context.Background(), tc.providerKey, "team/test-repo", "branch", "feature/x"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(git.deleteRefCalls) != 1 {
				t.Fatalf("expected 1 delete call, got %d", len(git.deleteRefCalls))
			}
			if !strings.Contains(git.deleteRefCalls[0].URL, tc.wantDeleteOn) {
				t.Errorf("deleted on %q, want %q — the direction is inverted", git.deleteRefCalls[0].URL, tc.wantDeleteOn)
			}
		})
	}
}

// TestSyncByTarget_TypeKeyStillMatches guards against a regression. When the host cannot be
// pinned down and the bare type string comes in, it has to behave exactly as it did before
// the narrowing existed: first match wins.
func TestSyncByTarget_TypeKeyStillMatches(t *testing.T) {
	git := &mockGitRunner{}
	svc := newTestService(twoInstanceRepos(), twoInstanceProviders(), &mockNotifier{}, git)

	if err := svc.SyncByTarget(context.Background(), "gitlab", "team/test-repo", EventMeta{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("expected 1 push call, got %d", len(git.pushCalls))
	}
	// The target is matched before the source, so it lands on target→source (the old behavior).
	if !strings.Contains(git.pushCalls[0].URL, "gitlab-old.example.com") {
		t.Errorf("type fallback pushed to %q, want the first match (target→source)", git.pushCalls[0].URL)
	}
}

// TestProviderMatches pins the matching rule itself down tightly. Whether it is a name or a
// type, it must be true only on an exact match — if it spreads to partial matches, events
// leak into the wrong repo.
func TestProviderMatches(t *testing.T) {
	tests := []struct {
		name, configuredName, providerType, key string
		want                                    bool
	}{
		{"provider name", "gitlab-old", "gitlab", "gitlab-old", true},
		{"provider type", "gitlab-old", "gitlab", "gitlab", true},
		{"other instance name", "gitlab-old", "gitlab", "gitlab-main", false},
		{"other type", "gitlab-old", "gitlab", "github", false},
		{"prefix is not a match", "gitlab-old", "gitlab", "gitlab-o", false},
		{"empty key", "gitlab-old", "gitlab", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerMatches(tc.configuredName, tc.providerType, tc.key); got != tc.want {
				t.Errorf("providerMatches(%q, %q, %q) = %v, want %v", tc.configuredName, tc.providerType, tc.key, got, tc.want)
			}
		})
	}
}

// --- credentials never reach the command line ---

// Credentials leave only through environment variables, never as git arguments. That is the
// whole point of this design, so it is nailed down here — the moment they ride in an argument,
// the token is readable from `ps` on the same node, and it also bleeds into the error message
// whenever git quotes the failing command.
func TestNewGitRemoteCmd_TokenIsNotOnTheCommandLine(t *testing.T) {
	rem := provider.Remote{
		URL:      "https://gitlab.example.com/team/repo.git",
		Username: "oauth2",
		Password: "glpat-supersecret",
	}
	cmd := newGitRemoteCmd(context.Background(), rem, "ls-remote", rem.URL)

	for _, arg := range cmd.Args {
		if strings.Contains(arg, rem.Password) {
			t.Fatalf("token leaked into the command line: %q", cmd.Args)
		}
	}

	env := envMap(t, cmd.Env)
	if env["GIT_BRIDGE_ASKPASS_PASSWORD"] != rem.Password {
		t.Errorf("password not handed to the helper: %q", env["GIT_BRIDGE_ASKPASS_PASSWORD"])
	}
	if env["GIT_BRIDGE_ASKPASS_USERNAME"] != rem.Username {
		t.Errorf("username not handed to the helper: %q", env["GIT_BRIDGE_ASKPASS_USERNAME"])
	}
	if env["GIT_BRIDGE_ASKPASS"] != "1" {
		t.Error("the helper marker is unset, so the re-exec would start the service instead")
	}
	if env["GIT_ASKPASS"] == "" {
		t.Error("GIT_ASKPASS is unset, so git would never ask the helper")
	}
	// The helper reads the prompt string to decide which value to hand back, so the locale has to be pinned.
	if env["LC_ALL"] != "C" {
		t.Errorf("LC_ALL = %q, want C so the prompt stays parseable", env["LC_ALL"])
	}
	// In a pod with no tty, a git prompt turns into a timeout rather than a failure.
	if env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q, want 0", env["GIT_TERMINAL_PROMPT"])
	}
}

// A remote with no credentials gets no askpass attached. Attaching one makes the helper
// answer with empty values, turning an unauthenticated access into presenting empty credentials.
func TestNewGitRemoteCmd_NoCredentialsMeansNoAskpass(t *testing.T) {
	cmd := newGitRemoteCmd(context.Background(), provider.Remote{URL: "/tmp/local.git"}, "ls-remote", "/tmp/local.git")

	env := envMap(t, cmd.Env)
	if _, ok := env["GIT_ASKPASS"]; ok {
		t.Error("GIT_ASKPASS was set for a remote that needs no credentials")
	}
	if env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q, want 0 even without credentials", env["GIT_TERMINAL_PROMPT"])
	}
}

// Our helper wins even when a GIT_ASKPASS came in from the outside. exec takes the last value
// for a duplicate key, so appending after os.Environ() is enough — if that order is reversed,
// someone else's helper takes the place of our token.
func TestGitRemoteEnv_OverridesInheritedAskpass(t *testing.T) {
	t.Setenv("GIT_ASKPASS", "/usr/bin/inherited-askpass")

	env := envMap(t, gitRemoteEnv(provider.Remote{
		URL:      "https://gitlab.example.com/team/repo.git",
		Username: "oauth2",
		Password: "tok",
	}))
	if env["GIT_ASKPASS"] == "/usr/bin/inherited-askpass" {
		t.Error("the inherited GIT_ASKPASS survived; ours must be last")
	}
}

// helperIsExecutable checks whether the file GIT_ASKPASS points at is in a state git can
// actually execute. The helper works by re-executing itself, so if that path is gone or has
// lost its executable bit, the only symptom is "authentication does not work".
func helperIsExecutable(t *testing.T, env []string) bool {
	t.Helper()
	path := envMap(t, env)["GIT_ASKPASS"]
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode()&0o111 != 0
}

// envMap folds the environment into a map following the exec rule: for a duplicate key, the last value wins.
func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		m[k] = v
	}
	return m
}

// --- SanitizeCache ---

// A cache left behind by an older build still has a live token in its remote.origin.url.
// Stripping the token out of the code leaves that file on the PVC exactly as it was, so it
// has to be scrubbed here, the first time the cache is touched.
func TestDefaultGitRunner_SanitizeCache_RemovesLegacyCredentials(t *testing.T) {
	dir := t.TempDir() + "/mirror.git"
	runner := &defaultGitRunner{}
	if err := runner.EnsureBareDir(context.Background(), dir); err != nil {
		t.Fatalf("EnsureBareDir failed: %v", err)
	}
	runGit(t, dir, "config", "remote.origin.url", "https://oauth2:glpat-legacy@gitlab.example.com/team/repo.git")

	clean := provider.Remote{
		URL:      "https://gitlab.example.com/team/repo.git",
		Username: "oauth2",
		Password: "glpat-legacy",
	}
	scrubbed, err := runner.SanitizeCache(context.Background(), dir, clean)
	if err != nil {
		t.Fatalf("SanitizeCache failed: %v", err)
	}
	if !scrubbed {
		t.Error("scrubbing a credentialed cache must report that it did something")
	}
	if got := gitOut(t, dir, "config", "--get", "remote.origin.url"); got != clean.URL {
		t.Errorf("remote.origin.url = %q, want %q", got, clean.URL)
	}
}

// A cache that is already clean is left untouched and is not reported as scrubbed — if every
// fetch logs "scrubbed", the one real scrub becomes impossible to spot.
func TestDefaultGitRunner_SanitizeCache_CleanCacheIsUntouched(t *testing.T) {
	dir := t.TempDir() + "/mirror.git"
	runner := &defaultGitRunner{}
	if err := runner.EnsureBareDir(context.Background(), dir); err != nil {
		t.Fatalf("EnsureBareDir failed: %v", err)
	}
	clean := provider.Remote{URL: "https://gitlab.example.com/team/repo.git", Username: "oauth2", Password: "tok"}
	runGit(t, dir, "config", "remote.origin.url", clean.URL)

	scrubbed, err := runner.SanitizeCache(context.Background(), dir, clean)
	if err != nil {
		t.Fatalf("SanitizeCache failed: %v", err)
	}
	if scrubbed {
		t.Error("a clean cache must not report a scrub")
	}
}

// When only the address changed (a base_url change, say), it is brought into line but not reported as a scrub.
func TestDefaultGitRunner_SanitizeCache_AddressChangeIsSyncedQuietly(t *testing.T) {
	dir := t.TempDir() + "/mirror.git"
	runner := &defaultGitRunner{}
	if err := runner.EnsureBareDir(context.Background(), dir); err != nil {
		t.Fatalf("EnsureBareDir failed: %v", err)
	}
	runGit(t, dir, "config", "remote.origin.url", "https://old.example.com/team/repo.git")

	clean := provider.Remote{URL: "https://gitlab.example.com/team/repo.git"}
	scrubbed, err := runner.SanitizeCache(context.Background(), dir, clean)
	if err != nil {
		t.Fatalf("SanitizeCache failed: %v", err)
	}
	if scrubbed {
		t.Error("an address change is not a credential scrub")
	}
	if got := gitOut(t, dir, "config", "--get", "remote.origin.url"); got != clean.URL {
		t.Errorf("remote.origin.url = %q, want it synced to %q", got, clean.URL)
	}
}

// A repository with no origin (a restore directory EnsureBareDir created) has to pass
// through quietly too. git reports a missing key with nothing but exit 1, so reading that as
// an error makes the whole restore path fail.
func TestDefaultGitRunner_SanitizeCache_NoOriginIsNotAnError(t *testing.T) {
	dir := t.TempDir() + "/bare.git"
	runner := &defaultGitRunner{}
	if err := runner.EnsureBareDir(context.Background(), dir); err != nil {
		t.Fatalf("EnsureBareDir failed: %v", err)
	}

	scrubbed, err := runner.SanitizeCache(context.Background(), dir, provider.Remote{URL: "https://gitlab.example.com/x.git"})
	if err != nil {
		t.Fatalf("a repository without an origin must not be an error: %v", err)
	}
	if scrubbed {
		t.Error("nothing was there to scrub")
	}
}

// A directory that does not exist is a real error — swallowing a missing key (exit 1) and
// git not being able to enter at all (exit 128) together makes a vanished cache slip through
// quietly as "nothing to scrub".
func TestDefaultGitRunner_SanitizeCache_UnreachableDirIsAnError(t *testing.T) {
	runner := &defaultGitRunner{}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := runner.SanitizeCache(context.Background(), missing, provider.Remote{URL: "https://x.test/y.git"}); err == nil {
		t.Fatal("expected an error for a directory git cannot enter")
	}
}

// A scrub has to be attempted every time an existing cache is met. The path taken right
// after a clone is already clean, so it is not a target.
func TestEnsureMirror_ScrubsExistingCacheBeforeFetching(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)
	mirrorDir := t.TempDir() + "/mirror.git"
	if err := (&defaultGitRunner{}).EnsureBareDir(context.Background(), mirrorDir); err != nil {
		t.Fatalf("EnsureBareDir failed: %v", err)
	}

	err := svc.ensureMirror(context.Background(), defaultRepos()[0],
		provider.Remote{URL: "https://gitlab.example.com/team/repo.git"},
		mirrorDir, "route", EventMeta{}, slog.Default())
	if err != nil {
		t.Fatalf("ensureMirror failed: %v", err)
	}
	if len(git.sanitizeCalls) != 1 || git.sanitizeCalls[0] != mirrorDir {
		t.Errorf("SanitizeCache calls = %v, want exactly [%s]", git.sanitizeCalls, mirrorDir)
	}
}

// A failed scrub is not a failed mirror. What is left behind sits on this pod's volume, not
// on the destination, and stopping here would bring the whole sync to a halt over a single
// cache that needed washing.
func TestEnsureMirror_ScrubFailureDoesNotStopTheSync(t *testing.T) {
	git := &mockGitRunner{sanitizeErr: errors.New("config is read-only")}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)
	mirrorDir := t.TempDir() + "/mirror.git"
	if err := (&defaultGitRunner{}).EnsureBareDir(context.Background(), mirrorDir); err != nil {
		t.Fatalf("EnsureBareDir failed: %v", err)
	}

	err := svc.ensureMirror(context.Background(), defaultRepos()[0],
		provider.Remote{URL: "https://gitlab.example.com/team/repo.git"},
		mirrorDir, "route", EventMeta{}, slog.Default())
	if err != nil {
		t.Fatalf("a failed scrub must not fail the sync: %v", err)
	}
	if len(git.fetchCalls) != 1 {
		t.Errorf("fetch calls = %d, want 1 — the sync must carry on", len(git.fetchCalls))
	}
}

// TestMain makes this test binary usable as the askpass helper as well.
//
// The helper works by re-executing the binary os.Executable() points at, and inside a test
// that binary is this test binary. Without this branch, every time git asks for credentials
// the entire test suite runs again from the top and the first line of its output is read as
// the credential — neither a failure nor a success, just an authentication error with no
// discoverable cause.
func TestMain(m *testing.M) {
	if askpass.Serve(os.Args[1:], os.Stdout) {
		return
	}
	os.Exit(m.Run())
}

// Checks end to end that git really does ask the helper and send what it gets back as HTTP
// Basic. The unit tests above only look at whether the environment is assembled correctly,
// not at whether git actually uses that environment that way — and that is the point this
// whole design rests on.
func TestNewGitRemoteCmd_GitAuthenticatesThroughTheHelper(t *testing.T) {
	assertGitAuthenticates(t)
}

// Authentication has to work even when the surrounding environment has credential
// acquisition turned off.
//
// GitLab Runner puts exactly these variables into the job environment. git reads GIT_CONFIG_*
// as configuration at the same level as a command-line `-c`, so credential.interactive=never
// alone makes git die without ever asking askpass — there is no retry, so the symptom is not
// "authentication does not work" but "one request goes out and that is it". This is the real
// condition that turned CI red on 2026-08-25, and this test reproduces it.
func TestNewGitRemoteCmd_AuthenticatesDespiteHostileCredentialConfig(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.interactive")
	t.Setenv("GIT_CONFIG_VALUE_0", "never")
	// This also blocks the case where a surrounding helper hands out different credentials for the same host.
	t.Setenv("GIT_CONFIG_KEY_1", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_1", "!f() { echo username=wrong; echo password=wrong; }; f")

	assertGitAuthenticates(t)
}

// assertGitAuthenticates checks the whole sequence: git takes a 401, asks the helper, and
// retries with Basic auth. The two tests above run the same check with only the environment changed.
func assertGitAuthenticates(t *testing.T) {
	t.Helper()
	var gotAuth string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		auth := r.Header.Get("Authorization")
		if auth == "" {
			// The first request is anonymous. Only after a 401 plus a Basic challenge does
			// git finally ask GIT_ASKPASS.
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		gotAuth = auth
		// It does not matter what is returned here. The only thing being checked is that the
		// credential arrived, and ls-remote is free to fail right afterwards.
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rem := provider.Remote{
		URL:      srv.URL + "/team/repo.git",
		Username: "oauth2",
		Password: "glpat-supersecret",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := newGitRemoteCmd(ctx, rem, "ls-remote", rem.URL)
	// Capture git's stderr. There is only one way this test breaks — "only one request" — and
	// that single line cannot tell whether the helper was never called at all, was called and
	// answered with empty values, or git gave up on the prompt. git writes the reason to
	// stderr precisely, so it rides along in the failure message.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// The failure is expected (the handler above returns 404). The header is the only thing to look at.
	_ = cmd.Run()

	if calls < 2 {
		t.Fatalf("git made %d request(s); it never retried with credentials.\n"+
			"GIT_ASKPASS=%s (executable=%v)\ngit stderr:\n%s",
			calls, envMap(t, cmd.Env)["GIT_ASKPASS"], helperIsExecutable(t, cmd.Env), stderr.String())
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(rem.Username+":"+rem.Password))
	if gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

// Runs the argument list for a remote that carries credentials through real git once.
//
// With credentials, two more `-c` flags go in front, so -c and -C end up mixed together as in
// `git -c ... -C <dir> -c core.abbrev=40 push ...`. Every other real-git test uses a local
// remote with no credentials and therefore never builds this argument list, so if the order
// breaks, the unit tests stay entirely green and it only blows up in the deployment. A local
// remote never asks for credentials, so the one thing checked here is whether git accepts
// this command line and works normally.
func TestDefaultGitRunner_CredentialFlagsDoNotBreakTheCommandLine(t *testing.T) {
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/a.txt", "one")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "first")
	runGit(t, srcDir, "branch", "-M", "main")

	tgtDir := t.TempDir()
	runGit(t, tgtDir, "init", "--bare")

	// A local path, but with credentials attached — that combination is the only thing that
	// turns credentialFlags on, and that argument list is the point of this test.
	srcRem := provider.Remote{URL: srcDir, Username: "oauth2", Password: "tok"}
	tgtRem := provider.Remote{URL: tgtDir, Username: "oauth2", Password: "tok"}

	runner := &defaultGitRunner{}
	ctx := context.Background()
	mirrorDir := t.TempDir() + "/mirror.git"
	if err := runner.CloneMirror(ctx, srcRem, mirrorDir); err != nil {
		t.Fatalf("CloneMirror with credential flags failed: %v", err)
	}
	if err := runner.FetchMirror(ctx, srcRem, mirrorDir); err != nil {
		t.Fatalf("FetchMirror with credential flags failed: %v", err)
	}
	// push is the only path that uses -C and -c core.abbrev=40 together.
	res, err := runner.PushMirror(ctx, mirrorDir, tgtRem, localSpecs(t, runner, mirrorDir))
	if err != nil {
		t.Fatalf("PushMirror with credential flags failed: %v", err)
	}
	if !res.Changed {
		t.Error("the first push moved refs, so it must report a change")
	}
	if _, err := runner.RemoteRefs(ctx, tgtRem); err != nil {
		t.Fatalf("RemoteRefs with credential flags failed: %v", err)
	}
	if got := gitOut(t, tgtDir, "rev-parse", "refs/heads/main"); got == "" {
		t.Error("main did not arrive at the destination")
	}
}

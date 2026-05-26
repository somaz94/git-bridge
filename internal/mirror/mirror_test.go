package mirror

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"git-bridge/internal/config"
	"git-bridge/internal/notify"
	"git-bridge/internal/provider"
)

// mockGitRunner records calls and returns configurable errors.
type mockGitRunner struct {
	cloneCalls      []cloneCall
	fetchCalls      []fetchCall
	pushCalls       []pushCall
	deleteRefCalls  []deleteRefCall
	cloneErr        error
	fetchErr        error
	pushErr         error
	pushChanged     bool // when true, PushMirror reports changes were pushed
	deleteRefErr    error
	commitAuthor    string // return value for CommitAuthor
	commitAuthorErr error
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
	Dir string
	URL string
}

type deleteRefCall struct {
	URL     string
	RefType string
	RefName string
}

func (m *mockGitRunner) CloneMirror(_ context.Context, url, dir string) error {
	m.cloneCalls = append(m.cloneCalls, cloneCall{URL: url, Dir: dir})
	return m.cloneErr
}

func (m *mockGitRunner) FetchMirror(_ context.Context, url, dir string) error {
	m.fetchCalls = append(m.fetchCalls, fetchCall{URL: url, Dir: dir})
	return m.fetchErr
}

func (m *mockGitRunner) PushMirror(_ context.Context, dir, url string) (bool, error) {
	m.pushCalls = append(m.pushCalls, pushCall{Dir: dir, URL: url})
	return m.pushChanged, m.pushErr
}

func (m *mockGitRunner) DeleteRef(_ context.Context, _, url, refType, refName string) error {
	m.deleteRefCalls = append(m.deleteRefCalls, deleteRefCall{URL: url, RefType: refType, RefName: refName})
	return m.deleteRefErr
}

func (m *mockGitRunner) CommitAuthor(_ context.Context, _, _ string) (string, error) {
	return m.commitAuthor, m.commitAuthorErr
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
	git := &mockGitRunner{pushChanged: true, commitAuthor: "somaz"}
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
	if !strings.Contains(notif.messages[0].Body, "Pushed by: somaz") {
		t.Errorf("expected notification body to contain commit author, got %q", notif.messages[0].Body)
	}
	if !strings.Contains(notif.messages[0].Body, "Branch: main") {
		t.Errorf("expected notification body to contain branch info, got %q", notif.messages[0].Body)
	}
}

func TestDoMirror_SuccessNotification_WithTag(t *testing.T) {
	git := &mockGitRunner{pushChanged: true, commitAuthor: "somaz"}
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

	svc, err := New(cfg, notify.NewNoop())
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

	svc, err := New(cfg, notify.NewNoop())
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

	_, err := New(cfg, notify.NewNoop())
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

	err := runner.CloneMirror(context.Background(), srcDir, destDir)
	if err != nil {
		t.Fatalf("CloneMirror failed: %v", err)
	}
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

	if err := runner.CloneMirror(context.Background(), srcDir, mirrorDir); err != nil {
		t.Fatalf("CloneMirror failed: %v", err)
	}
	changed, err := runner.PushMirror(context.Background(), mirrorDir, tgtDir)
	if err != nil {
		t.Fatalf("PushMirror failed: %v", err)
	}
	if !changed {
		t.Error("expected changed=true for first push")
	}

	// Push again — should be up-to-date
	changed2, err := runner.PushMirror(context.Background(), mirrorDir, tgtDir)
	if err != nil {
		t.Fatalf("PushMirror second push failed: %v", err)
	}
	if changed2 {
		t.Error("expected changed=false for second push (up-to-date)")
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

	err := runner.DeleteRef(context.Background(), workDir, tgtDir, "branch", "feature-branch")
	if err != nil {
		t.Fatalf("DeleteRef failed: %v", err)
	}
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- defaultGitRunner error path tests ---

func TestDefaultGitRunner_CloneMirror_InvalidURL(t *testing.T) {
	runner := &defaultGitRunner{}
	destDir := t.TempDir() + "/mirror.git"

	err := runner.CloneMirror(context.Background(), "http://invalid.invalid.invalid/repo.git", destDir)
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
	if err := runner.CloneMirror(context.Background(), srcDir, mirrorDir); err != nil {
		t.Fatalf("CloneMirror failed: %v", err)
	}

	// Push to invalid URL should fail
	_, err := runner.PushMirror(context.Background(), mirrorDir, "http://invalid.invalid.invalid/repo.git")
	if err == nil {
		t.Fatal("expected error for invalid push URL")
	}
}

func TestDefaultGitRunner_DeleteRef_InvalidURL(t *testing.T) {
	runner := &defaultGitRunner{}
	workDir := t.TempDir() + "/delete-work.git"

	err := runner.DeleteRef(context.Background(), workDir, "http://invalid.invalid.invalid/repo.git", "branch", "main")
	if err == nil {
		t.Fatal("expected error for invalid delete URL")
	}
}

func TestDefaultGitRunner_CloneMirror_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	runner := &defaultGitRunner{}
	destDir := t.TempDir() + "/mirror.git"

	err := runner.CloneMirror(ctx, "http://example.com/repo.git", destDir)
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
	if err := runner.CloneMirror(context.Background(), srcDir, mirrorDir); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// Push branches succeeds to valid target, then we remove the target to fail tags push
	tgtDir := t.TempDir()
	runGit(t, tgtDir, "init", "--bare")

	// This should succeed (both branches and tags)
	if _, err := runner.PushMirror(context.Background(), mirrorDir, tgtDir); err != nil {
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

// --- hasPorcelainChanges tests ---

func TestHasPorcelainChanges(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"empty output", "", false},
		{"whitespace only", "  \n  ", false},
		{"up-to-date ref", "To /tmp/target.git\n=\trefs/heads/main:refs/heads/main\t[up to date]\nDone", false},
		{"new branch", "To /tmp/target.git\n*\trefs/heads/main:refs/heads/main\t[new branch]\nDone", true},
		{"forced update", "To /tmp/target.git\n+\trefs/heads/main:refs/heads/main\t(forced update)\nDone", true},
		{"normal update", "To /tmp/target.git\n \trefs/heads/main:refs/heads/main\tabc123..def456\nDone", true},
		{"mixed up-to-date and changed", "To /tmp/target.git\n=\trefs/heads/main:refs/heads/main\t[up to date]\n+\trefs/heads/dev:refs/heads/dev\t(forced)\nDone", true},
		{"all up-to-date", "To /tmp/target.git\n=\trefs/heads/main:refs/heads/main\t[up to date]\n=\trefs/heads/dev:refs/heads/dev\t[up to date]\nDone", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasPorcelainChanges(tt.output); got != tt.want {
				t.Errorf("hasPorcelainChanges(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
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
	mirrorDir := svc.workDir + "/test-repo-codecommit.git"
	os.MkdirAll(mirrorDir, 0o755)
	os.WriteFile(mirrorDir+"/HEAD", []byte("ref: refs/heads/main\n"), 0o644)

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
	mirrorDir := svc.workDir + "/test-repo-codecommit.git"
	os.MkdirAll(mirrorDir, 0o755)
	os.WriteFile(mirrorDir+"/HEAD", []byte("ref: refs/heads/main\n"), 0o644)

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
	mirrorDir := svc.workDir + "/test-repo-codecommit.git"
	os.MkdirAll(mirrorDir, 0o755)
	os.WriteFile(mirrorDir+"/HEAD", []byte("ref: refs/heads/main\n"), 0o644)

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
	if err := runner.CloneMirror(context.Background(), srcDir, mirrorDir); err != nil {
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
	if err := runner.CloneMirror(context.Background(), srcDir, mirrorDir); err != nil {
		t.Fatalf("CloneMirror failed: %v", err)
	}

	// Add a new commit to source
	writeFile(t, srcDir+"/file2.txt", "world")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "second")

	// Fetch should pick up the new commit
	if err := runner.FetchMirror(context.Background(), srcDir, mirrorDir); err != nil {
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
	os.WriteFile(tmpDir+"/HEAD", []byte("ref: refs/heads/main\n"), 0o644)
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
	if err := runner.CloneMirror(context.Background(), srcDir, mirrorDir); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	err := runner.FetchMirror(context.Background(), "http://invalid.invalid.invalid/repo.git", mirrorDir)
	if err == nil {
		t.Fatal("expected error for invalid fetch URL")
	}
}

func TestDefaultGitRunner_DeleteRef_MkdirFail(t *testing.T) {
	runner := &defaultGitRunner{}
	// Use a path under a file (not a directory) to trigger MkdirAll failure
	tmpFile := t.TempDir() + "/file"
	writeFile(t, tmpFile, "not a dir")

	err := runner.DeleteRef(context.Background(), tmpFile+"/sub", "http://example.com/repo.git", "branch", "main")
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
	if err := runner.CloneMirror(context.Background(), srcDir, mirrorDir); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// Push branches to valid target, but cancel context before tags
	ctx, cancel := context.WithCancel(context.Background())
	// We can't easily cancel between branches and tags, so just test normal path
	defer cancel()

	tgtDir := t.TempDir()
	runGit(t, tgtDir, "init", "--bare")
	changed, err := runner.PushMirror(ctx, mirrorDir, tgtDir)
	if err != nil {
		t.Fatalf("PushMirror failed: %v", err)
	}
	if !changed {
		t.Error("expected changed=true")
	}
}

func TestDefaultGitRunner_CloneMirror_CleanupExistingDir(t *testing.T) {
	srcDir := t.TempDir()
	runGit(t, srcDir, "init", "--bare")

	runner := &defaultGitRunner{}
	destDir := t.TempDir() + "/mirror.git"
	os.MkdirAll(destDir, 0o755)
	writeFile(t, destDir+"/stale-file", "old data")

	err := runner.CloneMirror(context.Background(), srcDir, destDir)
	if err != nil {
		t.Fatalf("CloneMirror failed: %v", err)
	}

	// stale file should be removed
	if _, err := os.Stat(destDir + "/stale-file"); err == nil {
		t.Error("expected stale file to be removed")
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
	git := &mockGitRunner{pushChanged: true, commitAuthor: "developer"}
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
	git := &mockGitRunner{pushChanged: true, commitAuthor: "tagger"}
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

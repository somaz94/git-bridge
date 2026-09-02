package mirror

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git-bridge/internal/config"
	"git-bridge/internal/history"
	"git-bridge/internal/notify"
	"git-bridge/internal/provider"
)

// recordingRecorder captures every history event the service emits.
type recordingRecorder struct {
	mu     sync.Mutex
	events []history.Event
}

func (r *recordingRecorder) Record(ev history.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingRecorder) all() []history.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]history.Event(nil), r.events...)
}

// only returns the single recorded event, failing when the count is not one.
// Every terminal path must record exactly once — a path that records twice
// double-counts in the console, one that records zero times goes invisible.
func (r *recordingRecorder) only(t *testing.T) history.Event {
	t.Helper()
	got := r.all()
	if len(got) != 1 {
		t.Fatalf("recorded %d events, want exactly 1: %+v", len(got), got)
	}
	return got[0]
}

func newRecordingService(repos []config.RepoConfig, providers map[string]provider.Provider, notif notify.Notifier, git *mockGitRunner) (*Service, *recordingRecorder) {
	rec := &recordingRecorder{}
	svc := newTestService(repos, providers, notif, git)
	svc.recorder = rec
	return svc, rec
}

func assertOutcome(t *testing.T, ev history.Event, action, result, reason string) {
	t.Helper()
	if ev.Action != action {
		t.Errorf("Action = %q, want %q", ev.Action, action)
	}
	if ev.Result != result {
		t.Errorf("Result = %q, want %q", ev.Result, result)
	}
	if ev.Reason != reason {
		t.Errorf("Reason = %q, want %q", ev.Reason, reason)
	}
}

func TestHistory_SuccessfulSyncRecordsOK(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{Ref: "refs/heads/main", Source: SourceSQS}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	ev := rec.only(t)
	assertOutcome(t, ev, history.ActionMirror, history.ResultOK, "")
	if ev.Repo != "my-repo" {
		t.Errorf("Repo = %q, want my-repo", ev.Repo)
	}
	// The route is recorded as provider name + path, matching the Slack Route
	// line. The name — not the type — is what distinguishes two providers of the
	// same type (gitlab-main vs gitlab-old).
	if ev.From != "codecommit-eu/my-repo" || ev.To != "gitlab-main/team/my-repo" {
		t.Errorf("route = %q → %q, want codecommit-eu/my-repo → gitlab-main/team/my-repo", ev.From, ev.To)
	}
	if ev.Ref != "refs/heads/main" {
		t.Errorf("Ref = %q, want refs/heads/main", ev.Ref)
	}
	if ev.Source != SourceSQS {
		t.Errorf("Source = %q, want %q", ev.Source, SourceSQS)
	}
	if ev.Err != "" {
		t.Errorf("Err = %q, want empty on success", ev.Err)
	}
}

// "Nothing to do" is the most common outcome by far. Recording it as ok would
// make the console read as if every webhook produced a real mirror.
func TestHistory_AlreadyUpToDateRecordsSkip(t *testing.T) {
	git := &mockGitRunner{pushChanged: false}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultSkip, history.ReasonAlreadyUpToDate)
}

func TestHistory_PushFailureRecordsFailWithTheError(t *testing.T) {
	git := &mockGitRunner{pushErr: errors.New("remote hung up")}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{}); err == nil {
		t.Fatal("Sync() error = nil, want an error")
	}

	ev := rec.only(t)
	assertOutcome(t, ev, history.ActionMirror, history.ResultFail, history.ReasonPush)
	if !strings.Contains(ev.Err, "remote hung up") {
		t.Errorf("Err = %q, want it to carry the push failure", ev.Err)
	}
}

func TestHistory_CloneFailureRecordsFail(t *testing.T) {
	git := &mockGitRunner{cloneErr: errors.New("no such repo")}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{}); err == nil {
		t.Fatal("Sync() error = nil, want an error")
	}

	ev := rec.only(t)
	assertOutcome(t, ev, history.ActionMirror, history.ResultFail, history.ReasonClone)
	if !strings.Contains(ev.Err, "no such repo") {
		t.Errorf("Err = %q, want it to carry the clone failure", ev.Err)
	}
}

// A ref_override skip and an already-up-to-date skip are both "skip" but mean
// completely different things; the reason is the only thing separating them.
func TestHistory_RefOverrideSkipIsDistinguishable(t *testing.T) {
	repos := []config.RepoConfig{{
		Name:       "bidi-repo",
		Source:     "codecommit-eu",
		Target:     "gitlab-main",
		SourcePath: "bidi-repo",
		TargetPath: "team/bidi-repo",
		Direction:  "bidirectional",
		RefOverrides: []config.RefOverride{
			{Pattern: "main", From: "gitlab-main", To: "codecommit-eu"},
		},
	}}
	git := &mockGitRunner{pushChanged: true}
	svc, rec := newRecordingService(repos, makeProviders(), &mockNotifier{}, git)

	// source → target on a ref pinned to the opposite direction.
	if err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: "refs/heads/main"}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	ev := rec.only(t)
	assertOutcome(t, ev, history.ActionMirror, history.ResultSkip, history.ReasonRefOverride)
	if len(git.pushCalls) != 0 {
		t.Errorf("push was attempted %d times, want 0", len(git.pushCalls))
	}
}

func TestHistory_SuccessfulDeleteRecordsDeleteAction(t *testing.T) {
	git := &mockGitRunner{}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.SyncDelete(context.Background(), "my-repo", "branch", "feature-x"); err != nil {
		t.Fatalf("SyncDelete() error = %v", err)
	}

	ev := rec.only(t)
	assertOutcome(t, ev, history.ActionDelete, history.ResultOK, "")
	// Deletes carry the short name on the wire; the history stores the full ref
	// so a branch and a tag of the same name stay distinguishable.
	if ev.Ref != "refs/heads/feature-x" {
		t.Errorf("Ref = %q, want refs/heads/feature-x", ev.Ref)
	}
	if ev.Source != SourceSQS {
		t.Errorf("Source = %q, want %q for the source-side delete path", ev.Source, SourceSQS)
	}
}

func TestHistory_TagDeleteRecordsTheTagRef(t *testing.T) {
	git := &mockGitRunner{}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.SyncDelete(context.Background(), "my-repo", "tag", "v1.0.0"); err != nil {
		t.Fatalf("SyncDelete() error = %v", err)
	}

	if ev := rec.only(t); ev.Ref != "refs/tags/v1.0.0" {
		t.Errorf("Ref = %q, want refs/tags/v1.0.0", ev.Ref)
	}
}

// A delete is the one operation that leaves nothing behind to look up: once it
// runs, the destination no longer names the ref at all. The tip read just
// before the delete is therefore the only record of what was removed, and
// without it "someone's branch vanished" has no starting point for a recovery.
func TestHistory_SuccessfulDeleteRecordsTheDeletedTip(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	git := &mockGitRunner{refTip: tip}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.SyncDelete(context.Background(), "my-repo", "branch", "feature-x"); err != nil {
		t.Fatalf("SyncDelete() error = %v", err)
	}

	if ev := rec.only(t); ev.DeletedTip != tip {
		t.Errorf("DeletedTip = %q, want %q", ev.DeletedTip, tip)
	}
}

// The recording pipeline must not truncate the tip on its way to the history:
// a remote rejects an abbreviated object name, so a shortened SHA here would be
// a record nobody can act on. That the tip is full-length in the first place is
// TestDefaultGitRunner_RefTip's job; this pins that nothing shortens it after.
func TestHistory_DeletedTipIsRecordedUntruncated(t *testing.T) {
	git := &mockGitRunner{}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.SyncDelete(context.Background(), "my-repo", "branch", "feature-x"); err != nil {
		t.Fatalf("SyncDelete() error = %v", err)
	}

	if got := rec.only(t).DeletedTip; len(got) != 40 {
		t.Errorf("DeletedTip = %q (len %d), want a full 40-character object name", got, len(got))
	}
}

// A delete that removed nothing must not claim it discarded a tip, or the
// history would name objects that are still perfectly reachable.
func TestHistory_AlreadyAbsentDeleteRecordsNoTip(t *testing.T) {
	git := &mockGitRunner{refMissing: true}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.SyncDelete(context.Background(), "my-repo", "branch", "gone"); err != nil {
		t.Fatalf("SyncDelete() error = %v", err)
	}

	if ev := rec.only(t); ev.DeletedTip != "" {
		t.Errorf("DeletedTip = %q, want empty on an already-absent delete", ev.DeletedTip)
	}
}

// A failed delete removed nothing either, so it must not record a tip as
// discarded — the ref is still on the destination.
func TestHistory_FailedDeleteRecordsNoTip(t *testing.T) {
	git := &mockGitRunner{deleteRefErr: errors.New("boom")}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.SyncDelete(context.Background(), "my-repo", "branch", "feature-x"); err == nil {
		t.Fatal("expected SyncDelete to fail")
	}

	if ev := rec.only(t); ev.DeletedTip != "" {
		t.Errorf("DeletedTip = %q, want empty when the delete failed", ev.DeletedTip)
	}
}

// The idempotent no-op is what terminates the bidirectional delete echo, so it
// has to be visible in the history — it is the evidence the loop-breaker fired.
func TestHistory_AlreadyAbsentDeleteRecordsSkip(t *testing.T) {
	git := &mockGitRunner{refMissing: true}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.SyncDelete(context.Background(), "my-repo", "branch", "gone"); err != nil {
		t.Fatalf("SyncDelete() error = %v", err)
	}

	assertOutcome(t, rec.only(t), history.ActionDelete, history.ResultSkip, history.ReasonAlreadyAbsent)
}

func TestHistory_RefTipFailureRecordsCheckRef(t *testing.T) {
	git := &mockGitRunner{refTipErr: errors.New("auth failed")}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.SyncDelete(context.Background(), "my-repo", "branch", "x"); err == nil {
		t.Fatal("SyncDelete() error = nil, want an error")
	}

	ev := rec.only(t)
	assertOutcome(t, ev, history.ActionDelete, history.ResultFail, history.ReasonCheckRef)
	if !strings.Contains(ev.Err, "auth failed") {
		t.Errorf("Err = %q, want it to carry the check failure", ev.Err)
	}
}

func TestHistory_DeleteRefFailureRecordsDeleteRef(t *testing.T) {
	git := &mockGitRunner{deleteRefErr: errors.New("protected branch")}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.SyncDelete(context.Background(), "my-repo", "branch", "x"); err == nil {
		t.Fatal("SyncDelete() error = nil, want an error")
	}

	ev := rec.only(t)
	assertOutcome(t, ev, history.ActionDelete, history.ResultFail, history.ReasonDeleteRef)
	if !strings.Contains(ev.Err, "protected branch") {
		t.Errorf("Err = %q, want it to carry the delete failure", ev.Err)
	}
}

// A webhook-driven delete is a different trigger from an SQS one, and telling
// them apart is what makes an echo loop legible in the console.
func TestHistory_TargetSideDeleteIsLabelledWebhook(t *testing.T) {
	git := &mockGitRunner{}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.SyncDeleteByTarget(context.Background(), "gitlab", "team/bidi-repo", "branch", "x"); err != nil {
		t.Fatalf("SyncDeleteByTarget() error = %v", err)
	}

	ev := rec.only(t)
	assertOutcome(t, ev, history.ActionDelete, history.ResultOK, "")
	if ev.Source != SourceWebhook {
		t.Errorf("Source = %q, want %q", ev.Source, SourceWebhook)
	}
}

func TestHistory_RetryIsLabelledRetryAPI(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Retry(context.Background(), "my-repo", "", EventMeta{Source: SourceRetryAPI}); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}

	if ev := rec.only(t); ev.Source != SourceRetryAPI {
		t.Errorf("Source = %q, want %q", ev.Source, SourceRetryAPI)
	}
}

func TestHistory_DurationIsRecorded(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	if got := rec.only(t).DurationMS; got < 0 {
		t.Errorf("DurationMS = %d, want a non-negative duration", got)
	}
}

// A bidirectional sync runs both legs, and each leg is its own event: the
// console has to show which direction did what.
func TestHistory_BidirectionalSyncRecordsBothLegs(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "bidi-repo", EventMeta{}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	got := rec.all()
	if len(got) == 0 {
		t.Fatal("recorded no events")
	}
	for i, ev := range got {
		if ev.From == "" || ev.To == "" {
			t.Errorf("event[%d] has an empty route: %+v", i, ev)
		}
		if ev.Repo != "bidi-repo" {
			t.Errorf("event[%d].Repo = %q, want bidi-repo", i, ev.Repo)
		}
	}
}

// A nil recorder must not panic: history is bolted onto mirroring and must
// never be able to break the sync it describes.
func TestHistory_NilRecorderDoesNotBreakMirroring(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)
	svc.recorder = nil

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if err := svc.SyncDelete(context.Background(), "my-repo", "branch", "x"); err != nil {
		t.Fatalf("SyncDelete() error = %v", err)
	}
}

func TestHistory_NewInstallsANoopRecorderWhenGivenNil(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"gitlab-main": {Type: "gitlab", BaseURL: "https://gitlab.example.com", Credentials: map[string]string{"token": "t"}},
			"github-main": {Type: "github", Credentials: map[string]string{"token": "t"}},
		},
		Repos: defaultRepos()[2:],
	}

	svc, err := New(cfg, notify.NewNoop(), nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if svc.recorder == nil {
		t.Fatal("recorder is nil, want a no-op recorder")
	}
}

// Housekeeping runs after an incremental fetch, where fragmentation actually
// accumulates. Every fetch leaves loose objects and another packfile behind.
func TestGC_RunsAfterAnIncrementalFetch(t *testing.T) {
	dir := t.TempDir()
	mirrorDir := filepath.Join(dir, "my-repo-codecommit-eu.git")
	// Make the mirror dir look like an existing bare repo so ensureMirror takes
	// the incremental path instead of the initial clone.
	if err := os.MkdirAll(mirrorDir, 0o755); err != nil {
		t.Fatalf("seed mirror dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mirrorDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("seed HEAD: %v", err)
	}

	git := &mockGitRunner{pushChanged: true}
	svc, _ := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)
	svc.workDir = dir

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	if len(git.fetchCalls) != 1 {
		t.Fatalf("fetch calls = %d, want 1", len(git.fetchCalls))
	}
	if len(git.gcCalls) != 1 || git.gcCalls[0] != mirrorDir {
		t.Errorf("GCAuto calls = %v, want exactly [%s]", git.gcCalls, mirrorDir)
	}
}

// A fresh clone is already packed, so running housekeeping on it would be pure
// waste.
func TestGC_SkippedAfterAnInitialClone(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	svc, _ := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)
	svc.workDir = t.TempDir()

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	if len(git.cloneCalls) != 1 {
		t.Fatalf("clone calls = %d, want 1", len(git.cloneCalls))
	}
	if len(git.gcCalls) != 0 {
		t.Errorf("GCAuto ran %v after an initial clone, want none", git.gcCalls)
	}
}

// Housekeeping is not mirroring: the sync already succeeded by then, so a gc
// failure must not turn it into a failure.
func TestGC_FailureDoesNotFailTheSync(t *testing.T) {
	dir := t.TempDir()
	mirrorDir := filepath.Join(dir, "my-repo-codecommit-eu.git")
	if err := os.MkdirAll(mirrorDir, 0o755); err != nil {
		t.Fatalf("seed mirror dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mirrorDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("seed HEAD: %v", err)
	}

	git := &mockGitRunner{pushChanged: true, gcErr: errors.New("gc exploded")}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)
	svc.workDir = dir

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{}); err != nil {
		t.Fatalf("Sync() error = %v, want the sync to succeed anyway", err)
	}
	if len(git.pushCalls) != 1 {
		t.Errorf("push calls = %d, want the push to still happen", len(git.pushCalls))
	}
	assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultOK, "")
}

func TestGCStatsRanOnlyWhenPacksDropped(t *testing.T) {
	cases := map[string]struct {
		stats GCStats
		want  bool
	}{
		"consolidated":     {GCStats{PacksBefore: 19, PacksAfter: 1}, true},
		"no-op":            {GCStats{PacksBefore: 3, PacksAfter: 3}, false},
		"count failed (0)": {GCStats{PacksBefore: 0, PacksAfter: 0}, false},
		// fetch added a pack and gc never ran — a rising count is not a consolidation.
		"grew": {GCStats{PacksBefore: 3, PacksAfter: 4}, false},
		// Pruning a marker only makes a pack eligible again; the consolidation
		// itself happens at some later gc, and claiming it here would report a
		// repack that never ran.
		"keeps pruned only": {GCStats{PacksBefore: 3, PacksAfter: 3, KeepsPruned: 1}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.stats.Ran(); got != tc.want {
				t.Errorf("Ran() = %v, want %v (%+v)", got, tc.want, tc.stats)
			}
		})
	}
}

// Even when gc really did consolidate, the mirror outcome is unchanged (only the log grows).
func TestGC_RepackDoesNotChangeTheSyncOutcome(t *testing.T) {
	dir := t.TempDir()
	mirrorDir := filepath.Join(dir, "my-repo-codecommit-eu.git")
	if err := os.MkdirAll(mirrorDir, 0o755); err != nil {
		t.Fatalf("seed mirror dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mirrorDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("seed HEAD: %v", err)
	}

	git := &mockGitRunner{pushChanged: true, gcStats: GCStats{PacksBefore: 19, PacksAfter: 1}}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)
	svc.workDir = dir

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultOK, "")
}

// countPacks counts *.pack only — counting the .idx / .rev files sitting in the same
// directory inflates the number several times over and skews the consolidation verdict.
func TestCountPacksCountsOnlyPackFiles(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("seed pack dir: %v", err)
	}
	for _, name := range []string{"a.pack", "a.idx", "a.rev", "b.pack", "tmp_file"} {
		if err := os.WriteFile(filepath.Join(packDir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	if got := countPacks(dir); got != 2 {
		t.Errorf("countPacks() = %d, want 2", got)
	}
}

// Zero when the mirror does not exist yet or cannot be read — this is an observability
// number, so it must never turn into an error.
func TestCountPacksMissingDirIsZero(t *testing.T) {
	if got := countPacks(t.TempDir()); got != 0 {
		t.Errorf("countPacks() = %d, want 0", got)
	}
}

// seedKeepDir writes a pack-directory entry with a chosen modification time.
func seedKeepDir(t *testing.T, packDir, name string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(packDir, name)
	if err := os.WriteFile(path, []byte("fetch-pack 335 on some-pod\n"), 0o644); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return path
}

// A .keep left behind by a fetch that died pins its packfile out of every later
// repack, so housekeeping removes the abandoned markers — and nothing else. A
// marker a running fetch is holding must survive: deleting it would let gc pull
// the pack out from under that fetch.
func TestPruneStaleKeepsRemovesOnlyAbandonedMarkers(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("seed pack dir: %v", err)
	}

	old := time.Now().Add(-staleKeepAge - time.Hour)
	abandoned := seedKeepDir(t, packDir, "pack-a.keep", old)
	inflight := seedKeepDir(t, packDir, "pack-b.keep", time.Now())
	// Age alone is not the test: the packfiles themselves are older still, and
	// removing one would destroy the mirror rather than tidy it.
	pack := seedKeepDir(t, packDir, "pack-a.pack", old)

	if got := pruneStaleKeeps(dir); got != 1 {
		t.Fatalf("pruneStaleKeeps() = %d, want 1", got)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("abandoned marker survived, stat error = %v", err)
	}
	for _, path := range []string{inflight, pack} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed, want it kept: %v", filepath.Base(path), err)
		}
	}
}

// The boundary belongs to the fetch: a marker still inside the window stays,
// however narrowly. Seeding it exactly at the threshold cannot be asserted —
// the clock advances between writing the timestamp and reading it — so this
// pins the side of the boundary that has to hold.
func TestPruneStaleKeepsKeepsMarkerInsideTheWindow(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("seed pack dir: %v", err)
	}
	marker := seedKeepDir(t, packDir, "pack-a.keep", time.Now().Add(-staleKeepAge+time.Minute))

	if got := pruneStaleKeeps(dir); got != 0 {
		t.Errorf("pruneStaleKeeps() = %d, want 0", got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker inside the window was removed: %v", err)
	}
}

// Zero when the mirror does not exist yet — for the same reason as countPacks, a failed
// cleanup must not block the mirror.
func TestPruneStaleKeepsMissingDirIsZero(t *testing.T) {
	if got := pruneStaleKeeps(t.TempDir()); got != 0 {
		t.Errorf("pruneStaleKeeps() = %d, want 0", got)
	}
}

// Pruning a marker is housekeeping, not mirroring: it must leave the sync
// result exactly as it was.
func TestGC_PrunedKeepDoesNotChangeTheSyncOutcome(t *testing.T) {
	dir := t.TempDir()
	mirrorDir := filepath.Join(dir, "my-repo-codecommit-eu.git")
	if err := os.MkdirAll(mirrorDir, 0o755); err != nil {
		t.Fatalf("seed mirror dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mirrorDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("seed HEAD: %v", err)
	}

	// gc stayed a no-op below the threshold while a marker was still removed —
	// the case the live cache is actually in.
	git := &mockGitRunner{pushChanged: true, gcStats: GCStats{PacksBefore: 3, PacksAfter: 3, KeepsPruned: 1}}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)
	svc.workDir = dir

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultOK, "")
}

// --- forced update tests ---

// A rewind rides on a successful push, so nothing in the result distinguishes
// it. The reason field is what makes it findable at all.
func TestForcedUpdate_RecordsReasonAndOverwrittenRefsOnAnOKEvent(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		pushForced: []history.ForcedRef{
			{Ref: "refs/heads/main", Old: "aaa111", New: "bbb222"},
		},
	}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{Ref: "refs/heads/main"}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	ev := rec.only(t)
	assertOutcome(t, ev, history.ActionMirror, history.ResultOK, history.ReasonForcedUpdate)
	if !ev.IsForced() {
		t.Fatal("IsForced() = false, want true")
	}
	if len(ev.Forced) != 1 || ev.Forced[0].Old != "aaa111" || ev.Forced[0].New != "bbb222" {
		t.Errorf("Forced = %+v, want the old and new tip of refs/heads/main", ev.Forced)
	}
}

// The ordinary path must stay untouched: a fast-forward carries no reason and no
// forced refs, or every routine sync would read as data loss.
func TestForcedUpdate_FastForwardRecordsNothingExtra(t *testing.T) {
	git := &mockGitRunner{pushChanged: true}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{Ref: "refs/heads/main"}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	ev := rec.only(t)
	assertOutcome(t, ev, history.ActionMirror, history.ResultOK, "")
	if ev.IsForced() {
		t.Errorf("Forced = %+v, want empty on a fast-forward", ev.Forced)
	}
}

// An overwritten branch has no benign explanation in a mirror, so it interrupts.
func TestForcedUpdate_OverwrittenBranchAlerts(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		pushForced: []history.ForcedRef{
			{Ref: "refs/heads/main", Old: "aaa111", New: "bbb222"},
		},
	}
	notif := &mockNotifier{}
	svc, _ := newRecordingService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{Ref: "refs/heads/main"}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	var alert *notify.Message
	for i := range notif.messages {
		if strings.HasPrefix(notif.messages[i].Title, "Forced Update:") {
			alert = &notif.messages[i]
		}
	}
	if alert == nil {
		t.Fatalf("no forced-update alert sent; got %+v", notif.messages)
	}
	if alert.Level != "error" {
		t.Errorf("Level = %q, want error", alert.Level)
	}
	// The old tip is the only recoverable pointer to the discarded commits, so
	// an alert without it is not actionable.
	if !strings.Contains(alert.Body, "aaa111") {
		t.Errorf("alert body missing the overwritten tip: %q", alert.Body)
	}
}

// Build pipelines re-point tags constantly. Alerting on that would train people
// to dismiss the alert, so a tag is recorded but stays quiet.
func TestForcedUpdate_OverwrittenTagIsRecordedButDoesNotAlert(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		pushForced: []history.ForcedRef{
			{Ref: "refs/tags/build-1", Old: "aaa111", New: "bbb222"},
		},
	}
	notif := &mockNotifier{}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{Ref: "refs/tags/build-1"}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	if ev := rec.only(t); !ev.IsForced() || ev.Reason != history.ReasonForcedUpdate {
		t.Errorf("tag rewrite must still be recorded, got reason=%q forced=%+v", ev.Reason, ev.Forced)
	}
	for _, msg := range notif.messages {
		if strings.HasPrefix(msg.Title, "Forced Update:") {
			t.Errorf("a tag rewrite must not alert, got %+v", msg)
		}
	}
}

// A push that mixes both must alert on the branch without the tag suppressing it.
func TestForcedUpdate_BranchAlertsEvenWhenMixedWithTags(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		pushForced: []history.ForcedRef{
			{Ref: "refs/tags/build-1", Old: "111", New: "222"},
			{Ref: "refs/heads/main", Old: "aaa111", New: "bbb222"},
		},
	}
	notif := &mockNotifier{}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	if got := len(rec.only(t).Forced); got != 2 {
		t.Errorf("recorded %d forced refs, want both", got)
	}
	var alerted bool
	for _, msg := range notif.messages {
		if strings.HasPrefix(msg.Title, "Forced Update:") {
			alerted = true
			if strings.Contains(msg.Body, "refs/tags/build-1") {
				t.Errorf("the alert should name only the branch, got %q", msg.Body)
			}
		}
	}
	if !alerted {
		t.Error("expected the branch rewrite to alert")
	}
}

// A push reporting no change cannot have overwritten anything, so the skip path
// must not acquire a forced record on the way through.
func TestForcedUpdate_NotRecordedOnAnUpToDateSkip(t *testing.T) {
	git := &mockGitRunner{pushChanged: false}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	if ev := rec.only(t); ev.IsForced() {
		t.Errorf("Forced = %+v, want empty on a skip", ev.Forced)
	}
}

// A branch overwrite replaces the success message instead of arriving beside it.
// The two together read as a contradiction — the same push reported as both
// failed and fine — and the reader has to work out that one event produced both.
func TestForcedUpdate_BranchOverwriteReplacesTheSuccessMessage(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		pushForced: []history.ForcedRef{
			{Ref: "refs/heads/main", Old: "aaa111", New: "bbb222"},
		},
	}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{Ref: "refs/heads/main"}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	if len(notif.messages) != 1 {
		t.Fatalf("expected exactly one message, got %d: %+v", len(notif.messages), notif.messages)
	}
	msg := notif.messages[0]
	if !strings.HasPrefix(msg.Title, "Forced Update:") {
		t.Errorf("the surviving message must be the alert, got %q", msg.Title)
	}
	// Everything the suppressed success message carried has to be here instead.
	for _, want := range []string{"Route:", "Duration:", "Target:"} {
		if !strings.Contains(msg.Body, want) {
			t.Errorf("alert body missing %q, so suppressing the success loses it: %q", want, msg.Body)
		}
	}
}

// A tag overwrite raises no alert, so the success message must still be sent —
// suppressing it would leave the push entirely unreported in Slack.
func TestForcedUpdate_TagOverwriteKeepsTheSuccessMessage(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		pushForced: []history.ForcedRef{
			{Ref: "refs/tags/build-1", Old: "aaa111", New: "bbb222"},
		},
	}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{Ref: "refs/tags/build-1"}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	if len(notif.messages) != 1 {
		t.Fatalf("expected exactly one message, got %d: %+v", len(notif.messages), notif.messages)
	}
	if !strings.HasPrefix(notif.messages[0].Title, "Mirror Sync:") {
		t.Errorf("a tag rewrite should still report success, got %q", notif.messages[0].Title)
	}
}

// The recovery command must carry the actual tip. This alert is read when
// something has already gone wrong, often by someone who did not cause it, and
// making them transplant a SHA from one line into a placeholder on another is
// the step that gets fumbled.
// The delete alert carries the same recovery line as the forced-update one, so
// it inherits the same invariant: the SHA is real, and the URL is NOT.
//
// The only destination URL this process holds is the clone URL, and every
// provider embeds a credential in it. Slack retains and indexes what it is
// sent, so interpolating it here would publish a live token — this test is the
// guard, because the comment alone did not stop it from being written once.
func TestDelete_RecoveryCommandCarriesTheSHAButNotTheCredential(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	git := &mockGitRunner{refTip: tip}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.SyncDelete(context.Background(), "my-repo", "branch", "feature-x"); err != nil {
		t.Fatalf("SyncDelete() error = %v", err)
	}

	if len(notif.messages) == 0 {
		t.Fatal("expected a Slack message for a successful delete")
	}
	body := notif.messages[0].Body

	if want := "git fetch <clone-url> " + tip; !strings.Contains(body, want) {
		t.Errorf("missing recovery line %q in:\n%s", want, body)
	}
	// The destination clone URL itself must never reach the body. Assert
	// against the real thing rather than a marker, so this keeps holding if a
	// provider changes how it formats credentials.
	tgtURL := makeProviders()["gitlab-main"].Remote("team/my-repo").URL
	if strings.Contains(body, tgtURL) {
		t.Errorf("delete alert body contains the credential-bearing clone URL in:\n%s", body)
	}
	// ...and the secret on its own, in case only the userinfo is interpolated.
	if strings.Contains(body, "glpat-test") {
		t.Errorf("delete alert body leaks the provider token in:\n%s", body)
	}
}

// The fetch alone restores nothing — it pulls the objects into a local clone
// and leaves the destination exactly as deleted. Printing only that half would
// promise an undo the command does not perform, so the push that re-creates the
// ref has to be there too, with the ref spelled out rather than left implied.
func TestDelete_RestoreCommandIsComplete(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, &mockGitRunner{refTip: tip})

	if err := svc.SyncDelete(context.Background(), "my-repo", "branch", "feature-x"); err != nil {
		t.Fatalf("SyncDelete() error = %v", err)
	}

	body := notif.messages[0].Body
	for _, want := range []string{
		"git fetch <clone-url> " + tip,
		"git push <clone-url> " + tip + ":refs/heads/feature-x",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

// The commands live in a code block; the tip does not.
//
// The block is what earns Slack's copy button and a monospaced font, which is
// what these two lines are for. The tip is read and compared against the
// history rather than pasted into a shell, so wrapping it in code formatting
// would only make it harder to skim.
func TestDelete_RestoreCommandsAreInACodeBlock(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, &mockGitRunner{refTip: tip})

	if err := svc.SyncDelete(context.Background(), "my-repo", "branch", "feature-x"); err != nil {
		t.Fatalf("SyncDelete() error = %v", err)
	}

	body := notif.messages[0].Body
	_, after, found := strings.Cut(body, "```")
	if !found {
		t.Fatalf("no code block in:\n%s", body)
	}
	block, closed, found := strings.Cut(after, "```")
	if !found || closed != "" {
		t.Fatalf("code block is not closed at the end of the body:\n%s", body)
	}
	for _, want := range []string{"git fetch ", "git push "} {
		if !strings.Contains(block, want) {
			t.Errorf("%q is outside the code block in:\n%s", want, body)
		}
	}
	if strings.Contains(block, "Deleted tip:") {
		t.Errorf("the tip line belongs outside the code block, got:\n%s", body)
	}
}

// A tag delete must restore the tag ref, not a branch of the same name.
func TestDelete_RestoreCommandUsesTheTagRefForTags(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, &mockGitRunner{refTip: tip})

	if err := svc.SyncDelete(context.Background(), "my-repo", "tag", "v1.0.0"); err != nil {
		t.Fatalf("SyncDelete() error = %v", err)
	}

	body := notif.messages[0].Body
	if want := "git push <clone-url> " + tip + ":refs/tags/v1.0.0"; !strings.Contains(body, want) {
		t.Errorf("missing %q in:\n%s", want, body)
	}
	if strings.Contains(body, "refs/heads/v1.0.0") {
		t.Errorf("tag delete must not restore a branch ref, got:\n%s", body)
	}
}

func TestForcedUpdate_RecoveryCommandCarriesTheRealSHA(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		pushForced: []history.ForcedRef{
			{Ref: "refs/heads/main", Old: "962cea79c51d0000000000000000000000000000", New: "dfe2531f"},
			{Ref: "refs/heads/master-b", Old: "1111111111111111111111111111111111111111", New: "2222222"},
		},
	}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	body := notif.messages[0].Body
	for _, f := range git.pushForced {
		want := "git fetch <clone-url> " + f.Old
		if !strings.Contains(body, want) {
			t.Errorf("missing runnable recovery line %q in:\n%s", want, body)
		}
	}
	// The old placeholder must be gone, or the command is still not runnable.
	if strings.Contains(body, "<old-sha>") {
		t.Errorf("recovery command still carries the SHA placeholder: %q", body)
	}
}

// --- restore ---

// The happy path: the hole is still there, so the ref goes back.
func TestRestore_RecreatesTheRefAndRecordsTheTip(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	git := &mockGitRunner{refMissing: true} // ref absent on the destination
	notif := &mockNotifier{}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), notif, git)

	err := svc.RestoreRef(context.Background(), "my-repo", "gitlab/team/my-repo", "branch", "feature-x", tip, "alice")
	if err != nil {
		t.Fatalf("RestoreRef() error = %v", err)
	}

	if len(git.createRefCalls) != 1 {
		t.Fatalf("expected 1 CreateRef call, got %d", len(git.createRefCalls))
	}
	if got := git.createRefCalls[0]; got.SHA != tip || got.FullRef != "refs/heads/feature-x" {
		t.Errorf("CreateRef(%+v), want sha=%s ref=refs/heads/feature-x", got, tip)
	}

	ev := rec.only(t)
	assertOutcome(t, ev, history.ActionRestore, history.ResultOK, "")
	if ev.RestoredTip != tip {
		t.Errorf("RestoredTip = %q, want %q", ev.RestoredTip, tip)
	}
	if ev.Actor != "alice" {
		t.Errorf("Actor = %q, want alice — a restore writes to a repo, so who did it has to survive", ev.Actor)
	}
	if ev.Source != SourceConsole {
		t.Errorf("Source = %q, want %q", ev.Source, SourceConsole)
	}
}

// The safety property. Someone re-created the branch after the delete, so the
// restore must refuse rather than overwrite them — that overwrite would be the
// same accident this feature exists to undo, with a different victim.
func TestRestore_RefusesWhenTheRefCameBack(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	git := &mockGitRunner{refTip: "1111111111111111111111111111111111111111"}
	notif := &mockNotifier{}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), notif, git)

	err := svc.RestoreRef(context.Background(), "my-repo", "gitlab/team/my-repo", "branch", "feature-x", tip, "alice")
	if err == nil {
		t.Fatal("expected RestoreRef to refuse when the ref exists")
	}
	if len(git.createRefCalls) != 0 {
		t.Errorf("must not write when refusing, got %d CreateRef calls", len(git.createRefCalls))
	}
	assertOutcome(t, rec.only(t), history.ActionRestore, history.ResultFail, history.ReasonRefExists)
	if len(notif.messages) != 1 || notif.messages[0].Level != "error" {
		t.Errorf("a refusal must alert; got %+v", notif.messages)
	}
}

// Pressing restore twice must not be an error the second time: the ref is
// already exactly where the button would put it.
func TestRestore_IsANoOpWhenAlreadyAtThatTip(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	git := &mockGitRunner{refTip: tip}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.RestoreRef(context.Background(), "my-repo", "gitlab/team/my-repo", "branch", "feature-x", tip, "alice"); err != nil {
		t.Fatalf("RestoreRef() error = %v", err)
	}
	if len(git.createRefCalls) != 0 {
		t.Errorf("must not push when already at the tip, got %d", len(git.createRefCalls))
	}
	assertOutcome(t, rec.only(t), history.ActionRestore, history.ResultSkip, history.ReasonAlreadyUpToDate)
}

// git garbage-collected the commit and the remote cannot supply it either, so
// there is nothing left to put back. Reported, not silently skipped.
func TestRestore_FailsWhenTheObjectIsGone(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	git := &mockGitRunner{
		refMissing:     true,
		objectMissing:  true,
		fetchObjectErr: errors.New("upload-pack: not our ref"),
	}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.RestoreRef(context.Background(), "my-repo", "gitlab/team/my-repo", "branch", "feature-x", tip, "alice"); err == nil {
		t.Fatal("expected RestoreRef to fail when the commit is gone")
	}
	if len(git.createRefCalls) != 0 {
		t.Errorf("must not push when the object is missing, got %d", len(git.createRefCalls))
	}
	assertOutcome(t, rec.only(t), history.ActionRestore, history.ResultFail, history.ReasonObjectGone)
}

// The mirror cache usually still has the commit; only reach for the network
// when it does not.
func TestRestore_SkipsTheFetchWhenTheObjectIsCached(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	git := &mockGitRunner{refMissing: true} // objectMissing false → cached
	svc, _ := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.RestoreRef(context.Background(), "my-repo", "gitlab/team/my-repo", "branch", "feature-x", tip, "alice"); err != nil {
		t.Fatalf("RestoreRef() error = %v", err)
	}
	if len(git.fetchObjectCalls) != 0 {
		t.Errorf("expected no FetchObject when the object is cached, got %d", len(git.fetchObjectCalls))
	}
}

// A tag restores as a tag ref, not a branch of the same name.
func TestRestore_UsesTheTagRefForTags(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	git := &mockGitRunner{refMissing: true}
	svc, _ := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.RestoreRef(context.Background(), "my-repo", "gitlab/team/my-repo", "tag", "v1.0.0", tip, "alice"); err != nil {
		t.Fatalf("RestoreRef() error = %v", err)
	}
	if got := git.createRefCalls[0].FullRef; got != "refs/tags/v1.0.0" {
		t.Errorf("FullRef = %q, want refs/tags/v1.0.0", got)
	}
}

// An endpoint that names neither side of the repo must not silently restore to
// whichever one happened to be first.
func TestRestore_RejectsAnUnknownDestination(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	git := &mockGitRunner{refMissing: true}
	svc, _ := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.RestoreRef(context.Background(), "my-repo", "gitlab/someone/else", "branch", "x", tip, "alice"); err == nil {
		t.Fatal("expected an error for a destination that is not a side of this repo")
	}
	if len(git.createRefCalls) != 0 {
		t.Errorf("must not write for an unknown destination, got %d", len(git.createRefCalls))
	}
}

// A restore writes, so it obeys the repo's direction like every other write.
// Without this the button could push to a side the service otherwise never
// touches, just by naming it in the request.
func TestRestore_RefusesADirectionTheRepoDoesNotAllow(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	repos := []config.RepoConfig{{
		Name: "oneway", Source: "codecommit-eu", Target: "gitlab-main",
		SourcePath: "oneway", TargetPath: "team/oneway", Direction: "source-to-target",
	}}
	git := &mockGitRunner{refMissing: true}
	svc, _ := newRecordingService(repos, makeProviders(), &mockNotifier{}, git)

	// Writing back to the source is never done for a source-to-target repo.
	err := svc.RestoreRef(context.Background(), "oneway", "codecommit/oneway", "branch", "x", tip, "alice")
	if err == nil {
		t.Fatal("expected a refusal for a direction the repo does not allow")
	}
	if len(git.createRefCalls) != 0 {
		t.Errorf("must not write, got %d CreateRef calls", len(git.createRefCalls))
	}
}

// ref_overrides keep one side authoritative for a ref. A delete already honours
// them; a restore that did not would re-create exactly what the override exists
// to protect.
func TestRestore_RefusesADirectionARefOverrideBlocks(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	repos := []config.RepoConfig{{
		Name: "bidi", Source: "codecommit-eu", Target: "gitlab-main",
		SourcePath: "bidi", TargetPath: "team/bidi", Direction: "bidirectional",
		RefOverrides: []config.RefOverride{
			{Pattern: "main", From: "gitlab-main", To: "codecommit-eu"},
		},
	}}
	git := &mockGitRunner{refMissing: true}
	svc, _ := newRecordingService(repos, makeProviders(), &mockNotifier{}, git)

	// main is pinned gitlab→codecommit, so restoring it onto gitlab is blocked.
	err := svc.RestoreRef(context.Background(), "bidi", "gitlab/team/bidi", "branch", "main", tip, "alice")
	if err == nil {
		t.Fatal("expected a refusal for a ref pinned away from this direction")
	}
	if len(git.createRefCalls) != 0 {
		t.Errorf("must not write, got %d CreateRef calls", len(git.createRefCalls))
	}
}

// The restore failure alert embeds git's stderr, which is the one place a
// credential could re-enter a Slack message. git strips userinfo from its own
// error output today, so this pins that rather than discovering it changed.
func TestRestore_FailureAlertCarriesNoCredential(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	git := &mockGitRunner{refTip: "1111111111111111111111111111111111111111"}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	_ = svc.RestoreRef(context.Background(), "my-repo", "gitlab/team/my-repo", "branch", "feature-x", tip, "alice")

	if len(notif.messages) == 0 {
		t.Fatal("a refused restore must alert")
	}
	body := notif.messages[0].Body
	tgtURL := makeProviders()["gitlab-main"].Remote("team/my-repo").URL
	if strings.Contains(body, tgtURL) {
		t.Errorf("restore failure body contains the credential-bearing clone URL:\n%s", body)
	}
	if strings.Contains(body, "glpat-test") {
		t.Errorf("restore failure body leaks the provider token:\n%s", body)
	}
}

// The refusals that happen before any git runs are the ones a hand-crafted
// request lands on, so they are exactly the ones that must not be invisible.
func TestRestore_RefusalsBeforeGitAreStillRecorded(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	oneway := []config.RepoConfig{{
		Name: "oneway", Source: "codecommit-eu", Target: "gitlab-main",
		SourcePath: "oneway", TargetPath: "team/oneway", Direction: "source-to-target",
	}}
	for name, tc := range map[string]struct {
		repos  []config.RepoConfig
		repo   string
		to     string
		reason string
	}{
		"direction":    {oneway, "oneway", "codecommit/oneway", history.ReasonDirection},
		"unknown side": {oneway, "oneway", "gitlab/someone/else", history.ReasonUnknownSide},
		"unknown repo": {oneway, "nope", "gitlab/team/oneway", history.ReasonUnknownRepo},
	} {
		t.Run(name, func(t *testing.T) {
			git := &mockGitRunner{refMissing: true}
			svc, rec := newRecordingService(tc.repos, makeProviders(), &mockNotifier{}, git)

			if err := svc.RestoreRef(context.Background(), tc.repo, tc.to, "branch", "x", tip, "alice"); err == nil {
				t.Fatal("expected a refusal")
			}
			ev := rec.only(t)
			assertOutcome(t, ev, history.ActionRestore, history.ResultFail, tc.reason)
			if ev.Actor != "alice" {
				t.Errorf("Actor = %q, want it recorded on a refusal too", ev.Actor)
			}
			if len(git.createRefCalls) != 0 {
				t.Errorf("must not write, got %d CreateRef calls", len(git.createRefCalls))
			}
		})
	}
}

// The restore runs inside an HTTP request, so it must not queue behind a mirror
// that could hold the lock for minutes — the connection would be held with it.
func TestRestore_RefusesWhenTheRepoIsBusy(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	git := &mockGitRunner{refMissing: true}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	// Hold the per-repo lock the way an in-flight mirror would.
	mu := svc.repoLock("my-repo")
	mu.Lock()
	defer mu.Unlock()

	err := svc.RestoreRef(context.Background(), "my-repo", "gitlab/team/my-repo", "branch", "feature-x", tip, "alice")
	if !errors.Is(err, ErrRepoBusy) {
		t.Fatalf("error = %v, want ErrRepoBusy", err)
	}
	if len(git.createRefCalls) != 0 {
		t.Errorf("must not write while busy, got %d CreateRef calls", len(git.createRefCalls))
	}
	assertOutcome(t, rec.only(t), history.ActionRestore, history.ResultFail, history.ReasonRepoBusy)
}

// Every refusal carries its sentinel, because the console maps them to distinct
// statuses and matching on message text would invert silently on a typo.
func TestRestore_RefusalsCarryTheirSentinel(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	oneway := []config.RepoConfig{{
		Name: "oneway", Source: "codecommit-eu", Target: "gitlab-main",
		SourcePath: "oneway", TargetPath: "team/oneway", Direction: "source-to-target",
	}}
	for name, tc := range map[string]struct {
		repos  []config.RepoConfig
		repo   string
		to     string
		git    *mockGitRunner
		target error
	}{
		"direction":    {oneway, "oneway", "codecommit/oneway", &mockGitRunner{refMissing: true}, ErrDirectionNotAllowed},
		"unknown side": {oneway, "oneway", "gitlab/nope/nope", &mockGitRunner{refMissing: true}, ErrUnknownSide},
		"unknown repo": {oneway, "missing", "gitlab/team/oneway", &mockGitRunner{refMissing: true}, ErrUnknownRepo},
		"ref exists":   {defaultRepos(), "my-repo", "gitlab/team/my-repo", &mockGitRunner{refTip: "1111111111111111111111111111111111111111"}, ErrRefExists},
		"object gone": {defaultRepos(), "my-repo", "gitlab/team/my-repo",
			&mockGitRunner{refMissing: true, objectMissing: true, fetchObjectErr: errors.New("not our ref")}, ErrObjectGone},
	} {
		t.Run(name, func(t *testing.T) {
			svc, _ := newRecordingService(tc.repos, makeProviders(), &mockNotifier{}, tc.git)
			err := svc.RestoreRef(context.Background(), tc.repo, tc.to, "branch", "x", tip, "alice")
			if !errors.Is(err, tc.target) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.target)
			}
		})
	}
}

// --- endpoint notation: telling same-type provider pairs apart + backward compatibility ---

// On a pair whose two provider types are the same, gitlab↔gitlab for instance, the Slack
// Route and the history have to show the two instances apart. Back when endpoint/route used
// the provider type, both sides read "gitlab/...", so the notification alone never told you
// which instance the sync went to.
func TestRoute_SameTypeProvidersAreDistinguishable(t *testing.T) {
	providers := map[string]provider.Provider{
		"gitlab-main": NewGitLab(config.ProviderConfig{
			Type: "gitlab", BaseURL: "https://gitlab.example.com",
			Credentials: map[string]string{"token": "t"},
		}),
		"gitlab-old": NewGitLab(config.ProviderConfig{
			Type: "gitlab", BaseURL: "https://gitlab-old.example.com",
			Credentials: map[string]string{"token": "t"},
		}),
	}
	repo := config.RepoConfig{
		Name: "migration", Source: "gitlab-old", Target: "gitlab-main",
		SourcePath: "backup/app", TargetPath: "test/app", Direction: "bidirectional",
	}
	git := &mockGitRunner{pushChanged: true}
	notif := &mockNotifier{}
	svc, rec := newRecordingService([]config.RepoConfig{repo}, providers, notif, git)
	svc.workDir = t.TempDir()

	err := svc.doMirror(context.Background(), repo, "gitlab-old", "backup/app", "gitlab-main", "test/app", EventMeta{})
	if err != nil {
		t.Fatalf("doMirror() error = %v", err)
	}

	ev := rec.only(t)
	if ev.From != "gitlab-old/backup/app" || ev.To != "gitlab-main/test/app" {
		t.Errorf("history route = %q → %q, want gitlab-old/backup/app → gitlab-main/test/app", ev.From, ev.To)
	}
	if len(notif.messages) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notif.messages))
	}
	const want = "Route: gitlab-old/backup/app → gitlab-main/test/app"
	if !strings.Contains(notif.messages[0].Body, want) {
		t.Errorf("notification should carry %q, got %q", want, notif.messages[0].Body)
	}
}

// The console's restore button sends the history row's To value back untouched. The volume
// still holds older rows recorded in the provider-type notation, so both notations have to
// resolve to the same side. If one is not accepted, the button on every one of those rows is
// refused with "not a side of this repo".
func TestRestore_AcceptsBothEndpointNotations(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	for name, to := range map[string]string{
		"provider name notation": "gitlab-main/team/my-repo",
		"legacy type notation":   "gitlab/team/my-repo",
	} {
		t.Run(name, func(t *testing.T) {
			git := &mockGitRunner{refTip: tip}
			svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

			if err := svc.RestoreRef(context.Background(), "my-repo", to, "branch", "feature-x", tip, "alice"); err != nil {
				t.Fatalf("RestoreRef(%q) error = %v", to, err)
			}
			// Whichever notation came in, the record is written in the new one.
			ev := rec.only(t)
			if ev.To != "gitlab-main/team/my-repo" {
				t.Errorf("recorded To = %q, want gitlab-main/team/my-repo", ev.To)
			}
		})
	}
}

// An unknown endpoint matches neither notation, so it must still be refused — this checks
// that adding backward compatibility did not loosen the matching.
func TestRestore_StillRefusesAnUnknownEndpoint(t *testing.T) {
	const tip = "abcdef0123456789abcdef0123456789abcdef01"
	git := &mockGitRunner{refTip: tip}
	svc, _ := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	err := svc.RestoreRef(context.Background(), "my-repo", "gitlab-old/team/my-repo", "branch", "feature-x", tip, "alice")
	if !errors.Is(err, ErrUnknownSide) {
		t.Errorf("error = %v, want ErrUnknownSide", err)
	}
}

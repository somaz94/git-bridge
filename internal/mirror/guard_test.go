package mirror

import (
	"context"
	"errors"
	"strings"
	"testing"

	"git-bridge/internal/config"
	"git-bridge/internal/history"
	"git-bridge/internal/notify"
	"git-bridge/internal/provider"
)

// The two tips from the 2026-08-11 demo-repo loss, kept verbatim so the tests
// below read as the incident rather than as an abstraction of it.
//
//	15:05:25  sunddu pushes a045472 to GitLab
//	15:05:34  gl→cc carries it to CodeCommit
//	15:05:41  CodeCommit echoes it back; the fetch that answers takes 55s
//	15:06:29  sunddu pushes 95a8c37 on top — GitLab is now ahead
//	15:06:36  the echo writes its 55-second-old snapshot with --force
//	15:06:37  refs/heads/version/4.3.0: 95a8c37 → a045472, one commit gone
const (
	incidentRef      = "refs/heads/version/4.3.0"
	incidentEchoTip  = "a045472ce1a5f639d08fe9f627a634a9ed9bdf3b" // what the echo carried
	incidentLostTip  = "95a8c3753195f3e50fea456e7bea4c376737655e" // what the destination had
	incidentOtherRef = "refs/heads/master-b"
)

// The incident itself: the destination holds a commit the source side has never
// fetched, so the echo must not write over it.
func TestPushGuard_DestinationHoldsUnknownCommit_WithholdsPush(t *testing.T) {
	git := &mockGitRunner{
		pushChanged:   true,
		listRefs:      []string{incidentRef},
		localTips:     map[string]string{incidentRef: incidentEchoTip},
		remoteTips:    map[string]string{incidentRef: incidentLostTip},
		objectMissing: true, // the echo's mirror never saw 95a8c37
	}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: incidentRef, Source: SourceSQS})
	if err != nil {
		t.Fatalf("a withheld push is a normal outcome, not an error: %v", err)
	}
	if len(git.pushCalls) != 0 {
		t.Fatalf("the echo must not push over an unknown destination tip, got %+v", git.pushCalls)
	}
	assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultSkip, history.ReasonDestinationAhead)
}

// Same refusal when the destination tip IS known and simply sits ahead: pushing
// would be a rewind, and the destination already contains everything we hold.
func TestPushGuard_DestinationIsDescendant_WithholdsPush(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{incidentRef},
		localTips:   map[string]string{incidentRef: incidentEchoTip},
		remoteTips:  map[string]string{incidentRef: incidentLostTip},
		ancestors:   map[string]bool{incidentEchoTip + ">" + incidentLostTip: true},
	}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: incidentRef}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.pushCalls) != 0 {
		t.Fatalf("a push that would rewind the destination must be withheld, got %+v", git.pushCalls)
	}
	assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultSkip, history.ReasonDestinationAhead)
}

// The ordinary case still goes through, and carries the tip it was checked
// against so the write cannot land on a destination that moved since.
func TestPushGuard_DestinationBehind_PushesWithObservedTipAsLease(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{incidentRef},
		localTips:   map[string]string{incidentRef: incidentLostTip},
		remoteTips:  map[string]string{incidentRef: incidentEchoTip},
	}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: incidentRef}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []PushSpec{{Ref: incidentRef, Lease: incidentEchoTip}}
	if len(git.pushCalls) != 1 || len(git.pushCalls[0].Specs) != 1 || git.pushCalls[0].Specs[0] != want[0] {
		t.Fatalf("expected a leased push %+v, got %+v", want, git.pushCalls)
	}
}

// A ref the destination does not have yet is created, and the empty lease says
// "must not exist" — so a ref that appears in the meantime is not overwritten.
func TestPushGuard_DestinationMissingRef_PushesWithMustNotExistLease(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{incidentRef},
		localTips:   map[string]string{incidentRef: incidentEchoTip},
		remoteTips:  map[string]string{}, // destination has nothing
	}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: incidentRef}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("expected 1 push, got %d", len(git.pushCalls))
	}
	if got := git.pushCalls[0].Specs; len(got) != 1 || got[0].Lease != "" {
		t.Errorf("a new ref must be leased against non-existence, got %+v", got)
	}
}

// Identical tips are dropped before git is invoked. This is also what keeps a
// full reconcile cheap on a repo carrying thousands of build tags.
func TestPushGuard_IdenticalTips_SkipWithoutInvokingPush(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{incidentRef},
		localTips:   map[string]string{incidentRef: incidentEchoTip},
		remoteTips:  map[string]string{incidentRef: incidentEchoTip},
	}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: incidentRef}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.pushCalls) != 0 {
		t.Fatalf("nothing to do should not reach git, got %+v", git.pushCalls)
	}
	assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultSkip, history.ReasonAlreadyUpToDate)
}

// Genuine divergence — neither side contains the other — keeps the previous
// behaviour: the push goes through as a forced update and alerts. There is no
// answer here that loses nothing, and stalling the mirror on it was considered
// and rejected; what the guard removes is the case that HAS a safe answer.
func TestPushGuard_Divergence_StillPushes(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{incidentRef},
		localTips:   map[string]string{incidentRef: incidentEchoTip},
		remoteTips:  map[string]string{incidentRef: incidentLostTip},
		ancestors:   map[string]bool{}, // neither direction holds
	}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: incidentRef}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("divergence must still be pushed and alerted, got %+v", git.pushCalls)
	}
}

// A full reconcile withholds only the refs that need it and carries the rest.
// One branch being ahead must not stop every other ref from being mirrored.
func TestPushGuard_FullSync_HoldsOnlyTheAheadRef(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{incidentRef, incidentOtherRef},
		localTips: map[string]string{
			incidentRef:      incidentEchoTip,
			incidentOtherRef: incidentEchoTip,
		},
		remoteTips: map[string]string{
			incidentRef: incidentLostTip, // ahead → held
			// master-b absent → created
		},
		ancestors: map[string]bool{incidentEchoTip + ">" + incidentLostTip: true},
	}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "bidi-repo", EventMeta{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("expected 1 push, got %d", len(git.pushCalls))
	}
	want := []string{incidentOtherRef}
	if got := pushedRefs(git.pushCalls[0].Specs); len(got) != 1 || got[0] != want[0] {
		t.Errorf("expected only the safe ref %v to be pushed, got %v", want, got)
	}
}

// The reconcile cron is the bigger exposure, not a lesser one: it names no ref
// and therefore pushes every branch at once, so it has to pass through the same
// per-ref guard an event does.
func TestPushGuard_CronReconcile_IsGuardedToo(t *testing.T) {
	git := &mockGitRunner{
		pushChanged:   true,
		listRefs:      []string{incidentRef},
		localTips:     map[string]string{incidentRef: incidentEchoTip},
		remoteTips:    map[string]string{incidentRef: incidentLostTip},
		objectMissing: true,
	}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	err := svc.Retry(context.Background(), "bidi-repo", "target-to-source", EventMeta{Source: SourceCron})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.pushCalls) != 0 {
		t.Fatalf("the hourly reconcile must not rewind either, got %+v", git.pushCalls)
	}
	assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultSkip, history.ReasonDestinationAhead)
}

// A lease git refused is a skip, not a failure. Returning an error here would
// retry the SQS message and eventually park it in the DLQ, for a condition the
// destination's own event resolves within seconds.
func TestPushGuard_LeaseRejection_RecordsSkipAndReturnsNil(t *testing.T) {
	git := &mockGitRunner{
		pushChanged:  false, // nothing moved
		pushRejected: []string{incidentRef + ": [rejected] (stale info)"},
		listRefs:     []string{incidentRef},
		localTips:    map[string]string{incidentRef: incidentEchoTip},
		remoteTips:   map[string]string{incidentRef: incidentLostTip},
	}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: incidentRef})
	if err != nil {
		t.Fatalf("a stale lease must not surface as an error: %v", err)
	}
	assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultSkip, history.ReasonLeaseRejected)
}

// Reading the destination is what makes the guard possible, so failing to read
// it refuses the push. A skipped sync is repaired by the next event or the
// hourly reconcile; a commit overwritten without a lease is not repaired at all.
func TestPushGuard_DestinationUnreadable_RefusesToPush(t *testing.T) {
	git := &mockGitRunner{
		pushChanged:   true,
		listRefs:      []string{incidentRef},
		remoteTipsErr: errors.New("ls-remote: connection reset"),
	}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: incidentRef})
	if err == nil {
		t.Fatal("expected an error when the destination cannot be read, got nil")
	}
	if len(git.pushCalls) != 0 {
		t.Errorf("must not push blind, got %+v", git.pushCalls)
	}
}

// --- real git ---

// The incident, replayed against real git: read the destination, let it move,
// then push the now-stale view. Without the lease this is the write that lost a
// commit; with it, git refuses and the destination keeps what it had.
//
// This is also the regression test for the '+' trap. A per-refspec force beats
// --force-with-lease outright — git applies the update without ever comparing
// the expected value — so `+ref:ref` alongside a lease reads as belt and braces
// and is in fact neither. Every mock-based test above still passes with the '+'
// restored, because a mock cannot tell you what git does with the arguments; it
// takes a real push to a real repository, which is why this test exists.
func TestDefaultGitRunner_StaleLeaseIsRejectedAndDestinationSurvives(t *testing.T) {
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

	// The tip we would have read at the start of a slow fetch.
	staleLease := gitOut(t, mirrorDir, "rev-parse", "refs/heads/main")

	// The push that lands while that fetch is still running.
	writeFile(t, srcDir+"/b.txt", "two")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "second")
	runGit(t, srcDir, "push", tgtDir, "main")
	newTip := gitOut(t, tgtDir, "rev-parse", "refs/heads/main")
	if newTip == staleLease {
		t.Fatal("setup failed: the destination did not move")
	}

	// The echo finally writes, holding the tip it read a moment ago.
	res, err := runner.PushMirror(context.Background(), mirrorDir, localRemote(tgtDir),
		[]PushSpec{{Ref: "refs/heads/main", Lease: staleLease}})
	if err != nil {
		t.Fatalf("a stale lease must not be an error: %v", err)
	}
	if len(res.Rejected) == 0 {
		t.Fatal("expected git to reject the push against a stale lease")
	}
	if res.Changed {
		t.Error("a rejected push moved nothing, so it must not report a change")
	}
	if len(res.Forced) != 0 {
		t.Errorf("nothing was overwritten, so nothing may be reported as forced: %v", res.Forced)
	}
	if got := gitOut(t, tgtDir, "rev-parse", "refs/heads/main"); got != newTip {
		t.Errorf("destination tip = %s, want it untouched at %s", got, newTip)
	}
}

// RemoteRefs must report the tag object a ref holds, not the commit underneath
// it. ls-remote prints both, and taking the peeled line would leave every
// annotated tag looking permanently different from the local side.
func TestDefaultGitRunner_RemoteRefs_KeepsTagObjectNotPeeledCommit(t *testing.T) {
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/a.txt", "one")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "first")
	runGit(t, srcDir, "tag", "-a", "v1.0.0", "-m", "annotated")

	runner := &defaultGitRunner{}
	refs, err := runner.RemoteRefs(context.Background(), localRemote(srcDir))
	if err != nil {
		t.Fatalf("RemoteRefs failed: %v", err)
	}
	tagObject := gitOut(t, srcDir, "rev-parse", "refs/tags/v1.0.0")
	commit := gitOut(t, srcDir, "rev-parse", "refs/tags/v1.0.0^{commit}")
	if tagObject == commit {
		t.Fatal("setup failed: the tag is not annotated")
	}
	if got := refs["refs/tags/v1.0.0"]; got != tagObject {
		t.Errorf("refs/tags/v1.0.0 = %s, want the tag object %s", got, tagObject)
	}
	for ref := range refs {
		if len(ref) > 3 && ref[len(ref)-3:] == "^{}" {
			t.Errorf("peeled line leaked into the map: %q", ref)
		}
	}
}

// The local and remote sides must agree on what a tag ref holds, or an
// unchanged annotated tag would be re-pushed on every single sync.
func TestDefaultGitRunner_ListRefTips_MatchesRemoteRefsForAnnotatedTags(t *testing.T) {
	srcDir := t.TempDir()
	runGit(t, srcDir, "init")
	runGit(t, srcDir, "config", "user.email", "test@test.com")
	runGit(t, srcDir, "config", "user.name", "test")
	writeFile(t, srcDir+"/a.txt", "one")
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "first")
	runGit(t, srcDir, "tag", "-a", "v1.0.0", "-m", "annotated")

	runner := &defaultGitRunner{}
	mirrorDir := t.TempDir() + "/mirror.git"
	if err := runner.CloneMirror(context.Background(), localRemote(srcDir), mirrorDir); err != nil {
		t.Fatalf("CloneMirror failed: %v", err)
	}
	local, err := runner.ListRefTips(context.Background(), mirrorDir)
	if err != nil {
		t.Fatalf("ListRefTips failed: %v", err)
	}
	remote, err := runner.RemoteRefs(context.Background(), localRemote(srcDir))
	if err != nil {
		t.Fatalf("RemoteRefs failed: %v", err)
	}
	if local["refs/tags/v1.0.0"] != remote["refs/tags/v1.0.0"] {
		t.Errorf("tag tips disagree: local %s, remote %s",
			local["refs/tags/v1.0.0"], remote["refs/tags/v1.0.0"])
	}
}

// A ref that vanished between enumeration and the destination read is skipped
// rather than pushed from a tip we no longer hold.
func TestPlanPush_RefPrunedAfterEnumeration_IsDropped(t *testing.T) {
	git := &mockGitRunner{
		localTips:  map[string]string{}, // pruned
		remoteTips: map[string]string{incidentRef: incidentLostTip},
	}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	plan, err := svc.planPush(context.Background(), "/tmp/mirror.git", localRemote("http://example.invalid/repo.git"),
		[]string{incidentRef}, false, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Specs) != 0 || len(plan.Held) != 0 {
		t.Errorf("a pruned ref should produce neither a push nor a hold, got %+v", plan)
	}
}

// --- force ---

// A deliberate rewind goes through when someone says so, and carries the tip
// they were shown as the lease.
func TestPushGuard_Force_AppliesTheRewindPinnedToTheObservedTip(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{incidentRef},
		localTips:   map[string]string{incidentRef: incidentEchoTip},
		remoteTips:  map[string]string{incidentRef: incidentLostTip},
		ancestors:   map[string]bool{incidentEchoTip + ">" + incidentLostTip: true},
	}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	err := svc.Sync(context.Background(), "bidi-repo", EventMeta{
		Ref: incidentRef, Source: SourceRetryAPI,
		Force: true, ForceLease: incidentLostTip, Actor: "alice",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.pushCalls) != 1 || len(git.pushCalls[0].Specs) != 1 {
		t.Fatalf("a forced request must push the ref, got %+v", git.pushCalls)
	}
	if got := git.pushCalls[0].Specs[0]; got.Lease != incidentLostTip {
		t.Errorf("force must carry the observed tip as the lease, got %+v", got)
	}
}

// 🔴 The regression this exists for. The force path runs after a fetch that can
// take a minute, so reading the destination at push time and using THAT as the
// lease would quietly authorise overwriting whatever arrived during the fetch —
// the 2026-08-11 accident, performed on request. The lease must be the tip the
// operator was shown, even when the destination has since moved somewhere else.
func TestPushGuard_Force_LeaseIsTheOperatorsTipNotTheCurrentOne(t *testing.T) {
	const movedOnSince = "cccccccccccccccccccccccccccccccccccccccc"
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{incidentRef},
		localTips:   map[string]string{incidentRef: incidentEchoTip},
		// The destination is no longer where the operator saw it.
		remoteTips: map[string]string{incidentRef: movedOnSince},
	}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	err := svc.Sync(context.Background(), "bidi-repo", EventMeta{
		Ref: incidentRef, Source: SourceConsole,
		Force: true, ForceLease: incidentLostTip, Actor: "alice",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := git.pushCalls[0].Specs[0]
	if got.Lease == movedOnSince {
		t.Fatal("force adopted the destination's current tip as the lease — " +
			"that authorises overwriting whatever landed since the operator looked")
	}
	if got.Lease != incidentLostTip {
		t.Errorf("lease = %q, want the operator's tip %q", got.Lease, incidentLostTip)
	}
}

// The same destination without the flag stays held — the bypass has to be
// asked for, never inferred.
func TestPushGuard_WithoutForce_SameSituationStaysHeld(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{incidentRef},
		localTips:   map[string]string{incidentRef: incidentEchoTip},
		remoteTips:  map[string]string{incidentRef: incidentLostTip},
		ancestors:   map[string]bool{incidentEchoTip + ">" + incidentLostTip: true},
	}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: incidentRef}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.pushCalls) != 0 {
		t.Fatalf("expected the push to stay held, got %+v", git.pushCalls)
	}
}

// Force also covers the unknown-destination branch, which is the one a person
// hits after the source mirror was rebuilt and no longer knows the old tip.
func TestPushGuard_Force_AlsoCoversAnUnknownDestinationTip(t *testing.T) {
	git := &mockGitRunner{
		pushChanged:   true,
		listRefs:      []string{incidentRef},
		localTips:     map[string]string{incidentRef: incidentEchoTip},
		remoteTips:    map[string]string{incidentRef: incidentLostTip},
		objectMissing: true,
	}
	svc := newTestService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	err := svc.Sync(context.Background(), "bidi-repo",
		EventMeta{Ref: incidentRef, Source: SourceRetryAPI, Force: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("expected the forced push to go through, got %+v", git.pushCalls)
	}
}

// --- held notification ---

// A held branch is announced, because either someone is waiting on a rewind
// that did not happen or an echo lost a race, and only the log distinguishes
// them after the fact.
func TestReportHeld_BranchAlerts(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{incidentRef},
		localTips:   map[string]string{incidentRef: incidentEchoTip},
		remoteTips:  map[string]string{incidentRef: incidentLostTip},
		ancestors:   map[string]bool{incidentEchoTip + ">" + incidentLostTip: true},
	}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: incidentRef}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found *notify.Message
	for i := range notif.messages {
		if strings.HasPrefix(notif.messages[i].Title, "Push Withheld:") {
			found = &notif.messages[i]
		}
	}
	if found == nil {
		t.Fatalf("no withheld alert sent; got %+v", notif.messages)
	}
	if !strings.Contains(found.Body, incidentLostTip) {
		t.Errorf("the alert must name the destination tip, got %q", found.Body)
	}
	if !strings.Contains(found.Body, incidentRef) {
		t.Errorf("the alert must name the ref, got %q", found.Body)
	}
}

// A tag held stays quiet, the same way a tag overwritten does. A pipeline that
// reuses build tag names re-points them constantly, and an alert that fires on
// routine traffic is one people learn to ignore.
func TestReportHeld_TagStaysQuiet(t *testing.T) {
	const tagRef = "refs/tags/Build-2312"
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{tagRef},
		localTips:   map[string]string{tagRef: incidentEchoTip},
		remoteTips:  map[string]string{tagRef: incidentLostTip},
		ancestors:   map[string]bool{incidentEchoTip + ">" + incidentLostTip: true},
	}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: tagRef}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, msg := range notif.messages {
		if strings.HasPrefix(msg.Title, "Push Withheld:") {
			t.Errorf("a held tag must not alert, got %+v", msg)
		}
	}
}

// Nothing held, nothing said.
func TestReportHeld_NoHoldNoAlert(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{incidentRef},
		localTips:   map[string]string{incidentRef: incidentLostTip},
		remoteTips:  map[string]string{incidentRef: incidentEchoTip},
	}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: incidentRef}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, msg := range notif.messages {
		if strings.HasPrefix(msg.Title, "Push Withheld:") {
			t.Errorf("nothing was held, so nothing may be announced: %+v", msg)
		}
	}
}

// The alert has to hand over something that runs. A reader deciding whether
// their rewind went through should not be assembling a JSON body out of three
// fields scattered up the message — that is the step that gets fumbled.
func TestReportHeld_AlertCarriesARunnableForceCommand(t *testing.T) {
	git := &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{incidentRef},
		localTips:   map[string]string{incidentRef: incidentEchoTip},
		remoteTips:  map[string]string{incidentRef: incidentLostTip},
		ancestors:   map[string]bool{incidentEchoTip + ">" + incidentLostTip: true},
	}
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	// Sync() is source→target for bidi-repo: codecommit is the source, so this
	// leg is the one the retry API calls source-to-target.
	if err := svc.Sync(context.Background(), "bidi-repo", EventMeta{Ref: incidentRef}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var body string
	for _, msg := range notif.messages {
		if strings.HasPrefix(msg.Title, "Push Withheld:") {
			body = msg.Body
		}
	}
	if body == "" {
		t.Fatalf("no withheld alert sent; got %+v", notif.messages)
	}
	for _, want := range []string{
		"/retry/mirror",
		`"repo":"bidi-repo"`,
		`"direction":"source-to-target"`,
		`"ref":"` + incidentRef + `"`,
		`"force":true`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("alert is missing %q:\n%s", want, body)
		}
	}
	// The token is the one thing this process must never write into a message
	// that lands in a channel.
	if !strings.Contains(body, "$RETRY_API_TOKEN") {
		t.Errorf("the command must reference the token by name, never inline it:\n%s", body)
	}
}

// The direction words point at the config's source and target, not at the two
// providers, so the leg that reads as "gitlab to codecommit" is target-to-source
// on a CodeCommit-sourced repo. Getting this backwards hands over a command
// that syncs the wrong way.
func TestRetryDirectionFor_NamesTheLegFromTheConfigsPointOfView(t *testing.T) {
	repo := defaultRepos()[0]
	if got := retryDirectionFor(repo, repo.Source, repo.Target); got != "source-to-target" {
		t.Errorf("source→target = %q, want source-to-target", got)
	}
	if got := retryDirectionFor(repo, repo.Target, repo.Source); got != "target-to-source" {
		t.Errorf("target→source = %q, want target-to-source", got)
	}
	if got := retryDirectionFor(repo, "somewhere", "else"); got != "" {
		t.Errorf("an unrecognised pair must not guess, got %q", got)
	}
}

// A ref name is not safe to paste into a JSON literal.
func TestForceRequestBody_EscapesTheRef(t *testing.T) {
	got := forceRequestBody("r", "auto", `refs/heads/we"ird`, "deadbeef")
	if !strings.Contains(got, `refs/heads/we\"ird`) {
		t.Errorf("ref was not escaped: %s", got)
	}
}

// --- rejection classification ---

func TestAllStaleLease_OnlyTheLeaseCountsAsBenign(t *testing.T) {
	for name, tc := range map[string]struct {
		rejected []string
		want     bool
	}{
		"lease fired":      {[]string{"refs/heads/b: [rejected] (stale info)"}, true},
		"two leases":       {[]string{"a: (stale info)", "b: (stale info)"}, true},
		"protected hook":   {[]string{"refs/heads/main: [remote rejected] (pre-receive hook declined)"}, false},
		"mixed":            {[]string{"a: (stale info)", "b: (pre-receive hook declined)"}, false},
		"nothing rejected": {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := allStaleLease(tc.rejected); got != tc.want {
				t.Errorf("allStaleLease(%v) = %v, want %v", tc.rejected, got, tc.want)
			}
		})
	}
}

// --- ForcePush direction resolution ---
//
// This is the mapping the console comment calls "the one call where getting it
// backwards overwrites the wrong repository", so it is tested here rather than
// only through a fake in the server package.

func TestForcePush_ResolvesTheDestinationToADirection(t *testing.T) {
	repo := defaultRepos()[1] // bidirectional, so both sides are writable
	for name, tc := range map[string]struct {
		to       string
		wantFrom string
		wantTo   string
	}{
		// The new notation (provider name) — what the console renders today.
		"target side": {"gitlab-main/team/bidi-repo", "codecommit-eu/bidi-repo", "gitlab-main/team/bidi-repo"},
		"source side": {"codecommit-eu/bidi-repo", "gitlab-main/team/bidi-repo", "codecommit-eu/bidi-repo"},
		// The old notation (provider type) — what the button on a history row still
		// on the volume sends. Not accepting it here would refuse every restore and
		// force-push on those rows.
		"target side, legacy notation": {"gitlab/team/bidi-repo", "codecommit-eu/bidi-repo", "gitlab-main/team/bidi-repo"},
		"source side, legacy notation": {"codecommit/bidi-repo", "gitlab-main/team/bidi-repo", "codecommit-eu/bidi-repo"},
	} {
		t.Run(name, func(t *testing.T) {
			git := &mockGitRunner{pushChanged: true, listRefs: []string{incidentRef}}
			svc, rec := newRecordingService([]config.RepoConfig{repo}, makeProviders(), &mockNotifier{}, git)

			err := svc.ForcePush(context.Background(), repo.Name, tc.to, incidentRef, incidentLostTip, "alice")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			ev := rec.only(t)
			// What is recorded is always the new notation, old notation in or not.
			if ev.To != tc.wantTo || ev.From != tc.wantFrom {
				t.Errorf("route = %s → %s, want %s → %s", ev.From, ev.To, tc.wantFrom, tc.wantTo)
			}
			if ev.Actor != "alice" {
				t.Errorf("actor = %q, want alice", ev.Actor)
			}
		})
	}
}

func TestForcePush_RefusesAnUnknownSideOrRepo(t *testing.T) {
	repo := defaultRepos()[1]
	for name, tc := range map[string]struct {
		repoName, to string
		wantReason   string
	}{
		"unknown side": {repo.Name, "github/somewhere/else", history.ReasonUnknownSide},
		"unknown repo": {"not-configured", "gitlab/team/bidi-repo", history.ReasonUnknownRepo},
	} {
		t.Run(name, func(t *testing.T) {
			git := &mockGitRunner{pushChanged: true}
			svc, rec := newRecordingService([]config.RepoConfig{repo}, makeProviders(), &mockNotifier{}, git)

			err := svc.ForcePush(context.Background(), tc.repoName, tc.to, incidentRef, incidentLostTip, "alice")
			if err == nil {
				t.Fatal("expected a refusal, got nil")
			}
			if len(git.pushCalls) != 0 {
				t.Errorf("a refused force must not push, got %+v", git.pushCalls)
			}
			assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultFail, tc.wantReason)
		})
	}
}

// --- DirectionTo ---
//
// The console's re-run button resolves through this instead of sending "auto",
// so a failed leg is re-run in the direction that failed rather than in the one
// retry_direction happens to pin.

func TestDirectionTo_ResolvesEitherSide(t *testing.T) {
	repo := defaultRepos()[1] // bidirectional, so both sides resolve
	svc, _ := newRecordingService([]config.RepoConfig{repo}, makeProviders(), &mockNotifier{}, &mockGitRunner{})

	for name, tc := range map[string]struct{ to, want string }{
		"target side": {"gitlab-main/team/bidi-repo", config.DirectionSourceToTarget},
		"source side": {"codecommit-eu/bidi-repo", config.DirectionTargetToSource},
		// The button on an older row recorded under the type notation must stay alive.
		"target side, legacy notation": {"gitlab/team/bidi-repo", config.DirectionSourceToTarget},
		"source side, legacy notation": {"codecommit/bidi-repo", config.DirectionTargetToSource},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := svc.DirectionTo(repo.Name, tc.to)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("DirectionTo(%q) = %q, want %q", tc.to, got, tc.want)
			}
		})
	}
}

func TestDirectionTo_RefusesAnUnknownSideOrRepo(t *testing.T) {
	repo := defaultRepos()[1]
	svc, _ := newRecordingService([]config.RepoConfig{repo}, makeProviders(), &mockNotifier{}, &mockGitRunner{})

	for name, tc := range map[string]struct {
		repoName, to string
		want         error
	}{
		"unknown side": {repo.Name, "github/somewhere/else", ErrUnknownSide},
		"unknown repo": {"not-configured", "gitlab/team/bidi-repo", ErrUnknownRepo},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.DirectionTo(tc.repoName, tc.to); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// A one-way repo still records the side it declined to write, so the button can
// sit under a row whose direction the repo forbids. Resolving it must refuse
// rather than hand back a direction Retry would then run.
func TestDirectionTo_RefusesADirectionTheRepoForbids(t *testing.T) {
	repo := defaultRepos()[0] // source-to-target only
	svc, _ := newRecordingService([]config.RepoConfig{repo}, makeProviders(), &mockNotifier{}, &mockGitRunner{})

	if _, err := svc.DirectionTo(repo.Name, "codecommit/my-repo"); !errors.Is(err, ErrDirectionNotAllowed) {
		t.Errorf("error = %v, want ErrDirectionNotAllowed", err)
	}
}

// A repo pinned one-way must not be forced the other way — the same gate a
// restore passes.
func TestForcePush_RefusesADirectionTheRepoForbids(t *testing.T) {
	repo := defaultRepos()[0] // source-to-target only
	git := &mockGitRunner{pushChanged: true, listRefs: []string{incidentRef}}
	svc, rec := newRecordingService([]config.RepoConfig{repo}, makeProviders(), &mockNotifier{}, git)

	// Writing to the SOURCE side is target-to-source, which this repo forbids.
	err := svc.ForcePush(context.Background(), repo.Name, "codecommit/my-repo", incidentRef, incidentLostTip, "alice")

	if err == nil {
		t.Fatal("expected the direction gate to refuse, got nil")
	}
	if len(git.pushCalls) != 0 {
		t.Errorf("a refused force must not push, got %+v", git.pushCalls)
	}
	assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultFail, history.ReasonDirection)
}

// --- An endpoint that names both sides ---
//
// The old notation is provider-type based, so on a repo that mirrors two
// instances of the same type over the same path both sides collapse to the same
// string. A deployed config can hold exactly that shape: a two-hop repo that
// joins gitlab-old to gitlab-main over one path. Picking a side by first
// match makes the console button run the opposite leg.

func sameTypeSamePathRepo() (config.RepoConfig, map[string]provider.Provider) {
	providers := makeProviders()
	providers["gitlab-old"] = NewGitLab(config.ProviderConfig{
		Type:    "gitlab",
		BaseURL: "https://gitlab-old.example.com",
		Credentials: map[string]string{
			"token": "glpat-test",
		},
	})
	return config.RepoConfig{
		Name:       "same-path-repo",
		Source:     "gitlab-old",
		Target:     "gitlab-main",
		SourcePath: "server/same-path-repo",
		TargetPath: "server/same-path-repo",
		Direction:  "bidirectional",
	}, providers
}

func TestDirectionTo_RefusesAnEndpointThatNamesBothSides(t *testing.T) {
	repo, providers := sameTypeSamePathRepo()
	svc, _ := newRecordingService([]config.RepoConfig{repo}, providers, &mockNotifier{}, &mockGitRunner{})

	// Under the old notation both sides read "gitlab/server/same-path-repo".
	if _, err := svc.DirectionTo(repo.Name, "gitlab/server/same-path-repo"); !errors.Is(err, ErrUnknownSide) {
		t.Errorf("error = %v, want ErrUnknownSide", err)
	}

	// The new notation carries the provider name and cannot collide, so it must
	// still resolve — only the ambiguous case is refused, not every button on
	// this repo.
	for to, want := range map[string]string{
		"gitlab-main/server/same-path-repo": config.DirectionSourceToTarget,
		"gitlab-old/server/same-path-repo":  config.DirectionTargetToSource,
	} {
		got, err := svc.DirectionTo(repo.Name, to)
		if err != nil {
			t.Fatalf("DirectionTo(%q) unexpected error: %v", to, err)
		}
		if got != want {
			t.Errorf("DirectionTo(%q) = %q, want %q", to, got, want)
		}
	}
}

// A restore creates a ref that is not there. Guessing the side creates it on the
// wrong instance and that is the end of it, which makes this the least
// recoverable of the three paths the ambiguity reaches.
func TestRestoreRef_RefusesAnEndpointThatNamesBothSides(t *testing.T) {
	repo, providers := sameTypeSamePathRepo()
	git := &mockGitRunner{}
	svc, rec := newRecordingService([]config.RepoConfig{repo}, providers, &mockNotifier{}, git)

	err := svc.RestoreRef(context.Background(), repo.Name, "gitlab/server/same-path-repo",
		"branch", "feature-x", incidentLostTip, "alice")

	if !errors.Is(err, ErrUnknownSide) {
		t.Fatalf("error = %v, want ErrUnknownSide", err)
	}
	if len(git.createRefCalls) != 0 {
		t.Errorf("must not write when refusing, got %d CreateRef calls", len(git.createRefCalls))
	}
	assertOutcome(t, rec.only(t), history.ActionRestore, history.ResultFail, history.ReasonUnknownSide)
}

func TestForcePush_RefusesAnEndpointThatNamesBothSides(t *testing.T) {
	repo, providers := sameTypeSamePathRepo()
	git := &mockGitRunner{pushChanged: true, listRefs: []string{incidentRef}}
	svc, rec := newRecordingService([]config.RepoConfig{repo}, providers, &mockNotifier{}, git)

	err := svc.ForcePush(context.Background(), repo.Name, "gitlab/server/same-path-repo",
		incidentRef, incidentLostTip, "alice")

	if !errors.Is(err, ErrUnknownSide) {
		t.Fatalf("error = %v, want ErrUnknownSide", err)
	}
	if len(git.pushCalls) != 0 {
		t.Errorf("a refused force must not push, got %+v", git.pushCalls)
	}
	assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultFail, history.ReasonUnknownSide)
}

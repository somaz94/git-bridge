package mirror

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"git-bridge/internal/history"
	"git-bridge/internal/notify"
)

// The shape of the 2026-09-17 demo-repo rewrite, kept so the tests read as the
// event rather than an abstraction of it: version/4.5.0 was rebased onto a
// newer base and force-pushed, the mirror overwrote 3080df4 with b43fdce, and
// the alert said history was discarded — while both commits carried the same
// patch-id and nothing had been lost.
const (
	rewriteRef = "refs/heads/version/4.5.0"
	rewriteOld = "3080df47a47ddcd0b766c387fdf93afa5bdd1a4a"
	rewriteNew = "b43fdce0afacb5a0e3cce4633b7123cad522d7f1"
)

// --- UnmatchedCommits against real git ---

// rewriteRepo is a scratch repository for building the shapes a force-push can
// take. Every commit touches its own file unless a test says otherwise, so a
// rebase replays cleanly by default.
type rewriteRepo struct {
	t   *testing.T
	dir string
}

func newRewriteRepo(t *testing.T) *rewriteRepo {
	t.Helper()
	r := &rewriteRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.email", "test@test.com")
	r.git("config", "user.name", "tester")
	r.commit("base.txt", "base", "base")
	return r
}

func (r *rewriteRepo) git(args ...string) string {
	r.t.Helper()
	return runGitOut(r.t, r.dir, args...)
}

func (r *rewriteRepo) commit(file, content, msg string) string {
	r.t.Helper()
	writeFile(r.t, r.dir+"/"+file, content)
	r.git("add", file)
	r.git("commit", "-q", "-m", msg)
	return strings.TrimSpace(r.git("rev-parse", "HEAD"))
}

// runGitMaybe runs a git command whose failure is part of the scenario — a
// rebase that stops on a conflict exits non-zero by design.
func runGitMaybe(dir string, args ...string) ([]byte, error) {
	return exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
}

// runGitEnv runs a git command with extra environment, for the steps that would
// otherwise open an editor.
func runGitEnv(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func (r *rewriteRepo) unmatched(kept, discarded string) ([]history.LostCommit, int) {
	r.t.Helper()
	lost, total, err := (&defaultGitRunner{}).UnmatchedCommits(context.Background(), r.dir, kept, discarded)
	if err != nil {
		r.t.Fatalf("UnmatchedCommits(%s, %s) error = %v", kept, discarded, err)
	}
	if len(lost) > total {
		r.t.Fatalf("listed %d commits but counted %d", len(lost), total)
	}
	return lost, total
}

// A rebase onto a newer base re-creates every commit under a new SHA with the
// same diff. That is the whole of what happened on 2026-09-17, and it must come
// out as nothing lost.
func TestUnmatchedCommits_CleanRebaseLosesNothing(t *testing.T) {
	r := newRewriteRepo(t)
	r.git("checkout", "-q", "-b", "feature")
	r.commit("a.txt", "a", "feature one")
	old := r.commit("b.txt", "b", "feature two")
	r.git("checkout", "-q", "main")
	r.commit("main.txt", "main", "main moved on")
	r.git("checkout", "-q", "feature")
	r.git("rebase", "-q", "main")
	rebased := strings.TrimSpace(r.git("rev-parse", "HEAD"))

	if lost, total := r.unmatched(rebased, old); total != 0 {
		t.Errorf("a clean rebase lost %d commits: %+v", total, lost)
	}
}

// Replacing the work with something else is the loss the alert exists for.
// Author and subject have to come through, because they are what tells the
// reader whose work is gone.
func TestUnmatchedCommits_ResetAndReplaceListsTheDiscardedWork(t *testing.T) {
	r := newRewriteRepo(t)
	base := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	r.commit("a.txt", "a", "first discarded")
	old := r.commit("b.txt", "b", "second discarded")
	r.git("reset", "-q", "--hard", base)
	replacement := r.commit("c.txt", "c", "replacement")

	lost, total := r.unmatched(replacement, old)
	if total != 2 || len(lost) != 2 {
		t.Fatalf("lost = %+v (total %d), want both discarded commits", lost, total)
	}
	// Newest first, the order git log walks.
	if lost[0].SHA != old || lost[0].Subject != "second discarded" || lost[0].Author != "tester" {
		t.Errorf("lost[0] = %+v, want the old tip with its subject and author", lost[0])
	}
	if lost[1].Subject != "first discarded" {
		t.Errorf("lost[1] = %+v, want the older discarded commit", lost[1])
	}
}

// A rebase that had to resolve a conflict changes that commit's diff, so its
// patch-id no longer matches. The check cannot tell a careful resolution from a
// careless one, and it errs toward the alert — but only for that commit.
func TestUnmatchedCommits_ConflictResolvedDuringRebaseIsReported(t *testing.T) {
	r := newRewriteRepo(t)
	r.git("checkout", "-q", "-b", "feature")
	r.commit("shared.txt", "feature\n", "touches shared")
	old := r.commit("own.txt", "own", "touches only its own file")
	r.git("checkout", "-q", "main")
	r.commit("shared.txt", "main\n", "main also touches shared")
	r.git("checkout", "-q", "feature")
	// The rebase stops on the conflict; resolve it with both sides.
	_, _ = runGitMaybe(r.dir, "rebase", "main")
	writeFile(t, r.dir+"/shared.txt", "main\nfeature\n")
	r.git("add", "shared.txt")
	runGitEnv(t, r.dir, []string{"GIT_EDITOR=true"}, "rebase", "--continue")
	rebased := strings.TrimSpace(r.git("rev-parse", "HEAD"))

	lost, total := r.unmatched(rebased, old)
	if total != 1 || len(lost) != 1 || lost[0].Subject != "touches shared" {
		t.Errorf("lost = %+v (total %d), want only the commit whose conflict was resolved", lost, total)
	}
}

// A merge commit has no patch-id, so it is never matched — even when the rebase
// that replaced it lost nothing. The resolution a merge can carry is exactly
// what a patch comparison cannot see, so this is the safe direction to be wrong.
func TestUnmatchedCommits_MergeCommitIsNeverMatched(t *testing.T) {
	r := newRewriteRepo(t)
	r.git("checkout", "-q", "-b", "feature")
	r.commit("a.txt", "a", "feature work")
	r.git("checkout", "-q", "main")
	r.commit("main.txt", "main", "main moved on")
	r.git("checkout", "-q", "feature")
	r.git("merge", "-q", "--no-edit", "main")
	merged := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	r.git("checkout", "-q", "-b", "rebased", "HEAD~1")
	r.git("rebase", "-q", "main")
	rebased := strings.TrimSpace(r.git("rev-parse", "HEAD"))

	lost, total := r.unmatched(rebased, merged)
	if total != 1 || len(lost) != 1 || lost[0].SHA != merged {
		t.Errorf("lost = %+v (total %d), want only the merge commit", lost, total)
	}
}

// The case the first version of this check got wrong. The patch-id behind
// `git log --cherry-mark` ignores whitespace, so moving a YAML key out of its
// parent matched the original — a different document, reported as nothing lost.
func TestUnmatchedCommits_WhitespaceOnlyDifferenceIsReported(t *testing.T) {
	r := newRewriteRepo(t)
	base := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	old := r.commit("values.yaml", "a: 1\nc:\n  d: 2\n", "add c.d")
	r.git("reset", "-q", "--hard", base)
	r.commit("other.txt", "moved", "base moved on")
	replaced := r.commit("values.yaml", "a: 1\nc:\nd: 2\n", "add c.d")

	lost, total := r.unmatched(replaced, old)
	if total != 1 || len(lost) != 1 || lost[0].SHA != old {
		t.Errorf("lost = %+v (total %d), want the re-indented commit reported", lost, total)
	}
}

// Without --binary both sides of a binary change print "Binary files differ",
// and any two binary edits to the same path would match.
func TestUnmatchedCommits_DifferentBinaryContentIsReported(t *testing.T) {
	r := newRewriteRepo(t)
	base := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	old := r.commit("asset.bin", "\x00\x01old bytes", "update asset")
	r.git("reset", "-q", "--hard", base)
	r.commit("other.txt", "moved", "base moved on")
	replaced := r.commit("asset.bin", "\x00\x01new bytes", "update asset")

	if lost, total := r.unmatched(replaced, old); total != 1 {
		t.Errorf("lost = %+v (total %d), want the binary change reported", lost, total)
	}
}

// The same binary change replayed onto a new base is still a match.
func TestUnmatchedCommits_RebasedBinaryChangeLosesNothing(t *testing.T) {
	r := newRewriteRepo(t)
	r.commit("asset.bin", "\x00\x01v1", "add asset")
	r.git("checkout", "-q", "-b", "feature")
	old := r.commit("asset.bin", "\x00\x01v2", "update asset")
	r.git("checkout", "-q", "main")
	r.commit("other.txt", "moved", "base moved on")
	r.git("checkout", "-q", "feature")
	r.git("rebase", "-q", "main")
	rebased := strings.TrimSpace(r.git("rev-parse", "HEAD"))

	if lost, total := r.unmatched(rebased, old); total != 0 {
		t.Errorf("a rebased binary change lost %d commits: %+v", total, lost)
	}
}

// A rebase onto a base that edited another hunk of the same file shifts the line
// numbers and the blob names in the patch. Neither is content, and neither may
// turn a clean rebase into an alert.
func TestUnmatchedCommits_RebaseOverAnotherHunkOfTheSameFileLosesNothing(t *testing.T) {
	r := newRewriteRepo(t)
	var lines strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&lines, "%d\n", i)
	}
	r.commit("f.txt", lines.String(), "forty lines")
	r.git("checkout", "-q", "-b", "feature")
	old := r.commit("f.txt", strings.Replace(lines.String(), "\n35\n", "\n35 feature\n", 1), "edit near the end")
	r.git("checkout", "-q", "main")
	r.commit("f.txt", strings.Replace(lines.String(), "\n2\n", "\n2 main\n", 1), "edit near the start")
	r.git("checkout", "-q", "feature")
	r.git("rebase", "-q", "main")
	rebased := strings.TrimSpace(r.git("rev-parse", "HEAD"))

	if lost, total := r.unmatched(rebased, old); total != 0 {
		t.Errorf("a rebase over another hunk lost %d commits: %+v", total, lost)
	}
}

// An empty commit carries no content, so discarding one loses none — it must not
// turn an otherwise clean rewrite into an alert.
func TestUnmatchedCommits_DiscardedEmptyCommitIsNotALoss(t *testing.T) {
	r := newRewriteRepo(t)
	base := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	r.git("commit", "-q", "--allow-empty", "-m", "trigger CI")
	old := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	r.git("reset", "-q", "--hard", base)
	replaced := r.commit("other.txt", "x", "something else")

	if lost, total := r.unmatched(replaced, old); total != 0 {
		t.Errorf("an empty commit was reported lost: %+v (total %d)", lost, total)
	}
}

// A comparison too large to run inside the push's timeout is refused, and a
// refusal is an error — which the caller turns into the alert.
func TestUnmatchedCommits_RefusesAComparisonPastTheLimit(t *testing.T) {
	r := newRewriteRepo(t)
	base := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	r.commit("a.txt", "a", "one")
	old := r.commit("b.txt", "b", "two")
	r.git("reset", "-q", "--hard", base)
	replaced := r.commit("c.txt", "c", "three")

	prev := maxClassifyCommits
	maxClassifyCommits = 2
	t.Cleanup(func() { maxClassifyCommits = prev })

	_, _, err := (&defaultGitRunner{}).UnmatchedCommits(context.Background(), r.dir, replaced, old)
	if err == nil || !strings.Contains(err.Error(), "more than the 2") {
		t.Errorf("err = %v, want a refusal naming the limit", err)
	}
}

func TestParseSideCommits_RejectsAMalformedLine(t *testing.T) {
	if _, err := parseSideCommits("abc\x00def"); err == nil {
		t.Error("expected an error for a line with too few fields")
	}
	got, err := parseSideCommits("s1\x00p1 p2\x00ann\x00merge a\x00b\ns2\x00p1\x00bob\x00plain\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].merge || got[1].merge || got[0].Subject != "merge a\x00b" || got[1].Author != "bob" {
		t.Errorf("parseSideCommits = %+v", got)
	}
}

func TestParsePatchIDs_RejectsAMalformedLine(t *testing.T) {
	if _, err := parsePatchIDs("onlyonefield\n"); err == nil {
		t.Error("expected an error for a line without a commit")
	}
	got, err := parsePatchIDs("id1 c1\nid2 c2\n")
	if err != nil || got["c1"] != "id1" || got["c2"] != "id2" {
		t.Errorf("parsePatchIDs = %v, %v", got, err)
	}
}

// A long discarded branch lists a bounded number of commits and counts the rest.
func TestUnmatchedCommits_CapsTheListButCountsEverything(t *testing.T) {
	r := newRewriteRepo(t)
	base := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	const n = history.MaxLostCommits + 3
	var old string
	for i := range n {
		old = r.commit(fmt.Sprintf("f%d.txt", i), "x", fmt.Sprintf("discarded %d", i))
	}
	r.git("reset", "-q", "--hard", base)
	replacement := r.commit("other.txt", "y", "replacement")

	lost, total := r.unmatched(replacement, old)
	if total != n {
		t.Errorf("total = %d, want %d", total, n)
	}
	if len(lost) != history.MaxLostCommits {
		t.Errorf("listed %d commits, want the cap of %d", len(lost), history.MaxLostCommits)
	}
}

// A subject is free text. Splitting on anything a subject can contain would
// shift author into subject, so the separator is NUL.
func TestUnmatchedCommits_SubjectWithSeparatorLikeCharactersSurvives(t *testing.T) {
	r := newRewriteRepo(t)
	base := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	const subject = "fix: a | b \t c : d"
	old := r.commit("a.txt", "a", subject)
	r.git("reset", "-q", "--hard", base)
	replacement := r.commit("b.txt", "b", "replacement")

	lost, _ := r.unmatched(replacement, old)
	if len(lost) != 1 || lost[0].Subject != subject || lost[0].Author != "tester" {
		t.Errorf("lost = %+v, want subject %q intact", lost, subject)
	}
}

// A tip that is not in the repository is an error, never "nothing lost".
func TestUnmatchedCommits_MissingTipIsAnError(t *testing.T) {
	r := newRewriteRepo(t)
	head := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	_, _, err := (&defaultGitRunner{}).UnmatchedCommits(context.Background(), r.dir, head, rewriteOld)
	if err == nil {
		t.Fatal("expected an error for a tip that is not present")
	}
}

// classifyForced reads both tips out of the source's mirror dir after the push.
// That only works if a fetch that moved the ref leaves the old tip's objects
// behind — this drives the real fetch and push to prove it does, rather than
// trusting that git keeps unreachable objects until gc.
func TestDefaultGitRunner_OldTipSurvivesTheFetchForClassification(t *testing.T) {
	src := newRewriteRepo(t)
	src.git("checkout", "-q", "-b", "feature")
	src.commit("a.txt", "a", "feature work")
	src.git("checkout", "-q", "main")
	src.commit("main.txt", "main", "main moved on")

	tgtDir := t.TempDir()
	runGit(t, tgtDir, "init", "--bare")
	runner := &defaultGitRunner{}
	ctx := context.Background()
	mirrorDir := t.TempDir() + "/mirror.git"
	if err := runner.CloneMirror(ctx, localRemote(src.dir), mirrorDir); err != nil {
		t.Fatalf("CloneMirror: %v", err)
	}
	if _, err := runner.PushMirror(ctx, mirrorDir, localRemote(tgtDir), localSpecs(t, runner, mirrorDir)); err != nil {
		t.Fatalf("initial PushMirror: %v", err)
	}

	src.git("checkout", "-q", "feature")
	src.git("rebase", "-q", "main")
	if err := runner.FetchMirror(ctx, localRemote(src.dir), mirrorDir); err != nil {
		t.Fatalf("FetchMirror: %v", err)
	}
	res, err := runner.PushMirror(ctx, mirrorDir, localRemote(tgtDir), specsAgainst(t, runner, mirrorDir, tgtDir))
	if err != nil {
		t.Fatalf("rewriting PushMirror: %v", err)
	}
	var feature *history.ForcedRef
	for i := range res.Forced {
		if res.Forced[i].Ref == "refs/heads/feature" {
			feature = &res.Forced[i]
		}
	}
	if feature == nil {
		t.Fatalf("the rebase was not reported as forced: %+v", res.Forced)
	}

	svc := &Service{git: runner}
	forced := []history.ForcedRef{*feature}
	svc.classifyForced(ctx, mirrorDir, forced, testLogger())
	if !forced[0].Preserved {
		t.Errorf("a rebase pushed through the real mirror was not recognised as preserved: %+v", forced[0])
	}
}

// --- alerting ---

func rewriteGit(unmatched mockUnmatched, forced ...history.ForcedRef) *mockGitRunner {
	return &mockGitRunner{
		pushChanged: true,
		listRefs:    []string{rewriteRef, "refs/heads/main", "refs/tags/build-1"},
		pushForced:  forced,
		unmatched:   map[string]mockUnmatched{rewriteNew + "..." + rewriteOld: unmatched},
	}
}

func forcedAlert(msgs []notify.Message) *notify.Message {
	for i := range msgs {
		if strings.HasPrefix(msgs[i].Title, "Forced Update:") {
			return &msgs[i]
		}
	}
	return nil
}

// The fix itself: a rewrite that lost nothing is recorded as rewritten and does
// not interrupt anyone.
func TestForcedUpdate_PreservedRewriteDoesNotAlert(t *testing.T) {
	git := rewriteGit(mockUnmatched{}, history.ForcedRef{Ref: rewriteRef, Old: rewriteOld, New: rewriteNew})
	notif := &mockNotifier{}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{Ref: rewriteRef}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	if alert := forcedAlert(notif.messages); alert != nil {
		t.Fatalf("a rewrite that lost nothing must not alert, got %+v", alert)
	}
	ev := rec.only(t)
	assertOutcome(t, ev, history.ActionMirror, history.ResultOK, history.ReasonRewritten)
	if !ev.IsForced() || !ev.Forced[0].Preserved {
		t.Errorf("the rewrite must still be recorded, and as preserved: %+v", ev.Forced)
	}
}

// Silence in the alert channel is not silence everywhere: the success message
// that does go out names the rewrite.
func TestForcedUpdate_PreservedRewriteIsNamedInTheSuccessMessage(t *testing.T) {
	git := rewriteGit(mockUnmatched{}, history.ForcedRef{Ref: rewriteRef, Old: rewriteOld, New: rewriteNew})
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{Ref: rewriteRef}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	if len(notif.messages) != 1 || !strings.HasPrefix(notif.messages[0].Title, "Mirror Sync:") {
		t.Fatalf("want exactly the success message, got %+v", notif.messages)
	}
	if want := "Rewritten: " + rewriteRef + " (content preserved)"; !strings.Contains(notif.messages[0].Body, want) {
		t.Errorf("success message missing %q:\n%s", want, notif.messages[0].Body)
	}
}

// A real loss still alerts, and now says what was lost and by whom.
func TestForcedUpdate_LostCommitsAreListedInTheAlert(t *testing.T) {
	lost := []history.LostCommit{
		{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Author: "alice", Subject: "first discarded"},
		{SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Author: "bob", Subject: "second discarded"},
	}
	git := rewriteGit(mockUnmatched{lost: lost, total: 2}, history.ForcedRef{Ref: rewriteRef, Old: rewriteOld, New: rewriteNew})
	notif := &mockNotifier{}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{Ref: rewriteRef}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	alert := forcedAlert(notif.messages)
	if alert == nil {
		t.Fatalf("a rewrite that lost commits must alert, got %+v", notif.messages)
	}
	for _, want := range []string{
		"lost aaaaaaaaaa alice: first discarded",
		"lost bbbbbbbbbb bob: second discarded",
		"git fetch <clone-url> " + rewriteOld,
	} {
		if !strings.Contains(alert.Body, want) {
			t.Errorf("alert missing %q:\n%s", want, alert.Body)
		}
	}
	ev := rec.only(t)
	assertOutcome(t, ev, history.ActionMirror, history.ResultOK, history.ReasonForcedUpdate)
	if ev.Forced[0].Preserved || ev.Forced[0].LostTotal != 2 || len(ev.Forced[0].Lost) != 2 {
		t.Errorf("recorded forced ref = %+v, want two lost commits and not preserved", ev.Forced[0])
	}
}

// The list is capped; the count is not, and the alert says how many it left out.
func TestForcedUpdate_AlertCountsTheCommitsItDidNotList(t *testing.T) {
	lost := make([]history.LostCommit, history.MaxLostCommits)
	for i := range lost {
		lost[i] = history.LostCommit{SHA: fmt.Sprintf("%040d", i), Author: "a", Subject: "s"}
	}
	git := rewriteGit(mockUnmatched{lost: lost, total: history.MaxLostCommits + 4},
		history.ForcedRef{Ref: rewriteRef, Old: rewriteOld, New: rewriteNew})
	notif := &mockNotifier{}
	svc := newTestService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{Ref: rewriteRef}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	alert := forcedAlert(notif.messages)
	if alert == nil || !strings.Contains(alert.Body, "… and 4 more") {
		t.Errorf("alert must count the unlisted commits, got %+v", alert)
	}
}

// A check that could not run must alert, and must not present an empty list as
// if the check had found nothing.
func TestForcedUpdate_FailedCheckStillAlertsAndSaysSo(t *testing.T) {
	git := rewriteGit(mockUnmatched{err: fmt.Errorf("object missing")},
		history.ForcedRef{Ref: rewriteRef, Old: rewriteOld, New: rewriteNew})
	notif := &mockNotifier{}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{Ref: rewriteRef}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	alert := forcedAlert(notif.messages)
	if alert == nil {
		t.Fatalf("a failed check must fall back to alerting, got %+v", notif.messages)
	}
	if !strings.Contains(alert.Body, "could not determine which commits were discarded") {
		t.Errorf("alert must say the check did not run:\n%s", alert.Body)
	}
	assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultOK, history.ReasonForcedUpdate)
}

// One push, one loss and one harmless rewrite: the alert fires for the loss and
// names the rewrite too, so the reader sees the whole push.
func TestForcedUpdate_MixedPushAlertsOnTheLossAndNamesTheRewrite(t *testing.T) {
	const mainOld, mainNew = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	git := rewriteGit(mockUnmatched{},
		history.ForcedRef{Ref: rewriteRef, Old: rewriteOld, New: rewriteNew},
		history.ForcedRef{Ref: "refs/heads/main", Old: mainOld, New: mainNew})
	git.unmatched[mainNew+"..."+mainOld] = mockUnmatched{
		lost: []history.LostCommit{{SHA: mainOld, Author: "a", Subject: "gone"}}, total: 1,
	}
	notif := &mockNotifier{}
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), notif, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	alert := forcedAlert(notif.messages)
	if alert == nil {
		t.Fatalf("the lossy ref must alert, got %+v", notif.messages)
	}
	loss, rewrite, found := strings.Cut(alert.Body, "Also rewritten in this push")
	if !found {
		t.Fatalf("alert must name the preserved rewrite separately:\n%s", alert.Body)
	}
	if !strings.Contains(loss, "refs/heads/main") || strings.Contains(loss, rewriteRef) {
		t.Errorf("the loss section must hold main only:\n%s", loss)
	}
	if !strings.Contains(rewrite, rewriteRef) {
		t.Errorf("the rewrite section must hold %s:\n%s", rewriteRef, rewrite)
	}
	if strings.Contains(alert.Body, "git fetch <clone-url> "+rewriteOld) {
		t.Errorf("a preserved rewrite needs no recovery command:\n%s", alert.Body)
	}
	assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultOK, history.ReasonForcedUpdate)
}

// Tags never alert, so there is nothing to decide by checking them — and a build
// tag re-pointed across a long history is where the check would cost the most.
func TestForcedUpdate_TagsAreNotChecked(t *testing.T) {
	git := rewriteGit(mockUnmatched{}, history.ForcedRef{Ref: "refs/tags/build-1", Old: rewriteOld, New: rewriteNew})
	svc, rec := newRecordingService(defaultRepos(), makeProviders(), &mockNotifier{}, git)

	if err := svc.Sync(context.Background(), "my-repo", EventMeta{Ref: "refs/tags/build-1"}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(git.unmatchedCalls) != 0 {
		t.Errorf("a tag must not be checked, got calls %v", git.unmatchedCalls)
	}
	// Unchecked reads as a possible loss in the record, exactly as before.
	assertOutcome(t, rec.only(t), history.ActionMirror, history.ResultOK, history.ReasonForcedUpdate)
}

package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestWriter(t *testing.T) (*Writer, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, dir
}

// errInjected marks a failure this test suite caused on purpose.
var errInjected = errors.New("injected I/O failure")

// failRead makes every restore read fail until the test ends.
func failRead(t *testing.T) {
	t.Helper()
	original := openForRead
	openForRead = func(string) (*os.File, error) { return nil, errInjected }
	t.Cleanup(func() { openForRead = original })
}

// failAppendAfter makes every subsequent append-open fail until the test ends.
// Call it after the writer is constructed — New opens the file the same way, so
// injecting earlier would fail construction instead of the path under test.
func failAppendAfter(t *testing.T) {
	t.Helper()
	original := openForAppend
	openForAppend = func(string) (*os.File, error) { return nil, errInjected }
	t.Cleanup(func() { openForAppend = original })
}

func sampleEvent(repo string) Event {
	return Event{
		Repo:   repo,
		Action: ActionMirror,
		Source: "sqs",
		From:   "codecommit/my-repo",
		To:     "gitlab/team/my-repo",
		Ref:    "refs/heads/main",
	}
}

// readLines returns every JSONL line currently in the live history file.
func readLines(t *testing.T, dir string) []Event {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("open history file: %v", err)
	}
	defer func() { _ = f.Close() }()

	var evs []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("line is not valid JSON: %v (%q)", err, sc.Text())
		}
		evs = append(evs, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan history file: %v", err)
	}
	return evs
}

func TestRecordWritesOneJSONLineWithEveryField(t *testing.T) {
	w, dir := newTestWriter(t)

	w.Record(sampleEvent("my-repo").With(ResultFail, ReasonPush, errors.New("boom")))

	lines := readLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	got := lines[0]
	if got.Repo != "my-repo" || got.Action != ActionMirror || got.Source != "sqs" {
		t.Errorf("descriptive fields not persisted: %+v", got)
	}
	if got.Result != ResultFail || got.Reason != ReasonPush || got.Err != "boom" {
		t.Errorf("outcome fields not persisted: result=%q reason=%q err=%q", got.Result, got.Reason, got.Err)
	}
	if got.From != "codecommit/my-repo" || got.To != "gitlab/team/my-repo" || got.Ref != "refs/heads/main" {
		t.Errorf("route fields not persisted: %+v", got)
	}
	if got.TS.IsZero() {
		t.Error("TS was not stamped")
	}
}

func TestRecordDefaultsEmptySourceToWebhook(t *testing.T) {
	w, dir := newTestWriter(t)

	ev := sampleEvent("r")
	ev.Source = ""
	w.Record(ev.With(ResultOK, "", nil))

	if got := readLines(t, dir)[0].Source; got != defaultSource {
		t.Errorf("Source = %q, want %q", got, defaultSource)
	}
	if got := w.Recent(Query{Limit: 1, FailuresOnly: false})[0].Source; got != defaultSource {
		t.Errorf("in-memory Source = %q, want %q", got, defaultSource)
	}
}

func TestRecordKeepsExplicitTimestamp(t *testing.T) {
	w, _ := newTestWriter(t)
	want := time.Date(2026, 7, 28, 4, 12, 33, 0, time.UTC)

	ev := sampleEvent("r")
	ev.TS = want
	w.Record(ev.With(ResultOK, "", nil))

	if got := w.Recent(Query{Limit: 1, FailuresOnly: false})[0].TS; !got.Equal(want) {
		t.Errorf("TS = %v, want %v", got, want)
	}
}

func TestRecentReturnsNewestFirst(t *testing.T) {
	w, _ := newTestWriter(t)
	for _, name := range []string{"first", "second", "third"} {
		w.Record(sampleEvent(name).With(ResultOK, "", nil))
	}

	got := w.Recent(Query{Limit: 10, FailuresOnly: false})
	want := []string{"third", "second", "first"}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i, repo := range want {
		if got[i].Repo != repo {
			t.Errorf("event[%d].Repo = %q, want %q", i, got[i].Repo, repo)
		}
	}
}

func TestRecentHonoursLimit(t *testing.T) {
	w, _ := newTestWriter(t)
	for i := range 5 {
		w.Record(sampleEvent(fmt.Sprintf("r%d", i)).With(ResultOK, "", nil))
	}

	if got := w.Recent(Query{Limit: 2, FailuresOnly: false}); len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
}

func TestRecentLimitZeroOrNegativeReturnsNothing(t *testing.T) {
	w, _ := newTestWriter(t)
	w.Record(sampleEvent("r").With(ResultOK, "", nil))

	for _, limit := range []int{0, -1} {
		if got := w.Recent(Query{Limit: limit, FailuresOnly: false}); len(got) != 0 {
			t.Errorf("Recent(%d) returned %d events, want 0", limit, len(got))
		}
	}
}

// A failure is the reason the console has a filter at all, so the filter must
// pass failures and nothing else — skips are the noisiest result and would
// bury them.
func TestRecentFailuresOnlyKeepsOnlyFailures(t *testing.T) {
	w, _ := newTestWriter(t)
	w.Record(sampleEvent("ok-repo").With(ResultOK, "", nil))
	w.Record(sampleEvent("skip-repo").With(ResultSkip, ReasonAlreadyUpToDate, nil))
	w.Record(sampleEvent("fail-repo").With(ResultFail, ReasonPush, errors.New("nope")))

	got := w.Recent(Query{Limit: 10, FailuresOnly: true})
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].Repo != "fail-repo" {
		t.Errorf("Repo = %q, want %q", got[0].Repo, "fail-repo")
	}
}

func TestRecentFailuresOnlyStillHonoursLimit(t *testing.T) {
	w, _ := newTestWriter(t)
	for i := range 5 {
		w.Record(sampleEvent(fmt.Sprintf("f%d", i)).With(ResultFail, ReasonPush, errors.New("x")))
	}

	if got := w.Recent(Query{Limit: 3, FailuresOnly: true}); len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
}

func TestRingBufferDropsOldestOnceFull(t *testing.T) {
	w, _ := newTestWriter(t)
	w.ringSize = 3

	for i := range 6 {
		w.Record(sampleEvent(fmt.Sprintf("r%d", i)).With(ResultOK, "", nil))
	}

	got := w.Recent(Query{Limit: 100, FailuresOnly: false})
	if len(got) != 3 {
		t.Fatalf("ring holds %d events, want 3", len(got))
	}
	want := []string{"r5", "r4", "r3"}
	for i, repo := range want {
		if got[i].Repo != repo {
			t.Errorf("event[%d].Repo = %q, want %q", i, got[i].Repo, repo)
		}
	}
}

// Rotation must cap the volume footprint at two generations. If the live file
// were merely renamed without the old .1 being replaced, the history would grow
// without bound on a PVC whose quota is not actually enforced.
func TestRotationKeepsExactlyTwoGenerations(t *testing.T) {
	w, dir := newTestWriter(t)
	w.maxBytes = 400 // a handful of events per generation

	for i := range 60 {
		w.Record(sampleEvent(fmt.Sprintf("repo-%02d", i)).With(ResultOK, "", nil))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 {
		t.Fatalf("history dir holds %v, want exactly %s and %s.1", names, FileName, FileName)
	}

	for _, name := range []string{FileName, FileName + ".1"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Size() > w.maxBytes {
			t.Errorf("%s is %d bytes, want <= maxBytes %d", name, info.Size(), w.maxBytes)
		}
	}
}

func TestRotationKeepsTheNewestEventsInTheLiveFile(t *testing.T) {
	w, dir := newTestWriter(t)
	w.maxBytes = 400

	for i := range 60 {
		w.Record(sampleEvent(fmt.Sprintf("repo-%02d", i)).With(ResultOK, "", nil))
	}

	lines := readLines(t, dir)
	if len(lines) == 0 {
		t.Fatal("live file is empty after rotation")
	}
	if last := lines[len(lines)-1].Repo; last != "repo-59" {
		t.Errorf("newest persisted event = %q, want repo-59", last)
	}
}

// A restart must not blank the console — that was the whole point of putting
// the history on the volume rather than only in memory.
func TestNewRestoresTailFromExistingFile(t *testing.T) {
	dir := t.TempDir()

	first, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for _, name := range []string{"a", "b", "c"} {
		first.Record(sampleEvent(name).With(ResultOK, "", nil))
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	second, err := New(dir)
	if err != nil {
		t.Fatalf("New() after restart error = %v", err)
	}
	defer func() { _ = second.Close() }()

	got := second.Recent(Query{Limit: 10, FailuresOnly: false})
	if len(got) != 3 {
		t.Fatalf("restored %d events, want 3", len(got))
	}
	if got[0].Repo != "c" {
		t.Errorf("newest restored event = %q, want %q", got[0].Repo, "c")
	}
	if got[0].Result != ResultOK || got[0].Source != "sqs" {
		t.Errorf("restored event lost fields: %+v", got[0])
	}
}

func TestNewAppendsAfterRestoreInsteadOfTruncating(t *testing.T) {
	dir := t.TempDir()

	first, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	first.Record(sampleEvent("old").With(ResultOK, "", nil))
	_ = first.Close()

	second, err := New(dir)
	if err != nil {
		t.Fatalf("New() after restart error = %v", err)
	}
	defer func() { _ = second.Close() }()
	second.Record(sampleEvent("new").With(ResultOK, "", nil))

	lines := readLines(t, dir)
	if len(lines) != 2 {
		t.Fatalf("file holds %d lines, want 2 (restart truncated the history)", len(lines))
	}
	if lines[0].Repo != "old" || lines[1].Repo != "new" {
		t.Errorf("lines = %q, %q; want old, new", lines[0].Repo, lines[1].Repo)
	}
}

func TestNewRestoresAtMostRingSizeEvents(t *testing.T) {
	dir := t.TempDir()

	first, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	total := defaultRingSize + 25
	for i := range total {
		first.Record(sampleEvent(fmt.Sprintf("r%04d", i)).With(ResultOK, "", nil))
	}
	_ = first.Close()

	second, err := New(dir)
	if err != nil {
		t.Fatalf("New() after restart error = %v", err)
	}
	defer func() { _ = second.Close() }()

	got := second.Recent(Query{Limit: defaultRingSize * 2, FailuresOnly: false})
	if len(got) != defaultRingSize {
		t.Fatalf("restored %d events, want %d", len(got), defaultRingSize)
	}
	if want := fmt.Sprintf("r%04d", total-1); got[0].Repo != want {
		t.Errorf("newest restored event = %q, want %q", got[0].Repo, want)
	}
}

// A half-written or hand-edited line must not take the whole restore down with
// it; the tail window deliberately starts mid-line on a large file.
func TestNewSkipsUnparseableLinesOnRestore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	content := `{"repo":"good-1","result":"ok"}` + "\n" +
		"this is not json\n" +
		`{"repo":"good-2","result":"ok"}` + "\n"
	if err := os.WriteFile(path, []byte(content), filePerm); err != nil {
		t.Fatalf("seed history file: %v", err)
	}

	w, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = w.Close() }()

	got := w.Recent(Query{Limit: 10, FailuresOnly: false})
	if len(got) != 2 {
		t.Fatalf("restored %d events, want 2", len(got))
	}
	if got[0].Repo != "good-2" || got[1].Repo != "good-1" {
		t.Errorf("restored %q, %q; want good-2, good-1", got[0].Repo, got[1].Repo)
	}
}

func TestNewOnEmptyDirStartsWithNoEvents(t *testing.T) {
	w, _ := newTestWriter(t)
	if got := w.Recent(Query{Limit: 10, FailuresOnly: false}); len(got) != 0 {
		t.Errorf("fresh writer returned %d events, want 0", len(got))
	}
}

func TestNewFailsWhenHistoryPathIsNotAFile(t *testing.T) {
	dir := t.TempDir()
	// A directory sitting where the history file belongs makes OpenFile fail.
	if err := os.Mkdir(filepath.Join(dir, FileName), dirPerm); err != nil {
		t.Fatalf("seed blocking directory: %v", err)
	}

	if _, err := New(dir); err == nil {
		t.Fatal("New() error = nil, want an error")
	}
}

// The restore window starts mid-file once the history is large, so the seek
// branch and the resulting partial first line both have to hold up.
func TestNewRestoresFromAFileLargerThanTheScanWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		t.Fatalf("seed history file: %v", err)
	}
	bw := bufio.NewWriter(f)
	// Pad each line so the file comfortably exceeds tailScanBytes. Both the line
	// count and the padding are derived from the constants rather than fixed, so
	// raising the ring or the scan window cannot quietly turn this into a test
	// that no longer exceeds the window it is named after.
	padding := strings.Repeat("x", 300)
	const lineBytes = 350
	lines := max(defaultRingSize*2, int(tailScanBytes/lineBytes)+defaultRingSize)
	for i := range lines {
		if _, err := fmt.Fprintf(bw, `{"repo":"r%05d","result":"ok","err":%q}`+"\n", i, padding); err != nil {
			t.Fatalf("write seed line: %v", err)
		}
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("flush seed: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() <= tailScanBytes {
		t.Fatalf("seed file is %v bytes, want more than the %d byte scan window", info.Size(), tailScanBytes)
	}

	w, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = w.Close() }()

	got := w.Recent(Query{Limit: defaultRingSize * 2, FailuresOnly: false})
	if len(got) != defaultRingSize {
		t.Fatalf("restored %d events, want %d", len(got), defaultRingSize)
	}
	if want := fmt.Sprintf("r%05d", lines-1); got[0].Repo != want {
		t.Errorf("newest restored event = %q, want %q", got[0].Repo, want)
	}
}

// If the volume stops accepting writes mid-rotation the recorder has to give up
// on the file quietly. A mirror that works must not start failing because its
// audit trail cannot be persisted.
func TestRecordSurvivesARotationItCannotComplete(t *testing.T) {
	w, dir := newTestWriter(t)
	w.maxBytes = 120

	// Block the rename with a non-empty directory sitting on the rotation
	// target: renaming a file onto that fails for root too, unlike a chmod.
	rotated := filepath.Join(dir, FileName+".1")
	if err := os.Mkdir(rotated, dirPerm); err != nil {
		t.Fatalf("seed blocking directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rotated, "occupied"), []byte("x"), filePerm); err != nil {
		t.Fatalf("seed blocking directory content: %v", err)
	}
	// Fail the reopen that rotation performs afterwards. Injected rather than
	// done with chmod because chmod does nothing as root, which is how CI runs.
	failAppendAfter(t)

	// The first record crosses maxBytes and rotates: both the rename and the
	// reopen fail in a read-only directory.
	w.Record(sampleEvent("during-rotate").With(ResultOK, "", nil))
	// The second takes the "file is gone" path.
	w.Record(sampleEvent("after-rotate").With(ResultOK, "", nil))

	got := w.Recent(Query{Limit: 10, FailuresOnly: false})
	if len(got) < 2 {
		t.Fatalf("Recent() returned %d events, want the console still fed", len(got))
	}
	if got[0].Repo != "after-rotate" {
		t.Errorf("newest event = %q, want after-rotate", got[0].Repo)
	}
}

func TestNewFailsWhenDirCannotBeCreated(t *testing.T) {
	// A regular file cannot also be a directory, so MkdirAll must fail here.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), filePerm); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}

	if _, err := New(filepath.Join(blocker, "history")); err == nil {
		t.Fatal("New() error = nil, want an error")
	}
}

// The single replica still mirrors several repos at once, each in its own
// goroutine, so appends genuinely race. Run with -race.
func TestConcurrentRecordWritesEveryLineIntact(t *testing.T) {
	w, dir := newTestWriter(t)

	const goroutines, perGoroutine = 8, 25
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range perGoroutine {
				w.Record(sampleEvent(fmt.Sprintf("repo-%d-%d", g, i)).With(ResultOK, "", nil))
			}
		}(g)
	}
	wg.Wait()

	lines := readLines(t, dir)
	if want := goroutines * perGoroutine; len(lines) != want {
		t.Fatalf("wrote %d lines, want %d", len(lines), want)
	}
	seen := make(map[string]bool, len(lines))
	for _, ev := range lines {
		if seen[ev.Repo] {
			t.Fatalf("duplicate line for %q", ev.Repo)
		}
		seen[ev.Repo] = true
	}
}

func TestConcurrentRecordAndRecent(t *testing.T) {
	w, _ := newTestWriter(t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 200 {
			w.Record(sampleEvent(fmt.Sprintf("r%d", i)).With(ResultOK, "", nil))
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			_ = w.Recent(Query{Limit: 50, FailuresOnly: false})
		}
	}()
	wg.Wait()
}

// Losing the audit line is acceptable; taking a working mirror down with it is
// not. Record must stay silent after the file is gone.
func TestRecordAfterCloseDoesNotPanicAndStillFeedsTheConsole(t *testing.T) {
	w, _ := newTestWriter(t)
	if err := w.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	w.Record(sampleEvent("after-close").With(ResultOK, "", nil))

	got := w.Recent(Query{Limit: 10, FailuresOnly: false})
	if len(got) != 1 || got[0].Repo != "after-close" {
		t.Errorf("Recent() = %+v, want the event still in memory", got)
	}
}

// Same contract as the rotation failure, one layer down: a write that fails on
// a still-open handle is logged, not propagated.
func TestRecordSurvivesAWriteFailure(t *testing.T) {
	w, _ := newTestWriter(t)
	// Close the handle behind the writer's back so it stays non-nil and every
	// later Write fails.
	if err := w.f.Close(); err != nil {
		t.Fatalf("close underlying file: %v", err)
	}

	w.Record(sampleEvent("write-fails").With(ResultOK, "", nil))

	got := w.Recent(Query{Limit: 10, FailuresOnly: false})
	if len(got) != 1 || got[0].Repo != "write-fails" {
		t.Errorf("Recent() = %+v, want the event still in memory", got)
	}
}

// An unreadable history file must not stop the service from starting; it only
// costs the restored tail.
func TestNewStartsCleanWhenTheHistoryFileCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(`{"repo":"unreadable","result":"ok"}`+"\n"), filePerm); err != nil {
		t.Fatalf("seed history file: %v", err)
	}
	// Deny the restore read while leaving the append open. Injected rather than
	// done with chmod because chmod does nothing as root, which is how CI runs.
	failRead(t)

	w, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v, want the service to start anyway", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Recent(Query{Limit: 10, FailuresOnly: false}); len(got) != 0 {
		t.Errorf("Recent() = %+v, want no restored events", got)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	w, _ := newTestWriter(t)
	if err := w.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close() error = %v, want nil", err)
	}
}

func TestWithStampsOutcomeAndLeavesReceiverUnchanged(t *testing.T) {
	base := sampleEvent("r")

	got := base.With(ResultFail, ReasonClone, errors.New("kaboom"))

	if got.Result != ResultFail || got.Reason != ReasonClone || got.Err != "kaboom" {
		t.Errorf("With() = %+v, want the outcome stamped", got)
	}
	if base.Result != "" || base.Reason != "" || base.Err != "" {
		t.Errorf("With() mutated the receiver: %+v", base)
	}
	if got.Repo != base.Repo || got.From != base.From || got.Ref != base.Ref {
		t.Error("With() dropped descriptive fields")
	}
}

func TestWithNilErrorLeavesErrEmpty(t *testing.T) {
	got := sampleEvent("r").With(ResultSkip, ReasonAlreadyUpToDate, nil)
	if got.Err != "" {
		t.Errorf("Err = %q, want empty", got.Err)
	}
}

// An "ok" row carries no reason, so the JSON must omit the key rather than
// render an empty string the console would have to special-case.
func TestOkEventOmitsReasonAndErrInJSON(t *testing.T) {
	raw, err := json.Marshal(sampleEvent("r").With(ResultOK, "", nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"reason"`, `"err"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("JSON contains %s for an ok event: %s", key, raw)
		}
	}
}

func TestNopDiscardsEverything(t *testing.T) {
	n := NewNoop()
	n.Record(sampleEvent("r").With(ResultOK, "", nil))
	if got := n.Recent(Query{Limit: 10, FailuresOnly: false}); got != nil {
		t.Errorf("Recent() = %v, want nil", got)
	}
}

func TestRecentFiltersByRepo(t *testing.T) {
	w, _ := newTestWriter(t)
	w.Record(sampleEvent("alpha").With(ResultOK, "", nil))
	w.Record(sampleEvent("beta").With(ResultOK, "", nil))
	w.Record(sampleEvent("alpha").With(ResultSkip, ReasonAlreadyUpToDate, nil))

	got := w.Recent(Query{Limit: 10, Repo: "alpha"})
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	for i, ev := range got {
		if ev.Repo != "alpha" {
			t.Errorf("event[%d].Repo = %q, want alpha", i, ev.Repo)
		}
	}
}

func TestRecentEmptyRepoMatchesEverything(t *testing.T) {
	w, _ := newTestWriter(t)
	w.Record(sampleEvent("alpha").With(ResultOK, "", nil))
	w.Record(sampleEvent("beta").With(ResultOK, "", nil))

	if got := w.Recent(Query{Limit: 10}); len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
}

func TestRecentCombinesRepoAndFailureFilters(t *testing.T) {
	w, _ := newTestWriter(t)
	w.Record(sampleEvent("alpha").With(ResultFail, ReasonPush, errors.New("x")))
	w.Record(sampleEvent("beta").With(ResultFail, ReasonPush, errors.New("x")))
	w.Record(sampleEvent("alpha").With(ResultOK, "", nil))

	got := w.Recent(Query{Limit: 10, Repo: "alpha", FailuresOnly: true})
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].Repo != "alpha" || got[0].Result != ResultFail {
		t.Errorf("event = %+v, want alpha/fail", got[0])
	}
}

func TestRecentUnknownRepoReturnsNothing(t *testing.T) {
	w, _ := newTestWriter(t)
	w.Record(sampleEvent("alpha").With(ResultOK, "", nil))

	if got := w.Recent(Query{Limit: 10, Repo: "nope"}); len(got) != 0 {
		t.Errorf("got %d events, want 0", len(got))
	}
}

func TestReposIsSortedAndDeduplicated(t *testing.T) {
	w, _ := newTestWriter(t)
	for _, repo := range []string{"demo-repo", "alpha", "demo-repo", "alpha", "beta"} {
		w.Record(sampleEvent(repo).With(ResultOK, "", nil))
	}

	got := w.Repos()
	want := []string{"alpha", "beta", "demo-repo"}
	if len(got) != len(want) {
		t.Fatalf("Repos() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Repos()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReposIsEmptyBeforeAnyEvent(t *testing.T) {
	w, _ := newTestWriter(t)
	if got := w.Repos(); len(got) != 0 {
		t.Errorf("Repos() = %v, want empty", got)
	}
}

func TestReposSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	first, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	first.Record(sampleEvent("alpha").With(ResultOK, "", nil))
	first.Record(sampleEvent("beta").With(ResultOK, "", nil))
	_ = first.Close()

	second, err := New(dir)
	if err != nil {
		t.Fatalf("New() after restart error = %v", err)
	}
	defer func() { _ = second.Close() }()

	if got := second.Repos(); len(got) != 2 {
		t.Errorf("Repos() = %v, want both repos restored", got)
	}
}

func TestNopReturnsNoRepos(t *testing.T) {
	if got := NewNoop().Repos(); got != nil {
		t.Errorf("Repos() = %v, want nil", got)
	}
}

// The trigger filter exists because the reconcile cron dwarfs real pushes in
// the tail; being able to ask for just the webhooks is what keeps the console
// usable.
func TestRecentFiltersBySource(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = w.Close() }()

	for _, src := range []string{"webhook", "cron", "sqs", "cron"} {
		w.Record(Event{Repo: "r", Source: src, Result: ResultOK})
	}

	got := w.Recent(Query{Limit: 10, Source: "cron"})
	if len(got) != 2 {
		t.Fatalf("cron events = %d, want 2", len(got))
	}
	for _, ev := range got {
		if ev.Source != "cron" {
			t.Errorf("got source %q in a cron-filtered result", ev.Source)
		}
	}
	if all := w.Recent(Query{Limit: 10}); len(all) != 4 {
		t.Errorf("unfiltered = %d events, want 4", len(all))
	}
}

// Sources feeds the console dropdown, so it must report the distinct triggers
// present — sorted, deduplicated, and blanks dropped.
func TestSourcesListsDistinctTriggers(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = w.Close() }()

	for _, src := range []string{"webhook", "cron", "webhook", "", "sqs"} {
		w.Record(Event{Repo: "r", Source: src, Result: ResultOK})
	}

	got := w.Sources()
	want := []string{"cron", "sqs", "webhook"}
	if len(got) != len(want) {
		t.Fatalf("Sources() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Sources() = %v, want %v", got, want)
		}
	}
}

// The hourly reconcile reports "nothing to do" four times an hour, which would
// otherwise be ~96 of every 100 rows the console shows.
func TestRecentHidesIdleReconciles(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = w.Close() }()

	w.Record(Event{Repo: "r", Source: "cron", Result: ResultSkip, Reason: ReasonAlreadyUpToDate})
	w.Record(Event{Repo: "r", Source: "webhook", Result: ResultOK})

	got := w.Recent(Query{Limit: 10, RoutineSource: "cron"})
	if len(got) != 1 || got[0].Source != "webhook" {
		t.Fatalf("events = %+v, want only the webhook event", got)
	}
	if all := w.Recent(Query{Limit: 10}); len(all) != 2 {
		t.Errorf("unfiltered = %d events, want 2 — the row must still be recorded", len(all))
	}
}

// The two rows worth keeping: a reconcile that actually mirrored something is
// the safety net catching a missed push, and a failure matters more still.
// Hiding either would defeat the point of running the reconcile at all.
func TestRecentKeepsReconcilesThatDidSomething(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = w.Close() }()

	w.Record(Event{Repo: "r", Source: "cron", Result: ResultOK})
	w.Record(Event{Repo: "r", Source: "cron", Result: ResultFail, Err: "boom"})
	w.Record(Event{Repo: "r", Source: "cron", Result: ResultSkip, Reason: ReasonRefOverride})

	got := w.Recent(Query{Limit: 10, RoutineSource: "cron"})
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3 — only already-up-to-date skips are hidden", len(got))
	}
}

// The filter is scoped to the named trigger: a webhook that found nothing is
// still evidence a push arrived, so it must survive.
func TestRecentOnlyHidesTheNamedTrigger(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = w.Close() }()

	w.Record(Event{Repo: "r", Source: "webhook", Result: ResultSkip, Reason: ReasonAlreadyUpToDate})
	w.Record(Event{Repo: "r", Source: "cron", Result: ResultSkip, Reason: ReasonAlreadyUpToDate})

	got := w.Recent(Query{Limit: 10, RoutineSource: "cron"})
	if len(got) != 1 || got[0].Source != "webhook" {
		t.Fatalf("events = %+v, want the webhook no-op kept", got)
	}
}

// Limit must be filled with rows the reader wanted. Filtering after the limit
// would return a nearly empty page once the reconcile dominates the tail.
func TestRecentFillsTheLimitAfterHidingIdleReconciles(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = w.Close() }()

	for range 50 {
		w.Record(Event{Repo: "r", Source: "cron", Result: ResultSkip, Reason: ReasonAlreadyUpToDate})
	}
	for range 5 {
		w.Record(Event{Repo: "r", Source: "webhook", Result: ResultOK})
	}

	got := w.Recent(Query{Limit: 5, RoutineSource: "cron"})
	if len(got) != 5 {
		t.Fatalf("events = %d, want the limit filled with 5 real ones", len(got))
	}
	for _, ev := range got {
		if ev.Source != "webhook" {
			t.Errorf("got a %q event, want only webhook", ev.Source)
		}
	}
}

// A forced update is recorded as ok, so the failures filter can never surface
// one. ForcedOnly is the only way to answer "did we overwrite anything".
func TestRecentForcedOnlyKeepsOnlyOverwrites(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	w.Record(Event{Repo: "a", Result: ResultOK})
	w.Record(Event{Repo: "b", Result: ResultFail, Reason: ReasonPush})
	w.Record(Event{Repo: "c", Result: ResultSkip, Reason: ReasonAlreadyUpToDate})
	w.Record(Event{
		Repo: "d", Result: ResultOK, Reason: ReasonForcedUpdate,
		Forced: []ForcedRef{{Ref: "refs/heads/main", Old: "aaa", New: "bbb"}},
	})

	got := w.Recent(Query{Limit: 10, ForcedOnly: true})
	if len(got) != 1 {
		t.Fatalf("ForcedOnly returned %d events, want 1: %+v", len(got), got)
	}
	if got[0].Repo != "d" {
		t.Errorf("Repo = %q, want d", got[0].Repo)
	}

	// The failures filter must not have been widened into this.
	if fails := w.Recent(Query{Limit: 10, FailuresOnly: true}); len(fails) != 1 || fails[0].Repo != "b" {
		t.Errorf("FailuresOnly = %+v, want only the fail event", fails)
	}
}

// The forced refs must survive a restart, or the one pointer to the discarded
// commits dies with the pod that recorded it.
func TestForcedRefsSurviveAReload(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	w.Record(Event{
		Repo: "a", Result: ResultOK, Reason: ReasonForcedUpdate,
		Forced: []ForcedRef{{Ref: "refs/heads/main", Old: "aaa111", New: "bbb222"}},
	})
	_ = w.Close()

	second, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	got := second.Recent(Query{Limit: 10, ForcedOnly: true})
	if len(got) != 1 {
		t.Fatalf("restored %d forced events, want 1", len(got))
	}
	if got[0].Forced[0].Old != "aaa111" || got[0].Forced[0].New != "bbb222" {
		t.Errorf("restored Forced = %+v, want the original tips", got[0].Forced)
	}
}

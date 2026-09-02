// Package history records the outcome of every mirror operation as JSONL on the
// PVC and keeps the tail of it in memory for the console to read.
//
// Before this package existed the only trace a sync left behind was a log line
// and a Slack message, so "what happened to this repo an hour ago" could not be
// answered from the service itself. The file lives next to the mirror cache but
// in its own directory, because the cache is disposable (it can be re-cloned)
// while the history is not.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Action values — which kind of mirror operation produced the event.
const (
	ActionMirror = "mirror"
	ActionDelete = "delete"
	// ActionRestore is a console-driven re-creation of a ref a delete removed,
	// using the tip that delete recorded. It is deliberately its own action
	// rather than a mirror: nothing upstream asked for it, a person did.
	ActionRestore = "restore"
)

// Result values — how the operation ended.
const (
	ResultOK   = "ok"
	ResultSkip = "skip"
	ResultFail = "fail"
)

// Reason values for ResultSkip. A skip is not one thing: "nothing changed" and
// "a rule forbade this direction" look identical in the log without this field,
// and telling them apart is exactly what a mirror-loop investigation needs.
const (
	ReasonAlreadyUpToDate = "already-up-to-date"
	ReasonRefOverride     = "ref-override"
	ReasonNoRefsToPush    = "no-refs-to-push"
	ReasonAlreadyAbsent   = "already-absent"
	// ReasonDestinationAhead is a push withheld because the destination already
	// contains what this side would have written, or holds a commit this side
	// has never seen. Pushing would have rewound it.
	//
	// This is the quiet, correct outcome of an echo that arrives after the
	// destination moved on, so it is a skip and not an alert: the state that
	// made the destination newer raised its own event in the other direction,
	// and the two sides converge forward without anyone being called.
	ReasonDestinationAhead = "destination-ahead"
	// ReasonLeaseRejected is a push git refused because the destination moved
	// between the check and the write. Same meaning as above, caught one layer
	// lower — the check cannot be atomic with the push, so the lease is what
	// makes the gap between them safe.
	ReasonLeaseRejected = "lease-rejected"
)

// Reason values for ResultFail, naming the step that failed.
const (
	ReasonClone     = "clone"
	ReasonPush      = "push"
	ReasonCheckRef  = "check-ref"
	ReasonDeleteRef = "delete-ref"
	ReasonProvider  = "provider"
	// ReasonRefExists is a refused restore: the ref is back on the destination,
	// so re-creating it would overwrite whatever put it there. Refusing is the
	// point — a restore is only ever meant to fill a hole it can still see.
	ReasonRefExists = "ref-exists"
	// ReasonObjectGone is a refused restore: git has garbage-collected the
	// commit, so there is nothing left to put back.
	ReasonObjectGone = "object-gone"
	// ReasonCreateRef names a restore that failed at the push itself.
	ReasonCreateRef = "create-ref"
	// ReasonDirection is a write refused because the repo's direction never
	// writes to that side.
	ReasonDirection = "direction"
	// ReasonUnknownSide is a restore naming a destination that is neither side
	// of the repo, and ReasonUnknownRepo one naming a repo that is not
	// configured. Both mean the request did not come from a rendered row.
	ReasonUnknownSide = "unknown-side"
	ReasonUnknownRepo = "unknown-repo"
	// ReasonRepoBusy is a restore refused because a mirror operation holds the
	// per-repo lock. Nothing is wrong — it is a "try again in a moment".
	ReasonRepoBusy = "repo-busy"
)

// ReasonForcedUpdate narrows ResultOK: the push succeeded, but at least one ref
// was overwritten non-fast-forward — the tip it replaced is not an ancestor of
// the new one, so whatever was only on that tip is gone from the destination.
//
// It is a reason on a success rather than a failure because nothing went wrong
// mechanically. Force is how a mirror works: the destination is meant to match
// the source, and every push here is --force. What this reason marks is the one
// case where obeying that instruction destroyed history, which is invisible in
// the result alone — a rewind and a routine propagation both end as ok, and the
// echo from the other side settles to already-up-to-date either way, because by
// then both sides agree on the rewound state.
const ReasonForcedUpdate = "forced-update"

// ForcedRef is one ref a push overwrote non-fast-forward.
//
// Old is the point of the whole record. It is the tip that was discarded, and
// the destination no longer names it anywhere, so this line is the only thing
// standing between "someone's commits vanished" and a one-command recovery.
// git keeps the objects until it garbage-collects, so the window to use it is
// real but not indefinite.
type ForcedRef struct {
	Ref string `json:"ref"`
	Old string `json:"old"`
	New string `json:"new"`
}

// HeldRef is one ref the push guard refused to move, and the destination tip
// that made it refuse.
//
// Dest is what a person needs to decide: it names the commit that would have
// been discarded, so "my reset did not propagate" and "an echo arrived late"
// are told apart by looking at it rather than by guessing from the timing.
type HeldRef struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
	Dest   string `json:"dest"`
}

// defaultSource is what an event with no explicit source is recorded as, the
// same assumption EventMeta documents. The source vocabulary itself belongs to
// the mirror package, which owns EventMeta and cannot be imported from here
// without a cycle.
const defaultSource = "webhook"

// DirName is the history directory, expected to sit inside the mirror work
// directory on the same volume. It is a sibling of the per-repo mirror caches
// rather than mixed in with them: the caches are disposable and get wiped and
// re-cloned, the history must not be caught in that.
const DirName = ".history"

// FileName is the live history file inside the history directory. Rotation adds
// FileName + ".1" next to it and never more than that.
const FileName = "events.jsonl"

const (
	// defaultMaxBytes rotates the live file, so at most 2x this ever sits on
	// disk. At roughly 200 bytes per line that is well over a year of events
	// for the current repo set, against a mirror cache already past 200 MB.
	defaultMaxBytes = 10 << 20

	// defaultRingSize bounds the in-memory tail the console renders.
	//
	// It is sized against the noisiest producer, which is the reconcile cron:
	// at hourly it emits four events a day per repo pair — roughly 96 — and
	// those are almost all "already up to date". A 500-entry ring would be
	// spent in five days, so a webhook failure from last week would already
	// have been pushed out by routine no-ops. 5000 restores a ~50 day window
	// for about a megabyte of memory, which is nothing beside a 200 MB mirror
	// cache. Raising this beats dropping the quiet events: "not recorded" and
	// "ran and found nothing" must not look the same in an audit trail.
	defaultRingSize = 5000

	// tailScanBytes caps how much of the file is re-read at startup, so restore
	// time does not grow with the file. It has to cover defaultRingSize lines
	// or a restart silently restores a shorter history than the ring holds —
	// 4 MB is ~20x the ring at typical line length, with room for the long
	// lines that carry git stderr.
	tailScanBytes = 4 << 20

	dirPerm  = 0o755
	filePerm = 0o644
)

// File access is indirected through these two variables so tests can inject an
// I/O failure directly. The obvious alternative — revoking permissions with
// chmod — silently stops working as root, which is how CI runs: root bypasses
// the permission check, the injected failure never happens, and the test passes
// while verifying nothing. Production always uses the values set here.
var (
	openForRead = os.Open

	openForAppend = func(path string) (*os.File, error) {
		return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	}
)

// Event is one recorded mirror outcome, serialised as a single JSONL line.
type Event struct {
	TS     time.Time `json:"ts"`
	Repo   string    `json:"repo"`
	Action string    `json:"action"` // mirror | delete
	Source string    `json:"source"` // webhook | sqs | retry-api
	From   string    `json:"from"`   // "<provider-type>/<path>"
	To     string    `json:"to"`     // "<provider-type>/<path>"
	Ref    string    `json:"ref,omitempty"`
	Result string    `json:"result"` // ok | skip | fail
	// Reason narrows Result. Always set for skip and fail; on ok it is empty
	// unless the push overwrote something (ReasonForcedUpdate).
	Reason     string `json:"reason,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Err        string `json:"err,omitempty"`
	// Forced lists the refs this operation overwrote non-fast-forward, in the
	// order git reported them. Empty on every other outcome.
	Forced []ForcedRef `json:"forced,omitempty"`
	// Held lists the refs the push guard withheld, and the destination tip that
	// caused each. It is recorded rather than left to the log because the
	// console needs it: a full reconcile names no ref on the event itself, so
	// without this the only trace of which branch was held would be a log line,
	// and there would be nothing for a "apply this rewind" button to act on.
	Held []HeldRef `json:"held,omitempty"`
	// DeletedTip is the tip a delete removed, read from the destination just
	// before the delete ran.
	//
	// A delete is the one operation that leaves nothing behind to look up: the
	// ref is gone from the destination, so without this the event names what
	// was removed but not what it pointed at, and there is no way to ask for
	// the objects back. A forced update at least records the tip it discarded
	// (see ForcedRef.Old) — a delete used to record nothing at all.
	//
	// Set only on a delete that actually removed something. Empty on every
	// other outcome, including a delete that found the ref already absent.
	DeletedTip string `json:"deleted_tip,omitempty"`
	// RestoredTip is the tip an ActionRestore put back, set only when the ref
	// was actually re-created. It is the counterpart to DeletedTip: together
	// the two events tell the whole story of a ref that went away and came
	// back, without anyone having to correlate them by timestamp.
	RestoredTip string `json:"restored_tip,omitempty"`
	// Actor is the person a console-driven action is attributed to, read from
	// the portal's identity header.
	//
	// It is set only for actions a human triggers from the console, because
	// those are the only ones with a person behind them — a webhook or an SQS
	// event has a pusher, not an operator. A restore writes to a destination
	// repository, so "who did this" has to survive in the record rather than
	// only in whoever happened to be watching the channel.
	Actor string `json:"actor,omitempty"`
}

// IsForced reports whether this event overwrote history.
func (e Event) IsForced() bool { return len(e.Forced) > 0 }

// With returns a copy of the event stamped with an outcome, so a caller can
// build the descriptive half once and finish it differently on each exit path.
// err may be nil.
func (e Event) With(result, reason string, err error) Event {
	e.Result = result
	e.Reason = reason
	if err != nil {
		e.Err = err.Error()
	}
	return e
}

// Recorder accepts mirror outcomes. The mirror service depends on this rather
// than on *Writer so it never has to care whether history is actually enabled.
type Recorder interface {
	Record(ev Event)
}

// Query narrows what Recent returns. It is a struct rather than a parameter
// list because the console keeps gaining filters, and each new one would
// otherwise change the signature for every caller and mock.
//
// The zero value returns nothing: Limit must be set explicitly so an unbounded
// read can never happen by omission.
type Query struct {
	Limit        int
	FailuresOnly bool
	Repo         string // empty matches every repo
	// RoutineSource, when set, drops events from that trigger whose result was
	// a no-op skip — the reconcile cron reporting it found nothing.
	//
	// The filter has to run here rather than in the browser because Limit is
	// applied while walking the ring: hiding rows after the fact would return
	// 100 events and render four. The trigger name is passed in rather than
	// hardcoded so this package still does not import mirror (see defaultSource).
	RoutineSource string
	// ForcedOnly keeps only events that overwrote a ref non-fast-forward.
	//
	// It is separate from FailuresOnly rather than folded into it because the
	// two answer different questions — "what broke" versus "what did we
	// destroy" — and a forced update is not a failure: it is an ok row, so
	// FailuresOnly would never surface one. Without this the only way to find
	// one is to read every row.
	ForcedOnly bool
	// Source filters by what triggered the sync (webhook, sqs, cron, ...).
	// Empty matches every trigger. It earns its place because the triggers have
	// very different meanings: a webhook event is a real push, while a cron one
	// is the safety net reporting it found nothing, and the latter outnumbers
	// the former by an order of magnitude.
	Source string
}

// Reader exposes the in-memory tail to the console.
type Reader interface {
	Recent(q Query) []Event
	// Repos lists the repo names the console can filter by.
	Repos() []string
	// Sources lists the triggers the console can filter by.
	Sources() []string
}

// Nop discards writes and reads back nothing. It stands in whenever history is
// unavailable, mirroring notify's no-op notifier, so neither the mirror service
// nor the console ever holds a nil dependency.
type Nop struct{}

// Record discards the event.
func (Nop) Record(Event) {}

// Recent returns no events.
func (Nop) Recent(Query) []Event { return nil }

// Repos returns no repos.
func (Nop) Repos() []string { return nil }

// Sources returns no triggers.
func (Nop) Sources() []string { return nil }

// NewNoop returns a history sink that drops everything.
func NewNoop() Nop { return Nop{} }

// Writer appends events to the JSONL file and keeps the newest ones in memory.
//
// Every field below is guarded by mu. The deployment runs a single replica, but
// a single replica still mirrors several repos concurrently — each in its own
// goroutine under its own per-repo lock — so appends genuinely race. O_APPEND
// alone would keep individual lines intact yet still let rotation run
// underneath a write, so the mutex covers both.
type Writer struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	size     int64
	maxBytes int64
	ring     []Event
	ringSize int
}

// Compile-time proof that both roles are satisfied by the same value.
var (
	_ Recorder = (*Writer)(nil)
	_ Reader   = (*Writer)(nil)
	_ Recorder = Nop{}
	_ Reader   = Nop{}
)

// New opens (or creates) the history file under dir and restores the in-memory
// tail from it, so a pod restart does not blank the console.
func New(dir string) (*Writer, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("create history dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, FileName)
	f, err := openForAppend(path)
	if err != nil {
		return nil, fmt.Errorf("open history file %q: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat history file %q: %w", path, err)
	}
	return &Writer{
		path:     path,
		f:        f,
		size:     info.Size(),
		maxBytes: defaultMaxBytes,
		ringSize: defaultRingSize,
		ring:     tail(path, defaultRingSize),
	}, nil
}

// Record appends the event and adds it to the in-memory tail.
//
// It never returns an error on purpose. By the time we get here the mirror has
// already succeeded or failed on its own, and failing a sync that worked
// because an audit line could not be written would be strictly worse than
// losing the line. Problems are logged and swallowed.
func (w *Writer) Record(ev Event) {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	if ev.Source == "" {
		ev.Source = defaultSource
	}
	line, err := json.Marshal(ev)
	if err != nil {
		slog.Warn("history: marshal event failed", "repo", ev.Repo, "error", err)
		return
	}
	line = append(line, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()

	w.push(ev)

	if w.f == nil {
		return // rotation lost the handle; push above still feeds the console
	}
	// Rotate before the write that would cross the limit rather than after, so
	// the live file never exceeds maxBytes and the on-disk total stays bounded
	// by 2x maxBytes even for the last line.
	if w.size+int64(len(line)) > w.maxBytes {
		w.rotate()
		if w.f == nil {
			return
		}
	}
	n, err := w.f.Write(line)
	w.size += int64(n)
	if err != nil {
		slog.Warn("history: write failed", "path", w.path, "error", err)
	}
}

// rotate moves the live file aside to <name>.1 and starts a fresh one. Rename
// replaces any existing .1, which is what caps the history at two generations.
//
// Caller must hold w.mu.
func (w *Writer) rotate() {
	if err := w.f.Close(); err != nil {
		slog.Warn("history: close before rotate failed", "path", w.path, "error", err)
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("history: rotate failed", "path", w.path, "error", err)
	}
	f, err := openForAppend(w.path)
	if err != nil {
		// Give up on the file rather than retry-and-log on every later event.
		// The console keeps working off the ring buffer; only durability is
		// lost, and the pod restart that fixes it will start a new file.
		slog.Error("history: reopen after rotate failed, stopping history writes",
			"path", w.path, "error", err)
		w.f = nil
		return
	}
	w.f = f
	w.size = 0
}

// push appends to the bounded in-memory tail, dropping the oldest entry once
// full. Caller must hold w.mu.
func (w *Writer) push(ev Event) {
	if len(w.ring) >= w.ringSize {
		copy(w.ring, w.ring[len(w.ring)-w.ringSize+1:])
		w.ring = w.ring[:w.ringSize-1]
	}
	w.ring = append(w.ring, ev)
}

// Recent returns up to q.Limit events matching q, newest first.
//
// It reads the in-memory ring and never the file: a console page load must not
// be able to block a mirror operation on disk I/O, and the ring is already the
// answer for every window the console offers.
func (w *Writer) Recent(q Query) []Event {
	if q.Limit <= 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	out := make([]Event, 0, min(q.Limit, len(w.ring)))
	for i := len(w.ring) - 1; i >= 0 && len(out) < q.Limit; i-- {
		ev := w.ring[i]
		if q.FailuresOnly && ev.Result != ResultFail {
			continue
		}
		if q.ForcedOnly && !ev.IsForced() {
			continue
		}
		if q.Repo != "" && ev.Repo != q.Repo {
			continue
		}
		if q.Source != "" && ev.Source != q.Source {
			continue
		}
		if q.RoutineSource != "" && ev.Source == q.RoutineSource && isRoutineNoop(ev) {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// Repos returns the distinct repo names present in the in-memory tail, sorted.
//
// It is derived from the history rather than from the mirror config so the
// console can never offer a filter that returns nothing, and so this package
// stays independent of config.
// Sources lists the distinct triggers present in the tail, for the console's
// filter. Like Repos it reads the unfiltered ring, so selecting one trigger
// never removes the others from the dropdown.
// isRoutineNoop reports whether an event carries no information: the sync ran
// and found nothing to do.
//
// Only this exact shape is hidden. A cron event that actually mirrored
// something is the safety net catching a push that was missed — the single most
// useful row in the history — and a failure is worth more still, so neither is
// ever filtered out by RoutineSource.
func isRoutineNoop(ev Event) bool {
	return ev.Result == ResultSkip && ev.Reason == ReasonAlreadyUpToDate
}

func (w *Writer) Sources() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	seen := make(map[string]struct{}, len(w.ring))
	for _, ev := range w.ring {
		if ev.Source != "" {
			seen[ev.Source] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for src := range seen {
		out = append(out, src)
	}
	sort.Strings(out)
	return out
}

func (w *Writer) Repos() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	seen := make(map[string]struct{}, len(w.ring))
	for _, ev := range w.ring {
		if ev.Repo != "" {
			seen[ev.Repo] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for repo := range seen {
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}

// Close releases the history file. The in-memory tail stays readable.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	if err != nil {
		return fmt.Errorf("close history file %q: %w", w.path, err)
	}
	return nil
}

// tail reads the last n parseable events from path, newest last.
//
// Only the final tailScanBytes are scanned, so startup cost does not grow with
// the file. The first line in that window is usually cut in half; it fails to
// parse and is dropped, which is why unparseable lines are skipped rather than
// treated as corruption.
func tail(path string, n int) []Event {
	f, err := openForRead(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("history: open for restore failed", "path", path, "error", err)
		}
		return nil
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		slog.Warn("history: stat for restore failed", "path", path, "error", err)
		return nil
	}
	if info.Size() > tailScanBytes {
		if _, err := f.Seek(info.Size()-tailScanBytes, io.SeekStart); err != nil {
			slog.Warn("history: seek for restore failed", "path", path, "error", err)
			return nil
		}
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), tailScanBytes)
	evs := make([]Event, 0, n)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if len(evs) == n {
			copy(evs, evs[1:])
			evs = evs[:n-1]
		}
		evs = append(evs, ev)
	}
	if err := sc.Err(); err != nil {
		slog.Warn("history: scan for restore failed", "path", path, "error", err)
	}
	if len(evs) > 0 {
		slog.Info("history: restored recent events", "path", path, "count", len(evs))
	}
	return evs
}

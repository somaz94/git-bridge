package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"git-bridge/internal/askpass"
	"git-bridge/internal/config"
	"git-bridge/internal/history"
	"git-bridge/internal/notify"
	"git-bridge/internal/provider"
)

const (
	defaultWorkDir  = "/tmp/git-bridge"
	refsHeadsPrefix = "refs/heads/"
	refsTagsPrefix  = "refs/tags/"
	refTypeTag      = "tag" // the ref-kind label for a tag, as CodeCommit events write it
)

// fullRefName builds the full ref from the ref kind (refType) and the short
// name (refName). A refType of "tag" gets refs/tags/, anything else (a branch)
// gets refs/heads/.
func fullRefName(refType, refName string) string {
	if refType == refTypeTag {
		return refsTagsPrefix + refName
	}
	return refsHeadsPrefix + refName
}

// Trigger-origin vocabulary. Defined once here, in the package that owns
// EventMeta, so the Slack Source line and the history source column can never
// drift apart. The history package cannot import this one (it would cycle), so
// it carries only its own fallback default.
const (
	SourceWebhook  = "webhook"
	SourceSQS      = "sqs"
	SourceRetryAPI = "retry-api"
	// SourceConsole is a retry triggered from the console. It is kept separate
	// from SourceRetryAPI so the history shows whether a human clicked or
	// something called the endpoint.
	SourceConsole = "console"
	// SourceCron is the reconcile CronJob calling that same endpoint. It shares
	// the route with a human curl, so the caller declares itself and the
	// endpoint accepts this one extra value.
	//
	// Separating it is what makes the history readable: the cron runs hourly
	// and is almost always "already up to date", so it outnumbers real pushes
	// by an order of magnitude. It also lets the console's loop detector ignore
	// deliberate reconciles — two scheduled calls plus one manual retry inside
	// the same window looked exactly like an echo loop and raised a false
	// alarm, and a detector that cries wolf gets ignored.
	SourceCron = "cron"
)

// EventMeta carries webhook/event metadata through the sync pipeline.
type EventMeta struct {
	Ref    string // full ref (e.g. "refs/heads/main", "refs/tags/v1.0.0")
	Source string // trigger origin: SourceWebhook / SourceSQS / SourceRetryAPI. empty = webhook
	// Force propagates a rewind the push guard would otherwise withhold: the
	// destination already contains what this side holds, so writing it back
	// discards commits. That is normally a mistake and is refused, but a
	// deliberate reset is a real thing to want, and there has to be a way to
	// say so.
	//
	// Only a person can set it — the console button and a hand-run retry call.
	// Webhook and SQS never construct it, and the reconcile CronJob is refused
	// at the API, so no automatic path can reach it. It also requires a ref, so
	// the blast radius of one force is one ref.
	Force bool
	// ForceLease is the destination tip the operator was shown when they asked
	// for the force, and it is what makes "apply the rewind I looked at" true
	// rather than merely claimed.
	//
	// Without it the lease would be whatever the destination holds at push
	// time, read after a fetch that can run for a minute — so a commit landing
	// in that minute would be adopted as the expected value and overwritten,
	// which is the accident this whole guard exists to prevent, performed on
	// request. Pinning the observed tip means a destination that moved since
	// the click fails the lease and the push is refused.
	//
	// Required whenever Force is set; both API paths reject a force without it.
	ForceLease string
	// Actor attributes a console-driven action to the person who asked for it,
	// carried through to the history event and the Slack message.
	Actor string
}

// RefName returns the short ref name (e.g. "main", "v1.0.0").
func (m EventMeta) RefName() string {
	if strings.HasPrefix(m.Ref, refsHeadsPrefix) {
		return strings.TrimPrefix(m.Ref, refsHeadsPrefix)
	}
	if strings.HasPrefix(m.Ref, refsTagsPrefix) {
		return strings.TrimPrefix(m.Ref, refsTagsPrefix)
	}
	return m.Ref
}

// IsTag returns true if the ref is a tag.
func (m EventMeta) IsTag() bool {
	return strings.HasPrefix(m.Ref, refsTagsPrefix)
}

// GitRunner executes git clone/push operations.
type GitRunner interface {
	CloneMirror(ctx context.Context, rem provider.Remote, dir string) error
	FetchMirror(ctx context.Context, rem provider.Remote, dir string) error
	// PushMirror pushes the given specs to the target. Every spec carries the
	// destination tip it was checked against, so the push is force-with-lease
	// rather than bare force: git refuses the write if the destination moved
	// between the check and the push. An empty spec list is a no-op.
	//
	// There is deliberately no "push everything" mode. A caller that wants a
	// full sync enumerates the refs itself (ListRefTips) and gets the same
	// per-ref guard as a single-ref event, because the hourly reconcile is
	// exactly as capable of rewinding a branch as an echo is.
	PushMirror(ctx context.Context, dir string, rem provider.Remote, specs []PushSpec) (PushResult, error)
	// ListRefs returns every local branch/tag ref (full name) in the mirror dir.
	ListRefs(ctx context.Context, dir string) ([]string, error)
	// ListRefTips reads the mirror dir's local branches/tags as a fullRef → SHA
	// map. Unlike ListRefs it is used where the tips are needed too (comparing
	// against the destination before a push).
	ListRefTips(ctx context.Context, dir string) (map[string]string, error)
	// RemoteRefs reads the branches/tags of the remote rem as a fullRef → SHA map.
	// The peeled line of an annotated tag (`^{}`) is dropped — what the ref
	// actually points at is the tag object, and matching destination tips has to
	// be done between those values.
	RemoteRefs(ctx context.Context, rem provider.Remote) (map[string]string, error)
	// IsAncestor reports, inside dir, whether ancestor is an ancestor of
	// descendant. It is only meaningful when both SHAs are present locally
	// (false when they are not).
	IsAncestor(ctx context.Context, dir, ancestor, descendant string) bool
	DeleteRef(ctx context.Context, workDir string, rem provider.Remote, refType, refName string) error
	// RefTip reads the tip SHA of the remote rem's ref (refType/refName) with
	// ls-remote. It returns "" when the ref does not exist (not an error).
	//
	// One ls-remote does both the existence check and the tip read. It is there
	// for delete idempotency: "" means the ref is already gone, so the delete is
	// treated as a successful no-op and the bidirectional delete loop is broken.
	// And once the delete is done there is nothing left at the destination to
	// query, so this SHA, read just before the delete, becomes the only record of
	// what disappeared.
	RefTip(ctx context.Context, rem provider.Remote, refType, refName string) (string, error)
	// EnsureBareDir makes dir a usable bare repository. A no-op if it already is.
	EnsureBareDir(ctx context.Context, dir string) error
	// HasObject checks whether the sha object is still present in dir. False once
	// gc has collected it.
	HasObject(ctx context.Context, dir, sha string) bool
	// FetchObject brings a single sha from the remote rem into dir. It is used to
	// bring back an object whose ref is already gone, so it fails if the server
	// refuses requests for unreachable objects.
	FetchObject(ctx context.Context, dir string, rem provider.Remote, sha string) error
	// CreateRef creates sha as fullRef on the remote. It is not a force — a
	// restore brings back a ref that is missing rather than overwriting someone
	// else's work, so if somebody created the same name in the meantime it is
	// right for git to refuse.
	CreateRef(ctx context.Context, dir string, rem provider.Remote, sha, fullRef string) error
	// CommitAuthor returns the author name of the latest commit on the given ref.
	CommitAuthor(ctx context.Context, dir, ref string) (string, error)
	// GCAuto runs git's housekeeping on the mirror, but only when git itself
	// decides it is due. Failure is not a mirroring failure.
	GCAuto(ctx context.Context, dir string) (GCStats, error)
	// SanitizeCache removes the credentials the old scheme left behind in the
	// cache directory. A mirror cloned while credentials were embedded in the URL
	// sits on the PVC with a live token in remote.origin.url. It returns true
	// when it actually removed some (that is worth a log line, and in normal
	// operation it is false).
	SanitizeCache(ctx context.Context, dir string, rem provider.Remote) (bool, error)
}

// GCStats reports what housekeeping actually did.
//
// It exists so the caller can log the rare run that consolidated something
// without logging the common no-op. gc --auto is silent either way, and
// counting packfiles is the only way to tell the two apart — which matters,
// because otherwise "did the cache ever get cleaned up" can only be answered by
// exec'ing into the pod and counting files by hand.
type GCStats struct {
	PacksBefore int
	PacksAfter  int
	// KeepsPruned counts abandoned .keep markers removed before gc ran. It is
	// reported separately because it is worth a log line on its own: pruning
	// one is what makes a pack eligible for repacking again, and that can
	// happen on a fetch where gc itself is still a no-op.
	KeepsPruned int
}

// Ran reports whether housekeeping actually consolidated packfiles.
func (g GCStats) Ran() bool { return g.PacksAfter < g.PacksBefore }

// Service handles git mirror operations.
type Service struct {
	configs        []config.RepoConfig
	providers      map[string]provider.Provider
	notifier       notify.Notifier
	recorder       history.Recorder
	workDir        string
	git            GitRunner
	timeoutSeconds int
	repoLocks      map[string]*sync.Mutex
	repoLocksMu    sync.Mutex
}

// gitWaitDelay is the grace period the Go runtime waits, after a ctx
// cancellation (a timeout), before it force-closes the pipes when a git child
// process never releases stdout/stderr, so that Wait/CombinedOutput cannot block
// forever. Once it elapses Wait returns and the per-repo mutex is released
// (preventing the regression where an orphan held the pipe and leaked the mutex).
const gitWaitDelay = 10 * time.Second

// newGitCmd builds a git command configured so that, on a timeout or a
// cancellation, the children git forked (git-remote-http, index-pack, and the
// rest) are cleaned up along with it. exec.CommandContext's default cancellation
// only SIGKILLs the parent git, leaving the children as orphans, and such a
// child holding the stdout pipe blocked CombinedOutput/Run forever, which leaked
// the per-repo mutex so that it was never released again (the 2026-06 demo-repo
// incident). Setpgid puts it in a new process group, Cancel sends SIGKILL to the
// whole group (-pgid), and WaitDelay forces Wait to return even when the pipes
// are never closed.
func newGitCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// A negative PID = signal the whole process group. With Setpgid the pgid
		// equals the parent git's PID, so unlike the default behaviour of killing
		// only the parent, this cleans up the forked children in one go.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = gitWaitDelay
	return cmd
}

// credentialFlags detaches how credentials are obtained from the surrounding
// environment.
//
// Both values have to be ours to decide. The child git inherits os.Environ(),
// and if that carries GIT_CONFIG_COUNT/KEY_n/VALUE_n, git reads them as
// configuration at the same level as a command-line `-c`. So a single value
// injected from outside can change this service's authentication wholesale.
//
//   - credential.interactive: when it is off, git does not even **ask** askpass
//     and dies immediately with "unable to get password from user". There is not
//     even a retry. GitLab Runner puts this value in the job environment as
//     never — that was the actual cause of CI going red on 2026-08-25, and while
//     it only happened inside the runner then, it happens the same way in any
//     environment that sets the same variable.
//   - credential.helper: an empty value empties the helper list. It stops a
//     helper installed in the surroundings from handing out a **different** token
//     for the same host — this service's credentials always come from the
//     configuration, and there must be no other source.
//
// A command-line `-c` is applied after GIT_CONFIG_*, so the values here win
// (verified by measurement).
var credentialFlags = []string{
	"-c", "credential.interactive=true",
	"-c", "credential.helper=",
}

// newGitRemoteCmd builds a git command that talks to a remote. It does the same
// process-group cleanup as newGitCmd, and on top of that passes credentials only
// through the GIT_ASKPASS side channel (see internal/askpass — that is where the
// reason no token ever reaches the command line lives).
func newGitRemoteCmd(ctx context.Context, rem provider.Remote, args ...string) *exec.Cmd {
	if rem.HasCredentials() {
		args = append(append([]string{}, credentialFlags...), args...)
	}
	cmd := newGitCmd(ctx, args...)
	cmd.Env = gitRemoteEnv(rem)
	return cmd
}

// askpassHelper is the path to our own executable, to be re-run as the askpass
// helper.
//
// It is cached because it only has to be looked up once. Failure realistically
// does not happen (on Linux it reads /proc/self/exe), but if it does it must not
// pass silently: without the helper, connecting to a remote that needs
// authentication makes git fail for want of credentials, and thanks to
// GIT_TERMINAL_PROMPT=0 it does not hang but dies immediately. The reason is
// left here for whoever has to read that failure.
var askpassHelper = sync.OnceValue(func() string {
	exe, err := os.Executable()
	if err != nil {
		slog.Error("cannot locate own executable for GIT_ASKPASS; "+
			"authenticated git operations will fail", "error", err)
		return ""
	}
	return exe
})

// gitRemoteEnv builds the environment handed to a remote git command.
//
// It uses os.Environ() as the base and appends after it — exec takes the last
// value for a duplicated key, so even if something like GIT_ASKPASS came in from
// outside, the value set here wins.
func gitRemoteEnv(rem provider.Remote) []string {
	env := append(os.Environ(),
		// No interactive prompts. When askpass is absent or cannot answer, git
		// looks for a tty, and a pod has none, so instead of "authentication
		// failed" it hangs until the timeout and ends in a SIGKILL — a failure
		// whose cause cannot be told. Failing immediately is better.
		"GIT_TERMINAL_PROMPT=0",
	)
	helper := askpassHelper()
	if !rem.HasCredentials() || helper == "" {
		return env
	}
	return append(env,
		"GIT_ASKPASS="+helper,
		askpass.EnvActive+"=1",
		askpass.EnvUsername+"="+rem.Username,
		askpass.EnvPassword+"="+rem.Password,
		// The helper picks which value to hand back by looking at the prompt string
		// ("Username for ..."), so the locale is pinned. A translated prompt makes
		// everything read as the password.
		"LC_ALL=C",
	)
}

// defaultGitRunner executes real git commands.
type defaultGitRunner struct{}

// CloneMirror fetches a fresh mirror while preserving the existing cache. It
// clones into a temporary directory (dir+".tmp") first and swaps dir out with an
// atomic rename only on success. When the clone fails (a slow line, a timeout)
// the existing dir — a perfectly good mirror — is left untouched, so the next
// event can recover with an incremental fetch. It used to wipe the cache first
// with os.RemoveAll(dir) before cloning, so a failed fallback clone left an empty
// state with the cache gone, forcing a full clone every time in a vicious circle
// (the 2026-06 demo-repo incident).
func (d *defaultGitRunner) CloneMirror(ctx context.Context, rem provider.Remote, dir string) error {
	tmpDir := dir + ".tmp"
	// Clean up the leftovers a previous failure left in the temp directory (the
	// existing dir is not touched).
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("cleanup temp before clone: %w", err)
	}
	cmd := newGitRemoteCmd(ctx, rem, "clone", "--mirror", rem.URL, tmpDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Failure: clean up only the temporary copy and preserve the existing dir
		// (the cache).
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("%w: %s", err, string(out))
	}
	// Success: remove the existing dir, then move the temporary copy into place
	// (same parent directory, so the rename is atomic).
	if err := os.RemoveAll(dir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("cleanup before swap: %w", err)
	}
	if err := os.Rename(tmpDir, dir); err != nil {
		// On a rename failure (a rare case on an NFS-backed PVC, for instance)
		// clean up the freshly cloned temporary copy to avoid a half-state. dir
		// (the existing cache) was already removed above, so the next event
		// recovers through the initial clone path (single-pod + per-repo mutex,
		// so there is no concurrent race).
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("swap mirror into place: %w", err)
	}
	return nil
}

func (d *defaultGitRunner) FetchMirror(ctx context.Context, rem provider.Remote, dir string) error {
	cmd := newGitRemoteCmd(ctx, rem, "-C", dir, "fetch", "--prune", rem.URL, "+refs/*:refs/*")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch mirror: %w: %s", err, string(out))
	}
	return nil
}

// runPush runs a single git push --porcelain --force call and reports whether
// anything changed. It separates stdout from stderr so the progress log on
// stderr cannot contaminate the porcelain parsing. errLabel is used as the
// prefix of the error message on failure (the tests check that string).
func runPush(ctx context.Context, dir string, rem provider.Remote, errLabel string, pushArgs ...string) (PushResult, error) {
	// core.abbrev=40 is what makes a forced update recoverable. git abbreviates
	// the SHAs in the porcelain summary to 7 characters by default, and while
	// that is enough to look an object up in a local clone, it is not enough to
	// ask a remote for one: the fetch protocol wants a full object name, so
	// "git fetch <url> 576d962" is rejected while the 40-character form works.
	// Recording the short form would mean recording a tip nobody can retrieve.
	// 🔴 No --force here, and none may be added. Force and the lease are not
	// belt and braces: git evaluates `ref->force || force_update` first and
	// never promotes REF_STATUS_REJECT_STALE when either is set, so a global
	// --force disables every lease on the command line exactly the way a '+'
	// on the refspec does. Its absence is load-bearing. The lease supplies the
	// force on its own — see PushMirror, where the refspec is written without
	// a '+' for the same reason.
	args := append([]string{"-C", dir, "-c", "core.abbrev=40", "push", "--porcelain"}, pushArgs...)
	cmd := newGitRemoteCmd(ctx, rem, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// A SIGKILL caused by a timeout (the ctx deadline being exceeded) leaves
		// git saying only "signal: killed", which is indistinguishable from an
		// external kill such as an OOM. ctx.Err() identifies the cause, and
		// "timed out (deadline exceeded)" is added to the notification to help
		// triage timeout versus OOM.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return PushResult{}, fmt.Errorf("%s: timed out (deadline exceeded): %w: %s", errLabel, err, stderr.String())
		}
		// git exits non-zero when any ref is rejected, and a stale lease is a
		// rejection. That one is the guard firing, not a broken push, so it must
		// not surface as an error: an error retries the SQS message and
		// eventually parks it in the DLQ, for a condition the very next event
		// resolves.
		//
		// Only that one, though. A protected branch or a pre-receive hook
		// refusing is also a '!' line, and swallowing it would leave that
		// branch's mirroring permanently stopped with no failure anywhere —
		// reported as a successful sync if any other ref moved, and as the
		// guard working if none did. Anything that is not the lease keeps the
		// old loud path: notifyFailure, a fail row, and an SQS retry.
		if res := parsePush(stdout.String()); allStaleLease(res.Rejected) {
			return res, nil
		}
		return PushResult{}, fmt.Errorf("%s: %w: %s", errLabel, err, stderr.String())
	}
	return parsePush(stdout.String()), nil
}

// pushBatchSize caps how many refs go into one git invocation.
//
// A first sync of a repo that has accumulated thousands of build tags would
// otherwise put every one of them on a single command line. The batches are
// independent pushes, so a later batch failing does not undo an earlier one —
// acceptable here because each ref is guarded by its own lease and the next
// event or the hourly reconcile retries whatever did not land.
const pushBatchSize = 500

func (d *defaultGitRunner) PushMirror(ctx context.Context, dir string, rem provider.Remote, specs []PushSpec) (PushResult, error) {
	var res PushResult
	for start := 0; start < len(specs); start += pushBatchSize {
		end := min(start+pushBatchSize, len(specs))
		args := []string{rem.URL}
		for _, s := range specs[start:end] {
			// The lease is per-ref and explicit. The bare --force-with-lease
			// form reads the remote-tracking ref, which a mirror pushing to a
			// remote it never fetches from does not have, so it would silently
			// degrade to no protection at all.
			//
			// 🔴 The refspec must NOT carry a '+'. A per-refspec force wins over
			// the lease outright: git performs the update and never evaluates
			// the expected value, so `+ref:ref` with a deliberately wrong lease
			// still overwrites (verified against git 2.47). The lease alone
			// supplies the force — a legitimate non-fast-forward still lands,
			// and still reports itself as a forced update.
			args = append(args, "--force-with-lease="+s.Ref+":"+s.Lease, s.Ref+":"+s.Ref)
		}
		batch, err := runPush(ctx, dir, rem, "push refs", args...)
		if err != nil {
			return PushResult{}, err
		}
		res.Changed = res.Changed || batch.Changed
		res.Forced = append(res.Forced, batch.Forced...)
		res.Rejected = append(res.Rejected, batch.Rejected...)
	}
	return res, nil
}

// ListRefTips reads the mirror dir's local branches/tags as a fullRef → SHA map.
func (d *defaultGitRunner) ListRefTips(ctx context.Context, dir string) (map[string]string, error) {
	cmd := newGitCmd(ctx, "-C", dir, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/", "refs/tags/")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("for-each-ref tips: %w: %s", err, stderr.String())
	}
	return parseRefTips(stdout.String(), " "), nil
}

// RemoteRefs reads the branches/tags of the remote url as a fullRef → SHA map.
func (d *defaultGitRunner) RemoteRefs(ctx context.Context, rem provider.Remote) (map[string]string, error) {
	cmd := newGitRemoteCmd(ctx, rem, "ls-remote", "--heads", "--tags", rem.URL)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// The URL is deliberately not in this message. It no longer carries
		// credentials — those go over GIT_ASKPASS now — but this error reaches
		// Slack, the history and the console, and the route is already on the
		// event. Repeating the remote address in a routine "the destination
		// blinked" failure adds a line to read and nothing to act on.
		return nil, fmt.Errorf("ls-remote refs: %w: %s", err, stderr.String())
	}
	return parseRefTips(stdout.String(), "\t"), nil
}

// parseRefTips reads "<sha><sep><ref>" lines into a fullRef → SHA map.
//
// Peeled lines (`refs/tags/x^{}`) are dropped. ls-remote emits one for every
// annotated tag, holding the commit the tag points at rather than the tag
// object the ref itself holds. Keeping it would overwrite the real entry and
// make every annotated tag look permanently out of sync with the local side,
// which compares tag object to tag object.
func parseRefTips(output, sep string) map[string]string {
	tips := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), sep)
		if !ok {
			continue
		}
		ref = strings.TrimSpace(ref)
		if ref == "" || strings.HasSuffix(ref, "^{}") {
			continue
		}
		tips[ref] = strings.TrimSpace(sha)
	}
	return tips
}

// IsAncestor reports, inside dir, whether ancestor is an ancestor of descendant.
//
// Something that is not a commit (an unpeeled tag object, say) is resolved to a
// commit by git on its own. If either of the two is missing locally git fails and
// the result is false — which has to be read as "no judgement is made when it is
// unknown" rather than "unknown means not an ancestor", so the caller must always
// confirm existence with HasObject before using this function.
func (d *defaultGitRunner) IsAncestor(ctx context.Context, dir, ancestor, descendant string) bool {
	cmd := newGitCmd(ctx, "-C", dir, "merge-base", "--is-ancestor", ancestor, descendant)
	return cmd.Run() == nil
}

// ListRefs returns every local branch/tag ref (full name) in the mirror dir.
// On a full sync (no meta.Ref) it is used to exclude the refs a ref_override
// forbids in this direction.
func (d *defaultGitRunner) ListRefs(ctx context.Context, dir string) ([]string, error) {
	cmd := newGitCmd(ctx, "-C", dir, "for-each-ref", "--format=%(refname)", "refs/heads/", "refs/tags/")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("for-each-ref: %w: %s", err, stderr.String())
	}
	var refs []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			refs = append(refs, line)
		}
	}
	return refs, nil
}

// buildFullRefspecs builds, for a full sync (no meta.Ref), the list of force
// refspecs (+ref:ref) from the local ref list, excluding the refs a ref_override
// forbids in the current direction (fromProvider→toProvider). An empty result
// means there is no ref to push in this direction.
func buildFullRefspecs(localRefs []string, repoCfg config.RepoConfig, fromProvider, toProvider string) []string {
	specs := make([]string, 0, len(localRefs))
	for _, ref := range localRefs {
		short := EventMeta{Ref: ref}.RefName()
		if ov := repoCfg.MatchRefOverride(short); ov != nil && (ov.From != fromProvider || ov.To != toProvider) {
			continue // forbidden in this direction → excluded
		}
		specs = append(specs, ref)
	}
	return specs
}

// PushResult reports what a push did, beyond whether it did anything.
//
// The Changed bool used to be the entire return value, which meant a push that
// fast-forwarded and a push that rewound a branch were indistinguishable to
// everything downstream — the log, the history, Slack, the console. git had
// already told us the difference on stdout and we dropped it.
type PushResult struct {
	// Changed is true when at least one ref moved.
	Changed bool
	// Forced lists the refs that moved non-fast-forward, with the tip each one
	// replaced. Non-empty means commits reachable only from those old tips are
	// no longer reachable at the destination.
	Forced []history.ForcedRef
	// Rejected lists the refs the destination refused. With force-with-lease in
	// play the ordinary cause is a stale lease: the destination moved between
	// the check and the push, so git declined rather than overwrite whatever
	// arrived in between. That is the guard working, not a failure — the write
	// that moved the destination raises its own event in the other direction
	// and the two sides converge forward.
	Rejected []string
}

// PushSpec is one ref to push together with the destination tip it was checked
// against.
//
// Lease is what turns the check into a guarantee. Reading the destination and
// then pushing leaves a gap, and the gap is where 2026-08-11 happened: the tip
// read at the start of a 55-second fetch was written 55 seconds later, over a
// commit that landed in between. Handing git the observed tip closes it — the
// write only lands if the destination is still exactly there.
//
// An empty Lease means "this ref must not exist at the destination", which is
// what --force-with-lease=<ref>: expresses. It is the right expectation for a
// ref the destination did not report: if something created it in the meantime,
// this push must not be what overwrites it.
type PushSpec struct {
	Ref   string // full ref name, e.g. refs/heads/main
	Lease string // destination tip observed just before the push; "" = must not exist
}

// parsePush reads git push --porcelain output.
//
// Porcelain ref lines are "<flag>\t<from>:<to>\t<summary>", where flag is one
// of ' ' (fast-forward), '+' (forced, non-fast-forward), '-' (deleted),
// '*' (new ref), '=' (up to date) or '!' (rejected). For an updated ref the
// summary is "<old>..<new>", and for a forced one "<old>...<new>" — which is
// why the old tip can be recovered here without asking the remote, CloudTrail,
// or anything else: it is sitting in the output of the push that discarded it.
//
// The flag is read at byte 0, so lines must not be trimmed on the left: a
// fast-forward's flag IS a space, and trimming it shifts every field.
func parsePush(output string) PushResult {
	var res PushResult
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" ||
			strings.HasPrefix(line, "To ") || strings.HasPrefix(line, "Done") {
			continue
		}
		if line[0] == '=' {
			continue
		}
		// '!' is a ref the destination refused. It must be read before Changed
		// is set: a rejected ref did not move, and counting it as a change makes
		// a push that wrote nothing announce itself as a successful sync.
		if line[0] == '!' {
			res.Rejected = append(res.Rejected, parseRejectedLine(line))
			continue
		}
		res.Changed = true
		if line[0] != '+' {
			continue
		}
		if ref, old, updated, ok := parseForcedLine(line); ok {
			res.Forced = append(res.Forced, history.ForcedRef{Ref: ref, Old: old, New: updated})
		}
	}
	return res
}

// staleLeaseMarker is git's wording when --force-with-lease finds the
// destination at something other than the expected value.
const staleLeaseMarker = "stale info"

// allStaleLease reports whether every rejection in the push was the lease
// firing, i.e. whether the push can be treated as a benign no-op.
//
// An empty list is not benign — it means nothing was rejected at all, so the
// non-zero exit came from somewhere else entirely and must stay an error.
func allStaleLease(rejected []string) bool {
	if len(rejected) == 0 {
		return false
	}
	for _, r := range rejected {
		if !strings.Contains(r, staleLeaseMarker) {
			return false
		}
	}
	return true
}

// parseRejectedLine renders one '!' porcelain line as "<ref>: <summary>".
//
// The summary is kept because the two rejections that reach here mean opposite
// things: "(stale info)" is the lease doing its job and needs no attention,
// while a hook or protected-branch refusal is a real problem that will repeat
// on every retry. Recording only the ref name would make them indistinguishable
// in the log.
func parseRejectedLine(line string) string {
	fields := strings.Split(line, "\t")
	if len(fields) < 2 {
		return strings.TrimSpace(line)
	}
	ref := fields[1]
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		ref = ref[i+1:]
	}
	if len(fields) < 3 {
		return ref
	}
	return ref + ": " + strings.TrimSpace(fields[2])
}

// parseForcedLine pulls the destination ref and the two tips out of one '+'
// porcelain line. A line that does not parse is reported as such rather than
// recorded with blank SHAs: a ForcedRef whose Old is empty would claim a rewind
// happened while withholding the only field that makes the record actionable.
func parseForcedLine(line string) (ref, old, updated string, ok bool) {
	fields := strings.Split(line, "\t")
	if len(fields) < 3 {
		return "", "", "", false
	}
	// "<from>:<to>" — the destination ref is what was overwritten. Split at the
	// last colon so a local ref containing one cannot shift the boundary.
	ref = fields[1]
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		ref = ref[i+1:]
	}
	old, updated, found := strings.Cut(fields[2], "...")
	if !found {
		return "", "", "", false
	}
	// git may append " (<reason>)" to the summary; keep only the SHA.
	if i := strings.IndexByte(updated, ' '); i >= 0 {
		updated = updated[:i]
	}
	old, updated = strings.TrimSpace(old), strings.TrimSpace(updated)
	if ref == "" || old == "" || updated == "" {
		return "", "", "", false
	}
	return ref, old, updated, true
}

func (d *defaultGitRunner) CommitAuthor(ctx context.Context, dir, ref string) (string, error) {
	cmd := newGitCmd(ctx, "-C", dir, "log", "-1", "--format=%an", ref)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git log for ref %q: %w: %s", ref, err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

func (d *defaultGitRunner) DeleteRef(ctx context.Context, workDir string, rem provider.Remote, refType, refName string) error {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	initCmd := newGitCmd(ctx, "init", "--bare", workDir)
	if err := initCmd.Run(); err != nil {
		return fmt.Errorf("git init: %w", err)
	}

	ref := fullRefName(refType, refName)

	cmd := newGitRemoteCmd(ctx, rem, "-C", workDir, "push", rem.URL, ":"+ref)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("push delete ref: %w: %s", err, string(out))
	}
	return nil
}

// RefTip reads the exact full-ref tip SHA of the remote rem with git ls-remote.
// The full ref is passed directly to avoid prefix matching (refs/heads/feat
// matching refs/heads/feature). Empty output gives "" (exit 0); only a network
// or authentication error gives err.
//
// ls-remote emits one "<sha>\t<ref>" line. Because a full ref is queried there is
// at most one line.
func (d *defaultGitRunner) RefTip(ctx context.Context, rem provider.Remote, refType, refName string) (string, error) {
	ref := fullRefName(refType, refName)
	cmd := newGitRemoteCmd(ctx, rem, "ls-remote", rem.URL, ref)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ls-remote %s: %w: %s", ref, err, stderr.String())
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return "", nil
	}
	sha, _, _ := strings.Cut(out, "\t")
	return strings.TrimSpace(sha), nil
}

// EnsureBareDir makes dir a usable bare repository.
//
// Running `git init --bare` again on a dir that is already a repository is a
// no-op, so calling it against a live mirror cache is safe — only the first call
// means anything.
func (d *defaultGitRunner) EnsureBareDir(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	if err := newGitCmd(ctx, "init", "--bare", dir).Run(); err != nil {
		return fmt.Errorf("git init %s: %w", dir, err)
	}
	return nil
}

// HasObject checks whether the sha object is present in dir.
//
// `^{commit}` is appended so it also checks that the object resolves to a commit.
// A restore brings a branch tip back, so a state where only a blob or a tree
// survives is of no use.
func (d *defaultGitRunner) HasObject(ctx context.Context, dir, sha string) bool {
	cmd := newGitCmd(ctx, "-C", dir, "cat-file", "-e", sha+"^{commit}")
	return cmd.Run() == nil
}

// FetchObject brings a single sha from the remote rem into dir.
//
// The object being asked for here is unreachable from any ref — the ref having
// been deleted is the whole reason for restoring it. It fails if the server is
// configured to refuse requests for unreachable objects, and in that case the
// only remaining option is to hope the object is still in the mirror cache.
func (d *defaultGitRunner) FetchObject(ctx context.Context, dir string, rem provider.Remote, sha string) error {
	cmd := newGitRemoteCmd(ctx, rem, "-C", dir, "fetch", "--no-tags", rem.URL, sha)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fetch object %s: %w: %s", sha, err, stderr.String())
	}
	return nil
}

// CreateRef creates sha as fullRef on the remote. It fails if the ref already
// exists.
//
// The empty expect in `--force-with-lease=<ref>:` means "this ref must not
// exist", and git checks that atomically on the remote. The caller looks first
// with ls-remote, but the window between that check and the push is closed only
// here.
//
// It was initially left as a plain non-force push (no `+`), and that is not
// enough: all a non-force refuses is a non-fast-forward update, so if somebody
// re-created that name at an **ancestor** of sha after the delete, the push
// succeeds as a fast-forward and moves that person's ref. This was reproduced and
// confirmed — the assumption held for tags and was wrong for branches.
func (d *defaultGitRunner) CreateRef(ctx context.Context, dir string, rem provider.Remote, sha, fullRef string) error {
	cmd := newGitRemoteCmd(ctx, rem, "-C", dir, "push", "--force-with-lease="+fullRef+":", rem.URL, sha+":"+fullRef)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("create ref %s: %w: %s", fullRef, err, stderr.String())
	}
	return nil
}

// remoteOriginURLKey is the config key a clone writes the remote address into.
const remoteOriginURLKey = "remote.origin.url"

// SanitizeCache removes the credentials the old scheme left behind in the cache
// directory.
//
// While credentials were embedded in the clone URL, `git clone --mirror` wrote
// the whole URL it was given into remote.origin.url. That file sits on the PVC,
// so even after the token was taken out of the code, a cache that already exists
// keeps carrying the token. It is cleaned up here the first time the new code
// touches that cache.
//
// This one key is the only place inside the cache where credentials survive.
// FETCH_HEAD also writes the remote address on every line, but git strips the
// userinfo before writing it — verified by measurement and left out of scope.
//
// It also brings the address into line when only the address changed (a base_url
// change, say), but does not return true for that — the return value means
// "credentials were removed", and only that is worth a log line.
func (d *defaultGitRunner) SanitizeCache(ctx context.Context, dir string, rem provider.Remote) (bool, error) {
	cur, ok, err := gitConfigGet(ctx, dir, remoteOriginURLKey)
	if err != nil {
		return false, err
	}
	if !ok || cur == rem.URL {
		return false, nil
	}
	if err := gitConfigSet(ctx, dir, remoteOriginURLKey, rem.URL); err != nil {
		return false, err
	}
	return hasCredentialsInURL(cur), nil
}

// hasCredentialsInURL reports whether userinfo (credentials) is embedded in the
// address. A value that does not parse is not a URL (a local path, say), so it is
// treated as carrying no credentials either.
func hasCredentialsInURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.User != nil
}

// gitConfigGet reads the value of a key. A missing key gives ok=false and is not
// an error — git reports that case with nothing but exit 1.
func gitConfigGet(ctx context.Context, dir, key string) (string, bool, error) {
	cmd := newGitCmd(ctx, "-C", dir, "config", "--get", key)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("git config --get %s: %w: %s", key, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), true, nil
}

// gitConfigSet writes the value of a key.
func gitConfigSet(ctx context.Context, dir, key, value string) error {
	cmd := newGitCmd(ctx, "-C", dir, "config", key, value)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git config %s: %w: %s", key, err, stderr.String())
	}
	return nil
}

// gcAutoPackLimit is the packfile count above which housekeeping is due.
//
// git's default is 50, which is tuned for a repository a person works in. A
// mirror cache is different: every incremental fetch drops another packfile in,
// nothing ever consolidates them on its own, and the cost is paid by every
// later fetch and push having more files to scan. The live cache reached 19
// packfiles on one mirror while sitting far below the default, so the default
// would have let it drift for months before acting.
//
// 15 keeps a mirror in single digits most of the time while still only
// repacking roughly every fifteenth fetch, so the work stays amortised.
const gcAutoPackLimit = 15

// staleKeepAge is how long a .keep marker must have gone untouched before
// housekeeping treats it as abandoned.
//
// git drops a .keep beside an incoming packfile so a concurrent gc cannot
// delete the pack out from under the fetch, then removes it when the fetch
// finishes. A fetch that dies first leaves the marker behind — newGitCmd
// SIGKILLs the whole process group at the timeout, and a replaced pod takes its
// fetch with it — and repack then excludes that pack permanently. The live
// cache carried one from a pod that no longer exists, pinning 98MB and leaving
// the mirror at three packfiles where it should have consolidated to one.
//
// Age is deliberately the whole test. A fetch cannot outlive the configured
// timeout_seconds (600 in the live config), so a marker untouched for a day is
// abandoned by definition, with a margin no plausible reconfiguration closes.
// Reading the pid and hostname git writes into the marker would look more
// precise, but it decides the same cases while adding ways to be wrong: a
// hostname is only conclusive because there is a single writer today, and a pid
// is only meaningful on the host that recorded it.
const staleKeepAge = 24 * time.Hour

// pruneStaleKeeps removes abandoned .keep markers so the packs they pin become
// repackable again, and reports how many it removed.
//
// Errors are ignored throughout. This is opportunistic cleanup on the way into
// gc, so a marker that cannot be read or removed simply survives to the next
// fetch — the same reasoning that makes gc failure a warning rather than a
// mirroring failure.
func pruneStaleKeeps(dir string) int {
	packDir := filepath.Join(dir, "objects", "pack")
	entries, err := os.ReadDir(packDir)
	if err != nil {
		return 0
	}
	pruned := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".keep") {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) <= staleKeepAge {
			continue
		}
		if os.Remove(filepath.Join(packDir, e.Name())) == nil {
			pruned++
		}
	}
	return pruned
}

// GCAuto packs loose objects and consolidates packfiles when git decides it is
// due, and does nothing otherwise.
//
// Abandoned .keep markers are pruned first so the packs they pin can take part
// in the same pass — see staleKeepAge.
//
// Two settings are overridden:
//
//   - gc.autoDetach=false. At its default git forks the housekeeping into the
//     background, where it would outlive the per-repo lock the caller holds and
//     could run alongside the next fetch or push on the same mirror. Keeping it
//     in the foreground keeps it inside both the lock and the git timeout.
//   - gc.autoPackLimit, see above.
func (d *defaultGitRunner) GCAuto(ctx context.Context, dir string) (GCStats, error) {
	// Pruning comes first so an unpinned pack can join this very gc pass.
	pruned := pruneStaleKeeps(dir)
	stats := GCStats{KeepsPruned: pruned, PacksBefore: countPacks(dir)}

	cmd := newGitCmd(ctx, "-C", dir,
		"-c", "gc.autoDetach=false",
		"-c", fmt.Sprintf("gc.autoPackLimit=%d", gcAutoPackLimit),
		"gc", "--auto", "--quiet")
	if out, err := cmd.CombinedOutput(); err != nil {
		return stats, fmt.Errorf("gc --auto: %w: %s", err, string(out))
	}

	stats.PacksAfter = countPacks(dir)
	return stats, nil
}

// countPacks counts the packfiles in a bare mirror.
//
// Only packs are counted, not loose objects: the pack count is the threshold
// that was actually tuned, one directory read answers it, and a gc triggered by
// it packs the loose objects in the same pass anyway. Counting loose objects
// would mean walking 256 fanout directories on every fetch to learn nothing
// extra.
//
// Errors give 0. This number is only used to decide whether to log, so failing
// to read it must never surface as a mirroring problem.
func countPacks(dir string) int {
	entries, err := os.ReadDir(filepath.Join(dir, "objects", "pack"))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".pack") {
			n++
		}
	}
	return n
}

// WorkDir returns the mirror working directory: $WORK_DIR when set, otherwise
// the default. It is exported so the history file can be placed on the same
// volume without a second copy of this lookup drifting away from this one.
func WorkDir() string {
	if dir := os.Getenv("WORK_DIR"); dir != "" {
		return dir
	}
	return defaultWorkDir
}

// isGitDir returns true if dir exists and looks like a bare git repository.
func isGitDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "HEAD"))
	return err == nil && !info.IsDir()
}

// New creates a mirror service. Returns an error if any configured provider fails to initialize.
//
// recorder receives one history event per completed operation; pass
// history.NewNoop() to disable recording. A nil recorder is treated as no-op so
// the service never has to nil-check on the hot path.
func New(cfg *config.Config, notifier notify.Notifier, recorder history.Recorder) (*Service, error) {
	if recorder == nil {
		recorder = history.NewNoop()
	}
	providers := make(map[string]provider.Provider)
	for name, pcfg := range cfg.Providers {
		p, err := provider.New(name, pcfg)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		providers[name] = p
	}

	return &Service{
		configs:        cfg.Repos,
		providers:      providers,
		notifier:       notifier,
		recorder:       recorder,
		workDir:        WorkDir(),
		git:            &defaultGitRunner{},
		timeoutSeconds: cfg.Mirror.TimeoutSeconds,
		repoLocks:      make(map[string]*sync.Mutex),
	}, nil
}

// withGitTimeout builds the timeout ctx for a git operation. The point is when
// it is called — doMirror/doDeleteRef call it after taking the per-repo mutex, so
// the timeout runs from the moment the lock is acquired rather than from entering
// the queue. That prevents the problem where a burst of events for the same repo
// is serialised and a later event spends its budget waiting in the queue and then
// gets SIGKILLed in the middle of its own git push — each operation gets the full
// timeout_seconds budget regardless of queue depth. And since every operation is
// bound to finish within timeout_seconds of acquiring the lock, the waiting itself
// is bounded too, by (queue depth × timeout).
func (s *Service) withGitTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Duration(s.timeoutSeconds)*time.Second)
}

// allowsSourceToTarget returns true if direction permits source → target sync.
func allowsSourceToTarget(direction string) bool {
	dir := strings.ToLower(direction)
	return dir == config.DirectionSourceToTarget || dir == config.DirectionBidirectional
}

// allowsTargetToSource returns true if direction permits target → source sync.
func allowsTargetToSource(direction string) bool {
	dir := strings.ToLower(direction)
	return dir == config.DirectionTargetToSource || dir == config.DirectionBidirectional
}

// Sync mirrors a repository triggered by source-side event (e.g. CodeCommit → SQS).
// repoName is the source_path of the repo.
func (s *Service) Sync(ctx context.Context, repoName string, meta EventMeta) error {
	for _, repoCfg := range s.configs {
		if repoCfg.SourcePath != repoName {
			continue
		}
		if allowsSourceToTarget(repoCfg.Direction) {
			return s.doMirror(ctx, repoCfg, repoCfg.Source, repoCfg.SourcePath, repoCfg.Target, repoCfg.TargetPath, meta)
		}
		return fmt.Errorf("repo %q direction %q does not allow source-to-target sync", repoName, repoCfg.Direction)
	}
	return fmt.Errorf("repo %q not configured for mirroring", repoName)
}

// SyncByTarget mirrors a repository triggered by target-side event (e.g. GitLab/GitHub webhook).
// providerKey is a provider name or a provider type (see providerMatches),
// repoPath is the target_path of the repo.
func (s *Service) SyncByTarget(ctx context.Context, providerKey, repoPath string, meta EventMeta) error {
	for _, repoCfg := range s.configs {
		// Match by target provider + target path
		tgtProvider, ok := s.providers[repoCfg.Target]
		if !ok {
			continue
		}
		if providerMatches(repoCfg.Target, tgtProvider.Type(), providerKey) && repoCfg.TargetPath == repoPath {
			if allowsTargetToSource(repoCfg.Direction) {
				return s.doMirror(ctx, repoCfg, repoCfg.Target, repoCfg.TargetPath, repoCfg.Source, repoCfg.SourcePath, meta)
			}
			return fmt.Errorf("repo %q direction %q does not allow target-to-source sync", repoCfg.Name, repoCfg.Direction)
		}

		// Match by source provider + source path (for source-side webhook)
		srcProvider, ok := s.providers[repoCfg.Source]
		if !ok {
			continue
		}
		if providerMatches(repoCfg.Source, srcProvider.Type(), providerKey) && repoCfg.SourcePath == repoPath {
			if allowsSourceToTarget(repoCfg.Direction) {
				return s.doMirror(ctx, repoCfg, repoCfg.Source, repoCfg.SourcePath, repoCfg.Target, repoCfg.TargetPath, meta)
			}
			return fmt.Errorf("repo %q direction %q does not allow source-to-target sync", repoCfg.Name, repoCfg.Direction)
		}
	}
	return fmt.Errorf("no matching repo for provider=%q path=%q", providerKey, repoPath)
}

// providerMatches reports whether the key the webhook handed over names one of
// this repo's two providers.
//
// The key arrives in two forms. When the payload's instance host narrowed it
// down, it is the provider name ("gitlab-old"); when it could not be narrowed
// down, it is the type the route knows ("gitlab"). The name is matched first, so
// the direction is still resolved when there are two instances of the same type,
// and when it falls back to the type it behaves exactly as it did before the
// narrowing existed — not changing the path taken by the existing repos is the
// whole point of this function.
func providerMatches(configuredName, providerType, key string) bool {
	return configuredName == key || providerType == key
}

// Retry runs a manual mirror sync triggered by the retry API.
// repoName is matched against RepoConfig.Name. direction is one of
// "source-to-target", "target-to-source", "auto", or "" (= "auto").
//
// "auto" fallback:
//   - bidirectional repo → target-to-source (2026-05-19 incident pattern)
//   - one-way repo      → the allowed single direction
//
// An explicit direction that conflicts with the repo's configured Direction
// returns an error (e.g. requesting source-to-target on a target-to-source repo).
func (s *Service) Retry(ctx context.Context, repoName, direction string, meta EventMeta) error {
	var repoCfg *config.RepoConfig
	for i := range s.configs {
		if s.configs[i].Name == repoName {
			repoCfg = &s.configs[i]
			break
		}
	}
	if repoCfg == nil {
		return fmt.Errorf("repo %q not configured", repoName)
	}

	dir := strings.ToLower(strings.TrimSpace(direction))
	if dir == "" || dir == "auto" {
		// "auto" resolution order:
		//   1. repo's explicit retry_direction (operator-pinned)
		//   2. built-in fallback by repo.Direction (bidirectional → target-to-source)
		if repoCfg.RetryDirection != "" {
			dir = strings.ToLower(repoCfg.RetryDirection)
		} else {
			dir = resolveAutoDirection(repoCfg.Direction)
		}
	}
	if !directionAllowed(repoCfg.Direction, dir) {
		return fmt.Errorf("repo %q direction %q does not allow retry direction %q",
			repoName, repoCfg.Direction, dir)
	}

	switch dir {
	case config.DirectionSourceToTarget:
		return s.doMirror(ctx, *repoCfg, repoCfg.Source, repoCfg.SourcePath,
			repoCfg.Target, repoCfg.TargetPath, meta)
	case config.DirectionTargetToSource:
		return s.doMirror(ctx, *repoCfg, repoCfg.Target, repoCfg.TargetPath,
			repoCfg.Source, repoCfg.SourcePath, meta)
	}
	return fmt.Errorf("unknown retry direction %q", dir)
}

// DirectionTo reports which of the repo's directions writes to toEndpoint.
//
// The console's re-run button resolves through here instead of asking for
// "auto": the row it sits under records the side that was written, and "auto"
// throws that away and re-runs whatever retry_direction pins. On a bidirectional
// repo that pin is target-to-source, so a failed source-to-target leg answered a
// click with a sync in the direction that had nothing to carry — the destination
// was already ahead, so it could only skip — and the gap survived until the
// hourly reconcile ran the leg that had actually failed.
//
// It takes an endpoint rather than a direction for the same reason ForcePush
// does: the row knows which side was written, not what the config calls that way
// round. endpointMatches is what keeps rows written under the older endpoint
// notation clickable.
func (s *Service) DirectionTo(repoName, toEndpoint string) (string, error) {
	for _, repoCfg := range s.configs {
		if repoCfg.Name != repoName {
			continue
		}
		if s.endpointAmbiguous(repoCfg, toEndpoint) {
			return "", fmt.Errorf("repo %q: %q names both sides, so it is %w", repoName, toEndpoint, ErrUnknownSide)
		}
		var dir string
		switch {
		case s.endpointMatches(repoCfg.Target, repoCfg.TargetPath, toEndpoint):
			dir = config.DirectionSourceToTarget
		case s.endpointMatches(repoCfg.Source, repoCfg.SourcePath, toEndpoint):
			dir = config.DirectionTargetToSource
		default:
			return "", fmt.Errorf("repo %q: %q is %w", repoName, toEndpoint, ErrUnknownSide)
		}
		// A one-way repo can still hold a row in the other direction — a refusal
		// records the side it declined to write. Re-running that is the one
		// request the button must not pass on.
		if !directionAllowed(repoCfg.Direction, dir) {
			return "", fmt.Errorf("repo %q: %w (direction %q, %s)",
				repoName, ErrDirectionNotAllowed, repoCfg.Direction, dir)
		}
		return dir, nil
	}
	return "", fmt.Errorf("%q: %w", repoName, ErrUnknownRepo)
}

// resolveAutoDirection picks the default direction when retry direction is "auto".
// bidirectional → target-to-source (matches the 2026-05-19 incident pattern, where
// the GitLab → CodeCommit leg failed and manual retry was issued in that direction).
// one-way repos resolve to their single allowed direction.
func resolveAutoDirection(cfgDir string) string {
	switch strings.ToLower(cfgDir) {
	case config.DirectionBidirectional:
		return config.DirectionTargetToSource
	case config.DirectionSourceToTarget:
		return config.DirectionSourceToTarget
	case config.DirectionTargetToSource:
		return config.DirectionTargetToSource
	}
	// Unreachable in practice — config.validate() restricts Direction to the
	// three values above. Kept as a defensive default so a future code path
	// that bypasses validate() still produces a sane (refused-by-
	// directionAllowed) result instead of an empty direction string.
	return config.DirectionTargetToSource
}

// directionAllowed reports whether retryDir is compatible with the repo's
// configured direction. bidirectional accepts any; one-way accepts only itself.
func directionAllowed(cfgDir, retryDir string) bool {
	cfg := strings.ToLower(cfgDir)
	if cfg == config.DirectionBidirectional {
		return true
	}
	return cfg == strings.ToLower(retryDir)
}

// appendSource appends a "Source: <name>" line to a Slack notification body
// when the trigger source is non-webhook (e.g. "retry-api"). Webhook (the
// default trigger) is left implicit to avoid noise on routine sync messages.
func appendSource(body string, meta EventMeta) string {
	if meta.Source == "" || meta.Source == SourceWebhook {
		return body
	}
	return body + fmt.Sprintf("\nSource: %s", meta.Source)
}

// refOverrideBlocksDelete reports whether a ref delete in the from→to direction
// is forbidden by a ref_override (and logs the skip). It is the symmetric twin of
// doMirror's push-side guard — a delete only propagates in the direction the
// override allows, which blocks the accident of deleting a branch/tag on the
// authoritative side opposite it.
func refOverrideBlocksDelete(repoCfg config.RepoConfig, refName, from, to string) bool {
	ov := repoCfg.MatchRefOverride(refName)
	if ov == nil || (ov.From == from && ov.To == to) {
		return false
	}
	slog.Info("ref override: skipping reverse-direction delete",
		"repo", repoCfg.Name, "ref", refName,
		"this", from+"→"+to, "allowed", ov.From+"→"+ov.To)
	return true
}

// SyncDelete deletes a ref from the target triggered by source-side delete event.
func (s *Service) SyncDelete(ctx context.Context, repoName, refType, refName string) error {
	for _, repoCfg := range s.configs {
		if repoCfg.SourcePath != repoName {
			continue
		}
		// The per-ref direction override applies to a delete too (symmetric with
		// push). SyncDelete is always the source→target direction, so if the
		// override does not allow that direction it skips silently.
		if refOverrideBlocksDelete(repoCfg, refName, repoCfg.Source, repoCfg.Target) {
			return nil
		}
		if allowsSourceToTarget(repoCfg.Direction) {
			// The only path that arrives via a source-side event = the SQS consumer
			// (CodeCommit referenceDeleted).
			return s.doDeleteRef(ctx, repoCfg, repoCfg.Source, repoCfg.SourcePath, repoCfg.Target, repoCfg.TargetPath, refType, refName, SourceSQS)
		}
		return fmt.Errorf("repo %q direction %q does not allow source-to-target sync", repoName, repoCfg.Direction)
	}
	return fmt.Errorf("repo %q not configured for mirroring", repoName)
}

// SyncDeleteByTarget deletes a ref triggered by a target-side delete event
// (GitLab/GitHub webhook with after == zeroSHA, or GitHub deleted:true).
// It is SyncByTarget (dual-match) and SyncDelete (the symmetric override skip)
// combined: on a target provider+path match it deletes the ref on the source
// side, and on a source provider+path match it deletes it on the target side.
// Idempotency (skipping a ref that is already gone) is handled inside
// doDeleteRef.
func (s *Service) SyncDeleteByTarget(ctx context.Context, providerKey, repoPath, refType, refName string) error {
	for _, repoCfg := range s.configs {
		// Match by target provider + target path → delete on the source side
		if tgtProvider, ok := s.providers[repoCfg.Target]; ok &&
			providerMatches(repoCfg.Target, tgtProvider.Type(), providerKey) && repoCfg.TargetPath == repoPath {
			if !allowsTargetToSource(repoCfg.Direction) {
				return fmt.Errorf("repo %q direction %q does not allow target-to-source sync", repoCfg.Name, repoCfg.Direction)
			}
			if refOverrideBlocksDelete(repoCfg, refName, repoCfg.Target, repoCfg.Source) {
				return nil
			}
			return s.doDeleteRef(ctx, repoCfg, repoCfg.Target, repoCfg.TargetPath, repoCfg.Source, repoCfg.SourcePath, refType, refName, SourceWebhook)
		}

		// Match by source provider + source path → delete on the target side
		if srcProvider, ok := s.providers[repoCfg.Source]; ok &&
			providerMatches(repoCfg.Source, srcProvider.Type(), providerKey) && repoCfg.SourcePath == repoPath {
			if !allowsSourceToTarget(repoCfg.Direction) {
				return fmt.Errorf("repo %q direction %q does not allow source-to-target sync", repoCfg.Name, repoCfg.Direction)
			}
			if refOverrideBlocksDelete(repoCfg, refName, repoCfg.Source, repoCfg.Target) {
				return nil
			}
			return s.doDeleteRef(ctx, repoCfg, repoCfg.Source, repoCfg.SourcePath, repoCfg.Target, repoCfg.TargetPath, refType, refName, SourceWebhook)
		}
	}
	return fmt.Errorf("no matching repo for provider=%q path=%q", providerKey, repoPath)
}

// Restore outcomes a caller needs to tell apart.
//
// The console shows a refusal and a breakage very differently: a refusal means
// the safety rules worked and the operator should go look at what changed, a
// breakage means the service could not do its job. Matching on message text
// would put that distinction one typo away from silently inverting, so the
// reasons travel as sentinels and the handler maps them to a machine-readable
// `reason` beside the human message.
var (
	// ErrRefExists — the ref came back on the destination, so restoring would
	// overwrite whoever put it there.
	ErrRefExists = errors.New("ref already exists on the destination")
	// ErrObjectGone — git has garbage-collected the commit; nothing to put back.
	ErrObjectGone = errors.New("commit is no longer available")
	// ErrDirectionNotAllowed — the repo's direction never writes to that side.
	ErrDirectionNotAllowed = errors.New("direction does not allow writing to that side")
	// ErrRefOverridden — a ref_override pins the ref away from that direction.
	ErrRefOverridden = errors.New("a ref_override pins this ref away from that direction")
	// ErrUnknownSide / ErrUnknownRepo — the request did not come from a row the
	// console rendered.
	ErrUnknownSide = errors.New("not a side of this repo")
	ErrUnknownRepo = errors.New("repo is not configured for mirroring")
	// ErrRepoBusy — a mirror operation holds the per-repo lock. The restore is
	// refused rather than queued: this runs inside an HTTP request, and holding
	// a connection open for the length of a large fetch is how a console stops
	// answering. Clicking again once the sync lands is the whole recovery.
	ErrRepoBusy = errors.New("a mirror operation is in progress for this repo")
)

// RestoreRef re-creates a ref a delete removed, using the tip that delete
// recorded. It is what the console's restore button calls.
//
// toEndpoint names which side to put the ref back on, in the "<type>/<path>"
// form the history event's To field carries — the caller is acting on a row it
// is looking at, so it says which destination that row named rather than having
// this guess.
//
// The safety property is that a restore only ever fills a hole it can still
// see. If the ref is back on the destination, someone re-created it after the
// delete, and writing over that is exactly the accident this whole feature
// exists to undo — so it refuses instead. The ls-remote check and the
// non-force push are two halves of that: the check gives a readable refusal,
// and the push closes the race between checking and writing.
//
// actor is the person the console attributes the click to; it lands in the
// history and the notification, because this writes to a real repository.
func (s *Service) RestoreRef(ctx context.Context, repoName, toEndpoint, refType, refName, sha, actor string) error {
	// Every exit below records. These are the paths a wrong or hand-crafted
	// request lands on, so leaving them as a bare error would make the one case
	// worth auditing the only one with no trace but a log line.
	refused := func(from, to string, reason string, err error) error {
		s.record(history.Event{
			Repo: repoName, Action: history.ActionRestore, Source: SourceConsole,
			From: from, To: to, Ref: fullRefName(refType, refName), Actor: actor,
		}.With(history.ResultFail, reason, err), time.Now())
		return err
	}

	for _, repoCfg := range s.configs {
		if repoCfg.Name != repoName {
			continue
		}
		// A restore writes, so it passes the same two gates every other write
		// passes. Without them this would be a general "create any ref on
		// either side" API: it could push to a side the repo's direction says
		// is never written, or re-create exactly the ref a ref_override exists
		// to keep one side authoritative over. Deletes already carry this
		// symmetry (refOverrideBlocksDelete); leaving restore out of it would
		// let the button undo a protection the delete path respects.
		src := s.endpoint(repoCfg.Source, repoCfg.SourcePath)
		tgt := s.endpoint(repoCfg.Target, repoCfg.TargetPath)
		// Both the new and the old notation are accepted — the button on an older
		// history row still sitting on the volume sends that row's To value back
		// untouched (see the endpointMatches comment). The record is always written
		// in the new notation. But when the old notation matches both sides there
		// is no telling which one to restore on (see endpointAmbiguous). Picking
		// one here would end in creating a ref that does not exist on the wrong
		// instance, so it is refused instead.
		if s.endpointAmbiguous(repoCfg, toEndpoint) {
			// Recorded in the same shape as the unknown-side refusal below — not
			// knowing which side it is is the whole point, so neither side is
			// written into From/To.
			return refused("", toEndpoint, history.ReasonUnknownSide,
				fmt.Errorf("repo %q: %q names both sides, so it is %w", repoName, toEndpoint, ErrUnknownSide))
		}
		switch {
		case s.endpointMatches(repoCfg.Target, repoCfg.TargetPath, toEndpoint):
			if !allowsSourceToTarget(repoCfg.Direction) {
				return refused(src, tgt, history.ReasonDirection,
					fmt.Errorf("repo %q: %w (direction %q, target)", repoName, ErrDirectionNotAllowed, repoCfg.Direction))
			}
			if refOverrideBlocksDelete(repoCfg, refName, repoCfg.Source, repoCfg.Target) {
				return refused(src, tgt, history.ReasonRefOverride,
					fmt.Errorf("%q: %w", refName, ErrRefOverridden))
			}
			return s.doRestoreRef(ctx, repoCfg, repoCfg.Source, repoCfg.SourcePath, repoCfg.Target, repoCfg.TargetPath, refType, refName, sha, actor)
		case s.endpointMatches(repoCfg.Source, repoCfg.SourcePath, toEndpoint):
			if !allowsTargetToSource(repoCfg.Direction) {
				return refused(tgt, src, history.ReasonDirection,
					fmt.Errorf("repo %q: %w (direction %q, source)", repoName, ErrDirectionNotAllowed, repoCfg.Direction))
			}
			if refOverrideBlocksDelete(repoCfg, refName, repoCfg.Target, repoCfg.Source) {
				return refused(tgt, src, history.ReasonRefOverride,
					fmt.Errorf("a ref_override pins %q away from this direction", refName))
			}
			return s.doRestoreRef(ctx, repoCfg, repoCfg.Target, repoCfg.TargetPath, repoCfg.Source, repoCfg.SourcePath, refType, refName, sha, actor)
		}
		return refused("", toEndpoint, history.ReasonUnknownSide,
			fmt.Errorf("repo %q: %q is %w", repoName, toEndpoint, ErrUnknownSide))
	}
	return refused("", toEndpoint, history.ReasonUnknownRepo,
		fmt.Errorf("%q: %w", repoName, ErrUnknownRepo))
}

// ForcePush applies a rewind the guard withheld, on the side a person named.
//
// It is the console's half of the escape hatch, and it takes the destination
// endpoint rather than a direction for the same reason RestoreRef does: the
// operator clicked a row, and a row knows which side it was writing to. Asking
// the browser to translate that into "source-to-target" invites it to get the
// translation backwards, on the one call whose whole purpose is to overwrite.
//
// The direction gate below is the same one a restore passes. The ref_override
// gate is not repeated here — doMirror applies it to every push, forced or not,
// so a ref pinned to one direction stays pinned even through this route.
// dest is the destination tip the operator was shown. It travels through to the
// push as the lease, so a destination that moved between the click and the write
// is refused rather than overwritten.
func (s *Service) ForcePush(ctx context.Context, repoName, toEndpoint, ref, dest, actor string) error {
	refused := func(from, to, reason string, err error) error {
		s.record(history.Event{
			Repo: repoName, Action: history.ActionMirror, Source: SourceConsole,
			From: from, To: to, Ref: ref, Actor: actor,
		}.With(history.ResultFail, reason, err), time.Now())
		return err
	}
	meta := EventMeta{Ref: ref, Source: SourceConsole, Force: true, ForceLease: dest, Actor: actor}

	for _, repoCfg := range s.configs {
		if repoCfg.Name != repoName {
			continue
		}
		src := s.endpoint(repoCfg.Source, repoCfg.SourcePath)
		tgt := s.endpoint(repoCfg.Target, repoCfg.TargetPath)
		// Both the new and the old notation are accepted for the same reason as in
		// RestoreRef. Refusing when both sides match is for the same reason too
		// (see endpointAmbiguous).
		if s.endpointAmbiguous(repoCfg, toEndpoint) {
			// Recorded in the same shape as the unknown-side refusal below — not
			// knowing which side it is is the whole point, so neither side is
			// written into From/To.
			return refused("", toEndpoint, history.ReasonUnknownSide,
				fmt.Errorf("repo %q: %q names both sides, so it is %w", repoName, toEndpoint, ErrUnknownSide))
		}
		switch {
		case s.endpointMatches(repoCfg.Target, repoCfg.TargetPath, toEndpoint):
			if !allowsSourceToTarget(repoCfg.Direction) {
				return refused(src, tgt, history.ReasonDirection,
					fmt.Errorf("repo %q: %w (direction %q, target)", repoName, ErrDirectionNotAllowed, repoCfg.Direction))
			}
			return s.doMirror(ctx, repoCfg, repoCfg.Source, repoCfg.SourcePath, repoCfg.Target, repoCfg.TargetPath, meta)
		case s.endpointMatches(repoCfg.Source, repoCfg.SourcePath, toEndpoint):
			if !allowsTargetToSource(repoCfg.Direction) {
				return refused(tgt, src, history.ReasonDirection,
					fmt.Errorf("repo %q: %w (direction %q, source)", repoName, ErrDirectionNotAllowed, repoCfg.Direction))
			}
			return s.doMirror(ctx, repoCfg, repoCfg.Target, repoCfg.TargetPath, repoCfg.Source, repoCfg.SourcePath, meta)
		}
		return refused("", toEndpoint, history.ReasonUnknownSide,
			fmt.Errorf("repo %q: %q is %w", repoName, toEndpoint, ErrUnknownSide))
	}
	return refused("", toEndpoint, history.ReasonUnknownRepo,
		fmt.Errorf("%q: %w", repoName, ErrUnknownRepo))
}

// doRestoreRef puts sha back as refType/refName on the toProvider side.
//
// fromProvider is only a fallback source for the objects: the mirror cache for
// that side is where the commit most likely still lives, since it was fetched
// there before the delete propagated.
func (s *Service) doRestoreRef(ctx context.Context, repoCfg config.RepoConfig, fromProvider, fromPath, toProvider, toPath, refType, refName, sha, actor string) error {
	histStart := time.Now()
	fullRef := fullRefName(refType, refName)
	hist := history.Event{
		Repo:   repoCfg.Name,
		Action: history.ActionRestore,
		Source: SourceConsole,
		From:   s.endpoint(fromProvider, fromPath),
		To:     s.endpoint(toProvider, toPath),
		Ref:    fullRef,
		Actor:  actor,
	}

	// TryLock, not Lock: this is the one mirror operation that runs inside an
	// HTTP request. Waiting would hold the connection for the length of whatever
	// fetch is in flight, and a console that stops answering is worse than one
	// that says "busy, try again" — the restore is a click away either way.
	mu := s.repoLock(repoCfg.Name)
	if !mu.TryLock() {
		err := fmt.Errorf("%s: %w", repoCfg.Name, ErrRepoBusy)
		s.record(hist.With(history.ResultFail, history.ReasonRepoBusy, err), histStart)
		return err
	}
	defer mu.Unlock()

	ctx, cancel := s.withGitTimeout(ctx)
	defer cancel()

	src, ok := s.providers[fromProvider]
	if !ok {
		err := fmt.Errorf("provider %q not found", fromProvider)
		s.record(hist.With(history.ResultFail, history.ReasonProvider, err), histStart)
		return err
	}
	tgt, ok := s.providers[toProvider]
	if !ok {
		err := fmt.Errorf("provider %q not found", toProvider)
		s.record(hist.With(history.ResultFail, history.ReasonProvider, err), histStart)
		return err
	}
	tgtRem := tgt.Remote(toPath)
	route := fmt.Sprintf("%s/%s → %s/%s", fromProvider, fromPath, toProvider, toPath)
	logger := slog.With("repo", repoCfg.Name, "route", route, "ref", fullRef, "sha", sha, "actor", actor)

	// The hole has to still be there. A ref that came back means someone acted
	// after the delete, and a restore must never be the thing that erases them.
	switch tip, err := s.git.RefTip(ctx, tgtRem, refType, refName); {
	case err != nil:
		s.notifyRestoreFailed(repoCfg, refType, refName, route, actor, err)
		s.record(hist.With(history.ResultFail, history.ReasonCheckRef, err), histStart)
		return fmt.Errorf("check ref exists: %w", err)
	case tip == sha:
		logger.Info("ref already at the requested tip, nothing to restore")
		s.record(hist.With(history.ResultSkip, history.ReasonAlreadyUpToDate, nil), histStart)
		return nil
	case tip != "":
		err := fmt.Errorf("%s: %w — it is at %s on %s", fullRef, ErrRefExists, tip, hist.To)
		logger.Warn("refusing to restore over an existing ref", "existing", tip)
		s.notifyRestoreFailed(repoCfg, refType, refName, route, actor, err)
		s.record(hist.With(history.ResultFail, history.ReasonRefExists, err), histStart)
		return err
	}

	// The commit usually survives in the mirror cache for the other side; if gc
	// took it, ask that side's remote for it directly before giving up.
	// The cache dir may not exist yet — a pod that has only ever served the
	// other direction never created it. Without this the fallback fetch dies on
	// "cannot change to directory" and gets reported as a garbage-collected
	// commit, which is a different and much more alarming thing. doDeleteRef
	// does the same init for the same reason.
	mirrorDir := s.mirrorDirFor(repoCfg.Name, fromProvider)
	if err := s.git.EnsureBareDir(ctx, mirrorDir); err != nil {
		s.notifyRestoreFailed(repoCfg, refType, refName, route, actor, err)
		s.record(hist.With(history.ResultFail, history.ReasonClone, err), histStart)
		return err
	}
	if !s.git.HasObject(ctx, mirrorDir, sha) {
		if err := s.git.FetchObject(ctx, mirrorDir, src.Remote(fromPath), sha); err != nil {
			logger.Warn("object not available for restore", "error", err)
			gone := fmt.Errorf("%s: %w (garbage-collected): %v", sha, ErrObjectGone, err)
			s.notifyRestoreFailed(repoCfg, refType, refName, route, actor, gone)
			s.record(hist.With(history.ResultFail, history.ReasonObjectGone, gone), histStart)
			return gone
		}
	}

	logger.Info("restoring ref")
	if err := s.git.CreateRef(ctx, mirrorDir, tgtRem, sha, fullRef); err != nil {
		s.notifyRestoreFailed(repoCfg, refType, refName, route, actor, err)
		s.record(hist.With(history.ResultFail, history.ReasonCreateRef, err), histStart)
		return fmt.Errorf("restore ref: %w", err)
	}

	logger.Info("ref restored")
	s.notifier.Send(notify.Message{
		Level: "success",
		Title: fmt.Sprintf("Ref Restored: %s", repoCfg.Name),
		Body: fmt.Sprintf("Action: restore %s '%s'\nRoute: %s\nURL: %s\nRestored tip: %s\nRestored by: %s",
			refType, refName, route,
			notify.Link(tgt.WebURL(toPath), s.endpoint(toProvider, toPath)), sha, actor),
		WebhookURL: repoCfg.SlackWebhookURL,
	})
	hist.RestoredTip = sha
	s.record(hist.With(history.ResultOK, "", nil), histStart)
	return nil
}

// notifyRestoreFailed keeps the three restore failure exits sending the same
// shape, so a refusal is as legible in Slack as a success.
func (s *Service) notifyRestoreFailed(repoCfg config.RepoConfig, refType, refName, route, actor string, err error) {
	s.notifier.Send(notify.Message{
		Level: "error",
		Title: fmt.Sprintf("Ref Restore Failed: %s", repoCfg.Name),
		Body: fmt.Sprintf("Action: restore %s '%s'\nRoute: %s\nRequested by: %s\nError: %v",
			refType, refName, route, actor, err),
		WebhookURL: repoCfg.SlackWebhookURL,
	})
}

// repoLock returns a per-repo mutex, creating one if needed.
func (s *Service) repoLock(repoName string) *sync.Mutex {
	s.repoLocksMu.Lock()
	defer s.repoLocksMu.Unlock()
	if mu, ok := s.repoLocks[repoName]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	s.repoLocks[repoName] = mu
	return mu
}

// doDeleteRef deletes a specific branch or tag from the destination (toProvider).
// fromProvider/fromPath identify where the delete originated; they are used only to
// build the Route line in notifications (symmetric: doMirror prints a Route too).
// Since bidirectional delete propagation arrived, the direction (gitlab→codecommit,
// say) has to be readable from Slack alone.
//
// source is the trigger label for the history. Unlike the push path, the delete
// path carries no EventMeta, so the entry point supplies it instead
// (SyncDelete = SQS, SyncDeleteByTarget = webhook). A new caller must pass the
// label matching its own trigger; otherwise the recorded source silently lies.
func (s *Service) doDeleteRef(ctx context.Context, repoCfg config.RepoConfig, fromProvider, fromPath, toProvider, toPath, refType, refName, source string) error {
	histStart := time.Now()
	hist := history.Event{
		Repo:   repoCfg.Name,
		Action: history.ActionDelete,
		Source: source,
		From:   s.endpoint(fromProvider, fromPath),
		To:     s.endpoint(toProvider, toPath),
		Ref:    fullRefName(refType, refName),
	}

	mu := s.repoLock(repoCfg.Name)
	mu.Lock()
	defer mu.Unlock()

	// The timeout runs from the moment the lock is acquired (queue waiting
	// excluded) — see the withGitTimeout comment.
	ctx, cancel := s.withGitTimeout(ctx)
	defer cancel()

	tgt, ok := s.providers[toProvider]
	if !ok {
		err := fmt.Errorf("provider %q not found", toProvider)
		s.record(hist.With(history.ResultFail, history.ReasonProvider, err), histStart)
		return err
	}
	// The value is unused, but the existence check is kept — it surfaces a typo in
	// the configuration here rather than at the push step.
	if _, ok := s.providers[fromProvider]; !ok {
		err := fmt.Errorf("provider %q not found", fromProvider)
		s.record(hist.With(history.ResultFail, history.ReasonProvider, err), histStart)
		return err
	}

	tgtRem := tgt.Remote(toPath)
	route := fmt.Sprintf("%s/%s → %s/%s", fromProvider, fromPath, toProvider, toPath)
	logger := slog.With("repo", repoCfg.Name, "route", route, "ref", refType+"/"+refName)

	deleteDir := filepath.Join(s.workDir, repoCfg.Name+"-delete.git")
	defer func() {
		if err := os.RemoveAll(deleteDir); err != nil {
			slog.Warn("failed to clean up directory", "path", deleteDir, "error", err)
		}
	}()

	// Idempotency check: a ref that is already gone ends as a successful no-op,
	// which breaks the bidirectional delete loop.
	// (gitlab delete → codecommit delete → codecommit referenceDeleted echo →
	// skipped here)
	// A RefTip failure is usually an authentication/network problem → surfaced
	// fail-closed (an error + a notification + a retry).
	//
	// The same call also returns the tip as it was just before the delete. Once
	// the delete is done there is nothing left at the destination to query, so if
	// it were not read here nobody could tell what disappeared.
	tip, err := s.git.RefTip(ctx, tgtRem, refType, refName)
	if err != nil {
		s.notifier.Send(notify.Message{
			Level:      "error",
			Title:      fmt.Sprintf("Ref Delete Failed: %s", repoCfg.Name),
			Body:       fmt.Sprintf("Action: check %s '%s'\nRoute: %s\nError: %v", refType, refName, route, err),
			WebhookURL: repoCfg.SlackWebhookURL,
		})
		s.record(hist.With(history.ResultFail, history.ReasonCheckRef, err), histStart)
		return fmt.Errorf("check ref exists: %w", err)
	}
	if tip == "" {
		logger.Info("ref already absent on target, skipping delete (idempotent no-op)")
		s.record(hist.With(history.ResultSkip, history.ReasonAlreadyAbsent, nil), histStart)
		return nil
	}

	logger.Info("deleting ref from target", "tip", tip)
	if err := s.git.DeleteRef(ctx, deleteDir, tgtRem, refType, refName); err != nil {
		s.notifier.Send(notify.Message{
			Level:      "error",
			Title:      fmt.Sprintf("Ref Delete Failed: %s", repoCfg.Name),
			Body:       fmt.Sprintf("Action: delete %s '%s'\nRoute: %s\nError: %v", refType, refName, route, err),
			WebhookURL: repoCfg.SlackWebhookURL,
		})
		s.record(hist.With(history.ResultFail, history.ReasonDeleteRef, err), histStart)
		return fmt.Errorf("delete ref: %w", err)
	}

	logger.Info("ref deleted from target", "tip", tip)
	hist.DeletedTip = tip
	// The tip goes in the message body, not just the history: the ref is gone
	// from the destination now, so this line and the history entry are the only
	// places the discarded objects are still named. git keeps them until it
	// garbage-collects, which is what makes the fetch below worth printing.
	//
	// The URL stays a placeholder for the same reason it does on the forced
	// path: the remote address alone is not what the reader needs. The clone
	// URL no longer carries credentials, so printing it would be safe now —
	// but whoever runs these two commands does so from a checkout they already
	// have, against a remote they already have configured, and the placeholder
	// says exactly that. Naming the mirror's own remote would invite pasting an
	// address the reader cannot authenticate against.
	s.notifier.Send(notify.Message{
		Level: "success",
		Title: fmt.Sprintf("Ref Deleted: %s", repoCfg.Name),
		// "Restore if needed", not "Recover": this rides on every successful
		// delete, including the routine cleanup of a merged branch. A label that
		// reads like an incident on ordinary traffic is one people learn to
		// skip, so the line says what it is for and stays conditional.
		//
		// Both commands are printed because the fetch alone does not undo
		// anything — it pulls the objects into a local clone and leaves the
		// destination exactly as deleted. Whoever reads this is already
		// flustered, and assembling the second command from memory is precisely
		// where it goes wrong, so the ref is spelled out rather than implied.
		//
		// The two commands go in a code block, and the tip above does not. The
		// block is what these lines are for — Slack renders it monospaced and
		// attaches a copy button, and the wrapping that a <clone-url> plus a
		// 40-character SHA plus a full ref forces onto the narrow attachment
		// column then reads as formatting rather than as a broken line. The tip
		// stays plain because it is read and compared against the history,
		// not pasted into a shell.
		Body: fmt.Sprintf("Action: delete %s '%s'\nRoute: %s\nURL: %s\nDeleted tip: %s\n"+
			"Restore if needed:\n```git fetch <clone-url> %s\ngit push <clone-url> %s:%s```",
			refType, refName, route,
			notify.Link(tgt.WebURL(toPath), s.endpoint(toProvider, toPath)),
			tip, tip, tip, fullRefName(refType, refName)),
		WebhookURL: repoCfg.SlackWebhookURL,
	})
	s.record(hist.With(history.ResultOK, "", nil), histStart)
	return nil
}

// endpoint builds the endpoint recorded in the history as "<provider name>/<path>".
// It is the same notation as Slack's Route line. The provider's name is used
// rather than its type because, when there is more than one instance of the same
// type (two GitLab servers, say), the type alone cannot tell which of them it is.
func (s *Service) endpoint(providerName, repoPath string) string {
	return providerName + "/" + repoPath
}

// legacyEndpoint is the old, provider-type-based endpoint notation.
//
// History rows recorded in this form are still sitting on the volume, and the
// console's restore/force-push buttons send that row's To value back untouched.
// It is accepted alongside the new one in matching only, so that the buttons on
// older rows do not die — the record is always written in the new notation.
func (s *Service) legacyEndpoint(providerName, repoPath string) string {
	if p, ok := s.providers[providerName]; ok {
		return p.Type() + "/" + repoPath
	}
	return providerName + "/" + repoPath
}

// endpointMatches reports whether the endpoint the console sent back names this
// provider/path side. Both the new and the old notation are accepted (see the
// legacyEndpoint comment).
func (s *Service) endpointMatches(providerName, repoPath, candidate string) bool {
	return candidate == s.endpoint(providerName, repoPath) ||
		candidate == s.legacyEndpoint(providerName, repoPath)
}

// endpointAmbiguous reports whether the endpoint that came back matches both
// sides of this repo at once.
//
// legacyEndpoint writes the provider type rather than the provider name, so on a
// repo that mirrors between two instances of the same type under the same path
// (team/test-repo on gitlab-old ↔ gitlab-main, say) the old notation
// of the two sides is the same string. The new notation carries the provider name
// and can never collide, so this condition is true only on rows recorded in the
// old notation.
//
// Picking one of them by the switch's first match would then run the opposite
// leg — precisely the symptom the console button exists to remove. Since there
// is no telling which side it is, the caller must refuse rather than guess.
func (s *Service) endpointAmbiguous(repoCfg config.RepoConfig, toEndpoint string) bool {
	return s.endpointMatches(repoCfg.Target, repoCfg.TargetPath, toEndpoint) &&
		s.endpointMatches(repoCfg.Source, repoCfg.SourcePath, toEndpoint)
}

// record stamps the elapsed time onto the event and hands it to the recorder.
//
// since is taken at function entry, so the recorded duration includes waiting
// for the per-repo mutex. That is deliberate and differs from the duration in
// the Slack message, which starts after the lock: Slack answers "how long did
// this sync take", the history answers "how long did this event take to be
// dealt with", and a queue building up is only visible in the second one.
func (s *Service) record(ev history.Event, since time.Time) {
	// New() never leaves this nil, but a Service built as a struct literal
	// (tests do this) would. Recording is an audit trail bolted onto mirroring,
	// so it must not be able to panic the sync it is describing.
	if s.recorder == nil {
		return
	}
	ev.DurationMS = time.Since(since).Milliseconds()
	s.recorder.Record(ev)
}

// notifyFailure sends the standard error notification for a failed mirror
// operation. action is the step that failed ("clone" / "clone (fallback)" /
// "push"), route is the from→to path.
func (s *Service) notifyFailure(repoCfg config.RepoConfig, action, route string, meta EventMeta, err error) {
	s.notifier.Send(notify.Message{
		Level:      "error",
		Title:      fmt.Sprintf("Mirror Sync Failed: %s", repoCfg.Name),
		Body:       appendSource(fmt.Sprintf("Action: %s\nRoute: %s\nError: %v", action, route, err), meta),
		WebhookURL: repoCfg.SlackWebhookURL,
	})
}

// mirrorDirFor returns the mirror cache path for the fromProvider direction.
//
// 🔴 The point is that the name is built from the **provider name**, not the
// provider type. Building it from the type makes the two directions share one
// directory on a pair whose sides have the same type, such as gitlab↔gitlab, and
// they then fetch --prune against each other, deleting and reviving the other
// side's refs over and over.
//
// When an older type-based name is still there, it is renamed once so the cache
// carries over. Without that migration a full clone runs again per direction
// right after a deployment, and the old directory stays on the PVC with nobody
// to delete it.
//
// Both call sites (doMirror and doRestoreRef) are inside the per-repo lock, so
// the rename never overlaps another sync.
func (s *Service) mirrorDirFor(repoName, fromProvider string) string {
	dir := filepath.Join(s.workDir, repoName+"-"+fromProvider+".git")
	if isGitDir(dir) {
		return dir
	}
	p, ok := s.providers[fromProvider]
	if !ok {
		// The caller fails right after this because it cannot find the provider.
		// All that happens here is returning the path; the judgement is left to
		// the caller.
		return dir
	}
	legacy := filepath.Join(s.workDir, repoName+"-"+p.Type()+".git")
	// When the provider name equals its type (the name is literally "gitlab", for
	// instance) there is nothing to migrate.
	if legacy == dir || !isGitDir(legacy) {
		return dir
	}
	// Same parent directory, so the rename is atomic (the same pattern as
	// CloneMirror's swap). A failure is not fatal — cloning under the new name is
	// enough, so it only leaves a warn and carries on.
	if err := os.Rename(legacy, dir); err != nil {
		slog.Warn("legacy mirror cache rename failed, will re-clone",
			"from", legacy, "to", dir, "error", err)
		return dir
	}
	slog.Info("migrated mirror cache to provider-name path", "from", legacy, "to", dir)
	return dir
}

// ensureMirror does an incremental fetch when the mirror is there and a full
// clone when it is not. On a fetch failure it falls back to a full clone, and on
// a clone failure it sends the failure notification and returns the wrapped error.
func (s *Service) ensureMirror(ctx context.Context, repoCfg config.RepoConfig, srcRem provider.Remote, mirrorDir, route string, meta EventMeta, logger *slog.Logger) error {
	if isGitDir(mirrorDir) {
		// If this is a cache created back when credentials were embedded in the
		// URL, it is scrubbed here. It has nothing to do with mirroring itself, so
		// a failure does not stop the run — what is left behind is this pod's
		// volume, not the destination, and the next event tries again.
		switch scrubbed, err := s.git.SanitizeCache(ctx, mirrorDir, srcRem); {
		case err != nil:
			logger.Warn("failed to scrub credentials from mirror cache, continuing", "error", err)
		case scrubbed:
			logger.Info("scrubbed credentials left in mirror cache by an older build", "dir", mirrorDir)
		}
		logger.Info("fetching from source (incremental)")
		if err := s.git.FetchMirror(ctx, srcRem, mirrorDir); err != nil {
			logger.Warn("incremental fetch failed, falling back to full clone", "error", err)
			if cerr := s.git.CloneMirror(ctx, srcRem, mirrorDir); cerr != nil {
				s.notifyFailure(repoCfg, "clone (fallback)", route, meta, cerr)
				return fmt.Errorf("clone: %w", cerr)
			}
			// A fallback clone is a mirror that was just freshly packed, so there is
			// nothing to clean up.
			return nil
		}
		// Housekeeping only runs after a successful fetch. Below the threshold git
		// ends immediately as a no-op, so the normal cost is zero, and being inside
		// the per-repo lock it never overlaps a fetch or a push. A failure only
		// leaves a log line and carries on, because the mirroring itself has already
		// succeeded.
		// On most fetches gc is a no-op and therefore silent. Only the rare moment
		// where it actually consolidated something must be recorded, so that "when
		// was this cache last cleaned up" can be answered without going into the pod
		// and counting files.
		switch stats, err := s.git.GCAuto(ctx, mirrorDir); {
		case err != nil:
			logger.Warn("git gc --auto failed, continuing", "error", err)
		case stats.Ran():
			logger.Info("mirror cache repacked",
				"packs_before", stats.PacksBefore, "packs_after", stats.PacksAfter,
				"keeps_pruned", stats.KeepsPruned)
		case stats.KeepsPruned > 0:
			// A marker was removed while gc itself stayed a no-op below the
			// threshold. The pack only folds in at some later consolidation, so
			// without this line the cause of that repack is unrecoverable.
			logger.Info("stale pack .keep markers pruned", "count", stats.KeepsPruned)
		}
		return nil
	}
	logger.Info("cloning from source (initial)")
	if err := s.git.CloneMirror(ctx, srcRem, mirrorDir); err != nil {
		s.notifyFailure(repoCfg, "clone", route, meta, err)
		return fmt.Errorf("clone: %w", err)
	}
	return nil
}

// resolveRefs returns the list of full ref names to be pushed in this direction.
//
//   - meta.Ref present → only the single triggered ref, and only when it actually
//     exists locally (an empty list otherwise → a retry for a branch that is gone,
//     or a prune race between fetch and push, is skipped as a no-op rather than
//     raised as an error).
//   - meta.Ref absent → everything except the override refs forbidden in this
//     direction.
//
// A failure to enumerate is an error. It used to be "fall back to --all", but
// --all cannot carry a per-ref lease, so the guard drops out entirely. Skipping
// one sync is recovered by the next event or the hourly reconcile, but a commit
// lost to a push made without the guard is not recovered.
func (s *Service) resolveRefs(ctx context.Context, repoCfg config.RepoConfig, mirrorDir, fromProvider, toProvider string, meta EventMeta) ([]string, error) {
	refs, err := s.git.ListRefs(ctx, mirrorDir)
	if err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}
	// An event names the ref it is about; pushing anything else is the mirror
	// acting on state it was not told about, on a ref nobody asked it to touch.
	//
	// Not narrowing here is what destroyed a commit on 2026-08-10: a demo-repo
	// event for version/4.2.0 pushed every ref, so it also force-wrote master-b
	// from a source that had not yet seen the commit pushed there 49 seconds
	// earlier. Refs that never get their own event are still reconciled by the
	// hourly cron, which sends no ref and therefore still covers everything.
	if meta.Ref != "" {
		if slices.Contains(refs, meta.Ref) {
			return []string{meta.Ref}, nil
		}
		return nil, nil
	}
	return buildFullRefspecs(refs, repoCfg, fromProvider, toProvider), nil
}

// Reasons a ref was withheld from a push, recorded on the held entry so the log
// says which of the two conditions fired.
const (
	holdDestinationAhead   = "destination-ahead"
	holdDestinationUnknown = "destination-unknown"
)

// pushPlan is the outcome of comparing local tips against the destination.
//
// Held uses the history type directly rather than a local one: it is recorded
// verbatim on the event so the console can offer a button per held ref, and a
// parallel struct here would exist only to be copied field for field.
type pushPlan struct {
	Specs []PushSpec
	Held  []history.HeldRef
}

// planPush decides, per ref, whether pushing it would move the destination
// forward or wind it back.
//
// This is the guard the 2026-08-11 loss needed. The echo of a push carries the
// state the destination had a minute ago, and the fetch that produces it can
// take a minute on its own, so by the time it is written the destination may
// already be further along. Force-pushing then is not mirroring, it is undoing
// whatever landed in between — and it reports itself as an ordinary success.
//
// Ancestry is the test, not commit timestamps. A clock can be wrong and a
// rebase rewrites dates freely; "does the destination already contain this"
// has exactly one correct answer and the object graph holds it.
//
// force skips the classification entirely and pins forceLease as the expected
// tip. It has to be the tip the operator was SHOWN, not the one read here: this
// runs after a fetch that can take a minute, so adopting the current value
// would quietly authorise overwriting anything that landed during it — the
// accident the guard exists to prevent, performed on request. Force implies a
// single ref, which is enforced at both API entry points.
func (s *Service) planPush(ctx context.Context, mirrorDir string, tgtRem provider.Remote, want []string, force bool, forceLease string) (pushPlan, error) {
	var plan pushPlan
	if len(want) == 0 {
		return plan, nil
	}
	local, err := s.git.ListRefTips(ctx, mirrorDir)
	if err != nil {
		return plan, fmt.Errorf("read local tips: %w", err)
	}
	remote, err := s.git.RemoteRefs(ctx, tgtRem)
	if err != nil {
		return plan, fmt.Errorf("read destination tips: %w", err)
	}
	for _, ref := range want {
		src, ok := local[ref]
		if !ok {
			continue // pruned between enumeration and now — nothing to push
		}
		if force {
			plan.Specs = append(plan.Specs, PushSpec{Ref: ref, Lease: forceLease})
			continue
		}
		dst, exists := remote[ref]
		switch {
		case !exists:
			// New ref at the destination. The empty lease says "must not exist",
			// so a ref created in the meantime is not silently overwritten.
			plan.Specs = append(plan.Specs, PushSpec{Ref: ref})
		case dst == src:
			// Identical. Dropping it here is also what keeps a full reconcile
			// small: a repo with thousands of build tags pushes the handful
			// that actually moved instead of restating every one of them.
		case !s.git.HasObject(ctx, mirrorDir, dst):
			// The destination holds something this side has never fetched. We
			// cannot prove it is ahead, and we cannot prove it is not — and a
			// push here would destroy it either way, so it is withheld. This is
			// the branch that would have stopped 2026-08-11.
			plan.Held = append(plan.Held, history.HeldRef{Ref: ref, Reason: holdDestinationUnknown, Dest: dst})
		case s.git.IsAncestor(ctx, mirrorDir, src, dst):
			plan.Held = append(plan.Held, history.HeldRef{Ref: ref, Reason: holdDestinationAhead, Dest: dst})
		default:
			// Either a fast-forward, or a genuine divergence where neither side
			// contains the other. Divergence still goes through as a forced push
			// with its alert — unchanged behaviour, because there is no answer
			// here that does not lose something, and stalling the mirror on it
			// was considered and rejected.
			plan.Specs = append(plan.Specs, PushSpec{Ref: ref, Lease: dst})
		}
	}
	return plan, nil
}

// doMirror performs the actual git clone --mirror + git push --mirror.
func (s *Service) doMirror(ctx context.Context, repoCfg config.RepoConfig, fromProvider, fromPath, toProvider, toPath string, meta EventMeta) error {
	// Build the descriptive half of the history event once; each exit path
	// below fills in only the outcome via With(). histStart is taken before the
	// lock, so the recorded duration includes the wait (see record).
	histStart := time.Now()
	hist := history.Event{
		Repo:   repoCfg.Name,
		Action: history.ActionMirror,
		Source: meta.Source,
		From:   s.endpoint(fromProvider, fromPath),
		To:     s.endpoint(toProvider, toPath),
		Ref:    meta.Ref,
		Actor:  meta.Actor,
	}

	// Per-ref direction override: when the triggering ref matches a ref_override
	// and the current sync direction (fromProvider→toProvider) differs from the
	// allowed one, it is skipped silently (a terminal nil). Returning nil rather
	// than an error is what makes SQS delete the message, so there is no retry or
	// DLQ churn.
	// (On a bidirectional repo this pins a given branch to one direction only and
	// blocks the opposite side from overwriting it.)
	if meta.Ref != "" {
		if ov := repoCfg.MatchRefOverride(meta.RefName()); ov != nil && (ov.From != fromProvider || ov.To != toProvider) {
			slog.Info("ref override: skipping reverse-direction sync",
				"repo", repoCfg.Name, "ref", meta.RefName(),
				"this", fromProvider+"→"+toProvider, "allowed", ov.From+"→"+ov.To)
			s.record(hist.With(history.ResultSkip, history.ReasonRefOverride, nil), histStart)
			return nil
		}
	}

	mu := s.repoLock(repoCfg.Name)
	mu.Lock()
	defer mu.Unlock()

	// The timeout runs from the moment the lock is acquired (queue waiting
	// excluded) — see the withGitTimeout comment.
	ctx, cancel := s.withGitTimeout(ctx)
	defer cancel()

	src, ok := s.providers[fromProvider]
	if !ok {
		err := fmt.Errorf("provider %q not found", fromProvider)
		s.record(hist.With(history.ResultFail, history.ReasonProvider, err), histStart)
		return err
	}
	tgt, ok := s.providers[toProvider]
	if !ok {
		err := fmt.Errorf("provider %q not found", toProvider)
		s.record(hist.With(history.ResultFail, history.ReasonProvider, err), histStart)
		return err
	}

	start := time.Now()
	srcRem := src.Remote(fromPath)
	tgtRem := tgt.Remote(toPath)

	logger := slog.With(
		"repo", repoCfg.Name,
		"from", fromProvider+"/"+fromPath,
		"to", toProvider+"/"+toPath,
	)

	mirrorDir := s.mirrorDirFor(repoCfg.Name, fromProvider)

	route := fmt.Sprintf("%s/%s → %s/%s", fromProvider, fromPath, toProvider, toPath)

	// Prepare the mirror (incremental fetch when it exists, full clone when it
	// does not, falling back to a clone when the fetch fails)
	if err := s.ensureMirror(ctx, repoCfg, srcRem, mirrorDir, route, meta, logger); err != nil {
		s.record(hist.With(history.ResultFail, history.ReasonClone, err), histStart)
		return err
	}

	// Decide the refs to push (the single triggered ref, or everything except the
	// override exclusions)
	refs, err := s.resolveRefs(ctx, repoCfg, mirrorDir, fromProvider, toProvider, meta)
	if err != nil {
		s.notifyFailure(repoCfg, "push", route, meta, err)
		s.record(hist.With(history.ResultFail, history.ReasonPush, err), histStart)
		return fmt.Errorf("resolve refs: %w", err)
	}
	if len(refs) == 0 {
		// No ref to push (the triggered ref is absent locally, or every ref is
		// excluded by an override in this direction) → no-op
		logger.Info("no refs to push for this direction (triggered ref absent or all excluded), skipping")
		s.record(hist.With(history.ResultSkip, history.ReasonNoRefsToPush, nil), histStart)
		return nil
	}

	if meta.Force {
		// A bypass has to be loud. It is only ever reachable by a person, and
		// the whole point of the guard is that this write is the one that can
		// destroy something.
		logger.Warn("push guard bypassed on request", "ref", meta.Ref, "actor", meta.Actor)
	}

	// Read the destination before pushing and judge it per ref. This is the point
	// where a rewind is stopped.
	plan, err := s.planPush(ctx, mirrorDir, tgtRem, refs, meta.Force, meta.ForceLease)
	if err != nil {
		s.notifyFailure(repoCfg, "push", route, meta, err)
		s.record(hist.With(history.ResultFail, history.ReasonPush, err), histStart)
		return fmt.Errorf("plan push: %w", err)
	}
	s.reportHeld(repoCfg, meta, heldReport{
		route:     route,
		dest:      hist.To,
		repo:      repoCfg.Name,
		direction: retryDirectionFor(repoCfg, fromProvider, toProvider),
		held:      plan.Held,
	}, logger)
	// Recorded on the event, not just logged, and set before any exit so a push
	// that carried some refs and withheld others still says which. A reconcile
	// names no ref of its own, so this is the only thing telling the console
	// which branch to offer a button for.
	hist.Held = plan.Held

	if len(plan.Specs) == 0 {
		reason := history.ReasonAlreadyUpToDate
		if len(plan.Held) > 0 {
			reason = history.ReasonDestinationAhead
		}
		logger.Info("nothing to push", "reason", reason, "held", len(plan.Held))
		s.record(hist.With(history.ResultSkip, reason, nil), histStart)
		return nil
	}

	// Push to target. The scope is logged because it is otherwise invisible:
	// a push of one ref and a push of several thousand produce identical lines,
	// and telling them apart is how anyone confirms an event stayed within the
	// ref it named.
	logger.Info("pushing to target", "refs", len(plan.Specs), "held", len(plan.Held))
	res, err := s.git.PushMirror(ctx, mirrorDir, tgtRem, plan.Specs)
	if err != nil {
		s.notifyFailure(repoCfg, "push", route, meta, err)
		s.record(hist.With(history.ResultFail, history.ReasonPush, err), histStart)
		return fmt.Errorf("push: %w", err)
	}

	elapsed := time.Since(start)

	if len(res.Rejected) > 0 {
		logger.Info("destination refused refs", "refs", res.Rejected, "duration", elapsed.String())
	}

	if !res.Changed {
		reason := history.ReasonAlreadyUpToDate
		if len(res.Rejected) > 0 {
			reason = history.ReasonLeaseRejected
		}
		logger.Info("nothing moved, skipping notification", "reason", reason, "duration", elapsed.String())
		s.record(hist.With(history.ResultSkip, reason, nil), histStart)
		return nil
	}

	logger.Info("mirror sync done", "duration", elapsed.String())

	// A forced update rides on an otherwise ordinary success, so it is recorded
	// as a reason on the ok row rather than changing the result. Set before
	// With() copies the event.
	histReason := ""
	alerted := false
	if len(res.Forced) > 0 {
		hist.Forced = res.Forced
		histReason = history.ReasonForcedUpdate
		alerted = s.reportForced(repoCfg, meta, forcedReport{
			route:   route,
			dest:    hist.To,
			target:  notify.Link(tgt.WebURL(toPath), s.endpoint(toProvider, toPath)),
			elapsed: elapsed,
			forced:  res.Forced,
		}, logger)
	}

	// A push that overwrote a branch has already been reported, and reporting it
	// a second time as a success is worse than saying nothing: the two land
	// together and read as a contradiction, so the reader has to work out that
	// one push produced both. The alert carries route, duration and target for
	// exactly this reason — nothing is lost by staying quiet here.
	if alerted {
		s.record(hist.With(history.ResultOK, histReason, nil), histStart)
		return nil
	}

	body := fmt.Sprintf("Action: branches + tags synced\nRoute: %s\nDuration: %s\nTarget: %s",
		route,
		elapsed.Round(time.Millisecond),
		notify.Link(tgt.WebURL(toPath), s.endpoint(toProvider, toPath)))
	if meta.Ref != "" {
		if meta.IsTag() {
			body += fmt.Sprintf("\nTag: %s", meta.RefName())
		} else {
			body += fmt.Sprintf("\nBranch: %s", meta.RefName())
		}
		// Get the actual commit author from the pushed ref
		if author, err := s.git.CommitAuthor(ctx, mirrorDir, meta.Ref); err == nil && author != "" {
			body += fmt.Sprintf("\nPushed by: %s", author)
		}
	}

	s.notifier.Send(notify.Message{
		Level:      "success",
		Title:      fmt.Sprintf("Mirror Sync: %s", repoCfg.Name),
		Body:       appendSource(body, meta),
		WebhookURL: repoCfg.SlackWebhookURL,
	})

	s.record(hist.With(history.ResultOK, histReason, nil), histStart)
	return nil
}

// reportForced logs every overwritten ref and alerts on the ones that matter.
//
// Only branches raise a Slack alert. A tag moving non-fast-forward is routine
// here — a build pipeline that reuses tag names re-points them on every run, and
// an alert that fires on routine traffic is one people learn to dismiss, which
// is the exact failure this change exists to prevent. A branch is different: in
// a mirror there is no ordinary reason for one to move non-fast-forward. It is
// either a deliberate rewrite upstream or this service overwriting a push it had
// not fetched yet, and the second one is silent data loss. Tags are still
// recorded in the history either way, so the evidence is never thrown away —
// only the interruption is rationed.
// forcedReport is everything an overwrite alert needs to stand on its own.
//
// It carries route, target and duration because the alert replaces the success
// message for that push rather than arriving alongside it, so anything the
// reader would have got there has to be here instead.
type forcedReport struct {
	route   string
	dest    string // "<provider-type>/<path>" — what was overwritten
	target  string // browsable URL for the destination
	elapsed time.Duration
	forced  []history.ForcedRef
}

// heldReport carries what a withheld push needs to describe itself.
type heldReport struct {
	route string
	dest  string
	repo  string
	// direction is the retry-API name for the leg that was withheld, so the
	// alert can hand over a command that runs rather than one to be adapted.
	direction string
	held      []history.HeldRef
}

// forceRequestBody renders the retry-API payload that applies one withheld ref.
//
// Marshalled rather than formatted. A ref name is not a safe thing to paste
// into a JSON literal, and a command that arrives subtly malformed is worse
// than no command at all — it fails at the far end, for a reason that reads as
// the API being broken rather than as the message having been wrong.
// dest is required: it is the tip the reader is being asked to discard, and it
// becomes the push's lease, so the command they copy from here overwrites that
// commit and refuses if the destination has moved on since this message.
func forceRequestBody(repo, direction, ref, dest string) string {
	b, err := json.Marshal(struct {
		Repo      string `json:"repo"`
		Direction string `json:"direction,omitempty"`
		Ref       string `json:"ref"`
		Dest      string `json:"dest"`
		Force     bool   `json:"force"`
	}{Repo: repo, Direction: direction, Ref: ref, Dest: dest, Force: true})
	if err != nil {
		return ""
	}
	return string(b)
}

// retryDirectionFor names the leg from → to in the retry API's vocabulary.
//
// The direction is the field most easily got backwards by hand — the words
// point at the config's source and target, not at the two providers, so a
// GitLab-to-CodeCommit push on a CodeCommit-sourced repo is target-to-source.
// Working that out under pressure is exactly the kind of step that goes wrong,
// so the process that already knows it writes it down.
func retryDirectionFor(repoCfg config.RepoConfig, fromProvider, toProvider string) string {
	if fromProvider == repoCfg.Source && toProvider == repoCfg.Target {
		return config.DirectionSourceToTarget
	}
	if fromProvider == repoCfg.Target && toProvider == repoCfg.Source {
		return config.DirectionTargetToSource
	}
	return ""
}

// reportHeld logs every withheld ref and alerts on the ones a person may be
// waiting on.
//
// Tags stay quiet, exactly as they do for a forced update: a pipeline that
// reuses build tag names re-points them constantly, so a tag hold is routine
// traffic, and an alert that fires on routine traffic is one people learn to
// ignore. A branch hold is worth saying out loud — it is either an echo that
// lost a race, which resolves itself within seconds, or someone's deliberate
// rewind that did not happen and will not happen until they say so. Nothing in
// the event distinguishes the two, so the message names the trigger and lets
// the reader decide.
func (s *Service) reportHeld(repoCfg config.RepoConfig, meta EventMeta, rep heldReport, logger *slog.Logger) {
	var branches []history.HeldRef
	for _, h := range rep.held {
		// Logged per ref, at info. A withheld push is the normal outcome of an
		// echo that lost the race, not an incident — but it is also the only
		// trace that the guard did anything, so it must not be silent.
		logger.Info("withholding push: destination is not behind",
			"ref", h.Ref, "reason", h.Reason, "destination_tip", h.Dest)
		if strings.HasPrefix(h.Ref, refsHeadsPrefix) {
			branches = append(branches, h)
		}
	}
	if len(branches) == 0 {
		return
	}

	// One line describing each held ref, and one runnable command per ref.
	//
	// The command is written out with the repository, direction and ref already
	// filled in, for the same reason the forced-update alert writes the SHA into
	// its recovery line: this is read by someone deciding whether a rewind of
	// theirs went through, and asking them to assemble a JSON body from three
	// fields scattered up the message is the step that gets fumbled. Only the
	// two values this process genuinely does not hold stay as placeholders —
	// its own external URL, and a token it must never print.
	lines := make([]string, 0, len(branches))
	commands := make([]string, 0, len(branches))
	for _, h := range branches {
		lines = append(lines, fmt.Sprintf("• %s: destination is at %s (%s)", h.Ref, h.Dest, h.Reason))
		commands = append(commands, fmt.Sprintf(
			"  curl -X POST <git-bridge-url>/retry/mirror -H \"Authorization: Bearer $RETRY_API_TOKEN\" "+
				"-H 'Content-Type: application/json' -d '%s'",
			forceRequestBody(rep.repo, rep.direction, h.Ref, h.Dest)))
	}

	body := fmt.Sprintf("Route: %s\nTarget: %s\nWithheld refs:\n%s\n\n"+
		"Nothing was written. The destination already holds what this side would "+
		"have pushed, so the write would have discarded commits there instead of "+
		"mirroring them.\n\n"+
		"An echo that arrived late settles on its own — the newer side raises its "+
		"own event and the two converge, so there is nothing to do here.\n\n"+
		"A rewind someone meant to make does not settle on its own. Apply it from "+
		"the console, where the row carries a button per held ref — or here, one "+
		"call per ref:\n%s",
		rep.route, rep.dest, strings.Join(lines, "\n"), strings.Join(commands, "\n"))

	s.notifier.Send(notify.Message{
		Level:      "warning",
		Title:      fmt.Sprintf("Push Withheld: %s", repoCfg.Name),
		Body:       appendSource(body, meta),
		WebhookURL: repoCfg.SlackWebhookURL,
	})
}

func (s *Service) reportForced(repoCfg config.RepoConfig, meta EventMeta, rep forcedReport, logger *slog.Logger) bool {
	var branches []history.ForcedRef
	for _, f := range rep.forced {
		logger.Warn("forced update: destination ref overwritten non-fast-forward",
			"ref", f.Ref, "old", f.Old, "new", f.New)
		if strings.HasPrefix(f.Ref, refsHeadsPrefix) {
			branches = append(branches, f)
		}
	}
	if len(branches) == 0 {
		return false
	}

	// One recovery line per branch, each carrying its own old tip.
	//
	// The SHA is written into the command rather than left as a placeholder
	// because this alert is read when something has already gone wrong, often by
	// someone who did not cause it: making them copy a SHA out of the line above
	// and into a template is exactly the step that gets fumbled. The URL stays a
	// placeholder on purpose — the only destination URL this process holds is
	// the clone URL, and that one carries the credentials.
	lines := make([]string, 0, len(branches))
	recovery := make([]string, 0, len(branches))
	for _, f := range branches {
		lines = append(lines, fmt.Sprintf("• %s: %s → %s", f.Ref, f.Old, f.New))
		recovery = append(recovery, fmt.Sprintf("  git fetch <clone-url> %s", f.Old))
	}

	body := fmt.Sprintf("Route: %s\nDuration: %s\nTarget: %s\nOverwritten refs:\n%s\n\n"+
		"Commits reachable only from an old tip are gone from %s. They survive "+
		"until git garbage-collects them — recover with:\n%s",
		rep.route, rep.elapsed.Round(time.Millisecond), rep.target,
		strings.Join(lines, "\n"), rep.dest, strings.Join(recovery, "\n"))

	s.notifier.Send(notify.Message{
		Level:      "error",
		Title:      fmt.Sprintf("Forced Update: %s", repoCfg.Name),
		Body:       appendSource(body, meta),
		WebhookURL: repoCfg.SlackWebhookURL,
	})
	return true
}

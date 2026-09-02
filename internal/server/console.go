package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"git-bridge/internal/consumer"
	"git-bridge/internal/history"
	"git-bridge/internal/mirror"
	"git-bridge/internal/task"
)

//go:embed console.html
var consoleHTML []byte

const (
	// consolePagePath serves the console page itself. It sits at the root of
	// the console listener so the portal can proxy to the service address with
	// no path suffix, the same shape slack-qr-bot uses.
	consolePagePath = "/"
	// consoleHistoryPath serves the history as JSON for the page to render.
	consoleHistoryPath = "/console/api/history"
	// consoleRetryPath re-runs a sync for one repo. This is the only console
	// route that changes anything.
	consoleRetryPath = "/console/api/retry"
	// consoleMePath reports who the portal says is viewing the page.
	consoleMePath = "/console/api/me"
	// consoleRestorePath re-creates a ref a delete removed. Together with the
	// retry route it is the second thing on the console that writes anything.
	consoleRestorePath = "/console/api/restore"
	// consoleForcePath applies a rewind the push guard withheld. The third and
	// last writing route, and the only one whose whole purpose is to overwrite.
	consoleForcePath = "/console/api/force"

	// defaultHistoryLimit is what the page asks for when it does not say.
	defaultHistoryLimit = 100
	// maxHistoryLimit caps the limit query parameter so a hand-crafted request
	// cannot ask for an unbounded response.
	maxHistoryLimit = 500

	// maxRetryBodySize bounds the retry request body. It only ever carries
	// three short strings.
	maxRetryBodySize = 4 << 10

	// consoleReadHeaderTimeout bounds a client that opens a connection and
	// dawdles over the request line.
	consoleReadHeaderTimeout = 10 * time.Second
	// consoleWriteTimeout bounds a response. It is generous because the restore
	// route talks to a remote git provider before answering, and stingy enough
	// that a wedged one cannot pin a connection indefinitely.
	consoleWriteTimeout = 90 * time.Second
)

// Identity headers the reverse-proxy portal sets on every request it proxies.
//
// They are trusted here for display only, and only because of where they
// arrive: the console runs on its own listener that nothing but the portal
// reaches, and the portal overwrites whatever a client tried to send under these
// names. On the public listener the same headers would be caller-controlled,
// which is why nothing on that side reads them.
const (
	headerAuthUser   = "X-Auth-User"
	headerAuthName   = "X-Auth-Name"
	headerAuthEmail  = "X-Auth-Email"
	headerAuthGroups = "X-Auth-Groups"
)

// consolePaths is every path the console listener serves.
//
// It exists so a test can assert the public mux answers 404 on each of them.
// That separation is the entire guard: the console handlers are registered on
// their own mux bound to its own listener, so which listener accepted the
// connection is the only thing that decides whether a console handler is
// reachable — and unlike a header or a Host value, the accepting socket is not
// something a client can set.
var consolePaths = []string{consolePagePath, consoleHistoryPath, consoleRetryPath, consoleMePath, consoleRestorePath, consoleForcePath}

// RefRestorer re-creates a ref a delete removed, using the tip that delete
// recorded. Only the console calls it — there is no event that means "put it
// back", so this exists purely to give a person a way to act on the record.
type RefRestorer interface {
	RestoreRef(ctx context.Context, repo, toEndpoint, refType, refName, sha, actor string) error
}

// ConsoleOption configures optional console capabilities.
//
// The restorer arrives this way rather than as another positional parameter
// because it is genuinely optional — a deployment with no mirror service wired
// in still serves the history — and because NewConsoleMux already takes four
// arguments that every caller has to spell out.
type ConsoleOption func(*consoleOptions)

type consoleOptions struct {
	restorer RefRestorer
	forcer   PushForcer
}

// WithRestorer enables the restore route. Without it the route answers 404 and
// the page hides the button, the same way a nil retrier disables retry.
func WithRestorer(r RefRestorer) ConsoleOption {
	return func(o *consoleOptions) { o.restorer = r }
}

// ConsoleRetrier re-runs a sync for one repo.
//
// DirectionTo is what lets the re-run follow the row it was clicked on: the row
// names the destination that was written, and this turns that side into the
// direction that writes it. Without it the console could only ask for "auto",
// which resolves the same way every time — so a leg that failed source-to-target
// was answered by a target-to-source sync that had nothing to do.
type ConsoleRetrier interface {
	consumer.MirrorRetrier
	DirectionTo(repoName, toEndpoint string) (string, error)
}

// PushForcer applies a rewind the push guard withheld.
//
// It takes the destination endpoint rather than a direction because that is
// what the row the operator clicked actually knows. Translating a side into a
// direction is the step most easily got backwards, and this is the one call
// where getting it backwards overwrites the wrong repository.
// dest is the destination tip the operator was shown; it becomes the push's
// lease so a destination that moved since is refused rather than overwritten.
type PushForcer interface {
	ForcePush(ctx context.Context, repo, toEndpoint, ref, dest, actor string) error
}

// WithForcer enables the force route. Without it the route answers 404 and the
// page hides the button.
func WithForcer(f PushForcer) ConsoleOption {
	return func(o *consoleOptions) { o.forcer = f }
}

// NewConsoleMux builds the handler served on the console port.
//
// reader is the in-memory history tail. Handlers here never touch the history
// file, so rendering the page cannot contend with a mirror operation.
//
// retrier re-runs a sync on request. It is called in-process, which is the
// point: the retry API token stays in the pod and never reaches the browser.
// A nil retrier disables the retry route, the same way an empty token disables
// the public one.
// tasks owns the sync a retry starts, so shutdown waits for it instead of
// killing the git command mid-fetch.
// apiDocsURL is where the console's docs link points; empty hides the link.
func NewConsoleMux(reader history.Reader, retrier ConsoleRetrier, tasks *task.Group, apiDocsURL string, opts ...ConsoleOption) *http.ServeMux {
	var cfg consoleOptions
	for _, opt := range opts {
		opt(&cfg)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(consolePagePath, consolePageHandler)
	mux.HandleFunc(consoleHistoryPath, historyHandler(reader))
	mux.HandleFunc(consoleRetryPath, retryHandler(retrier, tasks))
	mux.HandleFunc(consoleMePath, meHandler(apiDocsURL, cfg.restorer != nil, cfg.forcer != nil))
	mux.HandleFunc(consoleRestorePath, restoreHandler(cfg.restorer, reader))
	mux.HandleFunc(consoleForcePath, forceHandler(cfg.forcer, reader, tasks))
	return mux
}

// meHandler reports the viewer the portal authenticated.
//
// It is a route rather than a value templated into the page because the page is
// a static embedded asset, and keeping it static means the only dynamic thing on
// this listener stays JSON — the browser gets identity the same way it already
// gets history.
//
// Every field is optional. Reaching the console without the portal, which is
// what a port-forward during debugging looks like, leaves them all empty, and
// the page then shows no user rather than inventing one.
func meHandler(apiDocsURL string, restoreEnabled, forceEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}

		// Encode an empty array rather than JSON null so the page can iterate
		// the result without a special case, matching historyHandler.
		groups := []string{}
		for _, g := range strings.Split(r.Header.Get(headerAuthGroups), ",") {
			if g = strings.TrimSpace(g); g != "" {
				groups = append(groups, g)
			}
		}

		// Identity is per-viewer; a cached copy would show one person another
		// person's name.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{
			"user":   strings.TrimSpace(r.Header.Get(headerAuthUser)),
			"name":   strings.TrimSpace(r.Header.Get(headerAuthName)),
			"email":  strings.TrimSpace(r.Header.Get(headerAuthEmail)),
			"groups": groups,
			// Carried on the identity response rather than baked into the page
			// so the deployment decides it, matching how slack-qr-bot serves the
			// same link on the same route.
			"api_docs_url": apiDocsURL,
			// Whether the restore route is wired in. The page asks rather than
			// assuming, so a deployment without a mirror service shows no button
			// instead of one that 404s when pressed.
			"restore_enabled": restoreEnabled,
			// Same reasoning for the force route.
			"force_enabled": forceEnabled,
		})
	}
}

// consolePageHandler serves the console page.
//
// The page is registered at "/", which in Go's mux matches every unclaimed
// path, so anything else on this listener would silently render the console
// instead of 404ing. Unknown paths are rejected explicitly to keep that honest.
func consolePageHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != consolePagePath {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(consoleHTML)
}

// historyHandler returns recent mirror events as JSON, newest first.
func historyHandler(reader history.Reader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}

		q := history.Query{
			Limit:        defaultHistoryLimit,
			FailuresOnly: r.URL.Query().Get("failures") == "true",
			ForcedOnly:   r.URL.Query().Get("forced") == "true",
			Repo:         strings.TrimSpace(r.URL.Query().Get("repo")),
			Source:       strings.TrimSpace(r.URL.Query().Get("source")),
		}
		// Opt-in at the API so a direct caller gets the whole history; the page
		// asks for it because 96 of every 100 rows would otherwise be the hourly
		// reconcile reporting it found nothing.
		if r.URL.Query().Get("hide_routine") == "true" {
			q.RoutineSource = mirror.SourceCron
		}
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be a positive integer"})
				return
			}
			q.Limit = min(n, maxHistoryLimit)
		}

		events := reader.Recent(q)
		if events == nil {
			// Encode an empty array rather than JSON null so the page can
			// iterate the result without a special case.
			events = []history.Event{}
		}
		// repos comes from the unfiltered tail on purpose: the filter dropdown
		// must keep offering every repo even while one of them is selected.
		repos := reader.Repos()
		if repos == nil {
			repos = []string{}
		}
		// Same reasoning as repos: the trigger list comes from the unfiltered
		// tail so picking one trigger never hides the rest.
		sources := reader.Sources()
		if sources == nil {
			sources = []string{}
		}
		// The history is a live view; a cached copy is worse than no copy.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{
			"events":  events,
			"count":   len(events),
			"repos":   repos,
			"sources": sources,
		})
	}
}

// retryHandler re-runs a sync for one repo.
//
// The console has no API token and must never be given one — this handler calls
// the mirror service directly, in-process, so a browser session (already behind
// the portal's login and group check) is the only credential involved.
//
// Like the public retry endpoint it answers immediately and does the work in a
// goroutine: a mirror of a large repo takes longer than any sensible request
// timeout, and the result shows up in the history the page is already polling.
func retryHandler(retrier ConsoleRetrier, tasks *task.Group) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if retrier == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		// A JSON content type cannot be sent by a cross-site form without a
		// preflight, so requiring it is what keeps this write off the end of a
		// link someone was tricked into opening. This route earns the same gate
		// the other two carry now that "to" lets the caller pick which side is
		// written, rather than only asking for the pinned direction again.
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "content-type must be application/json"})
			return
		}

		var req struct {
			Repo      string `json:"repo"`
			Direction string `json:"direction"`
			To        string `json:"to"`
			Ref       string `json:"ref"`
		}
		dec := json.NewDecoder(io.LimitReader(r.Body, maxRetryBodySize))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		req.Repo = strings.TrimSpace(req.Repo)
		if req.Repo == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo is required"})
			return
		}
		// The row's destination decides the direction whenever the caller sends
		// one. An explicit direction still wins, for a caller that means a
		// particular way round, and a row too old to carry a destination falls
		// back to "auto" rather than losing its button.
		req.To = strings.TrimSpace(req.To)
		req.Direction = strings.TrimSpace(req.Direction)
		if req.To != "" && req.Direction == "" {
			dir, err := retrier.DirectionTo(req.Repo, req.To)
			if err != nil {
				// The refusal is logged because it is the one outcome with no
				// other trace: a refused restore or force records a history
				// event the page then shows, while this one only ever reached
				// the toast in the browser that asked. "The button did nothing"
				// is answerable from the pod log or it is not answerable.
				slog.Warn("console: retry refused", "source", mirror.SourceConsole,
					"repo", req.Repo, "to", req.To, "error", err)
				// 409 rather than 400: the request is well formed and names a
				// row that exists — it is the configuration behind that row
				// that no longer supports the sync, which is a state problem
				// and not something the caller typed wrong.
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			req.Direction = dir
		}
		if req.Direction == "" {
			req.Direction = "auto"
		}
		if !consumer.IsValidRetryDirection(req.Direction) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid direction"})
			return
		}

		logger := slog.With("source", mirror.SourceConsole, "repo", req.Repo,
			"direction", req.Direction, "ref", req.Ref)
		logger.Info("console: retry requested")

		// r.Context() is cancelled the moment this response is written, so the
		// background sync gets the task group's context instead — which also
		// makes it something shutdown can wait for.
		meta := mirror.EventMeta{Ref: req.Ref, Source: mirror.SourceConsole}
		tasks.Go(func(ctx context.Context) {
			if err := retrier.Retry(ctx, req.Repo, req.Direction, meta); err != nil {
				logger.Error("console: retry failed", "error", err)
			}
		})

		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status":    "accepted",
			"repo":      req.Repo,
			"direction": req.Direction,
		})
	}
}

// fullSHA matches a complete object name. A remote rejects an abbreviated one,
// and anything that is not hex has no business being handed to git.
var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// gateQuery is the tail the restore and force gates check a request against.
//
// The window has to be the one the console page renders. The page loads with
// "Hide idle reconciles" on (checked in console.html), so dropping this filter
// leaves only the gate's window filling up with the hourly reconcile's no-ops
// and shrinking to a far shorter span. The page then draws a button on a row
// that answers the click with a 409 for no-matching-delete.
//
// A page with the checkbox off reads the unfiltered tail, which covers a
// shorter span still — and delete rows are not no-ops, so they all land inside
// this window anyway. Filtering unconditionally therefore makes the gate's
// window cover both states, and no button is clickable without working.
func gateQuery(repo string) history.Query {
	return history.Query{Limit: maxHistoryLimit, Repo: repo, RoutineSource: mirror.SourceCron}
}

// matchingDeleteExists reports whether the history still records a delete that
// removed exactly this ref, at this commit, from this destination.
//
// It reads the same bounded tail the console page renders (gateQuery), so a
// delete old enough to have aged out is no longer restorable through the
// button. That is the intended failure mode: the commands in the Slack alert
// and the history entry still work by hand, and an operator reaching further
// back than the console shows should be doing it deliberately rather than by
// clicking.
func matchingDeleteExists(reader history.Reader, repo, to, ref, sha string) bool {
	if reader == nil {
		return false
	}
	for _, ev := range reader.Recent(gateQuery(repo)) {
		if ev.Action == history.ActionDelete && ev.Result == history.ResultOK &&
			ev.To == to && ev.Ref == ref && ev.DeletedTip == sha {
			return true
		}
	}
	return false
}

// matchingHoldExists reports whether the history still records the push guard
// withholding exactly this ref, on this destination, for this repository.
//
// It is the same cross-check a restore passes, for the same reason: without it
// this route is a general "force any ref onto either side" API reachable by
// anyone who can reach the console. With it, the button can only finish
// something the mirror already decided not to do and said so.
//
// Reading the same bounded tail the page renders (gateQuery) means a hold old
// enough to have aged out is no longer clickable. That is intended — the
// command in the Slack alert still works, and reaching further back than the
// console shows should be a deliberate act rather than a click.
// The tip is part of the match, not just the ref. Without it a hold that is
// still in the visible tail would authorise a force forever — including one
// pressed after the situation resolved and the destination moved on to
// something else entirely.
func matchingHoldExists(reader history.Reader, repo, to, ref, dest string) bool {
	if reader == nil {
		return false
	}
	for _, ev := range reader.Recent(gateQuery(repo)) {
		if ev.To != to {
			continue
		}
		for _, h := range ev.Held {
			if h.Ref == ref && h.Dest == dest {
				return true
			}
		}
	}
	return false
}

// forceHandler applies a rewind the push guard withheld.
//
// Asynchronous like retry rather than synchronous like restore: this runs a
// full mirror sync, and a fetch of a large repository outlasts any sensible
// request timeout. The outcome lands in the history the page is already
// polling, and a refusal inside ForcePush records itself there too.
func forceHandler(forcer PushForcer, reader history.Reader, tasks *task.Group) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if forcer == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}

		// A JSON content type cannot be sent by a cross-site form without a
		// preflight, so requiring it is what keeps this write off the end of a
		// link someone was tricked into opening. This is the most destructive
		// of the three writing routes, so it is the last one that may skip it.
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "content-type must be application/json"})
			return
		}

		var req struct {
			Repo string `json:"repo"`
			To   string `json:"to"`
			Ref  string `json:"ref"`
			Dest string `json:"dest"`
		}
		dec := json.NewDecoder(io.LimitReader(r.Body, maxRetryBodySize))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		req.Repo = strings.TrimSpace(req.Repo)
		req.To = strings.TrimSpace(req.To)
		req.Ref = strings.TrimSpace(req.Ref)
		req.Dest = strings.TrimSpace(req.Dest)
		if req.Repo == "" || req.To == "" || req.Ref == "" || req.Dest == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo, to, ref and dest are required"})
			return
		}
		if !matchingHoldExists(reader, req.Repo, req.To, req.Ref, req.Dest) {
			// 409 rather than 400: the request is well formed, the state it
			// refers to is not there. Most often that means the hold already
			// resolved on its own, which is the good outcome and not an error
			// the operator should be made to feel responsible for.
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "no withheld push recorded for that ref at that tip — it may have already resolved",
			})
			return
		}

		actor := strings.TrimSpace(r.Header.Get(headerAuthUser))
		logger := slog.With("source", mirror.SourceConsole, "repo", req.Repo,
			"to", req.To, "ref", req.Ref, "dest", req.Dest, "actor", actor)
		logger.Info("console: force requested")

		// r.Context() dies with this response, so the sync runs on the task
		// group's context — which is also what makes shutdown wait for it.
		tasks.Go(func(ctx context.Context) {
			if err := forcer.ForcePush(ctx, req.Repo, req.To, req.Ref, req.Dest, actor); err != nil {
				logger.Error("console: force failed", "error", err)
			}
		})

		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": "accepted",
			"repo":   req.Repo,
			"ref":    req.Ref,
		})
	}
}

// restoreHandler re-creates a ref a delete removed.
//
// Unlike retry this runs synchronously and reports the real outcome. Retry can
// answer "accepted" because its result is just another history row, but the
// interesting result here is the refusal — the ref came back, so the restore
// declined to overwrite it — and that has to reach the person who clicked
// rather than being something they discover later by re-reading the list.

// restoreFailureStatus separates a refusal from a breakage.
//
// Both used to come back as a bare 409, which told the page that something went
// wrong but not whether the safety rules had worked. A refusal means the
// operator should go look at what changed on the destination; a breakage means
// the service could not do its job and the request is worth retrying or
// escalating. 503 for a busy repo says "the same request will work shortly",
// which is exactly true while a mirror holds the lock.
func restoreFailureStatus(err error) (int, string) {
	switch {
	case errors.Is(err, mirror.ErrRefExists):
		return http.StatusConflict, "ref-exists"
	case errors.Is(err, mirror.ErrObjectGone):
		return http.StatusGone, "object-gone"
	case errors.Is(err, mirror.ErrDirectionNotAllowed):
		return http.StatusForbidden, "direction"
	case errors.Is(err, mirror.ErrRefOverridden):
		return http.StatusForbidden, "ref-override"
	case errors.Is(err, mirror.ErrUnknownSide):
		return http.StatusBadRequest, "unknown-side"
	case errors.Is(err, mirror.ErrUnknownRepo):
		return http.StatusBadRequest, "unknown-repo"
	case errors.Is(err, mirror.ErrRepoBusy):
		return http.StatusServiceUnavailable, "repo-busy"
	default:
		return http.StatusInternalServerError, "restore-failed"
	}
}

func restoreHandler(restorer RefRestorer, reader history.Reader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if restorer == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		// A JSON content type cannot be sent by a cross-site form without a
		// preflight, so requiring it is what keeps this write off the end of a
		// link someone was tricked into opening.
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "content-type must be application/json"})
			return
		}

		var req struct {
			Repo string `json:"repo"`
			To   string `json:"to"`
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
		}
		dec := json.NewDecoder(io.LimitReader(r.Body, maxRetryBodySize))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		req.Repo = strings.TrimSpace(req.Repo)
		req.To = strings.TrimSpace(req.To)
		req.Ref = strings.TrimSpace(req.Ref)
		req.SHA = strings.TrimSpace(req.SHA)
		if req.Repo == "" || req.To == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo and to are required"})
			return
		}
		// git prints lowercase and the console only ever sends what it was
		// given, but a hand-run curl copied from a tool that upcases hex should
		// not fail on presentation.
		req.SHA = strings.ToLower(req.SHA)
		if !fullSHA.MatchString(req.SHA) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sha must be a full 40-character object name"})
			return
		}

		var refType, refName string
		switch {
		case strings.HasPrefix(req.Ref, "refs/heads/"):
			refType, refName = "branch", strings.TrimPrefix(req.Ref, "refs/heads/")
		case strings.HasPrefix(req.Ref, "refs/tags/"):
			refType, refName = "tag", strings.TrimPrefix(req.Ref, "refs/tags/")
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ref must be a refs/heads/ or refs/tags/ ref"})
			return
		}
		if refName == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ref name is empty"})
			return
		}

		// The portal is the only thing that can say who this is; a client-set
		// header never reaches here because the portal overwrites it.
		//
		// Identity is resolved before any gate so that every rejection below is
		// attributable. A request that fails the checks is the one most worth
		// having a name on, and an earlier version returned 409 here without so
		// much as a log line.
		actor := strings.TrimSpace(r.Header.Get(headerAuthUser))
		if actor == "" {
			actor = "unknown"
		}

		logger := slog.With("source", mirror.SourceConsole, "repo", req.Repo,
			"to", req.To, "ref", req.Ref, "sha", req.SHA, "actor", actor)
		logger.Info("console: restore requested")

		// A restore may only put back something a delete actually took away.
		//
		// Without this the route is a general "create any ref, anywhere, from
		// any commit still in the cache" API, and the rule that it only fills a
		// hole a delete left would live nowhere but the browser's decision to
		// render the button. The history tail is the same data the page renders
		// from, so anything a person can see a button for is findable here.
		//
		// This rejection stays in the log rather than the history: nothing was
		// mirrored, and the history records mirror operations. The service-side
		// refusals further in — direction, ref_overrides, an unknown side — do
		// record, because by then a real repository was the subject.
		if !matchingDeleteExists(reader, req.Repo, req.To, req.Ref, req.SHA) {
			logger.Warn("console: restore refused, no recorded delete matches")
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":  "no recorded delete matches this repo, destination, ref and commit",
				"reason": "no-matching-delete",
			})
			return
		}

		if err := restorer.RestoreRef(r.Context(), req.Repo, req.To, refType, refName, req.SHA, actor); err != nil {
			status, reason := restoreFailureStatus(err)
			logger.Warn("console: restore refused or failed", "reason", reason, "error", err)
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, status, map[string]string{"error": err.Error(), "reason": reason})
			return
		}

		logger.Info("console: restore completed")
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "restored",
			"repo":   req.Repo,
			"ref":    req.Ref,
			"sha":    req.SHA,
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Warn("console: encode response failed", "error", err)
	}
}

// RunConsole starts the console listener on its own port.
//
// It is a second http.Server rather than a path prefix on the public one: the
// public HTTPRoute forwards to the public port only, so keeping the console on
// a separate listener is what keeps it off the internet without touching the
// route or the existing machine endpoints.
func RunConsole(ctx context.Context, port int, reader history.Reader, retrier ConsoleRetrier, tasks *task.Group, apiDocsURL string, opts ...ConsoleOption) {
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: NewConsoleMux(reader, retrier, tasks, apiDocsURL, opts...),
		// The restore route runs its git work inside the request, so this
		// listener needs bounds a purely read-only one never did. The write
		// budget covers an ls-remote, a possible object fetch and a push
		// against a remote provider, with room to spare; past that the client
		// is better off seeing the connection close than waiting on something
		// that is not coming back.
		ReadHeaderTimeout: consoleReadHeaderTimeout,
		WriteTimeout:      consoleWriteTimeout,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("console started", "port", port, "path", consoleHistoryPath)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("console server error", "error", err)
	}
}

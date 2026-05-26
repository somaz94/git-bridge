package mirror

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git-bridge/internal/config"
	"git-bridge/internal/notify"
	"git-bridge/internal/provider"
)

const defaultWorkDir = "/tmp/git-bridge"

// EventMeta carries webhook/event metadata through the sync pipeline.
type EventMeta struct {
	Ref    string // full ref (e.g. "refs/heads/main", "refs/tags/v1.0.0")
	Source string // trigger origin: "webhook", "sqs", "retry-api". empty = webhook
}

// RefName returns the short ref name (e.g. "main", "v1.0.0").
func (m EventMeta) RefName() string {
	if strings.HasPrefix(m.Ref, "refs/heads/") {
		return strings.TrimPrefix(m.Ref, "refs/heads/")
	}
	if strings.HasPrefix(m.Ref, "refs/tags/") {
		return strings.TrimPrefix(m.Ref, "refs/tags/")
	}
	return m.Ref
}

// IsTag returns true if the ref is a tag.
func (m EventMeta) IsTag() bool {
	return strings.HasPrefix(m.Ref, "refs/tags/")
}

// GitRunner executes git clone/push operations.
type GitRunner interface {
	CloneMirror(ctx context.Context, url, dir string) error
	FetchMirror(ctx context.Context, url, dir string) error
	// PushMirror pushes branches and tags. Returns (true, nil) if changes were pushed,
	// (false, nil) if already up-to-date, or (false, err) on failure.
	PushMirror(ctx context.Context, dir, url string) (changed bool, err error)
	DeleteRef(ctx context.Context, workDir, url, refType, refName string) error
	// CommitAuthor returns the author name of the latest commit on the given ref.
	CommitAuthor(ctx context.Context, dir, ref string) (string, error)
}

// Service handles git mirror operations.
type Service struct {
	configs        []config.RepoConfig
	providers      map[string]provider.Provider
	notifier       notify.Notifier
	workDir        string
	git            GitRunner
	timeoutSeconds int
	repoLocks      map[string]*sync.Mutex
	repoLocksMu    sync.Mutex
}

// defaultGitRunner executes real git commands.
type defaultGitRunner struct{}

func (d *defaultGitRunner) CloneMirror(ctx context.Context, url, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("cleanup before clone: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--mirror", url, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

func (d *defaultGitRunner) FetchMirror(ctx context.Context, url, dir string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "fetch", "--prune", url, "+refs/*:refs/*")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch mirror: %w: %s", err, string(out))
	}
	return nil
}

func (d *defaultGitRunner) PushMirror(ctx context.Context, dir, url string) (bool, error) {
	changed := false

	// Push all branches (--porcelain for reliable change detection)
	// Use separate stdout/stderr to avoid stderr progress lines polluting porcelain parsing
	pushAll := exec.CommandContext(ctx, "git", "-C", dir, "push", "--porcelain", "--force", "--all", url)
	var stdoutAll, stderrAll bytes.Buffer
	pushAll.Stdout = &stdoutAll
	pushAll.Stderr = &stderrAll
	if err := pushAll.Run(); err != nil {
		return false, fmt.Errorf("push branches: %w: %s", err, stderrAll.String())
	}
	if hasPorcelainChanges(stdoutAll.String()) {
		changed = true
	}

	// Push all tags (--porcelain for reliable change detection)
	pushTags := exec.CommandContext(ctx, "git", "-C", dir, "push", "--porcelain", "--force", "--tags", url)
	var stdoutTags, stderrTags bytes.Buffer
	pushTags.Stdout = &stdoutTags
	pushTags.Stderr = &stderrTags
	if err := pushTags.Run(); err != nil {
		return false, fmt.Errorf("push tags: %w: %s", err, stderrTags.String())
	}
	if hasPorcelainChanges(stdoutTags.String()) {
		changed = true
	}

	return changed, nil
}

// hasPorcelainChanges parses git push --porcelain output and returns true if any
// ref was actually updated. Porcelain format: each ref line starts with a flag character.
// '=' means up-to-date (no change), any other flag means a change was pushed.
func hasPorcelainChanges(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "To ") || strings.HasPrefix(line, "Done") {
			continue
		}
		// Porcelain ref lines: <flag>\t<from>:<to>\t<summary>
		// flag: ' ' (success), '+' (forced), '-' (deleted), '*' (new), '=' (up-to-date), '!' (rejected)
		if len(line) > 0 && line[0] != '=' {
			return true
		}
	}
	return false
}

func (d *defaultGitRunner) CommitAuthor(ctx context.Context, dir, ref string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "log", "-1", "--format=%an", ref)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git log for ref %q: %w: %s", ref, err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

func (d *defaultGitRunner) DeleteRef(ctx context.Context, workDir, url, refType, refName string) error {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare", workDir)
	if err := initCmd.Run(); err != nil {
		return fmt.Errorf("git init: %w", err)
	}

	var ref string
	if refType == "tag" {
		ref = "refs/tags/" + refName
	} else {
		ref = "refs/heads/" + refName
	}

	cmd := exec.CommandContext(ctx, "git", "-C", workDir, "push", url, ":"+ref)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("push delete ref: %w: %s", err, string(out))
	}
	return nil
}

// isGitDir returns true if dir exists and looks like a bare git repository.
func isGitDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "HEAD"))
	return err == nil && !info.IsDir()
}

// New creates a mirror service. Returns an error if any configured provider fails to initialize.
func New(cfg *config.Config, notifier notify.Notifier) (*Service, error) {
	providers := make(map[string]provider.Provider)
	for name, pcfg := range cfg.Providers {
		p, err := provider.New(name, pcfg)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		providers[name] = p
	}

	workDir := os.Getenv("WORK_DIR")
	if workDir == "" {
		workDir = defaultWorkDir
	}

	return &Service{
		configs:        cfg.Repos,
		providers:      providers,
		notifier:       notifier,
		workDir:        workDir,
		git:            &defaultGitRunner{},
		timeoutSeconds: cfg.Mirror.TimeoutSeconds,
		repoLocks:      make(map[string]*sync.Mutex),
	}, nil
}

// allowsSourceToTarget returns true if direction permits source → target sync.
func allowsSourceToTarget(direction string) bool {
	dir := strings.ToLower(direction)
	return dir == "source-to-target" || dir == "bidirectional"
}

// allowsTargetToSource returns true if direction permits target → source sync.
func allowsTargetToSource(direction string) bool {
	dir := strings.ToLower(direction)
	return dir == "target-to-source" || dir == "bidirectional"
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
// providerName is the provider type, repoPath is the target_path of the repo.
func (s *Service) SyncByTarget(ctx context.Context, providerName, repoPath string, meta EventMeta) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.timeoutSeconds)*time.Second)
	defer cancel()
	for _, repoCfg := range s.configs {
		// Match by target provider + target path
		tgtProvider, ok := s.providers[repoCfg.Target]
		if !ok {
			continue
		}
		if tgtProvider.Type() == providerName && repoCfg.TargetPath == repoPath {
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
		if srcProvider.Type() == providerName && repoCfg.SourcePath == repoPath {
			if allowsSourceToTarget(repoCfg.Direction) {
				return s.doMirror(ctx, repoCfg, repoCfg.Source, repoCfg.SourcePath, repoCfg.Target, repoCfg.TargetPath, meta)
			}
			return fmt.Errorf("repo %q direction %q does not allow source-to-target sync", repoCfg.Name, repoCfg.Direction)
		}
	}
	return fmt.Errorf("no matching repo for provider=%q path=%q", providerName, repoPath)
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
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.timeoutSeconds)*time.Second)
	defer cancel()

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
	case "source-to-target":
		return s.doMirror(ctx, *repoCfg, repoCfg.Source, repoCfg.SourcePath,
			repoCfg.Target, repoCfg.TargetPath, meta)
	case "target-to-source":
		return s.doMirror(ctx, *repoCfg, repoCfg.Target, repoCfg.TargetPath,
			repoCfg.Source, repoCfg.SourcePath, meta)
	}
	return fmt.Errorf("unknown retry direction %q", dir)
}

// resolveAutoDirection picks the default direction when retry direction is "auto".
// bidirectional → target-to-source (matches the 2026-05-19 incident pattern, where
// the GitLab → CodeCommit leg failed and manual retry was issued in that direction).
// one-way repos resolve to their single allowed direction.
func resolveAutoDirection(cfgDir string) string {
	switch strings.ToLower(cfgDir) {
	case "bidirectional":
		return "target-to-source"
	case "source-to-target":
		return "source-to-target"
	case "target-to-source":
		return "target-to-source"
	}
	// Unreachable in practice — config.validate() restricts Direction to the
	// three values above. Kept as a defensive default so a future code path
	// that bypasses validate() still produces a sane (refused-by-
	// directionAllowed) result instead of an empty direction string.
	return "target-to-source"
}

// directionAllowed reports whether retryDir is compatible with the repo's
// configured direction. bidirectional accepts any; one-way accepts only itself.
func directionAllowed(cfgDir, retryDir string) bool {
	cfg := strings.ToLower(cfgDir)
	if cfg == "bidirectional" {
		return true
	}
	return cfg == strings.ToLower(retryDir)
}

// appendSource appends a "Source: <name>" line to a Slack notification body
// when the trigger source is non-webhook (e.g. "retry-api"). Webhook (the
// default trigger) is left implicit to avoid noise on routine sync messages.
func appendSource(body string, meta EventMeta) string {
	if meta.Source == "" || meta.Source == "webhook" {
		return body
	}
	return body + fmt.Sprintf("\nSource: %s", meta.Source)
}

// SyncDelete deletes a ref from the target triggered by source-side delete event.
func (s *Service) SyncDelete(ctx context.Context, repoName, refType, refName string) error {
	for _, repoCfg := range s.configs {
		if repoCfg.SourcePath != repoName {
			continue
		}
		if allowsSourceToTarget(repoCfg.Direction) {
			return s.doDeleteRef(ctx, repoCfg, repoCfg.Target, repoCfg.TargetPath, refType, refName)
		}
		return fmt.Errorf("repo %q direction %q does not allow source-to-target sync", repoName, repoCfg.Direction)
	}
	return fmt.Errorf("repo %q not configured for mirroring", repoName)
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

// doDeleteRef deletes a specific branch or tag from the target.
func (s *Service) doDeleteRef(ctx context.Context, repoCfg config.RepoConfig, toProvider, toPath, refType, refName string) error {
	mu := s.repoLock(repoCfg.Name)
	mu.Lock()
	defer mu.Unlock()

	tgt, ok := s.providers[toProvider]
	if !ok {
		return fmt.Errorf("provider %q not found", toProvider)
	}

	tgtURL := tgt.CloneURL(toPath)
	logger := slog.With("repo", repoCfg.Name, "target", tgt.Type()+"/"+toPath, "ref", refType+"/"+refName)

	deleteDir := filepath.Join(s.workDir, repoCfg.Name+"-delete.git")
	defer func() {
		if err := os.RemoveAll(deleteDir); err != nil {
			slog.Warn("failed to clean up directory", "path", deleteDir, "error", err)
		}
	}()

	logger.Info("deleting ref from target")
	if err := s.git.DeleteRef(ctx, deleteDir, tgtURL, refType, refName); err != nil {
		s.notifier.Send(notify.Message{
			Level:      "error",
			Title:      fmt.Sprintf("Ref Delete Failed: %s", repoCfg.Name),
			Body:       fmt.Sprintf("Action: delete %s '%s'\nTarget: %s/%s\nError: %v", refType, refName, tgt.Type(), toPath, err),
			WebhookURL: repoCfg.SlackWebhookURL,
		})
		return fmt.Errorf("delete ref: %w", err)
	}

	logger.Info("ref deleted from target")
	s.notifier.Send(notify.Message{
		Level:      "success",
		Title:      fmt.Sprintf("Ref Deleted: %s", repoCfg.Name),
		Body:       fmt.Sprintf("Action: delete %s '%s'\nTarget: %s/%s\nURL: %s", refType, refName, tgt.Type(), toPath, tgt.WebURL(toPath)),
		WebhookURL: repoCfg.SlackWebhookURL,
	})
	return nil
}

// doMirror performs the actual git clone --mirror + git push --mirror.
func (s *Service) doMirror(ctx context.Context, repoCfg config.RepoConfig, fromProvider, fromPath, toProvider, toPath string, meta EventMeta) error {
	mu := s.repoLock(repoCfg.Name)
	mu.Lock()
	defer mu.Unlock()

	src, ok := s.providers[fromProvider]
	if !ok {
		return fmt.Errorf("provider %q not found", fromProvider)
	}
	tgt, ok := s.providers[toProvider]
	if !ok {
		return fmt.Errorf("provider %q not found", toProvider)
	}

	start := time.Now()
	srcURL := src.CloneURL(fromPath)
	tgtURL := tgt.CloneURL(toPath)

	logger := slog.With(
		"repo", repoCfg.Name,
		"from", src.Type()+"/"+fromPath,
		"to", tgt.Type()+"/"+toPath,
	)

	mirrorDir := filepath.Join(s.workDir, repoCfg.Name+"-"+src.Type()+".git")

	route := fmt.Sprintf("%s/%s → %s/%s", src.Type(), fromPath, tgt.Type(), toPath)

	// Incremental fetch if mirror exists, otherwise full clone
	if isGitDir(mirrorDir) {
		logger.Info("fetching from source (incremental)")
		if err := s.git.FetchMirror(ctx, srcURL, mirrorDir); err != nil {
			logger.Warn("incremental fetch failed, falling back to full clone", "error", err)
			if err := s.git.CloneMirror(ctx, srcURL, mirrorDir); err != nil {
				s.notifier.Send(notify.Message{
					Level:      "error",
					Title:      fmt.Sprintf("Mirror Sync Failed: %s", repoCfg.Name),
					Body:       appendSource(fmt.Sprintf("Action: clone (fallback)\nRoute: %s\nError: %v", route, err), meta),
					WebhookURL: repoCfg.SlackWebhookURL,
				})
				return fmt.Errorf("clone: %w", err)
			}
		}
	} else {
		logger.Info("cloning from source (initial)")
		if err := s.git.CloneMirror(ctx, srcURL, mirrorDir); err != nil {
			s.notifier.Send(notify.Message{
				Level:      "error",
				Title:      fmt.Sprintf("Mirror Sync Failed: %s", repoCfg.Name),
				Body:       appendSource(fmt.Sprintf("Action: clone\nRoute: %s\nError: %v", route, err), meta),
				WebhookURL: repoCfg.SlackWebhookURL,
			})
			return fmt.Errorf("clone: %w", err)
		}
	}

	// Push to target
	logger.Info("pushing to target")
	changed, err := s.git.PushMirror(ctx, mirrorDir, tgtURL)
	if err != nil {
		s.notifier.Send(notify.Message{
			Level:      "error",
			Title:      fmt.Sprintf("Mirror Sync Failed: %s", repoCfg.Name),
			Body:       appendSource(fmt.Sprintf("Action: push\nRoute: %s\nError: %v", route, err), meta),
			WebhookURL: repoCfg.SlackWebhookURL,
		})
		return fmt.Errorf("push: %w", err)
	}

	elapsed := time.Since(start)

	if !changed {
		logger.Info("already up-to-date, skipping notification", "duration", elapsed.String())
		return nil
	}

	logger.Info("mirror sync done", "duration", elapsed.String())

	body := fmt.Sprintf("Action: branches + tags synced\nRoute: %s\nDuration: %s\nTarget: %s",
		route,
		elapsed.Round(time.Millisecond),
		tgt.WebURL(toPath))
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

	return nil
}

package mirror

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"git-bridge/internal/config"
	"git-bridge/internal/notify"
	"git-bridge/internal/provider"
)

const (
	defaultWorkDir  = "/tmp/git-bridge"
	refsHeadsPrefix = "refs/heads/"
	refsTagsPrefix  = "refs/tags/"
	refTypeTag      = "tag" // CodeCommit/이벤트의 ref 종류 라벨(태그)
)

// fullRefName은 ref 종류(refType)와 짧은 이름(refName)으로 전체 ref를 만든다.
// refType이 "tag"면 refs/tags/, 그 외(브랜치)는 refs/heads/를 붙인다.
func fullRefName(refType, refName string) string {
	if refType == refTypeTag {
		return refsTagsPrefix + refName
	}
	return refsHeadsPrefix + refName
}

// EventMeta carries webhook/event metadata through the sync pipeline.
type EventMeta struct {
	Ref    string // full ref (e.g. "refs/heads/main", "refs/tags/v1.0.0")
	Source string // trigger origin: "webhook", "sqs", "retry-api". empty = webhook
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
	CloneMirror(ctx context.Context, url, dir string) error
	FetchMirror(ctx context.Context, url, dir string) error
	// PushMirror pushes refs to the target. refspecs가 비어 있으면 기존처럼 모든
	// 브랜치 + 태그를 force push하고, 값이 있으면 해당 refspec만 force push한다(ref 스코프).
	// Returns (true, nil) if changes were pushed, (false, nil) if already up-to-date,
	// or (false, err) on failure.
	PushMirror(ctx context.Context, dir, url string, refspecs []string) (changed bool, err error)
	// ListRefs는 미러 dir의 모든 로컬 브랜치/태그 ref(전체 이름)를 반환한다.
	ListRefs(ctx context.Context, dir string) ([]string, error)
	DeleteRef(ctx context.Context, workDir, url, refType, refName string) error
	// RefExists는 원격 url에 ref(refType/refName)가 존재하는지 ls-remote로 확인한다.
	// 삭제 멱등성용: 이미 없는 ref 삭제는 성공 no-op으로 처리해 양방향 삭제 루프를 끊는다.
	RefExists(ctx context.Context, url, refType, refName string) (bool, error)
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

// runPush는 단일 git push --porcelain --force 호출을 실행하고 변경 여부를 반환한다.
// stdout/stderr를 분리해 stderr 진행 로그가 porcelain 파싱을 오염시키지 않도록 한다.
// errLabel은 실패 시 에러 메시지 접두사로 쓰인다(테스트가 이 문자열을 검사).
func runPush(ctx context.Context, dir, errLabel string, pushArgs ...string) (bool, error) {
	args := append([]string{"-C", dir, "push", "--porcelain", "--force"}, pushArgs...)
	cmd := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("%s: %w: %s", errLabel, err, stderr.String())
	}
	return hasPorcelainChanges(stdout.String()), nil
}

func (d *defaultGitRunner) PushMirror(ctx context.Context, dir, url string, refspecs []string) (bool, error) {
	// refspec이 지정되면 해당 ref만 force push(브랜치/태그 혼합 가능).
	if len(refspecs) > 0 {
		return runPush(ctx, dir, "push refs", append([]string{url}, refspecs...)...)
	}

	// 브랜치 전체 + 태그 전체를 각각 force push하고 둘 중 하나라도 변경되면 changed.
	branchesChanged, err := runPush(ctx, dir, "push branches", "--all", url)
	if err != nil {
		return false, err
	}
	tagsChanged, err := runPush(ctx, dir, "push tags", "--tags", url)
	if err != nil {
		return false, err
	}
	return branchesChanged || tagsChanged, nil
}

// ListRefs는 미러 dir의 모든 로컬 브랜치/태그 ref(전체 이름)를 반환한다.
// full-sync(meta.Ref 없음) 시 ref_override가 금지한 방향의 ref를 제외하는 데 쓰인다.
func (d *defaultGitRunner) ListRefs(ctx context.Context, dir string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "for-each-ref", "--format=%(refname)", "refs/heads/", "refs/tags/")
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

// buildFullRefspecs는 full-sync(meta.Ref 없음) 시, 로컬 ref 목록에서 현재 방향
// (fromProvider→toProvider)에서 ref_override가 금지한 ref를 제외한 force refspec
// (+ref:ref) 목록을 만든다. 결과가 비면 이 방향엔 push할 ref가 없다는 뜻.
func buildFullRefspecs(localRefs []string, repoCfg config.RepoConfig, fromProvider, toProvider string) []string {
	specs := make([]string, 0, len(localRefs))
	for _, ref := range localRefs {
		short := EventMeta{Ref: ref}.RefName()
		if ov := repoCfg.MatchRefOverride(short); ov != nil && (ov.From != fromProvider || ov.To != toProvider) {
			continue // 이 방향에선 금지된 ref → 제외
		}
		specs = append(specs, "+"+ref+":"+ref)
	}
	return specs
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

	ref := fullRefName(refType, refName)

	cmd := exec.CommandContext(ctx, "git", "-C", workDir, "push", url, ":"+ref)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("push delete ref: %w: %s", err, string(out))
	}
	return nil
}

// RefExists는 git ls-remote로 원격 url에 정확한 full ref가 존재하는지 확인한다.
// full ref를 직접 전달해 prefix 매칭(refs/heads/feat가 refs/heads/feature에
// 매칭되는 것)을 회피한다. 출력이 비면 false(exit 0), 네트워크/인증 오류만 err.
func (d *defaultGitRunner) RefExists(ctx context.Context, url, refType, refName string) (bool, error) {
	ref := fullRefName(refType, refName)
	cmd := exec.CommandContext(ctx, "git", "ls-remote", url, ref)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("ls-remote %s: %w: %s", ref, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()) != "", nil
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
	case config.DirectionSourceToTarget:
		return s.doMirror(ctx, *repoCfg, repoCfg.Source, repoCfg.SourcePath,
			repoCfg.Target, repoCfg.TargetPath, meta)
	case config.DirectionTargetToSource:
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
	if meta.Source == "" || meta.Source == "webhook" {
		return body
	}
	return body + fmt.Sprintf("\nSource: %s", meta.Source)
}

// refOverrideBlocksDelete는 from→to 방향의 ref 삭제가 ref_override에 의해 금지되는지
// (그리고 skip을 로깅)를 반환한다. doMirror의 push-side 가드와 대칭 — 삭제는 override가
// 허용한 방향으로만 전파되어 반대편 권위 측 브랜치/태그 삭제 사고를 차단한다.
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
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.timeoutSeconds)*time.Second)
	defer cancel()
	for _, repoCfg := range s.configs {
		if repoCfg.SourcePath != repoName {
			continue
		}
		// per-ref 방향 오버라이드는 delete에도 적용한다(push와 대칭).
		// SyncDelete는 항상 source→target 방향이므로, override가 그 방향을 허용하지
		// 않으면 조용히 skip한다.
		if refOverrideBlocksDelete(repoCfg, refName, repoCfg.Source, repoCfg.Target) {
			return nil
		}
		if allowsSourceToTarget(repoCfg.Direction) {
			return s.doDeleteRef(ctx, repoCfg, repoCfg.Source, repoCfg.SourcePath, repoCfg.Target, repoCfg.TargetPath, refType, refName)
		}
		return fmt.Errorf("repo %q direction %q does not allow source-to-target sync", repoName, repoCfg.Direction)
	}
	return fmt.Errorf("repo %q not configured for mirroring", repoName)
}

// SyncDeleteByTarget deletes a ref triggered by a target-side delete event
// (GitLab/GitHub webhook with after == zeroSHA, or GitHub deleted:true).
// SyncByTarget(dual-match) + SyncDelete(override 대칭 skip)을 합친 형태:
// target provider+path 매칭이면 source 쪽에서, source provider+path 매칭이면
// target 쪽에서 ref를 삭제한다. 멱등성(이미 없는 ref skip)은 doDeleteRef 내부 처리.
func (s *Service) SyncDeleteByTarget(ctx context.Context, providerName, repoPath, refType, refName string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.timeoutSeconds)*time.Second)
	defer cancel()
	for _, repoCfg := range s.configs {
		// Match by target provider + target path → delete on the source side
		if tgtProvider, ok := s.providers[repoCfg.Target]; ok &&
			tgtProvider.Type() == providerName && repoCfg.TargetPath == repoPath {
			if !allowsTargetToSource(repoCfg.Direction) {
				return fmt.Errorf("repo %q direction %q does not allow target-to-source sync", repoCfg.Name, repoCfg.Direction)
			}
			if refOverrideBlocksDelete(repoCfg, refName, repoCfg.Target, repoCfg.Source) {
				return nil
			}
			return s.doDeleteRef(ctx, repoCfg, repoCfg.Target, repoCfg.TargetPath, repoCfg.Source, repoCfg.SourcePath, refType, refName)
		}

		// Match by source provider + source path → delete on the target side
		if srcProvider, ok := s.providers[repoCfg.Source]; ok &&
			srcProvider.Type() == providerName && repoCfg.SourcePath == repoPath {
			if !allowsSourceToTarget(repoCfg.Direction) {
				return fmt.Errorf("repo %q direction %q does not allow source-to-target sync", repoCfg.Name, repoCfg.Direction)
			}
			if refOverrideBlocksDelete(repoCfg, refName, repoCfg.Source, repoCfg.Target) {
				return nil
			}
			return s.doDeleteRef(ctx, repoCfg, repoCfg.Source, repoCfg.SourcePath, repoCfg.Target, repoCfg.TargetPath, refType, refName)
		}
	}
	return fmt.Errorf("no matching repo for provider=%q path=%q", providerName, repoPath)
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
// build the Route line in notifications (대칭: doMirror도 Route를 찍는다). 양방향 삭제
// 전파가 생긴 뒤로 Slack만 봐도 방향(예: gitlab→codecommit)을 알 수 있어야 하기 때문이다.
func (s *Service) doDeleteRef(ctx context.Context, repoCfg config.RepoConfig, fromProvider, fromPath, toProvider, toPath, refType, refName string) error {
	mu := s.repoLock(repoCfg.Name)
	mu.Lock()
	defer mu.Unlock()

	tgt, ok := s.providers[toProvider]
	if !ok {
		return fmt.Errorf("provider %q not found", toProvider)
	}
	src, ok := s.providers[fromProvider]
	if !ok {
		return fmt.Errorf("provider %q not found", fromProvider)
	}

	tgtURL := tgt.CloneURL(toPath)
	route := fmt.Sprintf("%s/%s → %s/%s", src.Type(), fromPath, tgt.Type(), toPath)
	logger := slog.With("repo", repoCfg.Name, "route", route, "ref", refType+"/"+refName)

	deleteDir := filepath.Join(s.workDir, repoCfg.Name+"-delete.git")
	defer func() {
		if err := os.RemoveAll(deleteDir); err != nil {
			slog.Warn("failed to clean up directory", "path", deleteDir, "error", err)
		}
	}()

	// 멱등성 체크: 이미 없는 ref는 성공 no-op으로 끝내 양방향 삭제 루프를 끊는다.
	// (gitlab 삭제 → codecommit 삭제 → codecommit referenceDeleted echo → 여기서 skip)
	// RefExists 실패는 보통 인증/네트워크 문제 → fail-closed로 드러낸다(에러 + 알림 + 재시도).
	exists, err := s.git.RefExists(ctx, tgtURL, refType, refName)
	if err != nil {
		s.notifier.Send(notify.Message{
			Level:      "error",
			Title:      fmt.Sprintf("Ref Delete Failed: %s", repoCfg.Name),
			Body:       fmt.Sprintf("Action: check %s '%s'\nRoute: %s\nError: %v", refType, refName, route, err),
			WebhookURL: repoCfg.SlackWebhookURL,
		})
		return fmt.Errorf("check ref exists: %w", err)
	}
	if !exists {
		logger.Info("ref already absent on target, skipping delete (idempotent no-op)")
		return nil
	}

	logger.Info("deleting ref from target")
	if err := s.git.DeleteRef(ctx, deleteDir, tgtURL, refType, refName); err != nil {
		s.notifier.Send(notify.Message{
			Level:      "error",
			Title:      fmt.Sprintf("Ref Delete Failed: %s", repoCfg.Name),
			Body:       fmt.Sprintf("Action: delete %s '%s'\nRoute: %s\nError: %v", refType, refName, route, err),
			WebhookURL: repoCfg.SlackWebhookURL,
		})
		return fmt.Errorf("delete ref: %w", err)
	}

	logger.Info("ref deleted from target")
	s.notifier.Send(notify.Message{
		Level:      "success",
		Title:      fmt.Sprintf("Ref Deleted: %s", repoCfg.Name),
		Body:       fmt.Sprintf("Action: delete %s '%s'\nRoute: %s\nURL: %s", refType, refName, route, tgt.WebURL(toPath)),
		WebhookURL: repoCfg.SlackWebhookURL,
	})
	return nil
}

// notifyFailure는 미러 동작 실패에 대한 표준 에러 알림을 보낸다.
// action은 실패 단계("clone" / "clone (fallback)" / "push"), route는 from→to 경로.
func (s *Service) notifyFailure(repoCfg config.RepoConfig, action, route string, meta EventMeta, err error) {
	s.notifier.Send(notify.Message{
		Level:      "error",
		Title:      fmt.Sprintf("Mirror Sync Failed: %s", repoCfg.Name),
		Body:       appendSource(fmt.Sprintf("Action: %s\nRoute: %s\nError: %v", action, route, err), meta),
		WebhookURL: repoCfg.SlackWebhookURL,
	})
}

// ensureMirror는 미러가 있으면 incremental fetch, 없으면 full clone한다.
// fetch 실패 시 full clone으로 폴백하고, clone 실패 시 실패 알림 후 에러를 감싸 반환한다.
func (s *Service) ensureMirror(ctx context.Context, repoCfg config.RepoConfig, srcURL, mirrorDir, route string, meta EventMeta, logger *slog.Logger) error {
	if isGitDir(mirrorDir) {
		logger.Info("fetching from source (incremental)")
		if err := s.git.FetchMirror(ctx, srcURL, mirrorDir); err != nil {
			logger.Warn("incremental fetch failed, falling back to full clone", "error", err)
			if cerr := s.git.CloneMirror(ctx, srcURL, mirrorDir); cerr != nil {
				s.notifyFailure(repoCfg, "clone (fallback)", route, meta, cerr)
				return fmt.Errorf("clone: %w", cerr)
			}
		}
		return nil
	}
	logger.Info("cloning from source (initial)")
	if err := s.git.CloneMirror(ctx, srcURL, mirrorDir); err != nil {
		s.notifyFailure(repoCfg, "clone", route, meta, err)
		return fmt.Errorf("clone: %w", err)
	}
	return nil
}

// resolveRefspecs는 ref_overrides가 설정된 repo에서 push할 refspec과 scoped 여부를 반환한다.
// scoped=true면 ref 스코프가 활성(빈 refspecs는 "이 방향엔 push할 ref 없음 → skip" 신호).
// ref_overrides가 없거나 ListRefs 실패(fail-open) 시 (nil, false) → 기존 --all 동작 그대로(불변).
//   - meta.Ref 있음 → 트리거된 단일 ref만 push. 단 로컬에 실제로 있을 때만(없으면 빈 refspecs +
//     scoped=true → 없는 브랜치 retry / fetch~push 사이 prune race를 에러 대신 no-op skip).
//   - meta.Ref 없음 → 이 방향에서 금지된 override ref를 제외한 refspec 생성.
func (s *Service) resolveRefspecs(ctx context.Context, repoCfg config.RepoConfig, mirrorDir, fromProvider, toProvider string, meta EventMeta, logger *slog.Logger) (refspecs []string, scoped bool) {
	if len(repoCfg.RefOverrides) == 0 {
		return nil, false
	}
	refs, err := s.git.ListRefs(ctx, mirrorDir)
	if err != nil {
		// fail-open: ref 열거 실패 시 기존 --all push로 폴백(미러를 멈추지 않음)
		logger.Warn("ListRefs failed, falling back to full push", "error", err)
		return nil, false
	}
	if meta.Ref != "" {
		if slices.Contains(refs, meta.Ref) {
			return []string{"+" + meta.Ref + ":" + meta.Ref}, true
		}
		return nil, true // 로컬에 없음 → 빈 refspecs + scoped → 호출부에서 no-op skip
	}
	return buildFullRefspecs(refs, repoCfg, fromProvider, toProvider), true
}

// doMirror performs the actual git clone --mirror + git push --mirror.
func (s *Service) doMirror(ctx context.Context, repoCfg config.RepoConfig, fromProvider, fromPath, toProvider, toPath string, meta EventMeta) error {
	// per-ref 방향 오버라이드: 트리거 ref가 ref_override에 매칭되고 현재 sync 방향
	// (fromProvider→toProvider)이 허용 방향과 다르면 조용히 skip한다(터미널 nil).
	// 에러가 아니라 nil을 반환해야 SQS가 메시지를 삭제해 재시도/DLQ churn이 없다.
	// (양방향 repo에서 특정 브랜치를 한 방향으로만 고정해 반대편 덮어쓰기를 차단)
	if meta.Ref != "" {
		if ov := repoCfg.MatchRefOverride(meta.RefName()); ov != nil && (ov.From != fromProvider || ov.To != toProvider) {
			slog.Info("ref override: skipping reverse-direction sync",
				"repo", repoCfg.Name, "ref", meta.RefName(),
				"this", fromProvider+"→"+toProvider, "allowed", ov.From+"→"+ov.To)
			return nil
		}
	}

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

	// 미러 준비(있으면 incremental fetch, 없으면 full clone, fetch 실패 시 clone 폴백)
	if err := s.ensureMirror(ctx, repoCfg, srcURL, mirrorDir, route, meta, logger); err != nil {
		return err
	}

	// push refspec 결정(ref_overrides가 있는 repo에만 적용 — 그 외는 기존 --all 동작 불변)
	refspecs, scoped := s.resolveRefspecs(ctx, repoCfg, mirrorDir, fromProvider, toProvider, meta, logger)
	if scoped && len(refspecs) == 0 {
		// push할 ref 없음(트리거 ref가 로컬에 없거나 이 방향에서 전부 override 제외) → no-op
		logger.Info("no refs to push for this direction (triggered ref absent or all excluded), skipping")
		return nil
	}

	// Push to target
	logger.Info("pushing to target")
	changed, err := s.git.PushMirror(ctx, mirrorDir, tgtURL, refspecs)
	if err != nil {
		s.notifyFailure(repoCfg, "push", route, meta, err)
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

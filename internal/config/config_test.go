package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_ValidConfig(t *testing.T) {
	content := `
server:
  port: 9090
providers:
  codecommit-eu:
    type: codecommit
    region: eu-central-1
    credentials:
      git_username: user
      git_password: pass
  gitlab-main:
    type: gitlab
    base_url: http://gitlab.example.com
    credentials:
      token: glpat-test
repos:
  - name: test-repo
    source: codecommit-eu
    target: gitlab-main
    source_path: test-repo
    target_path: team/test-repo
    direction: source-to-target
consumer:
  type: sqs
  queue_url: https://sqs.eu-central-1.amazonaws.com/123456/test-queue
  region: eu-central-1
  credentials:
    access_key: AKIA_TEST
    secret_key: secret_test
notification:
  slack:
    webhook_url: ""
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("port = %d, want 9090", cfg.Server.Port)
	}
	if len(cfg.Providers) != 2 {
		t.Errorf("providers = %d, want 2", len(cfg.Providers))
	}
	if len(cfg.Repos) != 1 {
		t.Errorf("repos = %d, want 1", len(cfg.Repos))
	}
	if cfg.Repos[0].Direction != "source-to-target" {
		t.Errorf("direction = %q, want source-to-target", cfg.Repos[0].Direction)
	}
	if len(cfg.Consumers) != 1 {
		t.Fatalf("consumers = %d, want 1", len(cfg.Consumers))
	}
	if cfg.Consumers[0].QueueURL != "https://sqs.eu-central-1.amazonaws.com/123456/test-queue" {
		t.Errorf("queue_url mismatch")
	}
	if cfg.Consumers[0].Name != "default" {
		t.Errorf("consumer name = %q, want default", cfg.Consumers[0].Name)
	}
}

func TestLoad_DefaultPort(t *testing.T) {
	content := `
providers:
  cc:
    type: codecommit
    region: us-east-1
    credentials:
      git_username: u
      git_password: p
  gl:
    type: gitlab
    base_url: http://gl.test
    credentials:
      token: tok
repos:
  - name: r
    source: cc
    target: gl
    source_path: r
    target_path: r
    direction: bidirectional
consumer:
  queue_url: https://sqs.test/q
  region: us-east-1
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("default port = %d, want 8080", cfg.Server.Port)
	}
	if len(cfg.Consumers) != 1 || cfg.Consumers[0].Type != "sqs" {
		t.Errorf("consumer type = %q, want sqs", cfg.Consumers[0].Type)
	}
}

func TestLoad_EnvVarExpansion(t *testing.T) {
	t.Setenv("TEST_GIT_USER", "expanded-user")
	t.Setenv("TEST_GIT_PASS", "expanded-pass")

	content := `
providers:
  cc:
    type: codecommit
    region: eu-central-1
    credentials:
      git_username: ${TEST_GIT_USER}
      git_password: ${TEST_GIT_PASS}
  gl:
    type: gitlab
    base_url: http://gl.test
    credentials:
      token: tok
repos:
  - name: r
    source: cc
    target: gl
    source_path: r
    target_path: r
    direction: source-to-target
consumer:
  queue_url: https://sqs.test/q
  region: eu-central-1
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	creds := cfg.Providers["cc"].Credentials
	if creds["git_username"] != "expanded-user" {
		t.Errorf("git_username = %q, want expanded-user", creds["git_username"])
	}
	if creds["git_password"] != "expanded-pass" {
		t.Errorf("git_password = %q, want expanded-pass", creds["git_password"])
	}
}

func TestLoad_NoRepos(t *testing.T) {
	content := `
providers:
  cc:
    type: codecommit
    region: us-east-1
    credentials: {}
repos: []
consumer:
  queue_url: https://sqs.test/q
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for no repos")
	}
}

func TestLoad_InvalidDirection(t *testing.T) {
	content := `
providers:
  cc:
    type: codecommit
    region: us-east-1
    credentials: {}
  gl:
    type: gitlab
    base_url: http://gl.test
    credentials: {}
repos:
  - name: r
    source: cc
    target: gl
    source_path: r
    target_path: r
    direction: invalid
consumer:
  queue_url: https://sqs.test/q
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid direction")
	}
}

func TestLoad_UnknownProvider(t *testing.T) {
	content := `
providers:
  cc:
    type: codecommit
    region: us-east-1
    credentials: {}
repos:
  - name: r
    source: cc
    target: nonexistent
    source_path: r
    target_path: r
    direction: source-to-target
consumer:
  queue_url: https://sqs.test/q
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown target provider")
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/tmp/nonexistent-config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte("{{invalid yaml"), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoad_RepoNameEmpty(t *testing.T) {
	content := `
providers:
  cc:
    type: codecommit
    region: us-east-1
    credentials: {}
  gl:
    type: gitlab
    base_url: http://gl.test
    credentials: {}
repos:
  - name: ""
    source: cc
    target: gl
    source_path: r
    target_path: r
    direction: source-to-target
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty repo name")
	}
}

func TestLoad_RepoSourceEmpty(t *testing.T) {
	content := `
providers:
  cc:
    type: codecommit
    region: us-east-1
    credentials: {}
repos:
  - name: r
    source: ""
    target: cc
    source_path: r
    target_path: r
    direction: source-to-target
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty source")
	}
}

func TestLoad_UnknownSourceProvider(t *testing.T) {
	content := `
providers:
  gl:
    type: gitlab
    base_url: http://gl.test
    credentials: {}
repos:
  - name: r
    source: nonexistent
    target: gl
    source_path: r
    target_path: r
    direction: source-to-target
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown source provider")
	}
}

func TestLoad_EnvVarNotSet(t *testing.T) {
	content := `
providers:
  cc:
    type: codecommit
    region: us-east-1
    credentials:
      git_username: ${UNSET_VAR_12345}
      git_password: pass
  gl:
    type: gitlab
    base_url: http://gl.test
    credentials:
      token: tok
repos:
  - name: r
    source: cc
    target: gl
    source_path: r
    target_path: r
    direction: source-to-target
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Unset env vars should be preserved as literal
	if cfg.Providers["cc"].Credentials["git_username"] != "${UNSET_VAR_12345}" {
		t.Errorf("expected literal ${UNSET_VAR_12345}, got %q", cfg.Providers["cc"].Credentials["git_username"])
	}
}

func TestLoad_SQSOptional(t *testing.T) {
	content := `
providers:
  gl:
    type: gitlab
    base_url: http://gl.test
    credentials:
      token: tok
  gh:
    type: github
    credentials:
      token: tok
repos:
  - name: r
    source: gl
    target: gh
    source_path: team/r
    target_path: org/r
    direction: bidirectional
consumer:
  queue_url: ""
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Consumers) != 0 {
		t.Errorf("consumers should be empty, got %d", len(cfg.Consumers))
	}
}

func TestLoad_MultipleConsumers(t *testing.T) {
	content := `
providers:
  gitlab-main:
    type: gitlab
    base_url: http://gl.test
    credentials:
      token: tok
  github-main:
    type: github
    credentials:
      token: tok
  codecommit-us:
    type: codecommit
    region: us-east-1
    credentials:
      git_username: u1
      git_password: p1
  codecommit-eu:
    type: codecommit
    region: eu-central-1
    credentials:
      git_username: u2
      git_password: p2
repos:
  - name: repo-us
    source: codecommit-us
    target: gitlab-main
    source_path: repo-us
    target_path: team/repo-us
    direction: source-to-target
  - name: repo-eu
    source: codecommit-eu
    target: github-main
    source_path: repo-eu
    target_path: org/repo-eu
    direction: source-to-target
consumers:
  - name: sqs-us
    type: sqs
    queue_url: https://sqs.us-east-1.amazonaws.com/111111/queue-us
    region: us-east-1
    credentials:
      access_key: AKIA_US
      secret_key: secret_us
  - name: sqs-eu
    type: sqs
    queue_url: https://sqs.eu-central-1.amazonaws.com/222222/queue-eu
    region: eu-central-1
    credentials:
      access_key: AKIA_EU
      secret_key: secret_eu
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Consumers) != 2 {
		t.Fatalf("consumers = %d, want 2", len(cfg.Consumers))
	}
	if cfg.Consumers[0].Name != "sqs-us" {
		t.Errorf("consumer[0] name = %q, want sqs-us", cfg.Consumers[0].Name)
	}
	if cfg.Consumers[0].Region != "us-east-1" {
		t.Errorf("consumer[0] region = %q, want us-east-1", cfg.Consumers[0].Region)
	}
	if cfg.Consumers[1].Name != "sqs-eu" {
		t.Errorf("consumer[1] name = %q, want sqs-eu", cfg.Consumers[1].Name)
	}
	if cfg.Consumers[1].Region != "eu-central-1" {
		t.Errorf("consumer[1] region = %q, want eu-central-1", cfg.Consumers[1].Region)
	}
}

func TestLoad_DuplicateConsumerName(t *testing.T) {
	content := `
providers:
  gl:
    type: gitlab
    base_url: http://gl.test
    credentials:
      token: tok
  cc:
    type: codecommit
    region: us-east-1
    credentials:
      git_username: u
      git_password: p
repos:
  - name: r
    source: cc
    target: gl
    source_path: r
    target_path: r
    direction: source-to-target
consumers:
  - name: same-name
    queue_url: https://sqs.us-east-1.amazonaws.com/111/q1
    region: us-east-1
  - name: same-name
    queue_url: https://sqs.eu-central-1.amazonaws.com/222/q2
    region: eu-central-1
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate consumer names")
	}
}

func TestLoad_ConsumerEmptyQueueURL(t *testing.T) {
	content := `
providers:
  cc:
    type: codecommit
    region: us-east-1
    credentials:
      git_username: u
      git_password: p
  gl:
    type: gitlab
    base_url: http://gl.test
    credentials:
      token: tok
repos:
  - name: r
    source: cc
    target: gl
    source_path: r
    target_path: r
    direction: source-to-target
consumers:
  - name: bad
    queue_url: ""
    region: us-east-1
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty consumer queue_url")
	}
}

func TestLoad_ConsumerAutoName(t *testing.T) {
	content := `
providers:
  cc:
    type: codecommit
    region: us-east-1
    credentials:
      git_username: u
      git_password: p
  gl:
    type: gitlab
    base_url: http://gl.test
    credentials:
      token: tok
repos:
  - name: r
    source: cc
    target: gl
    source_path: r
    target_path: r
    direction: source-to-target
consumers:
  - queue_url: https://sqs.us-east-1.amazonaws.com/111/q1
    region: us-east-1
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Consumers[0].Name != "sqs-0" {
		t.Errorf("consumer name = %q, want sqs-0", cfg.Consumers[0].Name)
	}
	if cfg.Consumers[0].Type != "sqs" {
		t.Errorf("consumer type = %q, want sqs", cfg.Consumers[0].Type)
	}
}

func TestLoad_DuplicateRepoName(t *testing.T) {
	content := `
providers:
  cc:
    type: codecommit
    region: us-east-1
    credentials: {}
  gl:
    type: gitlab
    base_url: http://gl.test
    credentials: {}
repos:
  - name: same-repo
    source: cc
    target: gl
    source_path: r1
    target_path: r1
    direction: source-to-target
  - name: same-repo
    source: cc
    target: gl
    source_path: r2
    target_path: r2
    direction: source-to-target
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate repo name")
	}
}

func TestLoad_SameSourceTarget(t *testing.T) {
	content := `
providers:
  cc:
    type: codecommit
    region: us-east-1
    credentials: {}
repos:
  - name: r
    source: cc
    target: cc
    source_path: r
    target_path: r
    direction: source-to-target
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for same source and target provider")
	}
}

// --- retry_direction validation ---

func writeAndLoad(t *testing.T, content string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Load(path)
}

const retryDirectionBaseYAML = `
providers:
  cc:
    type: codecommit
    region: us-east-1
    credentials:
      git_username: u
      git_password: p
  gl:
    type: gitlab
    base_url: http://gl.test
    credentials:
      token: tok
consumer:
  queue_url: https://sqs.test/q
  region: us-east-1
`

func TestLoad_RetryDirection_ValidOnBidirectional(t *testing.T) {
	yaml := retryDirectionBaseYAML + `repos:
  - name: r
    source: cc
    target: gl
    source_path: r
    target_path: t/r
    direction: bidirectional
    retry_direction: source-to-target
`
	cfg, err := writeAndLoad(t, yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Repos[0].RetryDirection != "source-to-target" {
		t.Errorf("retry_direction = %q, want source-to-target", cfg.Repos[0].RetryDirection)
	}
}

func TestLoad_RetryDirection_OmittedOK(t *testing.T) {
	yaml := retryDirectionBaseYAML + `repos:
  - name: r
    source: cc
    target: gl
    source_path: r
    target_path: t/r
    direction: bidirectional
`
	cfg, err := writeAndLoad(t, yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Repos[0].RetryDirection != "" {
		t.Errorf("retry_direction should be empty when omitted, got %q", cfg.Repos[0].RetryDirection)
	}
}

func TestLoad_RetryDirection_InvalidValue(t *testing.T) {
	yaml := retryDirectionBaseYAML + `repos:
  - name: r
    source: cc
    target: gl
    source_path: r
    target_path: t/r
    direction: bidirectional
    retry_direction: bidirectional
`
	_, err := writeAndLoad(t, yaml)
	if err == nil {
		t.Fatal("expected error for retry_direction=bidirectional (only source-to-target / target-to-source allowed)")
	}
}

func TestLoad_RetryDirection_ConflictsWithOneWay(t *testing.T) {
	yaml := retryDirectionBaseYAML + `repos:
  - name: r
    source: cc
    target: gl
    source_path: r
    target_path: t/r
    direction: source-to-target
    retry_direction: target-to-source
`
	_, err := writeAndLoad(t, yaml)
	if err == nil {
		t.Fatal("expected error for retry_direction conflicting with one-way direction")
	}
}

func TestLoad_LegacyConsumerBackwardCompat(t *testing.T) {
	content := `
providers:
  cc:
    type: codecommit
    region: us-east-1
    credentials:
      git_username: u
      git_password: p
  gl:
    type: gitlab
    base_url: http://gl.test
    credentials:
      token: tok
repos:
  - name: r
    source: cc
    target: gl
    source_path: r
    target_path: r
    direction: source-to-target
consumer:
  type: sqs
  queue_url: https://sqs.us-east-1.amazonaws.com/123/q
  region: us-east-1
  credentials:
    access_key: AKIA
    secret_key: secret
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(content), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Legacy consumer should be merged into consumers
	if len(cfg.Consumers) != 1 {
		t.Fatalf("consumers = %d, want 1", len(cfg.Consumers))
	}
	if cfg.Consumers[0].Name != "default" {
		t.Errorf("name = %q, want default", cfg.Consumers[0].Name)
	}
	if cfg.Consumers[0].Credentials.AccessKey != "AKIA" {
		t.Errorf("access_key = %q, want AKIA", cfg.Consumers[0].Credentials.AccessKey)
	}
}

// --- ref_overrides (Phase A) validation tests ---

func TestLoad_RefOverrides_Valid(t *testing.T) {
	yaml := retryDirectionBaseYAML + `repos:
  - name: example
    source: cc
    target: gl
    source_path: example
    target_path: t/example
    direction: bidirectional
    ref_overrides:
      - { pattern: "branch-a", from: gl, to: cc }
      - { pattern: "branch-*", from: gl, to: cc }
`
	cfg, err := writeAndLoad(t, yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Repos[0].RefOverrides) != 2 {
		t.Fatalf("expected 2 ref_overrides, got %d", len(cfg.Repos[0].RefOverrides))
	}
	if ov := cfg.Repos[0].MatchRefOverride("branch-a"); ov == nil || ov.From != "gl" || ov.To != "cc" {
		t.Errorf("MatchRefOverride(branch-a) = %+v, want gl→cc", ov)
	}
	// first-match: branch-c matches the second pattern only
	if ov := cfg.Repos[0].MatchRefOverride("branch-c"); ov == nil {
		t.Error("MatchRefOverride(branch-c) should match branch-*")
	}
	if ov := cfg.Repos[0].MatchRefOverride("feature-x"); ov != nil {
		t.Errorf("MatchRefOverride(feature-x) should be nil, got %+v", ov)
	}
}

func TestLoad_RefOverrides_EmptyPattern(t *testing.T) {
	yaml := retryDirectionBaseYAML + `repos:
  - name: example
    source: cc
    target: gl
    source_path: example
    target_path: t/example
    direction: bidirectional
    ref_overrides:
      - { pattern: "", from: gl, to: cc }
`
	if _, err := writeAndLoad(t, yaml); err == nil {
		t.Fatal("expected error for empty pattern")
	}
}

func TestLoad_RefOverrides_MissingFromTo(t *testing.T) {
	yaml := retryDirectionBaseYAML + `repos:
  - name: example
    source: cc
    target: gl
    source_path: example
    target_path: t/example
    direction: bidirectional
    ref_overrides:
      - { pattern: "branch-a", from: gl }
`
	if _, err := writeAndLoad(t, yaml); err == nil {
		t.Fatal("expected error for missing 'to'")
	}
}

func TestLoad_RefOverrides_SameFromTo(t *testing.T) {
	yaml := retryDirectionBaseYAML + `repos:
  - name: example
    source: cc
    target: gl
    source_path: example
    target_path: t/example
    direction: bidirectional
    ref_overrides:
      - { pattern: "branch-a", from: gl, to: gl }
`
	if _, err := writeAndLoad(t, yaml); err == nil {
		t.Fatal("expected error for from == to")
	}
}

func TestLoad_RefOverrides_UnknownProvider(t *testing.T) {
	yaml := retryDirectionBaseYAML + `repos:
  - name: example
    source: cc
    target: gl
    source_path: example
    target_path: t/example
    direction: bidirectional
    ref_overrides:
      - { pattern: "branch-a", from: gl, to: github }
`
	if _, err := writeAndLoad(t, yaml); err == nil {
		t.Fatal("expected error: from/to must be this repo's source and target")
	}
}

func TestLoad_RefOverrides_ConflictsWithOneWay(t *testing.T) {
	// repo는 source-to-target(cc→gl)만 허용인데 override가 gl→cc(역방향)를 요구 → 에러
	yaml := retryDirectionBaseYAML + `repos:
  - name: example
    source: cc
    target: gl
    source_path: example
    target_path: t/example
    direction: source-to-target
    ref_overrides:
      - { pattern: "branch-a", from: gl, to: cc }
`
	if _, err := writeAndLoad(t, yaml); err == nil {
		t.Fatal("expected error: override direction conflicts with one-way repo direction")
	}
}

func TestLoad_RefOverrides_InvalidPattern(t *testing.T) {
	yaml := retryDirectionBaseYAML + `repos:
  - name: example
    source: cc
    target: gl
    source_path: example
    target_path: t/example
    direction: bidirectional
    ref_overrides:
      - { pattern: "[invalid", from: gl, to: cc }
`
	if _, err := writeAndLoad(t, yaml); err == nil {
		t.Fatal("expected error for invalid glob pattern")
	}
}

func TestLoad_RefOverrides_DuplicatePattern(t *testing.T) {
	yaml := retryDirectionBaseYAML + `repos:
  - name: example
    source: cc
    target: gl
    source_path: example
    target_path: t/example
    direction: bidirectional
    ref_overrides:
      - { pattern: "branch-a", from: gl, to: cc }
      - { pattern: "branch-a", from: cc, to: gl }
`
	if _, err := writeAndLoad(t, yaml); err == nil {
		t.Fatal("expected error for duplicate ref_override pattern")
	}
}

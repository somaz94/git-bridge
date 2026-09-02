package config

import (
	"os"
	"path/filepath"
	"strings"
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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, "{{invalid yaml")

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	path := writeCfg(t, content)

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
	// The repo allows source-to-target (cc→gl) only, but the override asks for gl→cc (the reverse) → error
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

// minimalConfig is a valid config with the given server block spliced in, so a
// test can vary only the ports.
func minimalConfig(serverBlock string) string {
	return serverBlock + `
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
`
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoad_DefaultConsolePort(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig("server:\n  port: 8080\n")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.ConsolePort != 8081 {
		t.Errorf("console_port = %d, want 8081", cfg.Server.ConsolePort)
	}
}

func TestLoad_ExplicitConsolePort(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig("server:\n  port: 8080\n  console_port: 9091\n")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.ConsolePort != 9091 {
		t.Errorf("console_port = %d, want 9091", cfg.Server.ConsolePort)
	}
}

// Sharing a port would put the console behind the public HTTPRoute, which is
// exactly what the separate listener exists to prevent.
func TestLoad_ConsolePortMustDifferFromPort(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig("server:\n  port: 8080\n  console_port: 8080\n")))
	if err == nil {
		t.Fatal("error = nil, want console_port == port to be rejected")
	}
	if !strings.Contains(err.Error(), "console_port") {
		t.Errorf("error = %v, want it to name console_port", err)
	}
}

// baseConfig is the smallest config that Load accepts, used by the drain-window
// tests so the assertion is about that field and nothing else.
const baseConfig = `
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
`

func loadConfigFrom(t *testing.T, content string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return cfg
}

// An unset drain window must not become a zero one: shutdown would then wait no
// time at all and kill every sync in flight, which is the behaviour the setting
// exists to prevent.
func TestLoadDefaultsDrainTimeout(t *testing.T) {
	cfg := loadConfigFrom(t, baseConfig)
	if cfg.Mirror.DrainTimeoutSeconds != 120 {
		t.Errorf("DrainTimeoutSeconds = %d, want the 120s default", cfg.Mirror.DrainTimeoutSeconds)
	}
}

// An explicit value wins — the point of moving this out of a constant is that
// the right window depends on the repositories being mirrored.
func TestLoadHonoursExplicitDrainTimeout(t *testing.T) {
	cfg := loadConfigFrom(t, baseConfig+`
mirror:
  timeout_seconds: 600
  drain_timeout_seconds: 300
`)
	if cfg.Mirror.DrainTimeoutSeconds != 300 {
		t.Errorf("DrainTimeoutSeconds = %d, want 300", cfg.Mirror.DrainTimeoutSeconds)
	}
}

// consumerConfig builds a minimal config with the given mirror/consumer timeouts.
func consumerConfig(mirrorTimeout, visibility string) string {
	return `
mirror:
` + mirrorTimeout + `
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
consumers:
  - name: c1
    queue_url: https://sqs.test/q
    region: us-east-1
` + visibility
}

func loadConsumer(t *testing.T, content string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Load(path)
}

// A message is hidden for visibility_timeout_seconds while the batch is handled
// serially. If that window is shorter than a single sync, the message becomes
// visible again mid-sync, is redelivered, blocks on the per-repo mutex, and
// eventually lands in the DLQ. Config must make that impossible.
func TestLoad_VisibilityTimeoutCoversSyncTimeout(t *testing.T) {
	t.Run("defaults to mirror timeout", func(t *testing.T) {
		cfg, err := loadConsumer(t, consumerConfig("  timeout_seconds: 600", ""))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := cfg.Consumers[0].VisibilityTimeoutSeconds; got != 600 {
			t.Errorf("visibility timeout = %d, want 600 (mirror.timeout_seconds)", got)
		}
	})

	t.Run("rejects a window shorter than one sync", func(t *testing.T) {
		_, err := loadConsumer(t, consumerConfig("  timeout_seconds: 600", "    visibility_timeout_seconds: 120"))
		if err == nil {
			t.Fatal("expected an error: 120s cannot cover a 600s sync")
		}
		if !strings.Contains(err.Error(), "visibility_timeout_seconds") {
			t.Errorf("error should name the field, got: %v", err)
		}
	})

	t.Run("accepts an explicit longer window", func(t *testing.T) {
		cfg, err := loadConsumer(t, consumerConfig("  timeout_seconds: 300", "    visibility_timeout_seconds: 1800"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := cfg.Consumers[0].VisibilityTimeoutSeconds; got != 1800 {
			t.Errorf("visibility timeout = %d, want 1800", got)
		}
	})

	t.Run("rejects above the SQS maximum", func(t *testing.T) {
		_, err := loadConsumer(t, consumerConfig("  timeout_seconds: 300", "    visibility_timeout_seconds: 43201"))
		if err == nil || !strings.Contains(err.Error(), "43200") {
			t.Fatalf("expected the SQS 12h cap to be enforced, got: %v", err)
		}
	})
}

// writeCfg is a small helper for the collision tests below.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoad_CollidingDispatchEndpoints pins down that the dispatch key is
// (provider name, path).
//
// The webhook narrows the payload's instance host all the way down to the provider
// name, so two instances of the same type carrying the same path still take separate
// directions — the gitlab↔gitlab same-path setup that the old (type, path) key
// rejected is valid now. The same provider claiming the same path twice, on the other
// hand, is a real collision that the host cannot separate either, so it stays
// rejected. Under a rule that stops at the first match, that second claimant dies
// with neither an error nor a log line.
func TestLoad_CollidingDispatchEndpoints(t *testing.T) {
	const providers = `
providers:
  gl_a:
    type: gitlab
    base_url: http://a.test
    credentials: {}
  gl_b:
    type: gitlab
    base_url: http://b.test
    credentials: {}
  cc:
    type: codecommit
    region: us-east-1
    credentials: {}
`
	tests := []struct {
		name    string
		repos   string
		wantErr bool
	}{
		{
			// The same provider claims the same path twice (A's target = B's source).
			// The host is the same, so narrowing does not separate them — B is
			// swallowed by A forever.
			name: "same provider owns one path as A's target and B's source",
			repos: `
  - name: a
    source: cc
    target: gl_a
    source_path: r
    target_path: shared
    direction: bidirectional
  - name: b
    source: gl_a
    target: cc
    source_path: shared
    target_path: r2
    direction: bidirectional`,
			wantErr: true,
		},
		{
			// Two entries take the same path on the same provider as their target.
			name: "same provider, same path as two targets",
			repos: `
  - name: a
    source: cc
    target: gl_a
    source_path: r1
    target_path: shared
    direction: bidirectional
  - name: b
    source: cc
    target: gl_a
    source_path: r2
    target_path: shared
    direction: bidirectional`,
			wantErr: true,
		},
		{
			// The old constraint, case 1. A setup that ties two instances of the same
			// type to the same path, which is valid now because the host narrows it
			// all the way down to the provider name.
			name: "same type, different instances, same path is fine",
			repos: `
  - name: a
    source: gl_a
    target: gl_b
    source_path: same
    target_path: same
    direction: bidirectional`,
			wantErr: false,
		},
		{
			// One path is A's target and also B's source, but on different instances.
			name: "different instances share a path across entries",
			repos: `
  - name: a
    source: cc
    target: gl_a
    source_path: r
    target_path: shared
    direction: bidirectional
  - name: b
    source: gl_b
    target: cc
    source_path: shared
    target_path: r2
    direction: bidirectional`,
			wantErr: false,
		},
		{
			// Different types make dispatch split, so the same path is fine. Guards against over-rejecting.
			name: "same path on different provider types is fine",
			repos: `
  - name: a
    source: cc
    target: gl_a
    source_path: same
    target_path: same
    direction: bidirectional`,
			wantErr: false,
		},
		{
			// A real gitlab↔gitlab setup. The type is the same but the paths differ, so it is valid.
			name: "same type, different paths is fine",
			repos: `
  - name: a
    source: gl_a
    target: gl_b
    source_path: backup/x
    target_path: test/x
    direction: bidirectional`,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, providers+"repos:"+tc.repos+"\n"))
			if tc.wantErr && err == nil {
				t.Fatal("expected a colliding dispatch endpoint to be rejected; the later entry would silently never run")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("valid config rejected: %v", err)
			}
		})
	}
}

// --- HostResolver ---

// TestConfig_HostResolver pins down the host index built out of the base_url values.
// That index is the only thing the webhook has to go on when identifying an instance.
func TestConfig_HostResolver(t *testing.T) {
	cfg := &Config{Providers: map[string]ProviderConfig{
		"gitlab-main": {Type: "gitlab", BaseURL: "http://gitlab.example.com"},
		"gitlab-old":  {Type: "gitlab", BaseURL: "http://gitlab-old.example.com/"},
		// A provider with no base_url is not indexed.
		"codecommit-eu": {Type: "codecommit", Region: "ap-northeast-2"},
		"github-main":   {Type: "github"},
	}}

	got := cfg.HostResolver()
	want := HostResolver{
		"gitlab.example.com":     "gitlab-main",
		"gitlab-old.example.com": "gitlab-old",
	}
	if len(got) != len(want) {
		t.Fatalf("index = %v, want %v", got, want)
	}
	for host, name := range want {
		if got[host] != name {
			t.Errorf("index[%q] = %q, want %q", host, got[host], name)
		}
	}
}

// TestConfig_HostResolver_AmbiguousHostIsDropped — when two providers claim the same
// host, the host does not separate them either. Picking one of them at random mirrors
// silently in the wrong direction, so the host is dropped from the index entirely and
// left to fall through to type matching.
func TestConfig_HostResolver_AmbiguousHostIsDropped(t *testing.T) {
	cfg := &Config{Providers: map[string]ProviderConfig{
		"gl_a": {Type: "gitlab", BaseURL: "http://same.example.com"},
		"gl_b": {Type: "gitlab", BaseURL: "http://same.example.com/subpath"},
		"gl_c": {Type: "gitlab", BaseURL: "http://other.example.com"},
	}}

	got := cfg.HostResolver()
	if name, ok := got["same.example.com"]; ok {
		t.Errorf("ambiguous host resolved to %q; it must be dropped so dispatch falls back to type matching", name)
	}
	if got["other.example.com"] != "gl_c" {
		t.Errorf("unambiguous host was lost: %v", got)
	}
}

// TestHostResolver_Resolve pins down how the payload-side input is normalized.
func TestHostResolver_Resolve(t *testing.T) {
	r := HostResolver{
		"gitlab.example.com":      "gitlab-main",
		"gitlab.example.com:8443": "gitlab-alt",
	}
	tests := []struct {
		name, rawURL, want string
	}{
		{"plain", "http://gitlab.example.com/group/repo", "gitlab-main"},
		{"case folded", "HTTP://GitLab.Example.COM/group/repo", "gitlab-main"},
		{"port is part of the identity", "https://gitlab.example.com:8443/group/repo", "gitlab-alt"},
		{"scheme-less host", "gitlab.example.com/group/repo", "gitlab-main"},
		{"empty", "", ""},
		{"whitespace", "   ", ""},
		{"unknown host", "https://elsewhere.example.com/group/repo", ""},
		{"path only", "/group/repo", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.Resolve(tc.rawURL); got != tc.want {
				t.Errorf("Resolve(%q) = %q, want %q", tc.rawURL, got, tc.want)
			}
		})
	}
}

// TestHostResolver_NilIsSafe — with a nil index it must narrow nothing and fall back quietly.
func TestHostResolver_NilIsSafe(t *testing.T) {
	var r HostResolver
	if got := r.Resolve("http://gitlab.example.com/group/repo"); got != "" {
		t.Errorf("nil resolver returned %q, want an empty string", got)
	}
}

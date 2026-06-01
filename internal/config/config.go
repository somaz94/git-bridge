package config

import (
	"fmt"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// 미러 방향 값 상수 — repo의 sync 방향(Direction)과 retry 방향 비교에 쓰인다.
// config가 스키마의 소유자이므로 여기서 정의하고 다른 패키지가 재사용한다.
const (
	DirectionSourceToTarget = "source-to-target"
	DirectionTargetToSource = "target-to-source"
	DirectionBidirectional  = "bidirectional"
)

type Config struct {
	Server       ServerConfig              `yaml:"server"`
	Mirror       MirrorConfig              `yaml:"mirror"`
	Providers    map[string]ProviderConfig `yaml:"providers"`
	Repos        []RepoConfig              `yaml:"repos"`
	Consumer     ConsumerConfig            `yaml:"consumer"`  // legacy single consumer (backward compat)
	Consumers    []ConsumerConfig          `yaml:"consumers"` // multiple consumers
	Webhook      WebhookConfig             `yaml:"webhook"`
	Retry        RetryConfig               `yaml:"retry"`
	Notification NotificationConfig        `yaml:"notification"`
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type MirrorConfig struct {
	TimeoutSeconds int `yaml:"timeout_seconds"` // git operation timeout (default: 300)
}

type ProviderConfig struct {
	Type        string            `yaml:"type"` // codecommit, gitlab, github, bitbucket
	BaseURL     string            `yaml:"base_url,omitempty"`
	Region      string            `yaml:"region,omitempty"`
	Credentials map[string]string `yaml:"credentials"`
}

type RepoConfig struct {
	Name            string        `yaml:"name"`
	Source          string        `yaml:"source"`                  // provider name
	Target          string        `yaml:"target"`                  // provider name
	SourcePath      string        `yaml:"source_path"`             // repo path on source
	TargetPath      string        `yaml:"target_path"`             // repo path on target
	Direction       string        `yaml:"direction"`               // source-to-target, target-to-source, bidirectional
	RetryDirection  string        `yaml:"retry_direction"`         // override for retry "auto" on bidirectional repos: source-to-target / target-to-source. empty = built-in fallback (target-to-source)
	RefOverrides    []RefOverride `yaml:"ref_overrides,omitempty"` // ref별 단방향 고정(빈 값 = repo Direction 그대로)
	SlackWebhookURL string        `yaml:"slack_webhook_url"`       // per-repo override; empty falls back to notification.slack.webhook_url
}

// RefOverride는 특정 ref 패턴을 단일 방향(from→to provider)으로 고정한다.
// repo는 bidirectional을 유지하되, 매칭된 ref는 from→to 방향으로만 미러되고
// 반대 방향 이벤트는 조용히 skip된다. 양방향 미러에서 특정 브랜치의 권위(authoritative)
// 측이 한쪽으로 분명할 때, 반대편의 묵은 push나 오삭제가 권위 측을 덮어쓰는 사고를
// 구조적으로 차단한다.
// from/to는 provider 맵 키를 그대로 쓴다(source/target 라벨 의존을 피해 방향 역전 함정 제거).
type RefOverride struct {
	Pattern string `yaml:"pattern"` // ref 짧은 이름 glob(path.Match): "release", "release-*". 주의: glob '*'는 '/'를 넘지 못함("release/*"는 "release/x"만, "release/x/y"는 미매칭)
	From    string `yaml:"from"`    // 허용 방향의 출발 provider 이름
	To      string `yaml:"to"`      // 허용 방향의 도착 provider 이름
}

// MatchRefOverride는 ref 짧은 이름(refName)에 매칭되는 첫 RefOverride를 반환한다(없으면 nil).
// 패턴은 path.Match glob이며 설정 문서 순서상 first-match 규칙을 따른다.
func (r RepoConfig) MatchRefOverride(refName string) *RefOverride {
	for i := range r.RefOverrides {
		if ok, _ := path.Match(r.RefOverrides[i].Pattern, refName); ok {
			return &r.RefOverrides[i]
		}
	}
	return nil
}

type ConsumerConfig struct {
	Name        string `yaml:"name"` // consumer name (for logging)
	Type        string `yaml:"type"` // sqs
	QueueURL    string `yaml:"queue_url"`
	Region      string `yaml:"region"`
	Credentials struct {
		AccessKey string `yaml:"access_key"`
		SecretKey string `yaml:"secret_key"`
	} `yaml:"credentials"`
}

type WebhookConfig struct {
	GitLabSecret string `yaml:"gitlab_secret"` // X-Gitlab-Token verification
	GitHubSecret string `yaml:"github_secret"` // GitHub webhook secret
}

// RetryConfig holds settings for the manual retry HTTP endpoint.
// Empty APIToken disables the endpoint (handler returns 404).
type RetryConfig struct {
	APIToken string `yaml:"api_token"` // Bearer token; empty disables /retry/mirror
}

type NotificationConfig struct {
	Slack SlackConfig `yaml:"slack"`
}

type SlackConfig struct {
	WebhookURL string `yaml:"webhook_url"`
	Channel    string `yaml:"channel,omitempty"`
}

// Load reads and parses the config file, expanding environment variables.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	// Expand ${ENV_VAR} in config
	expanded := os.Expand(string(data), func(key string) string {
		if val, ok := os.LookupEnv(key); ok {
			return val
		}
		return "${" + key + "}"
	})

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Mirror.TimeoutSeconds == 0 {
		cfg.Mirror.TimeoutSeconds = 300
	}

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func validate(cfg *Config) error {
	if len(cfg.Repos) == 0 {
		return fmt.Errorf("no repos configured")
	}
	repoNames := make(map[string]bool)
	for i, r := range cfg.Repos {
		if r.Name == "" {
			return fmt.Errorf("repo[%d]: name required", i)
		}
		if repoNames[r.Name] {
			return fmt.Errorf("repo[%d]: duplicate repo name %q", i, r.Name)
		}
		repoNames[r.Name] = true
		if r.Source == "" || r.Target == "" {
			return fmt.Errorf("repo[%d] %s: source and target required", i, r.Name)
		}
		if _, ok := cfg.Providers[r.Source]; !ok {
			return fmt.Errorf("repo[%d] %s: unknown source provider %q", i, r.Name, r.Source)
		}
		if _, ok := cfg.Providers[r.Target]; !ok {
			return fmt.Errorf("repo[%d] %s: unknown target provider %q", i, r.Name, r.Target)
		}
		if r.Source == r.Target {
			return fmt.Errorf("repo[%d] %s: source and target cannot be the same provider", i, r.Name)
		}
		dir := strings.ToLower(r.Direction)
		if dir != DirectionSourceToTarget && dir != DirectionTargetToSource && dir != DirectionBidirectional {
			return fmt.Errorf("repo[%d] %s: direction must be source-to-target, target-to-source, or bidirectional", i, r.Name)
		}
		// retry_direction is optional; only validated when set.
		if r.RetryDirection != "" {
			rd := strings.ToLower(r.RetryDirection)
			if rd != DirectionSourceToTarget && rd != DirectionTargetToSource {
				return fmt.Errorf("repo[%d] %s: retry_direction must be source-to-target or target-to-source (got %q)", i, r.Name, r.RetryDirection)
			}
			// On one-way repos retry_direction must match the repo's direction.
			if dir != DirectionBidirectional && rd != dir {
				return fmt.Errorf("repo[%d] %s: retry_direction %q conflicts with one-way direction %q", i, r.Name, r.RetryDirection, r.Direction)
			}
		}
		// ref_overrides 검증: 패턴 유효성 + from/to가 이 repo의 두 provider여야 하고
		// repo Direction이 from→to 방향을 허용해야 한다(one-way repo와의 모순 방지).
		seenPatterns := make(map[string]bool)
		for j, ov := range r.RefOverrides {
			if ov.Pattern == "" {
				return fmt.Errorf("repo[%d] %s: ref_overrides[%d]: pattern required", i, r.Name, j)
			}
			// 중복 패턴은 first-match 규칙상 뒤 항목이 죽은 설정이 되므로 거부한다.
			if seenPatterns[ov.Pattern] {
				return fmt.Errorf("repo[%d] %s: ref_overrides[%d]: duplicate pattern %q", i, r.Name, j, ov.Pattern)
			}
			seenPatterns[ov.Pattern] = true
			if _, err := path.Match(ov.Pattern, "x"); err != nil {
				return fmt.Errorf("repo[%d] %s: ref_overrides[%d]: invalid pattern %q: %w", i, r.Name, j, ov.Pattern, err)
			}
			if ov.From == "" || ov.To == "" {
				return fmt.Errorf("repo[%d] %s: ref_overrides[%d]: from and to required", i, r.Name, j)
			}
			if ov.From == ov.To {
				return fmt.Errorf("repo[%d] %s: ref_overrides[%d]: from and to cannot be the same provider", i, r.Name, j)
			}
			isSrcToTgt := ov.From == r.Source && ov.To == r.Target
			isTgtToSrc := ov.From == r.Target && ov.To == r.Source
			if !isSrcToTgt && !isTgtToSrc {
				return fmt.Errorf("repo[%d] %s: ref_overrides[%d]: from/to must be this repo's source(%q) and target(%q)", i, r.Name, j, r.Source, r.Target)
			}
			if dir == DirectionSourceToTarget && !isSrcToTgt {
				return fmt.Errorf("repo[%d] %s: ref_overrides[%d]: direction %q does not allow %s→%s", i, r.Name, j, r.Direction, ov.From, ov.To)
			}
			if dir == DirectionTargetToSource && !isTgtToSrc {
				return fmt.Errorf("repo[%d] %s: ref_overrides[%d]: direction %q does not allow %s→%s", i, r.Name, j, r.Direction, ov.From, ov.To)
			}
		}
	}
	// Merge legacy single consumer into consumers list (backward compat)
	if cfg.Consumer.QueueURL != "" {
		if cfg.Consumer.Type == "" {
			cfg.Consumer.Type = "sqs"
		}
		if cfg.Consumer.Name == "" {
			cfg.Consumer.Name = "default"
		}
		cfg.Consumers = append(cfg.Consumers, cfg.Consumer)
	}

	// Validate consumers
	names := make(map[string]bool)
	for i, c := range cfg.Consumers {
		if c.QueueURL == "" {
			return fmt.Errorf("consumers[%d]: queue_url required", i)
		}
		if c.Type == "" {
			cfg.Consumers[i].Type = "sqs"
		}
		if c.Name == "" {
			cfg.Consumers[i].Name = fmt.Sprintf("sqs-%d", i)
		}
		if names[cfg.Consumers[i].Name] {
			return fmt.Errorf("consumers[%d]: duplicate name %q", i, cfg.Consumers[i].Name)
		}
		names[cfg.Consumers[i].Name] = true
	}

	return nil
}

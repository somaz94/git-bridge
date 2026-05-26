package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
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
	Name            string `yaml:"name"`
	Source          string `yaml:"source"`            // provider name
	Target          string `yaml:"target"`            // provider name
	SourcePath      string `yaml:"source_path"`       // repo path on source
	TargetPath      string `yaml:"target_path"`       // repo path on target
	Direction       string `yaml:"direction"`         // source-to-target, target-to-source, bidirectional
	RetryDirection  string `yaml:"retry_direction"`   // override for retry "auto" on bidirectional repos: source-to-target / target-to-source. empty = built-in fallback (target-to-source)
	SlackWebhookURL string `yaml:"slack_webhook_url"` // per-repo override; empty falls back to notification.slack.webhook_url
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
		if dir != "source-to-target" && dir != "target-to-source" && dir != "bidirectional" {
			return fmt.Errorf("repo[%d] %s: direction must be source-to-target, target-to-source, or bidirectional", i, r.Name)
		}
		// retry_direction is optional; only validated when set.
		if r.RetryDirection != "" {
			rd := strings.ToLower(r.RetryDirection)
			if rd != "source-to-target" && rd != "target-to-source" {
				return fmt.Errorf("repo[%d] %s: retry_direction must be source-to-target or target-to-source (got %q)", i, r.Name, r.RetryDirection)
			}
			// On one-way repos retry_direction must match the repo's direction.
			if dir != "bidirectional" && rd != dir {
				return fmt.Errorf("repo[%d] %s: retry_direction %q conflicts with one-way direction %q", i, r.Name, r.RetryDirection, r.Direction)
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

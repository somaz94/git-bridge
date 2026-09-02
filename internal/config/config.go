package config

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// Mirror direction values — used to compare a repo's sync Direction against a
// retry direction. config owns the schema, so they are defined here and every
// other package reuses them.
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
	// ConsolePort is the separate listener that serves only the human console.
	// The public HTTPRoute forwards to Port alone, so this port is unreachable
	// from outside the cluster and only the reverse-proxy portal proxies to it.
	// The two ports use different muxes: the console handlers simply are not
	// registered on the public one, and that is the whole guard (see
	// server/console.go).
	ConsolePort int `yaml:"console_port"`
	// APIDocsURL is where the console's "API docs" link points.
	//
	// It has to be an absolute URL rather than a relative path: the docs are
	// served on Port, the console on ConsolePort, and the portal proxies only
	// the latter — so a relative link would resolve against the console
	// listener and 404. Empty hides the link rather than rendering a dead one.
	APIDocsURL string `yaml:"api_docs_url"`
}

type MirrorConfig struct {
	TimeoutSeconds int `yaml:"timeout_seconds"` // git operation timeout (default: 300)
	// DrainTimeoutSeconds is how long shutdown waits for in-flight syncs before
	// killing them (default: 120).
	//
	// It belongs in config rather than a constant because the right value is a
	// property of the repositories being mirrored, not of the program: a
	// deployment carrying a multi-gigabyte repo needs a longer window than one
	// mirroring small ones. Size it from observed sync durations, and keep the
	// pod's terminationGracePeriodSeconds above it — the kubelet SIGKILLs at
	// that deadline regardless of what this says.
	DrainTimeoutSeconds int `yaml:"drain_timeout_seconds"`
}

type ProviderConfig struct {
	Type        string            `yaml:"type"` // codecommit, gitlab, github, bitbucket
	BaseURL     string            `yaml:"base_url,omitempty"`
	Region      string            `yaml:"region,omitempty"`
	Credentials map[string]string `yaml:"credentials"`
}

// HostResolver indexes base_url hosts to provider names.
//
// A webhook route on its own only tells you the type ("gitlab"). With two instances
// of the same type carrying the same repository path, the type alone does not say
// which one the event came from. Looking the host of the instance URL the payload
// carries up in this index narrows it the rest of the way, down to the provider name.
type HostResolver map[string]string

// Resolve pulls the host out of an instance URL (the webhook payload's
// project.web_url, for example) and returns the provider name. An empty string means
// it could not be narrowed down, and the caller has to fall back to the existing type
// matching there — not narrowing is safer than narrowing to the wrong one.
func (r HostResolver) Resolve(rawURL string) string {
	host := normalizeHost(rawURL)
	if host == "" {
		return ""
	}
	return r[host]
}

// HostResolver builds the index out of the providers' base_url values.
//
// A provider with no base_url (codecommit and the like) is not indexed. When more
// than one claims the same host, that host is dropped from the index entirely — the
// host does not separate them either, so narrowing to one of the two at random
// mirrors silently in the wrong direction. A host that is not in the index falls
// through to type matching, which keeps the existing behavior.
func (c *Config) HostResolver() HostResolver {
	index := make(HostResolver)
	ambiguous := make(map[string]bool)
	for name, pcfg := range c.Providers {
		host := normalizeHost(pcfg.BaseURL)
		if host == "" {
			continue
		}
		if _, taken := index[host]; taken {
			ambiguous[host] = true
			continue
		}
		index[host] = name
	}
	for host := range ambiguous {
		delete(index, host)
	}
	return index
}

// normalizeHost pulls a comparable host out of a URL string.
//
// The point is that both sides — the base_url from the config and the web_url from
// the payload — go through the same function. Hosts are case-insensitive, so it folds
// to lower case, and it keeps the port: the same host on a different port can genuinely
// be a different instance. A value with no scheme ("gitlab.example.com") can also show
// up in the config, and then the string itself is taken as the host.
func normalizeHost(rawURL string) string {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	// No scheme: if a path is attached, only the first segment counts as the host.
	host, _, _ := strings.Cut(raw, "/")
	if strings.Contains(host, ":") && !strings.Contains(host, "]") {
		// Keep host:port as it is, but drop a value that is nothing but a leftover ":".
		if strings.HasPrefix(host, ":") {
			return ""
		}
	}
	return strings.ToLower(host)
}

type RepoConfig struct {
	Name            string        `yaml:"name"`
	Source          string        `yaml:"source"`                  // provider name
	Target          string        `yaml:"target"`                  // provider name
	SourcePath      string        `yaml:"source_path"`             // repo path on source
	TargetPath      string        `yaml:"target_path"`             // repo path on target
	Direction       string        `yaml:"direction"`               // source-to-target, target-to-source, bidirectional
	RetryDirection  string        `yaml:"retry_direction"`         // override for retry "auto" on bidirectional repos: source-to-target / target-to-source. empty = built-in fallback (target-to-source)
	RefOverrides    []RefOverride `yaml:"ref_overrides,omitempty"` // pins a ref to one direction (empty = the repo Direction as it is)
	SlackWebhookURL string        `yaml:"slack_webhook_url"`       // per-repo override; empty falls back to notification.slack.webhook_url
}

// RefOverride pins a particular ref pattern to a single direction (the from→to
// provider pair). The repo stays bidirectional, but a matching ref is mirrored only
// in the from→to direction and an event going the other way is silently skipped.
// When one side is clearly the authoritative one for a branch in a bidirectional
// mirror, this structurally blocks the accident where a stale push or a mistaken
// delete on the other side overwrites the authoritative side.
// from/to use the provider map keys directly, which avoids depending on the
// source/target labels and removes the direction-reversal trap that comes with them.
type RefOverride struct {
	Pattern string `yaml:"pattern"` // glob over the ref short name (path.Match): "release", "release-*". Careful: a glob '*' does not cross '/' ("release/*" matches "release/x" only, not "release/x/y")
	From    string `yaml:"from"`    // provider name the allowed direction starts from
	To      string `yaml:"to"`      // provider name the allowed direction arrives at
}

// MatchRefOverride returns the first RefOverride that matches the ref short name
// refName, or nil when none does. Patterns are path.Match globs and follow a
// first-match rule in the order they appear in the config document.
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
	// VisibilityTimeoutSeconds hides a received message from other consumers
	// while it is being handled. Messages are handled serially, so this must
	// cover a whole batch of mirror.timeout_seconds syncs — otherwise the
	// message reappears mid-sync, gets picked up again, blocks on the per-repo
	// mutex, and eventually lands in the DLQ. Defaults from mirror.timeout_seconds.
	VisibilityTimeoutSeconds int `yaml:"visibility_timeout_seconds"`
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
	if cfg.Server.ConsolePort == 0 {
		cfg.Server.ConsolePort = 8081
	}
	if cfg.Mirror.TimeoutSeconds == 0 {
		cfg.Mirror.TimeoutSeconds = 300
	}
	if cfg.Mirror.DrainTimeoutSeconds == 0 {
		cfg.Mirror.DrainTimeoutSeconds = 120
	}

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func validate(cfg *Config) error {
	// Sharing a port would put the console behind the public HTTPRoute, so
	// refuse to start at all. Separate listeners mean this also shows up as a
	// bind conflict, but that error does not say why it matters.
	if cfg.Server.ConsolePort == cfg.Server.Port {
		return fmt.Errorf("server: console_port must differ from port (both %d) — the console must not share the public listener", cfg.Server.Port)
	}
	if len(cfg.Repos) == 0 {
		return fmt.Errorf("no repos configured")
	}
	repoNames := make(map[string]bool)
	// (provider name, path) is the dispatch key. Remember which side claimed a key
	// first, and reject a second claimant when one shows up — see the comment below.
	seenEndpoints := make(map[string]string)
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
		// A dispatch key collision is rejected. This is the same reason a duplicate
		// ref_overrides pattern is rejected — under a first-match rule the second
		// claimant is dead config.
		//
		// SyncByTarget / SyncDeleteByTarget match on (provider, path) and return at
		// the first entry, and within a single entry they look at target before
		// source. So if some path is A's target and also B's source, B's source-side
		// webhook is swallowed by A forever, leaving neither an error nor a log line.
		//
		// The key is the provider name rather than the type because the webhook
		// narrows the payload's instance host all the way down to the provider name
		// (HostResolver), so two instances of the same type carrying the same path are
		// still told apart. The same provider with the same path, on the other hand,
		// is a real collision that the host cannot separate either, so it stays rejected.
		//
		// One remaining hole has to be understood — when identifying the host fails
		// and it falls back to type matching, first-match comes back to life, so with
		// two instances of the same type carrying the same path a later entry's event
		// can be swallowed by an earlier one. The webhook leaves a warning in that
		// case (consumer.dispatchPushEvent).
		//
		// The point at which this check can be relaxed further is when those two
		// functions fan out over everything instead of stopping at the first match.
		// Until then, blocking startup beats silently not running.
		for _, side := range []struct{ kind, provider, path string }{
			{"source", r.Source, r.SourcePath},
			{"target", r.Target, r.TargetPath},
		} {
			key := side.provider + "/" + side.path
			if owner, taken := seenEndpoints[key]; taken {
				return fmt.Errorf("repo[%d] %s: %s endpoint %q collides with %s — webhooks dispatch by provider, so only the first would ever run", i, r.Name, side.kind, key, owner)
			}
			seenEndpoints[key] = fmt.Sprintf("%s (%s)", r.Name, side.kind)
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
		// Validate ref_overrides: the pattern has to be valid, from/to have to be this
		// repo's two providers, and the repo Direction has to allow the from→to
		// direction (which keeps it from contradicting a one-way repo).
		seenPatterns := make(map[string]bool)
		for j, ov := range r.RefOverrides {
			if ov.Pattern == "" {
				return fmt.Errorf("repo[%d] %s: ref_overrides[%d]: pattern required", i, r.Name, j)
			}
			// A duplicate pattern is rejected: under first-match the later entry is dead config.
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

		// A batch is handled serially, so the visibility window has to cover the
		// whole batch, not one sync. Defaulting to the sync timeout alone would
		// still let the last message of a full batch reappear mid-sync.
		if cfg.Consumers[i].VisibilityTimeoutSeconds == 0 {
			cfg.Consumers[i].VisibilityTimeoutSeconds = cfg.Mirror.TimeoutSeconds
		}
		if cfg.Consumers[i].VisibilityTimeoutSeconds < cfg.Mirror.TimeoutSeconds {
			return fmt.Errorf(
				"consumers[%d] %s: visibility_timeout_seconds (%d) must be at least mirror.timeout_seconds (%d) — a shorter window makes the message visible again mid-sync, so it is redelivered, blocks on the per-repo mutex, and ends up in the DLQ",
				i, cfg.Consumers[i].Name, cfg.Consumers[i].VisibilityTimeoutSeconds, cfg.Mirror.TimeoutSeconds)
		}
		// SQS caps VisibilityTimeout at 12 hours.
		if cfg.Consumers[i].VisibilityTimeoutSeconds > 43200 {
			return fmt.Errorf("consumers[%d] %s: visibility_timeout_seconds (%d) exceeds the SQS maximum of 43200",
				i, cfg.Consumers[i].Name, cfg.Consumers[i].VisibilityTimeoutSeconds)
		}
	}

	return nil
}

# Advanced Configuration Guide

This guide covers various mirroring scenarios with complete configuration examples.

> **Naming Convention**: All environment variables follow the `<TYPE>_<NAME>_<FIELD>` pattern. See [naming-convention.md](naming-convention.md) for details.

<br/>

## Table of Contents

- [Provider Combinations](#provider-combinations)
- [Scenario 1: CodeCommit → GitLab (one-way)](#scenario-1-codecommit--gitlab-one-way)
- [Scenario 2: GitLab → CodeCommit (one-way)](#scenario-2-gitlab--codecommit-one-way)
- [Scenario 3: CodeCommit ↔ GitLab (bidirectional)](#scenario-3-codecommit--gitlab-bidirectional)
- [Scenario 4: GitHub → GitLab (one-way)](#scenario-4-github--gitlab-one-way)
- [Scenario 5: GitLab → GitHub (one-way)](#scenario-5-gitlab--github-one-way)
- [Scenario 6: CodeCommit → GitHub (one-way)](#scenario-6-codecommit--github-one-way)
- [Scenario 7: GitHub → CodeCommit (one-way)](#scenario-7-github--codecommit-one-way)
- [Scenario 8: GitHub ↔ GitLab (bidirectional)](#scenario-8-github--gitlab-bidirectional)
- [How Webhook Matching Works](#how-webhook-matching-works)
- [Other Config Keys](#other-config-keys)
- [Multi-Repo Configuration](#multi-repo-configuration)
- [Multi-Provider Configuration](#multi-provider-configuration)
- [Multi-SQS Consumer (Multi-AWS Environment)](#multi-sqs-consumer-multi-aws-environment)

<br/>

## Provider Combinations

| Source | Target | Direction | Event Trigger | Webhook Required | Status |
|--------|--------|-----------|--------------|-----------------|--------|
| CodeCommit | GitLab | `source-to-target` | SQS (EventBridge) | No | Tested |
| CodeCommit | GitLab | `target-to-source` | GitLab webhook | Yes (GitLab) | Tested |
| CodeCommit | GitLab | `bidirectional` | SQS + GitLab webhook | Yes (GitLab) | Tested |
| CodeCommit | GitHub | `source-to-target` | SQS (EventBridge) | No | Tested |
| CodeCommit | GitHub | `target-to-source` | GitHub webhook | Yes (GitHub) | Tested |
| CodeCommit | GitHub | `bidirectional` | SQS + GitHub webhook | Yes (GitHub) | Tested |
| GitLab | GitHub | `source-to-target` | GitLab webhook | Yes (GitLab) | Tested |
| GitLab | GitHub | `target-to-source` | GitHub webhook | Yes (GitHub) | Tested |
| GitLab | GitHub | `bidirectional` | GitLab + GitHub webhook | Yes (both) | Tested |

> **Key**: CodeCommit uses SQS (EventBridge) for source events. GitLab and GitHub use webhooks for both source and target events.

<br/>

## Scenario 1: CodeCommit → GitLab (one-way)

Push to CodeCommit automatically mirrors to GitLab via SQS.

<br/>

### Config

```yaml
providers:
  codecommit-eu:
    type: codecommit
    region: "${CODECOMMIT_EU_REGION}"
    credentials:
      git_username: "${CODECOMMIT_EU_GIT_USERNAME}"
      git_password: "${CODECOMMIT_EU_GIT_PASSWORD}"
  gitlab-main:
    type: gitlab
    base_url: "${GITLAB_MAIN_BASE_URL}"
    credentials:
      token: "${GITLAB_MAIN_TOKEN}"

repos:
  - name: my-repo
    source: codecommit-eu
    target: gitlab-main
    source_path: my-repo
    target_path: server/my-repo
    direction: source-to-target

consumers:
  - name: sqs-eu
    type: sqs
    queue_url: "${SQS_EU_QUEUE_URL}"
    region: "${SQS_EU_REGION}"
    credentials:
      access_key: "${SQS_EU_ACCESS_KEY}"
      secret_key: "${SQS_EU_SECRET_KEY}"
    # Optional. Defaults to mirror.timeout_seconds; must not be lower.
    visibility_timeout_seconds: 600
```

<br/>

### `visibility_timeout_seconds`

How long a received message stays hidden from other consumers while it is being
handled. It defaults to `mirror.timeout_seconds` and startup fails if it is set
lower — a shorter window is not a tuning choice, it is a correctness bug:

1. A sync that outlives the window makes its message visible again.
2. The message is redelivered while the original is still running.
3. The redelivered copy blocks on the per-repo mutex, then times out too.
4. After `maxReceiveCount` retries the message lands in the DLQ, even though the
   first sync may have succeeded.

A batch of up to 10 messages is handled **serially** and the window starts for
all of them at `ReceiveMessage`, so the last message in a full batch carries the
processing time of the nine before it. Raise this well above
`mirror.timeout_seconds` when a queue regularly delivers full batches of slow
repositories. SQS caps it at 43200 (12 hours).

<br/>

### Requirements

- SQS queue with EventBridge rule for CodeCommit `referenceUpdated` events
- IAM user with `codecommit:GitPull` permission
- GitLab token with `write_repository` scope
- **No webhook setup needed**

<br/>

## Scenario 2: GitLab → CodeCommit (one-way)

Push to GitLab triggers webhook, mirrors to CodeCommit.

<br/>

### Config

```yaml
repos:
  - name: my-repo
    source: codecommit-eu
    target: gitlab-main
    source_path: my-repo
    target_path: server/my-repo
    direction: target-to-source
```

<br/>

### Requirements

- IAM user with `codecommit:GitPull` and `codecommit:GitPush` permissions
- GitLab webhook configured on `server/my-repo` project
  - URL: `http://<git-bridge-host>/webhook/gitlab`
  - Trigger: Push events
  - See [gitlab-webhook-setup.md](gitlab-webhook-setup.md)

<br/>

## Scenario 3: CodeCommit ↔ GitLab (bidirectional)

Changes on either side are mirrored to the other.

<br/>

### Config

```yaml
repos:
  - name: my-repo
    source: codecommit-eu
    target: gitlab-main
    source_path: my-repo
    target_path: server/my-repo
    direction: bidirectional
```

<br/>

### Requirements

- SQS queue (for CodeCommit → GitLab direction)
- GitLab webhook (for GitLab → CodeCommit direction)
- IAM user with both `codecommit:GitPull` and `codecommit:GitPush` permissions

> **Loop Detection**: Bidirectional sync has built-in loop detection. When a sync pushes refs that are already up-to-date (no actual changes), the notification is skipped and the loop terminates naturally. For example: CodeCommit push → SQS → sync to GitLab → GitLab webhook → sync back to CodeCommit → no-op (already up-to-date) → no notification, loop ends.

<br/>

## Scenario 4: GitHub → GitLab (one-way)

Push to GitHub triggers webhook, mirrors to GitLab.

### Config

```yaml
providers:
  github-main:
    type: github
    credentials:
      token: "${GITHUB_MAIN_TOKEN}"
  gitlab-main:
    type: gitlab
    base_url: "${GITLAB_MAIN_BASE_URL}"
    credentials:
      token: "${GITLAB_MAIN_TOKEN}"

repos:
  - name: my-repo
    source: github-main
    target: gitlab-main
    source_path: org/my-repo
    target_path: team/my-repo
    direction: source-to-target
```

<br/>

### Requirements

- GitHub personal access token with `repo` scope
- GitLab token with `write_repository` scope
- GitHub webhook configured on `org/my-repo` repository
  - Payload URL: `http://<git-bridge-host>/webhook/github`
  - Events: Just the push event
  - See [github-webhook-setup.md](github-webhook-setup.md)
- **SQS is NOT needed** (GitHub uses webhook, not SQS)

### How It Works

The webhook handler receives a push event from GitHub and calls `SyncByTarget("github", "org/my-repo", meta)`. Inside `SyncByTarget`, it matches by **source provider + source path** and performs source-to-target sync (GitHub → GitLab).

<br/>

## Scenario 5: GitLab → GitHub (one-way)

Push to GitLab triggers webhook, mirrors to GitHub.

<br/>

### Config

```yaml
repos:
  - name: my-repo
    source: gitlab-main
    target: github-main
    source_path: team/my-repo
    target_path: org/my-repo
    direction: source-to-target
```

<br/>

### Requirements

- GitLab webhook configured on `team/my-repo` project
  - URL: `http://<git-bridge-host>/webhook/gitlab`
  - Trigger: Push events
- GitHub personal access token with `repo` scope

<br/>

### How It Works

The webhook handler receives a push event from GitLab and calls `SyncByTarget("gitlab", "team/my-repo", meta)`. It matches by **source provider + source path** and performs source-to-target sync (GitLab → GitHub).

<br/>

## Scenario 6: CodeCommit → GitHub (one-way)

Push to CodeCommit automatically mirrors to GitHub via SQS.

<br/>

### Config

```yaml
providers:
  codecommit-eu:
    type: codecommit
    region: "${CODECOMMIT_EU_REGION}"
    credentials:
      git_username: "${CODECOMMIT_EU_GIT_USERNAME}"
      git_password: "${CODECOMMIT_EU_GIT_PASSWORD}"
  github-main:
    type: github
    credentials:
      token: "${GITHUB_MAIN_TOKEN}"

repos:
  - name: my-repo
    source: codecommit-eu
    target: github-main
    source_path: my-repo
    target_path: org/my-repo
    direction: source-to-target
```

### Requirements

- SQS queue with EventBridge rule
- IAM user with `codecommit:GitPull` permission
- GitHub personal access token with `repo` scope
- **No webhook setup needed**

<br/>

## Scenario 7: GitHub → CodeCommit (one-way)

Push to GitHub triggers webhook, mirrors to CodeCommit.

<br/>

### Config

```yaml
repos:
  - name: my-repo
    source: codecommit-eu
    target: github-main
    source_path: my-repo
    target_path: org/my-repo
    direction: target-to-source
```

<br/>

### Requirements

- GitHub webhook configured on `org/my-repo` repository
  - Payload URL: `http://<git-bridge-host>/webhook/github`
  - Events: Just the push event
- IAM user with both `codecommit:GitPull` and `codecommit:GitPush` permissions

<br/>

### How It Works

The webhook handler receives a push event from GitHub and calls `SyncByTarget("github", "org/my-repo", meta)`. It matches by **target provider + target path** and performs target-to-source sync (GitHub → CodeCommit).

<br/>

## Scenario 8: GitHub ↔ GitLab (bidirectional)

Changes on either side are mirrored to the other.

<br/>

### Config

```yaml
repos:
  - name: my-repo
    source: github-main
    target: gitlab-main
    source_path: org/my-repo
    target_path: team/my-repo
    direction: bidirectional
```

<br/>

### Requirements

- GitHub webhook on `org/my-repo` (for GitHub → GitLab direction)
- GitLab webhook on `team/my-repo` (for GitLab → GitHub direction)
- Both webhooks must be configured

> **Note**: No SQS needed — both directions use webhooks.
>
> **Loop Detection**: Same as [Scenario 3](#scenario-3-codecommit--gitlab-bidirectional) — no-op pushes are detected and notifications are skipped, preventing redundant alerts.

<br/>

## How Webhook Matching Works

When a webhook event arrives, `SyncByTarget` walks the configured repos once and, for each repo, checks the target side before the source side:

1. **Target match**: Check if the incoming provider matches the repo's **target** provider and `target_path`. If matched and direction allows `target-to-source`, sync from target → source.

2. **Source match**: Check if the incoming provider matches the repo's **source** provider and `source_path`. If matched and direction allows `source-to-target`, sync from source → target.

Both checks happen inside the same loop iteration, so a repo whose target matches never has its source checked — the target side wins on that repo, not across the whole list. The first repo that matches either way returns, and if none matches the call errors with `no matching repo for provider=... path=...`.

This means any webhook event is automatically routed to the correct sync direction regardless of whether the provider is configured as source or target.

<br/>

### Example

```yaml
- name: web-app
  source: github-main
  target: gitlab-main
  source_path: org/web-app
  target_path: team/web-app
  direction: bidirectional
```

| Event | Webhook Call | Match | Sync Direction |
|-------|-------------|-------|---------------|
| Push to GitHub `org/web-app` | `SyncByTarget("github", "org/web-app", meta)` | Source match | GitHub → GitLab |
| Push to GitLab `team/web-app` | `SyncByTarget("gitlab", "team/web-app", meta)` | Target match | GitLab → GitHub |

<br/>

## Other Config Keys

The scenarios above only use `providers` / `repos` / `consumers`. These remaining
keys are what the rest of the behavior is tuned with. Fully commented versions of
all of them live in [examples/config.yaml](../examples/config.yaml) and
[examples/configmap.yaml](../examples/configmap.yaml).

<br/>

### `server`

```yaml
server:
  port: 8080
  console_port: 8081
  api_docs_url: "https://git-bridge.example.com/api-docs"
```

| Key | Default | Description |
|-----|---------|-------------|
| `port` | `8080` | The public listener. The HTTPRoute forwards here, and it serves health, the webhooks, the retry API, `/api-docs` and `/openapi.json` |
| `console_port` | `8081` | A separate listener that serves **only** the console. It must differ from `port`: the console handlers are simply not registered on the public mux, and that separation is the whole guard keeping the console off the internet. This value and the deployment's `containerPort` / Service port have to move together — changing one alone either closes the console or opens it publicly |
| `api_docs_url` | *(empty)* | Where the console's API-docs link points. It must be an **absolute** URL: the docs are served on `port` while the portal proxies only `console_port`, so a relative path would resolve against the console listener and 404. Empty hides the link instead of rendering a dead one |

<br/>

### `mirror`

```yaml
mirror:
  timeout_seconds: 600
  drain_timeout_seconds: 120
```

| Key | Default | Description |
|-----|---------|-------------|
| `timeout_seconds` | `300` | Budget for **one whole sync** (clone/fetch **plus** push share this single deadline — it is not applied per git command). On expiry the git child is SIGKILLed (`signal: killed`) and the sync is reported as failed. Raise it for large repos whose full clone approaches the limit. It is also the floor for `visibility_timeout_seconds` |
| `drain_timeout_seconds` | `120` | On SIGTERM the service stops accepting new work, then waits at most this long for syncs already in flight before killing them. It is a cap, not a delay — shutdown returns as soon as the work does. Keep the pod's `terminationGracePeriodSeconds` **above** it, or the kubelet SIGKILLs mid-drain and the wait buys nothing. A sync killed mid-fetch can leave a pack `.keep` marker behind, which excludes that packfile from every later repack until housekeeping prunes it |

<br/>

### Per-repo options

A webhook is dispatched by **provider name**, so `(name, path)` is the routing key and
`SyncByTarget` / `SyncDeleteByTarget` return on the first match. Validation therefore refuses to
start on any config where two sides resolve to the same `(name, path)` — whether that is two
different entries, or the two sides of a single entry — because the later one could never run.
Different providers may share a path freely, including two instances of the same type.

The route only carries the provider *type*, so the name comes from the payload: the handler
matches `project.web_url`'s host against the providers' `base_url` hosts. When that fails — no
`base_url`, no `web_url` in the payload, an unknown host, or two providers on the same host — it
falls back to matching by type and logs `dispatching by provider type`. Under that fallback
first-match applies again, so two same-type instances sharing a path can invert direction; see
`docs/gitlab-to-gitlab-mirror.md` constraint 1.

Beyond `name` / `source` / `target` / `source_path` / `target_path` / `direction`,
each entry in `repos` accepts:

```yaml
repos:
  - name: web-app
    source: codecommit-eu
    target: gitlab-main
    source_path: web-app
    target_path: frontend/web-app
    direction: bidirectional
    retry_direction: target-to-source
    ref_overrides:
      - { pattern: "release",   from: gitlab-main, to: codecommit-eu }
      - { pattern: "release-*", from: gitlab-main, to: codecommit-eu }
    slack_webhook_url: "${WEB_APP_SLACK_WEBHOOK_URL}"
```

| Key | Description |
|-----|-------------|
| `retry_direction` | Overrides how `"auto"` resolves for this repo on the retry API. Accepts `source-to-target` or `target-to-source`. Unset, `"auto"` falls back to `target-to-source` on a bidirectional repo. An explicit `direction` in the API call always beats this pin, and on a one-way repo it must match the repo's `direction` |
| `ref_overrides` | Pins specific refs to a single `from` → `to` provider direction while the repo stays bidirectional. `pattern` is a ref **short-name** glob (`path.Match`, so `*` does not cross `/`); `from` / `to` are provider map keys, not the `source`/`target` labels. Events in the opposite direction for a matched ref are silently skipped — the SQS message is still deleted, so no retries or DLQ churn. Matching is first-match in declaration order. Validation: `from` != `to`, both must be this repo's source and target, and duplicate patterns are rejected |
| `slack_webhook_url` | Routes every Slack notification for this repo (webhook, SQS, cron and retry triggered alike) to this URL instead of `notification.slack.webhook_url`. Useful for sending a test repo's traffic to a separate channel. Empty or unset falls back to the default |

> Push scoping is **not** controlled by `ref_overrides`. An event that names a ref
> narrows the push to that ref alone regardless of whether this repo declares
> overrides; only a ref-less trigger (a full sync, or the hourly reconcile cron)
> pushes broadly. See the `ref_overrides` section of the README for the full rule.

<br/>

## Multi-Repo Configuration

You can configure multiple repositories with different providers and directions in a single instance:

```yaml
providers:
  codecommit-eu:
    type: codecommit
    region: "${CODECOMMIT_EU_REGION}"
    credentials:
      git_username: "${CODECOMMIT_EU_GIT_USERNAME}"
      git_password: "${CODECOMMIT_EU_GIT_PASSWORD}"
  gitlab-main:
    type: gitlab
    base_url: "${GITLAB_MAIN_BASE_URL}"
    credentials:
      token: "${GITLAB_MAIN_TOKEN}"
  github-main:
    type: github
    credentials:
      token: "${GITHUB_MAIN_TOKEN}"

repos:
  # CodeCommit → GitLab (SQS auto-trigger)
  - name: backend-api
    source: codecommit-eu
    target: gitlab-main
    source_path: backend-api
    target_path: server/backend-api
    direction: source-to-target

  # CodeCommit ↔ GitLab (SQS + GitLab webhook)
  - name: shared-lib
    source: codecommit-eu
    target: gitlab-main
    source_path: shared-lib
    target_path: server/shared-lib
    direction: bidirectional

  # GitHub → GitLab (GitHub webhook)
  - name: open-source-tool
    source: github-main
    target: gitlab-main
    source_path: org/open-source-tool
    target_path: external/open-source-tool
    direction: source-to-target

  # GitLab → GitHub (GitLab webhook)
  - name: public-docs
    source: gitlab-main
    target: github-main
    source_path: team/public-docs
    target_path: org/public-docs
    direction: source-to-target

consumers:
  - name: sqs-eu
    type: sqs
    queue_url: "${SQS_EU_QUEUE_URL}"
    region: "${SQS_EU_REGION}"
    credentials:
      access_key: "${SQS_EU_ACCESS_KEY}"
      secret_key: "${SQS_EU_SECRET_KEY}"
    # Optional. Defaults to mirror.timeout_seconds; must not be lower.
    visibility_timeout_seconds: 600
```

<br/>

### Required Webhooks for This Setup

| Repo | Webhook On | Provider |
|------|-----------|----------|
| `backend-api` | None | SQS handles it |
| `shared-lib` | GitLab `server/shared-lib` | GitLab webhook |
| `open-source-tool` | GitHub `org/open-source-tool` | GitHub webhook |
| `public-docs` | GitLab `team/public-docs` | GitLab webhook |

<br/>

### Required Environment Variables

```
# Provider — pattern: <TYPE>_<NAME>_<FIELD>
# CodeCommit
CODECOMMIT_EU_REGION, CODECOMMIT_EU_GIT_USERNAME, CODECOMMIT_EU_GIT_PASSWORD

# GitLab
GITLAB_MAIN_BASE_URL, GITLAB_MAIN_TOKEN

# GitHub
GITHUB_MAIN_TOKEN

# SQS Consumer — pattern: SQS_<NAME>_QUEUE_URL, SQS_<NAME>_REGION, SQS_<NAME>_ACCESS_KEY, SQS_<NAME>_SECRET_KEY
SQS_EU_QUEUE_URL, SQS_EU_REGION, SQS_EU_ACCESS_KEY, SQS_EU_SECRET_KEY

# Webhook Secrets (optional)
WEBHOOK_GITLAB_SECRET, WEBHOOK_GITHUB_SECRET

# Retry API — effectively required. Empty disables POST /retry/mirror entirely
# (404), and the reconcile CronJob drives that endpoint with this token.
RETRY_API_TOKEN

# Notifications (optional)
SLACK_WEBHOOK_URL

# Per-repo Slack channel override (optional) — referenced as ${...} from
# repos[].slack_webhook_url, e.g. DEMO_REPO_SLACK_WEBHOOK_URL. Empty or unset
# falls back to SLACK_WEBHOOK_URL.
<REPO>_SLACK_WEBHOOK_URL
```

> Only the provider and consumer variables follow the `<TYPE>_<NAME>_<FIELD>`
> pattern. The webhook secrets, `RETRY_API_TOKEN`, `SLACK_WEBHOOK_URL` and the
> per-repo override sit outside it — see
> [naming-convention.md](naming-convention.md).

<br/>

## Multi-Provider Configuration

When you have repositories across multiple AWS regions/accounts or multiple GitLab/GitHub instances, configure multiple providers of the same type with different names.

> See [naming-convention.md](naming-convention.md) for the full naming convention guide.

### Config

```yaml
providers:
  codecommit-eu:
    type: codecommit
    region: "${CODECOMMIT_EU_REGION}"
    credentials:
      git_username: "${CODECOMMIT_EU_GIT_USERNAME}"
      git_password: "${CODECOMMIT_EU_GIT_PASSWORD}"

  codecommit-us:
    type: codecommit
    region: "${CODECOMMIT_US_REGION}"
    credentials:
      git_username: "${CODECOMMIT_US_GIT_USERNAME}"
      git_password: "${CODECOMMIT_US_GIT_PASSWORD}"

  gitlab-main:
    type: gitlab
    base_url: "${GITLAB_MAIN_BASE_URL}"
    credentials:
      token: "${GITLAB_MAIN_TOKEN}"

  gitlab-secondary:
    type: gitlab
    base_url: "${GITLAB_SECONDARY_BASE_URL}"
    credentials:
      token: "${GITLAB_SECONDARY_TOKEN}"

  github-main:
    type: github
    credentials:
      token: "${GITHUB_MAIN_TOKEN}"

repos:
  # EU CodeCommit → main GitLab
  - name: eu-service
    source: codecommit-eu
    target: gitlab-main
    source_path: eu-service
    target_path: server/eu-service
    direction: source-to-target

  # US CodeCommit → main GitLab
  - name: us-service
    source: codecommit-us
    target: gitlab-main
    source_path: us-service
    target_path: server/us-service
    direction: source-to-target

  # main GitLab → secondary GitLab
  - name: shared-config
    source: gitlab-main
    target: gitlab-secondary
    source_path: devops/shared-config
    target_path: infra/shared-config
    direction: source-to-target
```

Since `providers` is a map, the provider name (map key) is used in `repos.source` / `repos.target` — **no Go code changes needed** to support multi-provider.

<br/>

## Multi-SQS Consumer (Multi-AWS Environment)

When you have CodeCommit repositories in multiple AWS regions or accounts, configure multiple SQS consumers. Each consumer polls its own SQS queue with independent AWS credentials.

<br/>

### Env Var Pattern

```
SQS_<NAME>_QUEUE_URL
SQS_<NAME>_REGION
SQS_<NAME>_ACCESS_KEY
SQS_<NAME>_SECRET_KEY
```

To add a new environment, follow the pattern with a new name (e.g. `EU`, `US`, `AP`).

<br/>

### Config

```yaml
consumers:
  - name: sqs-eu
    type: sqs
    queue_url: "${SQS_EU_QUEUE_URL}"
    region: "${SQS_EU_REGION}"
    credentials:
      access_key: "${SQS_EU_ACCESS_KEY}"
      secret_key: "${SQS_EU_SECRET_KEY}"

  - name: sqs-us
    type: sqs
    queue_url: "${SQS_US_QUEUE_URL}"
    region: "${SQS_US_REGION}"
    credentials:
      access_key: "${SQS_US_ACCESS_KEY}"
      secret_key: "${SQS_US_SECRET_KEY}"
```

Each consumer runs as a separate goroutine, independently polling its SQS queue.

> **Backward Compatible**: The legacy single `consumer:` key still works. It is automatically merged into the `consumers` list with the name `default`.

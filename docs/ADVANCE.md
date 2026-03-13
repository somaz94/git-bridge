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
```

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

The webhook handler receives a push event from GitHub and calls `SyncByTarget("github", "org/my-repo")`. Inside `SyncByTarget`, it matches by **source provider + source path** and performs source-to-target sync (GitHub → GitLab).

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

The webhook handler receives a push event from GitLab and calls `SyncByTarget("gitlab", "team/my-repo")`. It matches by **source provider + source path** and performs source-to-target sync (GitLab → GitHub).

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

The webhook handler receives a push event from GitHub and calls `SyncByTarget("github", "org/my-repo")`. It matches by **target provider + target path** and performs target-to-source sync (GitHub → CodeCommit).

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

When a webhook event arrives, `SyncByTarget` uses a two-pass matching strategy:

1. **Target match**: Check if the incoming provider matches the repo's **target** provider and `target_path`. If matched and direction allows `target-to-source`, sync from target → source.

2. **Source match**: Check if the incoming provider matches the repo's **source** provider and `source_path`. If matched and direction allows `source-to-target`, sync from source → target.

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
| Push to GitHub `org/web-app` | `SyncByTarget("github", "org/web-app")` | Source match | GitHub → GitLab |
| Push to GitLab `team/web-app` | `SyncByTarget("gitlab", "team/web-app")` | Target match | GitLab → GitHub |

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

# Notifications (optional)
SLACK_WEBHOOK_URL
```

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

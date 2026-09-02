# Naming Convention

git-bridge's **provider and consumer** environment variables follow a consistent naming pattern, so that multi-provider and multi-consumer setups can be described without touching Go code.

The pattern covers exactly the variables that are per-instance. Service-wide settings and per-repo overrides are outside it — see [Variables outside the pattern](#variables-outside-the-pattern).

<br/>

## Pattern

```
<TYPE>_<NAME>_<FIELD>
```

| Component | Description | Example |
|-----------|-------------|---------|
| `TYPE` | Resource type | `CODECOMMIT`, `GITLAB`, `GITHUB`, `SQS` |
| `NAME` | Instance identifier (user-defined) | `EU`, `MAIN`, `SECONDARY` |
| `FIELD` | Field name | `REGION`, `TOKEN`, `QUEUE_URL` |

<br/>

## Recommended `<NAME>` by Service Type

`<NAME>` is a **free-form identifier** — you can use any name that makes sense for your setup.

| Service Type | Recommended Style | Examples | Reason |
|-------------|-------------------|---------|--------|
| **AWS services** (CodeCommit, SQS) | Geographic | `EU`, `US`, `AP` | Region-based services |
| **Platform services** (GitLab, GitHub) | Descriptive | `MAIN`, `SECONDARY` | Instance-based services |

<br/>

## Full Reference

The reference below is the **complete set of variables that follow the pattern**.
Everything else the service reads is listed in
[Variables outside the pattern](#variables-outside-the-pattern).

<br/>

### Provider Environment Variables

```
# CodeCommit — geographic naming (AWS region-based)
CODECOMMIT_<NAME>_REGION
CODECOMMIT_<NAME>_GIT_USERNAME
CODECOMMIT_<NAME>_GIT_PASSWORD

# GitLab — descriptive naming (instance-based)
GITLAB_<NAME>_BASE_URL
GITLAB_<NAME>_TOKEN

# GitHub — descriptive naming (instance-based)
GITHUB_<NAME>_TOKEN
```

<br/>

### Consumer Environment Variables

```
# SQS — geographic naming (AWS region-based)
SQS_<NAME>_QUEUE_URL
SQS_<NAME>_REGION
SQS_<NAME>_ACCESS_KEY
SQS_<NAME>_SECRET_KEY
```

<br/>

### Config Provider Names

The provider name in config (map key) matches the env var `<TYPE>-<NAME>` pattern (lowercase, hyphenated):

```yaml
providers:
  codecommit-eu:       # → CODECOMMIT_EU_*
    type: codecommit
    ...
  gitlab-main:         # → GITLAB_MAIN_*
    type: gitlab
    ...
  github-main:         # → GITHUB_MAIN_*
    type: github
    ...
```

<br/>

## Variables Outside the Pattern

Not every variable is per-instance, so not every variable has a `<TYPE>_<NAME>_<FIELD>`
shape. These are deliberately outside it — there is one of each per service, so
there is no `<NAME>` to put in the middle:

| Variable | Config key | Note |
|----------|-----------|------|
| `WEBHOOK_GITLAB_SECRET` | `webhook.gitlab_secret` | One secret per service, not per GitLab provider |
| `WEBHOOK_GITHUB_SECRET` | `webhook.github_secret` | Same, for the HMAC secret |
| `RETRY_API_TOKEN` | `retry.api_token` | Bearer token for `POST /retry/mirror`; empty disables the endpoint (404) |
| `SLACK_WEBHOOK_URL` | `notification.slack.webhook_url` | The default notification channel |
| `CONFIG_PATH` | — | Read directly from the environment, not from the config file |
| `WORK_DIR` | — | Same |

One variable is per-**repo** rather than per-provider, and so uses the repo name
as its prefix instead:

| Variable | Config key | Note |
|----------|-----------|------|
| `<REPO>_SLACK_WEBHOOK_URL` | `repos[].slack_webhook_url` | Per-repo channel override, referenced as `${...}` (e.g. `DEMO_REPO_SLACK_WEBHOOK_URL` for the `demo-repo` repo). Empty or unset falls back to `SLACK_WEBHOOK_URL` |

The `<REPO>` prefix is a convention, not something the code parses — the name is
resolved only through the `${...}` reference in the config, so any variable name
works. Matching it to the repo name is what keeps the Secret readable.

<br/>

## Examples

<br/>

### Single Environment (Default)

```
# Provider
CODECOMMIT_EU_REGION=eu-central-1
CODECOMMIT_EU_GIT_USERNAME=...
CODECOMMIT_EU_GIT_PASSWORD=...
GITLAB_MAIN_BASE_URL=http://gitlab.example.com
GITLAB_MAIN_TOKEN=glpat-...
GITHUB_MAIN_TOKEN=ghp_...

# Consumer
SQS_EU_QUEUE_URL=https://sqs.eu-central-1.amazonaws.com/...
SQS_EU_REGION=eu-central-1
SQS_EU_ACCESS_KEY=AKIA...
SQS_EU_SECRET_KEY=...
```

<br/>

### Multi-Region / Multi-Instance

```
# CodeCommit — two AWS regions
CODECOMMIT_EU_REGION=eu-central-1
CODECOMMIT_EU_GIT_USERNAME=...
CODECOMMIT_EU_GIT_PASSWORD=...
CODECOMMIT_US_REGION=us-east-1
CODECOMMIT_US_GIT_USERNAME=...
CODECOMMIT_US_GIT_PASSWORD=...

# GitLab — two instances
GITLAB_MAIN_BASE_URL=http://gitlab.example.com
GITLAB_MAIN_TOKEN=glpat-...
GITLAB_SECONDARY_BASE_URL=http://gitlab-staging.example.com
GITLAB_SECONDARY_TOKEN=glpat-...

# GitHub — two accounts
GITHUB_MAIN_TOKEN=ghp_...
GITHUB_SECONDARY_TOKEN=ghp_...

# SQS — two regions
SQS_EU_QUEUE_URL=...
SQS_EU_REGION=eu-central-1
SQS_EU_ACCESS_KEY=...
SQS_EU_SECRET_KEY=...
SQS_US_QUEUE_URL=...
SQS_US_REGION=us-east-1
SQS_US_ACCESS_KEY=...
SQS_US_SECRET_KEY=...
```

<br/>

## Custom Naming

`<NAME>` is not restricted to the examples above. You can use any identifier:

```
# By purpose
GITLAB_PROD_BASE_URL=...
GITLAB_STAGING_BASE_URL=...

# By team
GITHUB_FRONTEND_TOKEN=...
GITHUB_BACKEND_TOKEN=...

# By environment
CODECOMMIT_DEV_REGION=...
CODECOMMIT_PROD_REGION=...
```

The only rule is: **use the same `<NAME>` consistently** across the env var, K8s Secret, Deployment, and ConfigMap for the same provider instance.

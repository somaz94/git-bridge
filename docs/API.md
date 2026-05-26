# API Reference

<br/>

## Health Check

<br/>

### GET /health

Liveness probe endpoint.

#### Response

```json
{
  "status": "ok",
  "service": "git-bridge"
}
```

| Status Code | Description |
|-------------|-------------|
| 200 | Service is running |

<br/>

### GET /ready

Readiness probe endpoint. Same behavior as `/health`.

#### Response

```json
{
  "status": "ok",
  "service": "git-bridge"
}
```

| Status Code | Description |
|-------------|-------------|
| 200 | Service is ready to accept requests |

<br/>

## Webhooks

<br/>

### POST /webhook/gitlab

Receives push events from GitLab. Triggers mirror sync for the matching repository.

#### Headers

| Header | Required | Description |
|--------|----------|-------------|
| `X-Gitlab-Token` | No* | Secret token for verification |

> \* Required only when `WEBHOOK_GITLAB_SECRET` is configured

#### Request Body

GitLab push event payload (sent automatically by GitLab):

```json
{
  "event_name": "push",
  "user_name": "somaz",
  "ref": "refs/heads/main",
  "repository": {
    "name": "my-repo"
  },
  "project": {
    "path_with_namespace": "team/my-repo"
  }
}
```

| Field | Description |
|-------|-------------|
| `project.path_with_namespace` | Used to match against `target_path` or `source_path` in repo config |
| `ref` | Branch or tag reference — included in Slack notification |
| `user_name` | The person who pushed — logged for debugging (Slack shows commit author instead) |

#### Response

```json
{
  "status": "accepted"
}
```

| Status Code | Description |
|-------------|-------------|
| 200 | Event accepted, mirror sync started in background |
| 400 | Invalid request body |
| 401 | Invalid or missing `X-Gitlab-Token` |
| 405 | Method not allowed (only POST) |

<br/>

### POST /webhook/github

Receives push events from GitHub. Triggers mirror sync for the matching repository.

#### Headers

| Header | Required | Description |
|--------|----------|-------------|
| `X-Hub-Signature-256` | No* | HMAC-SHA256 signature for verification |

> \* Required only when `WEBHOOK_GITHUB_SECRET` is configured

#### Request Body

GitHub push event payload (sent automatically by GitHub):

```json
{
  "ref": "refs/heads/main",
  "pusher": {
    "name": "somaz"
  },
  "sender": {
    "login": "somaz"
  },
  "repository": {
    "name": "my-repo",
    "full_name": "org/my-repo"
  }
}
```

| Field | Description |
|-------|-------------|
| `repository.full_name` | Used to match against `target_path` or `source_path` in repo config |
| `ref` | Branch or tag reference — included in Slack notification |
| `pusher.name` | The person who pushed — logged for debugging (Slack shows commit author instead) |
| `sender.login` | Fallback for pusher name in logs when `pusher.name` is empty |

#### Response

```json
{
  "status": "accepted"
}
```

| Status Code | Description |
|-------------|-------------|
| 200 | Event accepted, mirror sync started in background |
| 400 | Invalid request body |
| 401 | Invalid or missing `X-Hub-Signature-256` |
| 405 | Method not allowed (only POST) |

<br/>

## Retry API

<br/>

### POST /retry/mirror

Manually re-runs a mirror sync for the specified repo. Designed for recovery
from transient failures (e.g. AWS region blip) where the original webhook/SQS
event has already been consumed and lost.

When `RETRY_API_TOKEN` is unset, the endpoint is disabled — every request
returns 404 (different policy from webhook endpoints, which fall back to
"skip verification" on empty secret).

#### Headers

| Header | Required | Description |
|--------|----------|-------------|
| `Authorization` | Yes | `Bearer <RETRY_API_TOKEN>` (constant-time compared) |
| `Content-Type` | Yes | `application/json` |

#### Request Body

```json
{
  "repo": "my-repo",
  "direction": "target-to-source",
  "ref": "refs/tags/Build-2231"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `repo` | Yes | `RepoConfig.Name` from `config.yaml` (not `source_path` / `target_path`) |
| `direction` | No | `source-to-target`, `target-to-source`, or `auto` (default). `auto` resolves to `target-to-source` for bidirectional repos, otherwise to the repo's single allowed direction |
| `ref` | No | Full ref (e.g. `refs/tags/v1.0.0`). When omitted, an incremental fetch + push is still performed — useful to catch up any missed refs |

#### Response

```json
{
  "status": "accepted",
  "repo": "my-repo",
  "direction": "target-to-source",
  "ref": "refs/tags/Build-2231",
  "queued_at": "2026-05-22T07:51:24Z"
}
```

| Status Code | Description |
|-------------|-------------|
| 200 | Request accepted, retry started in background goroutine |
| 400 | Missing/empty `repo`, invalid `direction`, or invalid JSON |
| 401 | Missing or invalid `Authorization` header |
| 404 | Endpoint disabled (`RETRY_API_TOKEN` not set) |
| 405 | Method not allowed (only POST) |

The Slack notification body for retry-triggered syncs includes an extra
`Source: retry-api` line so the on-call operator can immediately distinguish
manual retries from webhook-driven syncs.

#### Example

```bash
TOKEN=$(kubectl -n git-bridge get secret git-bridge-secret \
  -o jsonpath='{.data.RETRY_API_TOKEN}' | base64 -d)

curl -X POST https://git-bridge.example.com/retry/mirror \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "repo": "my-repo",
    "direction": "target-to-source",
    "ref": "refs/tags/Build-2231"
  }'
```

<br/>

## SQS Consumer (Internal)

Not an HTTP endpoint. The SQS consumer polls the configured SQS queue for CodeCommit events.

#### Event Format (EventBridge → SQS)

```json
{
  "detail": {
    "repositoryName": "my-repo",
    "referenceName": "refs/heads/main",
    "referenceType": "branch",
    "event": "referenceUpdated"
  }
}
```

| Field | Description |
|-------|-------------|
| `detail.repositoryName` | Used to match against `source_path` in repo config |
| `detail.referenceName` | Branch or tag reference — included in Slack notification |
| `detail.referenceType` | `branch` or `tag` — used to construct full ref path |

#### Behavior

- Long-polling: 20 seconds wait time
- Visibility timeout: 120 seconds
- On success: message deleted from queue
- On failure: message remains, retried up to 5 times → DLQ

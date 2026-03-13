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
| `ref` | Branch or tag reference (logged) |

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
  "repository": {
    "name": "my-repo",
    "full_name": "org/my-repo"
  }
}
```

| Field | Description |
|-------|-------------|
| `repository.full_name` | Used to match against `target_path` or `source_path` in repo config |
| `ref` | Branch or tag reference (logged) |

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
| `detail.referenceName` | Branch or tag reference (logged) |

#### Behavior

- Long-polling: 20 seconds wait time
- Visibility timeout: 120 seconds
- On success: message deleted from queue
- On failure: message remains, retried up to 5 times → DLQ

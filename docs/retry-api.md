# Retry API — Usage Guide

> How to call the `POST /retry/mirror` endpoint to manually re-run a mirror sync.
> A single HTTP call triggers an incremental fetch + push without going through the webhook path.
> For background / incident-recovery context see [mirror-retry-en.md](./mirror-retry-en.md).

<br/>

## 1. When to use it

- Webhook fired once and failed (e.g. AWS region transient blip) → a ref is stuck
- GitHub webhook redelivery is awkward (HMAC bypass)
- Operator wants to deliberately refresh a repo's mirror

> ✅ Concurrent calls on the same repo are safe — the per-repo Mutex serializes them naturally.
> ⚠️ Retry **always requires auth** — there is no "empty secret = skip verify" fallback like the webhook handler.
> When `RETRY_API_TOKEN` is unset the endpoint is fully disabled (404).

<br/>

## 2. Prerequisite — extract the token

```bash
TOKEN=$(kubectl -n git-bridge get secret git-bridge-secret \
  -o jsonpath='{.data.RETRY_API_TOKEN}' | base64 -d)
```

Required permission: `get` on `secrets/git-bridge-secret` in the `git-bridge` namespace.

<br/>

## 3. Basic call

### 3-1. Inside the corp network (workstation → external host)

```bash
curl -X POST https://git-bridge.example.com/retry/mirror \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"repo":"git-bridge-test","direction":"auto"}'
```

> `git-bridge.example.com` only resolves on internal DNS. From outside, use §3-2 or §3-3.

### 3-2. Outside the corp network — pod loopback

```bash
POD=$(kubectl -n git-bridge get pod -l app=git-bridge -o jsonpath='{.items[0].metadata.name}')

kubectl -n git-bridge exec "$POD" -- sh -c "
  wget -qO- \
    --post-data='{\"repo\":\"git-bridge-test\",\"direction\":\"auto\"}' \
    --header='Authorization: Bearer $TOKEN' \
    --header='Content-Type: application/json' \
    --timeout=20 \
    http://localhost:8080/retry/mirror
"
```

### 3-3. From another in-cluster pod — Service ClusterIP

```bash
curl -X POST http://git-bridge.git-bridge.svc.cluster.local/retry/mirror \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"repo":"git-bridge-test","direction":"auto"}'
```

<br/>

## 4. Request body

| Field | Required | Type | Description |
|---|---|---|---|
| `repo` | ✅ | string | `RepoConfig.Name` (the `name` field in config.yaml). Not `source_path` / `target_path`. |
| `direction` | ❌ | string | `source-to-target` / `target-to-source` / `auto`. Defaults to `auto`. |
| `ref` | ❌ | string | Full ref (`refs/heads/main`, `refs/tags/v1.0.0`). Omitting it still runs incremental fetch + push, which catches up all missed refs. |

### 4-1. `direction` resolution

`auto` is resolved in this order:

1. **API-call explicit direction** (`source-to-target` / `target-to-source`) — always wins
2. **Repo's `retry_direction`** (per-repo pin in config.yaml) — operator pre-declared intent
3. **Built-in fallback** — bidirectional → `target-to-source`, one-way → its single allowed direction

| Repo's `direction` | `retry_direction` (config) | API `direction` input | Result |
|---|---|---|---|
| `bidirectional` | (unset) | `auto` (or omitted) | **target-to-source** (built-in fallback) |
| `bidirectional` | `source-to-target` | `auto` (or omitted) | **source-to-target** (repo pin) |
| `bidirectional` | `target-to-source` | `source-to-target` (explicit) | **source-to-target** (API wins) |
| `bidirectional` | `source-to-target` | explicit source → target |
| `bidirectional` | `target-to-source` | explicit target → source |
| `source-to-target` (one-way) | `auto` / `source-to-target` | source → target |
| `source-to-target` (one-way) | `target-to-source` | **error** (direction conflict) |
| `target-to-source` (one-way) | `auto` / `target-to-source` | target → source |
| `target-to-source` (one-way) | `source-to-target` | **error** (direction conflict) |

### 4-2. `ref` behavior

- **Omitted**: `git fetch --prune` + `git push --all --tags`. All missed refs catch up in one call.
- **Set (e.g. `refs/tags/X`)**: same incremental fetch + push. The ref is used only to enrich the Slack notification body (`Tag:` / `Branch:` line, commit author lookup) — the fetch scope is always the full set.

> 💡 Even when only a single ref is missing, omitting `ref` is the efficient choice — one call sweeps every stuck ref at once.

<br/>

## 5. Response

Success (HTTP 200) — the response is synchronous but the sync runs in a background goroutine:

```json
{
  "status": "accepted",
  "repo": "git-bridge-test",
  "direction": "target-to-source",
  "ref": "",
  "queued_at": "2026-05-26T05:44:01Z"
}
```

| HTTP | Meaning |
|---|---|
| **200** | Request accepted, background sync started |
| **400** | Missing `repo` / invalid `direction` / malformed JSON |
| **401** | Missing `Authorization` header, missing `Bearer ` prefix, or token mismatch |
| **404** | Endpoint disabled (`RETRY_API_TOKEN` is unset) |
| **405** | Method not allowed (only POST) |

<br/>

## 6. Slack notification

The Slack body for retry-triggered syncs carries an extra **`Source: retry-api`** line, so on-call can immediately tell a manual retry apart from a routine webhook/SQS sync.

Example (success):

```
✅ Mirror Sync: git-bridge-test
Action: branches + tags synced
Route: gitlab/server/git-bridge-test → codecommit/git-bridge-test
Duration: 5.52s
Target: https://codecommit.eu-central-1.amazonaws.com/...
Source: retry-api
```

When a repo carries a `slack_webhook_url` override, the notification is routed to that channel (e.g. `git-bridge-test` → `GIT_BRIDGE_TEST_SLACK_WEBHOOK_URL`).

<br/>

## 7. Scenarios

### 7-1. Bidirectional repo — most common form (auto)

```bash
curl -X POST https://git-bridge.example.com/retry/mirror \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"repo":"git-bridge-test","direction":"auto"}'
```

→ Syncs `target-to-source` (gitlab → codecommit).

### 7-2. Bidirectional repo — explicit direction

```bash
curl -X POST https://git-bridge.example.com/retry/mirror \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"repo":"my-repo","direction":"source-to-target"}'
```

→ Forces `codecommit → gitlab`. Use when you know which side is the source of truth.

### 7-3. A specific tag is missing — enrich Slack body with `ref`

```bash
curl -X POST https://git-bridge.example.com/retry/mirror \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "repo": "git-bridge-test",
    "direction": "auto",
    "ref": "refs/tags/Test-Build-1234"
  }'
```

→ Incremental fetch picks up that tag along with any other missed refs. The Slack body then shows `Tag: Test-Build-1234` + `Pushed by: <author>`.

<br/>

## 8. Troubleshooting

| Symptom | Cause / Fix |
|---|---|
| `404 page not found` | `RETRY_API_TOKEN` is unset in the secret. Confirm with `kubectl get secret git-bridge-secret -o jsonpath='{.data.RETRY_API_TOKEN}'`. |
| `401 unauthorized` (with the right token) | Missing `Authorization: Bearer ` prefix. A bare `Authorization: <token>` (no `Bearer `) is rejected. |
| `400 bad request: invalid direction` | `direction` is not one of `source-to-target` / `target-to-source` / `auto`. |
| `400 bad request: repo required` | `repo` field missing or whitespace-only. |
| 200 OK but no Slack alert | Background sync may have failed — check `kubectl logs <pod> --tail=50 | grep retry-api`. `already up-to-date` correctly skips the notification. |
| `direction does not allow retry direction` | Requested direction conflicts with the repo's one-way setting (e.g. asking `target-to-source` on a `source-to-target` repo). Use `auto` or the correct direction. |

<br/>

## 9. Security notes

- Tokens are compared with `crypto/subtle.ConstantTimeCompare` (timing-attack safe).
- The `Authorization` header must carry the literal `Bearer ` prefix — bare tokens are rejected.
- The per-repo Mutex serializes concurrent calls on the same repo, so no extra rate limiter is in place. Guards against bulk abuse are deferred to Phase 2.
- The token is never logged or included in Slack notification bodies.

<br/>

## 10. Related docs

- [API Reference](./API.md#post-retrymirror) — endpoint spec (headers / status codes / fields)
- [Mirror Retry Guide](./mirror-retry-en.md) — background, the 2026-05-19 incident case, operational policy
- [Webhook setup — GitLab](./gitlab-webhook-setup.md), [GitHub](./github-webhook-setup.md) — webhook-side retry procedure

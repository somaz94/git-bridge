# Retry API — Usage Guide

> How to call the `POST /retry/mirror` endpoint to manually re-run a mirror sync.
> A single HTTP call triggers an incremental fetch + push without going through the webhook path.
> For background / incident-recovery context see [mirror-retry.md](./mirror-retry.md).

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
| `ref` | ❌ | string | Full ref (`refs/heads/main`, `refs/tags/v1.0.0`). Set it and **only that ref** is pushed; omit it to catch up every ref (§4-2). |
| `source` | ❌ | string | Caller identity. **Only `cron` is accepted**; anything else returns 400. Omitted is recorded as `retry-api`. The reconcile CronJob sets it so its scheduled calls are distinguishable from a hand-run one — there is no reason to send it by hand. |
| `force` | ❌ | bool | Applies a rewind the push guard would otherwise withhold (§4-3). **Requires `ref` and `dest`**, and is refused for `source: cron` — all three return 400. Defaults to false; an omitted field never reads as permission. |
| `dest` | with `force` | string | The destination tip you are overwriting. It becomes the push's lease, so the force only ever discards the commit you decided about. |
| `actor` | ❌ | string | Attributes the call in the history event and the Slack message. The console button fills it from the portal's `X-Auth-User`. |

### 4-3. `force` — applying a deliberate rewind

The mirror refuses a push that would move a ref **backwards** at the destination: if the destination already contains what this side holds, writing it back discards commits there. That refusal is what stops a late echo from undoing a commit that landed while it was in flight, and it is on by default in both directions.

The same rule also stops a rewind somebody meant to make — `git reset --hard <older>` followed by a force-push. The mirror cannot tell the two apart from the event, so `force` is how a person says which one this is.

```bash
curl -X POST http://git-bridge/retry/mirror \
  -H "Authorization: Bearer $RETRY_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"repo":"demo-repo","direction":"target-to-source",
       "ref":"refs/heads/version/4.3.0","dest":"<the tip in the alert>","force":true}'
```

What `force` does **not** do is skip the lease. The push still carries the destination tip it was checked against, so a commit that arrives between the check and the write is still refused (`lease-rejected`). Authorising a rewind of the tip you were shown is not the same as authorising the overwrite of whatever lands while you decide.

Three limits keep the bypass narrow:

- **One force, one ref.** A force with no `ref` is rejected — otherwise a single call would carry the bypass across every ref in the repository.
- **No automatic path can set it.** Webhook and SQS syncs never construct it, and `source: cron` is refused, so the hourly reconcile cannot carry a bypass over every repository unattended.
- **It is loud.** The bypass logs at WARN with the actor, and the resulting overwrite still raises the usual forced-update alert naming the discarded tip.

A withheld push records `skip` / `destination-ahead` in the history, and a withheld **branch** also raises a `Push Withheld` Slack alert. Tags stay quiet, the same way an overwritten tag does — a pipeline that reuses build tag names re-points them constantly, and an alert that fires on routine traffic is one people learn to ignore.

**The alert carries the command.** It arrives with the repository, direction and ref already filled in, one call per withheld ref, so applying a rewind is a copy rather than a reconstruction. Only two values stay as placeholders: `<git-bridge-url>`, which the process does not know, and `$RETRY_API_TOKEN`, which it must never print into a channel.

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
| `bidirectional` | `source-to-target` | `source-to-target` (explicit) | **source → target**, forced explicitly |
| `bidirectional` | `target-to-source` | `target-to-source` (explicit) | **target → source**, forced explicitly |
| `source-to-target` (one-way) | (unset) | `auto` / `source-to-target` | **source → target** |
| `source-to-target` (one-way) | (unset) | `target-to-source` | **error** (direction conflict) |
| `target-to-source` (one-way) | (unset) | `auto` / `target-to-source` | **target → source** |
| `target-to-source` (one-way) | (unset) | `source-to-target` | **error** (direction conflict) |

> **The console's re-run button does not use `auto`.** It sends the destination
> endpoint (`to`) of the row that was clicked to `POST /console/api/retry`, and
> the server turns that side back into a direction. Under `auto` the table above
> would resolve a bidirectional repo through `retry_direction` every time, so a
> re-run clicked on a row that failed `source-to-target` would run the other
> leg — whose destination is already ahead, so it ends in a skip while the real
> gap waits for the hourly reconcile. The precedence above still applies to
> callers holding the token (cron, a hand-run curl).

### 4-2. `ref` behavior

- **Omitted**: `git fetch --prune` + `git push --all --tags`. All missed refs catch up in one call.
- **Set (e.g. `refs/tags/X`)**: the fetch is still the full incremental one, but **the push is scoped to that single ref** (refspec `+<ref>:<ref>`). If the ref is absent from the local mirror the push is skipped entirely and recorded as `no-refs-to-push`. The ref value is also used to enrich the Slack notification body (`Tag:` / `Branch:` line, commit author lookup).

> 💡 When several refs are behind, **omit** `ref` — one call sweeps every stuck ref at once. Setting `ref` moves only that one, matching the `RetryRequest.ref` description in `internal/server/openapi.json`: "Optional single ref to sync. Omit to sync every ref."

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
Route: gitlab/team/test-repo → codecommit/git-bridge-test
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
  -d '{"repo":"demo-repo","direction":"source-to-target"}'
```

→ Forces `codecommit → gitlab`. Use when you know which side is the source of truth.

### 7-3. Exactly one tag is missing — set `ref`

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

→ The incremental fetch picks up every missed ref, but **the push is scoped to that one tag**. The Slack body then shows `Tag: Test-Build-1234` + `Pushed by: <author>`. Omit `ref` to move the other missed refs too.

<br/>

## 8. Troubleshooting

| Symptom | Cause / Fix |
|---|---|
| `404 page not found` | `RETRY_API_TOKEN` is unset in the secret. Confirm with `kubectl get secret git-bridge-secret -o jsonpath='{.data.RETRY_API_TOKEN}'`. |
| `401 unauthorized` (with the right token) | Missing `Authorization: Bearer ` prefix. A bare `Authorization: <token>` (no `Bearer `) is rejected. |
| `400 bad request: invalid direction` | `direction` is not one of `source-to-target` / `target-to-source` / `auto`. |
| `400 bad request: repo required` | `repo` field missing or whitespace-only. |
| 200 OK but no Slack alert | Background sync may have failed — check `kubectl logs <pod> --tail=50 \| grep retry-api`. `already up-to-date` correctly skips the notification. |
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
- [Mirror Retry Guide](./mirror-retry.md) — background, the 2026-05-19 incident case, operational policy
- [Webhook setup — GitLab](./gitlab-webhook-setup.md), [GitHub](./github-webhook-setup.md) — webhook-side retry procedure

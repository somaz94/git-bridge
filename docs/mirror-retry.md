# Mirror Retry Guide

> Procedure for retrying a failed mirror sync, plus the design sketch for a future dedicated retry API.
> First drafted: 2026-05-19 (post AWS eu-central-1 transient blip)

<br/>

## 1. Background — limits of automatic retry

git-bridge has two sync triggers:

| Trigger | Automatic retry policy |
|---|---|
| **HTTP webhook** (`POST /webhook/gitlab`, `POST /webhook/github`) | None. Single-shot — on failure, only the user / Slack are notified |
| **SQS event consumer** (CodeCommit → SQS path) | visibility timeout(120s) + max receive count(5), then DLQ |

**If external I/O fails transiently at the webhook moment, the ref is stuck:**

- A tag push has no follow-up push, so it never catches up
- A branch push stays missing until the next commit
- `cmd/git-bridge/main.go` has no startup-reconcile code → restarting the pod does not recover it

Note that `Sync()` itself is an **incremental fetch + push**. If even one later push lands, all missed refs catch up together. The problem is *when no later push happens*.

<br/>

## 2. Case study — 2026-05-19 incident

### 2-1. Symptom

The gitlab → codecommit (eu-central-1) mirror for `my-repo` failed for 8m 43s. Two tag pushes got stuck:

- `refs/tags/Test-C-Build-1916` (push at 07:24:00, failed at 07:26:33)
- `refs/tags/Build-2231` (push at 07:24:20, failed at 07:34:40 — git process SIGKILL after 8 min)

Error pattern:

```
ERROR  SQS receive error    name=sqs-eu    error=...net/http: TLS handshake timeout
ERROR  mirror sync failed   ref=Test-C-Build-1916         error=Recv failure: Connection reset by peer
ERROR  mirror sync failed   ref=Build-2231   error=signal: killed: ...Recv failure: Connection reset by peer
```

Two unrelated AWS services in the same region (SQS + CodeCommit, both eu-central-1) failed at the same minute → AWS region transient or a brief Korea↔eu Internet path drop.

<br/>

### 2-2. Diagnosis — cluster-side root cause ruled out

| Check | Result |
|---|---|
| TCP 443 to codecommit.eu-central-1 (pod-side `nc`) | OPEN (now) |
| TCP 443 to sqs.eu-central-1 (pod-side `nc`) | OPEN |
| HTTPS latency (pod-side, now) | 1–2s |
| HTTPS latency (external workstation, now) | tls=0.9s |
| Other region (ap-northeast-2) | Healthy |
| Subsequent ERRORs after the last failure | 0 in 60+ minutes |

→ **Not a cluster / LAN issue**. Most likely an AWS eu-central-1 transient or a brief Korea↔eu route blip.

<br/>

### 2-3. Recovery — manual webhook POST

Since no automatic recovery exists, we fire the webhook payload manually:

```bash
SECRET=$(kubectl -n git-bridge get secret git-bridge-secret \
  -o jsonpath='{.data.WEBHOOK_GITLAB_SECRET}' | base64 -d)

PAYLOAD='{"event_name":"push","user_name":"manual-retry","ref":"refs/tags/Build-2231","repository":{"name":"my-repo"},"project":{"path_with_namespace":"team/my-repo"}}'

kubectl -n git-bridge exec git-bridge-<podname> -- sh -c "
  wget -qO- --post-data='$PAYLOAD' \
    --header='Content-Type: application/json' \
    --header='X-Gitlab-Token: $SECRET' \
    --server-response --timeout=20 \
    http://localhost:8080/webhook/gitlab 2>&1
"
```

Result (07:51:24 → 07:51:35, 11.5s):

```
INFO  received gitlab push event   ref=refs/tags/Build-2231   pusher=manual-retry
INFO  fetching from source (incremental)
INFO  pushing to target
INFO  mirror sync done             duration=11.506315182s
```

→ Firing the most recent ref alone is enough — **incremental fetch caught up the missing 1916 too**. Slack received the usual ✅ Mirror Sync Done notification via the normal webhook flow.

<br/>

## 3. Manual retry procedure (currently available)

### 3-1. Prerequisites

- `kubectl` context pointing at the cluster where git-bridge runs
- Permissions on the git-bridge namespace (secret read + pod exec)

<br/>

### 3-2. Procedure — GitLab push failure

```bash
# (1) Find the running pod
POD=$(kubectl -n git-bridge get pod -l app=git-bridge -o jsonpath='{.items[0].metadata.name}')

# (2) Extract the webhook secret
SECRET=$(kubectl -n git-bridge get secret git-bridge-secret \
  -o jsonpath='{.data.WEBHOOK_GITLAB_SECRET}' | base64 -d)

# (3) Build the payload — only 4 fields are required
#   - event_name: "push"
#   - ref: the failed ref (one — the latest — is enough; incremental catches up older ones)
#   - repository.name: short repo name (e.g. "my-repo")
#   - project.path_with_namespace: must match target_path or source_path in git-bridge config
#                                  (e.g. "team/my-repo")
PAYLOAD='{"event_name":"push","user_name":"manual-retry","ref":"refs/tags/<TAG_OR_BRANCH>","repository":{"name":"<REPO>"},"project":{"path_with_namespace":"<NAMESPACE/REPO>"}}'

# (4) POST — use pod loopback (the external host git-bridge.example.com is internal-only;
#     external workstations cannot resolve it; use service ClusterIP or pod loopback)
kubectl -n git-bridge exec "$POD" -- sh -c "
  wget -qO- --post-data='$PAYLOAD' \
    --header='Content-Type: application/json' \
    --header='X-Gitlab-Token: $SECRET' \
    --timeout=20 \
    http://localhost:8080/webhook/gitlab
"
# Expected response: {"status":"accepted"}

# (5) Verify
kubectl -n git-bridge logs "$POD" --since=2m | grep -E "my-repo|mirror sync"
# → "mirror sync done" means success. Slack gets the same notification as a normal webhook.
```

<br/>

### 3-3. GitHub push failure

GitHub webhooks use HMAC-SHA256 signature verification (`X-Hub-Signature-256`), so payload spoofing is hard. The easiest options:

1. **GitHub UI redelivery** — Settings → Webhooks → the webhook → Recent Deliveries → Redeliver
2. **Push a trivial commit** — incremental fetch will sweep in the missed refs

(The future retry API will sidestep the signature — see §4.)

<br/>

### 3-4. SQS (CodeCommit → GitLab/GitHub) failure

SQS itself has retry + DLQ, so usually no action is needed:

| Stage | Behavior |
|---|---|
| 1st failure | After visibility timeout(120s) the message returns to the queue |
| 2nd–5th failure | Retried the same way |
| Beyond 5 | Moved to DLQ |

To redrive a DLQ message back to the main queue, use the AWS console's "Start DLQ redrive" or the CLI. Before DLQ, automatic retry handles it.

<br/>

### 3-5. Retry API (recommended — available from 2026-05-26)

A single `POST /retry/mirror` replaces the webhook procedure above and produces the same result. With the token, no payload assembly is needed:

```bash
# (1) Extract the token
TOKEN=$(kubectl -n git-bridge get secret git-bridge-secret \
  -o jsonpath='{.data.RETRY_API_TOKEN}' | base64 -d)

# (2) Call — when direction is omitted it defaults to "auto"
#     ("auto" on bidirectional falls back to target-to-source).
curl -X POST https://git-bridge.example.com/retry/mirror \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "repo": "my-repo",
    "direction": "target-to-source",
    "ref": "refs/tags/Build-2231"
  }'
# Expected response: {"status":"accepted","repo":"my-repo",...,"queued_at":"..."}

# (3) The Slack notification body carries a "Source: retry-api" line so on-call
#     can immediately distinguish manual retries from regular webhook syncs.
```

Advantages over §3-2:

- No payload-JSON assembly — just `repo` (name) + `direction` + `ref`
- No webhook-secret exposure — uses a dedicated retry token
- Works around GitHub HMAC — same endpoint regardless of provider
- Explicit `direction` — pin a specific side for bidirectional repos

`direction` values:

| Value | Behavior |
|---|---|
| `source-to-target` | sync source → target |
| `target-to-source` | sync target → source |
| `auto` (default) | falls back to `target-to-source` on bidirectional; otherwise the repo's single allowed direction |

`ref` is optional. Even when omitted, an incremental fetch + push runs — older missed refs catch up at the same time.

Full spec: [docs/API.md `POST /retry/mirror`](./API.md#post-retrymirror).

<br/>

### 3-6. Why external POST to `git-bridge.example.com` fails

`git-bridge.example.com` resolves only on the internal DNS (corp / iptime). Workstations outside the corp network fail at the connect stage. Use cluster-internal access (pod / service ClusterIP) or the corp network.

From inside the corp network, an external POST works:

```bash
SECRET=...  # extract as above
curl -X POST https://git-bridge.example.com/webhook/gitlab \
  -H "Content-Type: application/json" \
  -H "X-Gitlab-Token: $SECRET" \
  -d "$PAYLOAD"
```

<br/>

## 4. Retry API — landed 2026-05-26

See §3-5 for the usage procedure. The table below records the **final decisions**
versus the original sketch — useful for future tweaks.

### 4-1. Confirmed decisions

| Item | Final |
|---|---|
| Endpoint path | `POST /retry/mirror` |
| Auth | Bearer token (`RETRY_API_TOKEN` env), compared with `crypto/subtle.ConstantTimeCompare` |
| Token unset | endpoint **disabled (404)** — opposite of the webhook "skip verification" mode (retry always requires auth) |
| `repo` lookup key | `RepoConfig.Name` |
| `direction` enum | `source-to-target` / `target-to-source` / `auto` (default) |
| `auto` (bidirectional) | falls back to `target-to-source` (2026-05-19 incident pattern) |
| `auto` (one-way) | resolves to the repo's single allowed direction |
| direction conflict | requesting the opposite direction on a one-way repo returns an error (validated inside `mirror.Service.Retry`) |
| `ref` omitted | incremental fetch + push (catch-up only) |
| Bulk retry | not supported — deferred to Phase 2 |
| Slack notify | reuses the existing notification with an extra `Source: retry-api` line |
| Rate limit | none — the per-repo Mutex serializes naturally |

### 4-2. Future work (Phase 2)

- CLI helper `cmd/git-bridge-retry/` using `RETRY_API_TOKEN` + `GIT_BRIDGE_URL` envs
- Slack failure body automatically embeds the retry curl example
- Bulk retry — `refs: [...]`
- HTTPRoute-level IP allowlist (if exposed externally)

<br/>

## 4-old. (Reference) Original design sketch — archived 2026-05-22

> Pre-implementation tentative decisions. The actual landed version is §4.

### 4-old-1. Motivation

Pain points of the current manual procedure:

- The payload JSON must be hand-built per ref/repo (error-prone)
- Avoiding webhook-secret leakage requires pod exec → operator privilege + multiple steps
- GitHub-side HMAC cannot be bypassed (UI redelivery is the only path)
- Hard to retry multiple refs / multiple repos at once

A dedicated retry API would give us:

- One endpoint that retries mirrors for any provider (GitLab/GitHub/CodeCommit)
- A separate auth token so webhook secrets stay private
- Optional bulk retry (multiple refs)
- The ability to embed a retry URL/command in `mirror sync failed` Slack messages

<br/>

### 4-2. Candidate endpoint design (TBD)

```http
POST /retry/mirror
Authorization: Bearer <RETRY_API_TOKEN>
Content-Type: application/json

{
  "repo": "my-repo",
  "direction": "source-to-target",   // or "target-to-source", "auto"
  "ref": "refs/tags/Build-2231"   // when omitted, only incremental fetch
}
```

Response:

```json
{
  "status": "accepted",
  "repo": "my-repo",
  "ref": "refs/tags/Build-2231",
  "queued_at": "2026-05-19T07:51:24Z"
}
```

| Item | Decision needed |
|---|---|
| Auth method | Bearer token (env: `RETRY_API_TOKEN`) vs mTLS vs Kubernetes ServiceAccount |
| `ref` omitted | Limit to `git fetch` (incremental catch-up) vs force full resync |
| Bulk support | `refs: ["...","..."]` array vs one-ref-per-call |
| Sync/async | Background async (`{"status":"accepted"}` immediately), same as current webhook |
| Rate limit | Many requests on same repo: the per-repo Mutex in `internal/mirror` already serializes — may not need an extra limiter |
| Slack notify | Mark manual-retry origin (`source=retry-api` field) |

<br/>

### 4-3. Implementation sketch

#### Code locations

- `internal/server/health.go` `NewMux()` — register the new route
- `internal/consumer/` — add `retry.go`, structured like the webhook handler: payload validation + `mirror.Sync()` call
- `internal/config/` — add `retry_api_token: "${RETRY_API_TOKEN}"`
- k8s manifests — add `RETRY_API_TOKEN` to the secret

#### Security

- Compare the Authorization token with `crypto/subtle.ConstantTimeCompare`
- If the token is unset, disable the endpoint (404) — same pattern as the webhook handler
- For external exposure, add a separate Gateway/Ingress rule with IP allowlist or mTLS

#### Compatibility

- The existing webhook handlers stay untouched — retry API is purely additive
- `mirror.Sync()` signature does not change (already takes `repoName, EventMeta`)

<br/>

### 4-4. Optional CLI helper

```bash
git-bridge-retry --repo my-repo --ref refs/tags/Build-2231
```

Reads `RETRY_API_TOKEN` from the environment and calls the endpoint. Add under `cmd/` or as a separate small repo.

<br/>

### 4-5. Open decisions

| Item | Options | Tentative preference |
|---|---|---|
| Endpoint path | `/retry/mirror` vs `/api/v1/retry` vs `/webhook/retry` | `/retry/mirror` (simple) |
| Auth | Bearer token vs mTLS | Bearer (simpler ops) |
| Bulk | Supported vs single only | Single first, add bulk later |
| `ref` omitted | Catch-up only vs full resync | Catch-up only (natural use of incremental) |
| Slack notify | Reuse existing alarm vs separate channel/marker | Reuse + add `source` field |
| API docs | Add to `docs/API.md` | Yes |

<br/>

## 5. Operational reference

| Item | Value |
|---|---|
| Pod naming | `git-bridge-<rs>-<pod>` (Deployment, replicas=1, Recreate strategy) |
| Service | `git-bridge.git-bridge.svc.cluster.local:80` (ClusterIP, 80 → container 8080) |
| External hosts (corp-only) | `git-bridge.example.com`, `git-bridge.example.org` (both attached to the HTTPRoute) |
| Webhook secret | k8s secret `git-bridge-secret`, keys `WEBHOOK_GITLAB_SECRET` / `WEBHOOK_GITHUB_SECRET` |
| Config | k8s configmap `git-bridge-config`, key `config.yaml` |
| Failure alerting | Slack webhook (`SLACK_WEBHOOK_URL`) — messages starting with `:x: Mirror Sync Failed:` |

<br/>

## 6. Related docs

- [API Reference](./API.md) — currently exposed endpoints (will be updated once the retry API lands)
- [GitLab Webhook setup](./gitlab-webhook-setup.md) — webhook URL / secret setup
- [GitHub Webhook setup](./github-webhook-setup.md) — HMAC signature setup
- [Advanced Config](./ADVANCE.md) — multi-provider configuration examples

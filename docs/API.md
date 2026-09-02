# API Reference

The service also serves a machine-readable version of this reference:

| Path | Description |
|------|-------------|
| `/openapi.json` | OpenAPI 3.0 spec, embedded in the binary (`internal/server/openapi.json`). A unit test keeps it in sync with the route table, so it cannot drift from what is actually served. |
| `/api-docs` | Swagger UI rendering that spec. |

> This page and `openapi.json` are maintained separately, so keep them in step when adding or changing an endpoint — the drift test only covers the spec, not this document.

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
  "user_name": "alice",
  "ref": "refs/heads/main",
  "after": "9f2c1ab5d3e47b8c0a16f5d92e3b7c481a0d6e5f",
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
| `after` | The new tip. GitLab decides a delete by this field alone: `after` equal to the zero SHA (`0000000000000000000000000000000000000000`) means the ref was deleted, and the delete is propagated to the other side instead of a push. Sent for both `push` and `tag_push` |
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
  "after": "9f2c1ab5d3e47b8c0a16f5d92e3b7c481a0d6e5f",
  "deleted": false,
  "pusher": {
    "name": "alice"
  },
  "sender": {
    "login": "alice"
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
| `deleted` | `true` means the ref was deleted, and the delete is propagated to the other side instead of a push |
| `after` | The new tip. GitHub sets the zero SHA (`0000000000000000000000000000000000000000`) here on a delete as well, so **either** signal is accepted — `deleted: true` **or** a zero-SHA `after` |
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
  "repo": "demo-repo",
  "direction": "target-to-source",
  "ref": "refs/tags/Test-Build-2231"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `repo` | Yes | `RepoConfig.Name` from `config.yaml` (not `source_path` / `target_path`) |
| `direction` | No | `source-to-target`, `target-to-source`, or `auto` (default). `auto` resolves to `target-to-source` for bidirectional repos, otherwise to the repo's single allowed direction |
| `ref` | No | Full ref (e.g. `refs/tags/v1.0.0`). When omitted, an incremental fetch + push is still performed — useful to catch up any missed refs |
| `source` | No | Caller identity, recorded as the event's trigger. **Only `cron` is accepted**; any other value returns `400` and starts no sync. Omitted is recorded as `retry-api`. The reconcile CronJob sets it so scheduled calls are distinguishable from a hand-run retry — the whitelist is what keeps the trigger column a closed vocabulary rather than free text from whoever holds the token |
| `force` | No | Applies a rewind the push guard withheld. Requires `ref` **and** `dest`, and is refused for `source: cron` — all three return `400`. Defaults to false; an omitted field never reads as permission. See [Retry API](retry-api.md) §4-3 |
| `dest` | With `force` | The destination tip being overwritten, which becomes the push's lease. Without it the force would run against whatever the destination holds when the push finally executes, which after a minute-long fetch is not necessarily what the caller decided about |
| `actor` | No | Attribution recorded on the history event and included in the Slack message |

#### Response

```json
{
  "status": "accepted",
  "repo": "demo-repo",
  "direction": "target-to-source",
  "ref": "refs/tags/Test-Build-2231",
  "queued_at": "2026-05-22T07:51:24Z"
}
```

| Status Code | Description |
|-------------|-------------|
| 200 | Request accepted, retry started in background goroutine |
| 400 | Missing/empty `repo`, invalid `direction`, invalid `source`, a `force` without `ref` or without `dest`, a `force` from `source: cron`, or invalid JSON |
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
    "repo": "demo-repo",
    "direction": "target-to-source",
    "ref": "refs/tags/Test-Build-2231"
  }'
```

<br/>

## Console (separate port)

Everything above is served on `server.port` (8080), which the public HTTPRoute
forwards to. The endpoints below are served **only** on `server.console_port`
(8081) and are therefore unreachable from outside the cluster — a reverse-proxy
portal is what proxies to that port, and its login plus group check is the only
credential involved.

The two ports use **separate muxes**: the console handlers are simply not
registered on the public one. That is the whole guard, which is why no header,
`Host` value or forwarded port takes part in the decision — the socket that
accepted the connection is the only thing that decides. On the public port every
path below answers **404, not 403**, so a public caller cannot learn that the
console exists.

`console_port` and the deployment's `containerPort` / Service port have to move
together. Changing only one either closes the console or opens it on the public
port.

<br/>

### GET /

The console page: recent mirror activity with per-repository and per-trigger
filtering, failures-only view, idle-reconcile hiding (on by default), expandable
rows carrying the full stderr of a failed git command, and collapsing of echo
events.

Returns `200` with `text/html`. Any other path on this listener returns `404`.

<br/>

### GET /console/api/history

Recent mirror events, newest first. Served from an in-memory ring buffer, never
from the history file, so a page load cannot contend with a mirror operation on
disk I/O.

#### Query Parameters

| Parameter | Required | Description |
|-----------|----------|-------------|
| `limit` | No | Positive integer, default 100, capped at 500 |
| `failures` | No | `true` returns only events whose `result` is `fail` |
| `forced` | No | `true` returns only events whose `reason` is `forced-update` — a push that succeeded but overwrote a ref non-fast-forward. These stay `result: ok`, so the `failures` filter never surfaces them |
| `repo` | No | `RepoConfig.Name`. Empty or whitespace matches every repository |
| `source` | No | Trigger to filter by (`webhook`, `sqs`, `cron`, `retry-api`, `console`). Empty or whitespace matches every trigger |
| `hide_routine` | No | `true` drops `cron` events that ended in `skip` / `already-up-to-date` — the hourly reconcile reporting it found nothing, which is ~96 of every 100 rows. A reconcile that actually mirrored something (`ok`) or failed is **never** hidden. Opt-in, so a direct API caller still gets the whole history; the console page asks for it by default. Applied while walking the tail, so `limit` is filled with rows that survived the filter |

#### Response

```json
{
  "count": 1,
  "repos": ["git-bridge-test", "demo-repo"],
  "sources": ["cron", "sqs", "webhook"],
  "events": [
    {
      "ts": "2026-07-28T04:12:33Z",
      "repo": "git-bridge-test",
      "action": "mirror",
      "source": "webhook",
      "from": "gitlab/team/test-repo",
      "to": "codecommit/git-bridge-test",
      "ref": "refs/tags/v1.0.0",
      "result": "skip",
      "reason": "already-up-to-date",
      "duration_ms": 812
    }
  ]
}
```

| Field | Description |
|-------|-------------|
| `action` | `mirror` (branch/tag sync), `delete` (ref delete propagation) or `restore` (a console click re-creating a ref a delete removed). A restore is its own action rather than a mirror because nothing upstream asked for it — a person did |
| `source` | What triggered the sync: `webhook`, `sqs`, `cron` (the reconcile CronJob), `retry-api` (a hand-run call), or `console` |
| `result` | `ok`, `skip`, or `fail`. Failures also carry `err` |
| `reason` | Narrows `result`. Skips are `already-up-to-date`, `ref-override`, `no-refs-to-push` or `already-absent`; failures name the step that failed; a success can carry `forced-update`, meaning the push worked but overwrote a ref non-fast-forward. A refused restore is `ref-exists` (the ref came back on the destination) or `object-gone` (git collected the commit); `create-ref` is a restore that failed at the push itself |
| `deleted_tip` | Only on a `delete` that actually removed something: the SHA the ref pointed at, read just before the delete ran. Absent otherwise, including a delete that found the ref already gone. A delete leaves nothing behind to look up — afterwards the destination names neither the ref nor the commit — so this is the only record of what was discarded |
| `restored_tip` | Only on a `restore` that actually re-created the ref: the SHA it was put back at. The counterpart to `deleted_tip` — the two events together tell the whole story of a ref that went away and came back, without correlating them by timestamp |
| `actor` | Who a console-driven action is attributed to, read from the portal's `X-Auth-User` header. Set only for actions a person triggers, because those are the only ones with a person behind them — a webhook or an SQS event has a pusher, not an operator |
| `duration_ms` | Includes waiting for the per-repo lock, unlike the duration in the Slack message, which starts after the lock |

`repos` and `sources` are computed from the **unfiltered** tail, so the filter
dropdowns keep offering every repository and trigger even while one of them is
selected. They list only values that actually appear in the history, so a filter
can never be offered that would return nothing.

The trigger filter matters because the triggers are not equally common: the
reconcile CronJob runs hourly and is almost always `already-up-to-date`, so it
outnumbers real pushes by an order of magnitude. Filtering to `webhook` is how
you see actual pushes without the safety net's noise.

| Status Code | Description |
|-------------|-------------|
| 200 | Events returned (`events` is `[]`, never `null`, when there are none) |
| 400 | `limit` is not a positive integer |
| 405 | Method not allowed (only GET) |

<br/>

### POST /console/api/retry

Re-runs a mirror sync for one repository, the same operation as
`POST /retry/mirror`.

**No API token is involved.** The console asks this endpoint and the server
calls the mirror service in-process, so `RETRY_API_TOKEN` never leaves the pod
and never reaches a browser. Authentication is the portal session.

#### Request Body

```json
{
  "repo": "git-bridge-test",
  "to": "gitlab-main/team/test-repo"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `repo` | Yes | `RepoConfig.Name` |
| `to` | No | The destination endpoint of the row being re-run (`<provider>/<path>`, the event's `to`). The server turns that side into the direction that writes it, so the re-run repeats the sync the row records |
| `direction` | No | `source-to-target`, `target-to-source`, or `auto` — the same values the public retry endpoint accepts. An explicit direction beats `to`; with neither, the request falls back to `auto` |
| `ref` | No | Full ref. Omitted means an incremental fetch + push |

Unknown fields are rejected rather than ignored, so a typo cannot silently
change nothing.

**Why the console sends `to` rather than `direction`.** `auto` resolves through
the repo's `retry_direction`, which on a bidirectional repo pins
`target-to-source`. A row that failed `source-to-target` therefore answered a
click with a sync in the other direction — one whose destination was already
ahead, so it could only skip, leaving the real gap until the hourly reconcile.
Sending the row's destination makes the button re-run the leg the operator is
actually looking at, in either direction.

#### Response

```json
{
  "status": "accepted",
  "repo": "git-bridge-test",
  "direction": "auto"
}
```

| Status Code | Description |
|-------------|-------------|
| 202 | Request accepted, sync started in a background goroutine |
| 400 | Missing/empty `repo`, invalid `direction`, unknown field, or invalid JSON |
| 404 | Retry not wired up |
| 405 | Method not allowed (only POST) |
| 409 | `to` is not a side of that repo, names a direction the repo's `direction` forbids, or names *both* sides — an older-notation destination on a repo that mirrors two instances of the same provider type over the same path, where the side cannot be recovered |
| 415 | `Content-Type` is not `application/json`. A cross-site form cannot send that type without a preflight, so requiring it keeps this write off the end of a link. `to` picks which side is written, so this route carries the same gate as restore and force |

The sync is recorded with `source: console`, which distinguishes a human click
from the hourly reconcile job (`cron`), and the outcome appears in the
history the console is already polling.

<br/>

### POST /console/api/restore

Re-creates a ref a delete removed, at the tip that delete recorded
(`deleted_tip`). The console offers it as a button on delete rows that carry
one; there is no public counterpart, because no event means "put it back" — this
exists purely to let a person act on the record.

Like retry, no API token is involved: the server calls the mirror service
in-process and the portal session is the only credential.

#### Request Body

```json
{
  "repo": "git-bridge-test",
  "to": "codecommit/git-bridge-test",
  "ref": "refs/heads/feature-x",
  "sha": "c70e089e6660bfe57cf28a6df84aafad0e7e2b69"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `repo` | Yes | `RepoConfig.Name` |
| `to` | Yes | Which side to put the ref back on, in the `<provider-type>/<path>` form the history event's `to` field carries. The caller is acting on a row it is looking at, so it names the destination that row named rather than leaving the server to guess |
| `ref` | Yes | Full ref — `refs/heads/<name>` or `refs/tags/<name>`. Nothing else is accepted |
| `sha` | Yes | Full 40-character object name. An abbreviated one is rejected here rather than by the remote, which rejects it too |

Unknown fields are rejected rather than ignored, the same as retry.

#### Response

```json
{
  "status": "restored",
  "repo": "git-bridge-test",
  "ref": "refs/heads/feature-x",
  "sha": "c70e089e6660bfe57cf28a6df84aafad0e7e2b69"
}
```

| Status Code | Description |
|-------------|-------------|
| 200 | The ref was re-created — or was already at exactly that tip, which is recorded as a `skip` / `already-up-to-date` and is not an error |
| 400 | Missing `repo` / `to`, a `ref` that is not `refs/heads/` or `refs/tags/`, a `sha` that is not 40 hex characters, an unknown field, or invalid JSON. Also `unknown-side` — `to` is not a side of that repo, or is an older-notation destination that names both sides at once |
| 404 | Restore not wired up (no mirror service). `/console/api/me` reports this as `restore_enabled: false` so the page hides the button instead of offering one that 404s |
| 405 | Method not allowed (only POST) |
| 403 | A `direction` or `ref_overrides` rule forbids writing to that side |
| 409 | `no-matching-delete` (no recorded delete matches this repo, destination, ref and commit) or `ref-exists` (the ref is back on the destination) |
| 410 | `object-gone` — git has garbage-collected the commit; there is nothing left to put back |
| 500 | `restore-failed` — the push itself failed. Unlike the rows above this is a breakage, not a refusal |
| 503 | `repo-busy` — a mirror operation holds the per-repo lock. The same request will work once it lands; the restore is refused rather than queued because it runs inside the request and would otherwise hold the connection |
| 415 | `Content-Type` is not `application/json`. A cross-site form cannot send that type without a preflight, so requiring it keeps this write off the end of a link |

**Unlike retry, this route is synchronous.** Retry can answer `202` because its
result is just another history row, but the interesting outcome here is the
refusal, and that has to reach the person who clicked rather than being
something they discover later by re-reading the list.

Every response carries a machine-readable `reason` beside the human `error`, so
a caller can tell a refusal (the guard worked; go look at what changed) from a
breakage (the service could not do its job) without parsing prose. The reasons
travel from the mirror package as sentinel errors rather than as message text.

**A restore only ever fills a hole it can still see.** The server re-reads the
destination with `ls-remote` first and refuses if the ref returned, because
overwriting whoever re-created it would be the same accident this feature exists
to undo, with a different victim. The push that follows uses
`--force-with-lease=<ref>:` — an empty expect means "this ref must not exist" —
so the remote atomically closes the window between the check and the write.

A plain non-force push was tried first and is **not** sufficient: non-force only
rejects non-fast-forward updates, so a branch someone re-created at an *ancestor*
of the restored commit would have been advanced onto it instead of refused. That
is true for branches and not for tags, which is what made the wrong version look
correct.

Two further gates apply, matching every other write path in the service: the
restore is refused for a side the repo's `direction` never writes to, or for a
direction a `ref_overrides` entry pins away from, and it proceeds only when the
history tail still records a matching `delete` for that repo, destination, ref
and commit (`no-matching-delete`) — without that last check the route would be a
general "create any ref from any cached commit" API.

Finding the commit is two steps: the mirror cache for the other side usually
still has it, and if git collected it there, that side's remote is asked for it
directly before giving up.

The event is recorded as `action: restore`, `source: console`, with
`restored_tip` set and `actor` taken from the portal's `X-Auth-User` header —
attribution matters here because this writes to a real repository. Slack gets a
`Ref Restored` or `Ref Restore Failed` message for the same reason. Once the ref
exists again the destination's own push event carries it to the other side like
any other push.

A forced update is deliberately **not** restorable the same way. There the ref
still exists and points at something newer, so pushing the old tip back would
destroy whatever legitimately landed in the meantime — a restore fills a hole,
an overwrite leaves a decision. That case offers the `git fetch` line alone.

What it does have is the opposite operation: a push the guard **withheld** can
be applied on request, because there the write has not happened yet and the
question is whether it should. See [`POST /console/api/force`](#post-consoleapiforce).

<br/>

### POST /console/api/force

Applies a rewind the push guard withheld. The console offers it as a button on
rows that recorded a hold, one button per held ref.

The guard refuses a push that would move a ref **backwards** at the destination:
the destination already contains what this side holds, so the write would
discard commits there rather than mirror them. That refusal is what stops a late
echo from undoing a commit that landed while it was in flight — and it also
stops a rewind somebody meant to make. This route is how a person says which
one it was.

Like retry and restore, no API token is involved.

#### Request Body

```json
{
  "repo": "git-bridge-test",
  "to": "codecommit/git-bridge-test",
  "ref": "refs/heads/version/4.3.0"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `repo` | Yes | `RepoConfig.Name` |
| `to` | Yes | Which side to apply the rewind on, in the `<provider-type>/<path>` form the history event's `to` field carries. A destination rather than a direction, for the same reason restore takes one: the row the operator clicked knows which side it was writing to, and asking the browser to translate that into `source-to-target` invites it to get the translation backwards on the one call whose purpose is to overwrite |
| `ref` | Yes | Full ref. One call moves one ref — there is no repo-wide force |

Unknown fields are rejected rather than ignored.

#### Response

```json
{
  "status": "accepted",
  "repo": "git-bridge-test",
  "ref": "refs/heads/version/4.3.0"
}
```

| Status Code | Description |
|-------------|-------------|
| 202 | Accepted; the sync runs in the background and its outcome appears in the history as a `forced-update` |
| 400 | Missing `repo` / `to` / `ref`, an unknown field, or invalid JSON |
| 404 | Force not wired up (no mirror service). `/console/api/me` reports this as `force_enabled: false` so the page hides the button |
| 405 | Method not allowed (only POST) |
| 409 | No hold matching this repo, destination and ref is recorded in the visible history. Most often that means the hold already resolved on its own, which is the good outcome |
| 415 | `Content-Type` is not `application/json`, the same gate restore and retry carry |

**The 409 is the guard on this route.** Without it this is a general "force any
ref onto either side" API reachable by anyone who can reach the console. With
it, the button can only finish something the mirror already declined and wrote
down. The check reads the same bounded tail the page renders, so a hold old
enough to have aged out is no longer clickable — the command in the Slack alert
still works, and reaching further back than the console shows should be a
deliberate act rather than a click.

**Asynchronous, unlike restore.** This runs a full mirror sync, and a fetch of a
large repository outlasts any sensible request timeout. Refusals from inside the
sync — a `direction` rule, a `ref_overrides` pin — are recorded in the history
rather than returned here.

What the force does **not** skip is the lease. The push still carries the
destination tip it was checked against, so a commit arriving between the check
and the write is still refused. Authorising a rewind of the tip you were shown
is not the same as authorising the overwrite of whatever lands while you decide.

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
- Visibility timeout: the consumer's `visibility_timeout_seconds`, which defaults to `mirror.timeout_seconds` when unset and may not be lower than it (dev runs 600s). See [ADVANCE.md](ADVANCE.md#visibility_timeout_seconds)
- On success: message deleted from queue
- On failure: message remains, retried up to 5 times → DLQ

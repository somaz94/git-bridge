# GitLab ↔ GitLab Bidirectional Mirror Setup

Setup procedure for mirroring between a legacy GitLab instance (`gitlab-old.example.com`, 13.12)
and the current one (`gitlab.example.com`, 19.2) with git-bridge — and the **constraints that
only surface in this combination**.

Unlike the existing pairs (CodeCommit ↔ GitLab), **both providers here share the same type**, so
adding a repo entry naively misbehaves in five places. Config validation refuses startup for the
path collision (constraint 2); the rest it does not catch. Read the Constraints section before
the procedure.

<br/>

## Layout

| Role | Instance | Path | Provider name |
|---|---|---|---|
| source | `gitlab-old.example.com` (13.12) | `backup/git-bridge-test` | `gitlab-old` |
| target | `gitlab.example.com` (19.2) | `test/git-bridge-test` | `gitlab-main` |

```yaml
repos:
  - name: gitlab-migration-test
    source: gitlab-old
    target: gitlab-main
    source_path: backup/git-bridge-test
    target_path: test/git-bridge-test
    direction: bidirectional
    retry_direction: target-to-source
    slack_webhook_url: "${GIT_BRIDGE_TEST_SLACK_WEBHOOK_URL}"
```

<br/>

## Constraints — read first

<br/>

### 1. The webhook tells the instances apart by the payload's host

Events from both instances arrive at the same endpoint (`/webhook/gitlab`), so the route alone
only identifies the provider **type `"gitlab"`**. The handler therefore matches the push payload's
`project.web_url` host against the providers' `base_url` hosts to narrow it down to the provider
**name**.

- `internal/config/config.go` `HostResolver` — index of `base_url` host → provider name
- `internal/consumer/webhook.go` `dispatchPushEvent` — passes the name when narrowed, the type otherwise
- `internal/mirror/mirror.go` `providerMatches` — name first, type as the fallback

```
Route: gitlab-old/team/test-repo → gitlab-main/team/test-repo
```

Narrowing fails in three cases.

| Failure | Cause |
|---|---|
| The provider has no `base_url` | It never enters the index |
| The payload carries no `web_url` | Older instance |
| The host matches no `base_url` | Typo in `base_url`, or a reverse proxy rewriting the host |

Two providers pointing at the **same host** drop out of the index as well — the host cannot tell
them apart either, and picking one arbitrarily would mirror the wrong way silently.

When narrowing succeeds, the narrowed name rides along in the log's `provider_name` field. That
field is how you confirm two repos sharing a path actually split: push on one instance and
`provider_name` must name that instance.

```
INFO  mirror sync done   from=gitlab-old/group/app to=gitlab-main/group/app
ERROR mirror sync failed provider=gitlab provider_name=gitlab-main
      error=repo "app" direction "source-to-target" does not allow target-to-source sync
```

On failure the handler matches by type exactly as it did before narrowing existed, and logs a
warning:

```
WARN webhook instance URL matches no provider base_url, dispatching by provider type
  instance_url=http://gitlab-old.example.com/backup/git-bridge-test
WARN webhook payload carries no instance URL, dispatching by provider type
```

> 🔴 **`source_path` and `target_path` must differ wherever the fallback can be reached.**
> If they match, the loop checks the target condition first, so a push on the source side
> (gitlab-old) also matches as a target and syncs `target → source` — attempting to rewind the
> commit that was just pushed. The rewind guard in `planPush` prevents actual loss, but this is
> not correct behavior.

Here the groups are split — `backup/` for source, `test/` for target. That is safe whether or not
host narrowing works, and **splitting the paths stays the recommendation** for a new pair. Use
identical paths only after confirming **in the logs** that both instances do send `web_url`.

<br/>

### 2. A (provider, path) cannot have two owners — startup is refused

`SyncByTarget` / `SyncDeleteByTarget` **return on the first match**, so if two places claim the
same (provider name, path) the later one never runs. That is a genuine collision — the host cannot
separate them either.

`config.validate()` rejects that collision **at startup**. It used to check duplicate names only,
so the later entry died silently with no error and no log line; now the service refuses to come up
and says so:

```
repo[1] b: source endpoint "gitlab-main/shared" collides with a (target) —
webhooks dispatch by provider, so only the first would ever run
```

> 🔴 Do not reuse `gitlab-main:team/test-repo` as this entry's `target_path`. That path
> already belongs to the `git-bridge-test` (CodeCommit ↔ GitLab) entry.

That is why the target is a new path (`test/git-bridge-test`) as well.

The key is the **provider name**, not the type. Different instances (`gitlab-old` vs
`gitlab-main`) therefore pass validation even on the same path — constraint 1's host narrowing is
what separates them. But such a config falls back to first-match the moment narrowing fails, so
read constraint 1's warning first.

<br/>

### 3. Mirror caches are keyed by provider name

The mirror cache path is `<work_dir>/<repo name>-<provider name>.git`
(`mirrorDirFor` in `internal/mirror/mirror.go`).

It used to use the provider **type**, which meant a gitlab↔gitlab pair shared one
`<repo>-gitlab.git` between both directions, each `fetch --prune` deleting and restoring the
other side's refs. The provider name is the config key, so it is unique per direction.

If a type-named cache is still present, `mirrorDirFor` **renames it once to adopt the cache** —
a lazy migration that happens the first time each direction runs, with no batch step:

```
INFO migrated mirror cache to provider-name path
  from=/tmp/git-bridge/demo-repo-gitlab.git to=/tmp/git-bridge/demo-repo-gitlab-main.git
```

Triggering the reconcile CronJob once after deploy migrates every affected cache:

```bash
kubectl -n git-bridge create job --from=cronjob/git-bridge-reconcile reconcile-migrate
```

<br/>

### 4. Route labels use the provider name

Route labels in logs, Slack, and history are `<provider name>/<path>` (`endpoint` in
`internal/mirror/mirror.go`).

They used the provider **type** before, so both sides of a gitlab↔gitlab pair read as `gitlab/...`
and the two instances were indistinguishable. A provider name is the config key, so it is unique
per instance.

```
Route: gitlab-old/backup/git-bridge-test → gitlab-main/test/git-bridge-test
```

History rows already on the volume still carry the type-based notation, and the console's restore /
force-push buttons echo that row's `To` value back verbatim. `RestoreRef` / `ForcePush` therefore
accept **both notations** via `endpointMatches`. Newly recorded values always use the new notation.

On a pair that mirrors the two instances over the **same path** (constraint 1), though, both sides
collapse to the same old-notation string, `gitlab/<that path>`. Picking one would re-run the opposite
leg, so `DirectionTo` / `RestoreRef` / `ForcePush` **refuse rather than guess** (`endpointAmbiguous`
in `internal/mirror/mirror.go`). Only the buttons on rows recorded under the old notation stop
working for such a repo; rows recorded under the new notation are unaffected.

<br/>

### 5. A push touching more than 3 refs delivers no webhook

That is the instance setting `push_event_hooks_limit` (default **3**). When a single push changes
more branches or tags than that, **GitLab does not fire the webhook at all.** The hook config can
be perfectly valid and the delivery log stays empty, so it looks like a broken hook.

```bash
# check the current value (requires admin)
glab api application/settings | grep push_event_hooks_limit
```

For the mirror this means **that push does not propagate**. Initial syncs that bring up several
branches at once, and bulk cleanups, land squarely in it.

What covers it is a **periodic reconcile** — a CronJob that calls `POST /retry/mirror` for each
repo on a schedule (hourly here, so up to an hour of lag). That is why adding a new repo to the
reconcile's repo list is mandatory rather than optional — webhooks alone leave this hole. The
manifests in `k8s/` do not ship such a CronJob; see [Retry API](retry-api.md) for the call it
makes.

<br/>

## Mirroring a large repository

`mirror.timeout_seconds` is **one budget shared by the clone and the push**, and the initial sync
is a full clone, so it runs straight into it. Measured on a ~13 GB repository:

| Container CPU limit | Transfer | Delta resolution (index-pack) | Total |
|---|---|---|---|
| `500m` | ~12 min | ~14 min | **26 min 42 s** |
| `2` | 4 min 26 s | ~3 min | **7 min 30 s** |

Two things to take from it.

- **Delta resolution costs as much as the transfer, and it is CPU-bound.** Check the CPU limit
  before blaming the link. During that phase the cache directory stops growing, so it looks stalled.
- **Raise `timeout_seconds` for the initial sync and put it back afterwards.** Raise
  `consumers[].visibility_timeout_seconds` with it — validation requires it to be at least
  `mirror.timeout_seconds`, so leaving it behind refuses startup.

A timeout does not damage the cache. The clone lands in `<dir>.tmp` and is moved into place only
on success (`CloneMirror`), so a failure removes the temporary copy and leaves the cache intact.

<br/>

## Step 1 — Remote repositories

Create a new repo on each side. Mind constraint 1 (differing paths are the safe choice) and constraint 2 (do not
reuse an existing entry's path).

```bash
# current instance — group + repo
glab api -X POST groups -f name=Test -f path=test -f visibility=private
glab api -X POST projects -f name=git-bridge-test -f path=git-bridge-test \
  -f namespace_id=<TEST_GROUP_ID> -f visibility=private

# legacy instance — repo under the backup group
GITLAB_HOST=gitlab-old.example.com glab api -X POST projects \
  -f name=git-bridge-test -f path=git-bridge-test \
  -f namespace_id=<BACKUP_GROUP_ID> -f visibility=private
```

Register the legacy instance with `glab` first. **Both instances are HTTP-only**, so
`--api-protocol http` is required — without it glab tries HTTPS and every call fails.

```bash
glab auth login --hostname gitlab-old.example.com \
  --api-protocol http --git-protocol ssh --stdin
```

> `--stdin` prints no prompt. If it looks frozen, it is waiting — paste the token and press Ctrl-D.

<br/>

## Step 2 — Service account and token

git-bridge holds **one token per provider**, and that token must cover **every repo** the provider
will ever mirror. That rules out most token types.

| Token type | Coverage | Verdict |
|---|---|---|
| Project access token | 1 repo | ❌ re-seal and redeploy for every repo added |
| Group access token | whole group | ❌ requires GitLab **14.7+** — unavailable on 13.12 |
| User PAT | everything the user can reach | ✅ the only option |

Create a dedicated service account and give it coverage through **group Maintainer membership**,
so adding a repo to the group needs no token or config change.

```bash
# 1) service account (non-admin)
GITLAB_HOST=gitlab-old.example.com glab api -X POST users \
  -f username=git-bridge -f name="Git-Bridge Mirror" \
  -f email=git-bridge@example.com \
  -f skip_confirmation=true -f force_random_password=true

# 2) Maintainer (40) on the group being mirrored
GITLAB_HOST=gitlab-old.example.com glab api -X POST groups/<GROUP_ID>/members \
  -f user_id=<UID> -f access_level=40

# 3) issue the PAT — straight to a file, never to the screen
GITLAB_HOST=gitlab-old.example.com glab api --method POST \
  "users/<UID>/personal_access_tokens?name=git-bridge-mirror&scopes%5B%5D=read_repository&scopes%5B%5D=write_repository" \
  | python3 -c 'import json,sys;raw=sys.stdin.read();i=raw.find("{");d,_=json.JSONDecoder().raw_decode(raw[i:]);open("/tmp/gb-old-token.txt","w").write(d["token"]);print("scopes:",d.get("scopes"),"| expires_at:",d.get("expires_at"))'
chmod 600 /tmp/gb-old-token.txt
```

**`read_repository` + `write_repository` are sufficient.** git-bridge never calls the GitLab API —
`internal/provider/gitlab.go` only assembles clone URLs — and talks git over
`http://oauth2:<token>@host/path.git`. Leaving `api` out means a leaked token cannot touch the API.

> ⚠️ **Passing `-f "scopes[]=..."` returns HTTP 400.** `glab api` sends the POST body as JSON, so
> `scopes[]` becomes a literal key and GitLab rejects the request for a missing `scopes`.
> Pass them in the **query string** (`scopes%5B%5D=`) as above.

> Omitting `expires_at` yields a non-expiring token on 13.12 (`expires_at: None`). Newer GitLab
> versions enforce expiry, so verify before using the same approach on the current instance.

Verify both read and write:

```bash
TOK=$(cat /tmp/gb-old-token.txt)
git -c credential.helper= ls-remote "http://oauth2:${TOK}@gitlab-old.example.com/backup/git-bridge-test.git"
# write: push a throwaway branch, then delete it
```

Group Maintainer satisfies a protected branch whose push access is *Maintainers*. Note that on a
branch with `allow_force_push: false`, a divergence results in a rejected push rather than a
forced one — nothing is lost, but it only shows up in the logs.

<br/>

## Step 3 — Store the secrets

The second instance needs two new keys, `GITLAB_OLD_BASE_URL` and `GITLAB_OLD_TOKEN`. Add them to
the `git-bridge-secret` Secret in `k8s/secret.yaml`.

Never commit a real token. Keep the value in a pipe so it does not reach the screen or the shell
history, and let an encrypting operator (SealedSecrets, External Secrets, SOPS — whichever the
cluster already runs) hold the ciphertext:

```bash
kubectl -n git-bridge create secret generic git-bridge-secret \
  --dry-run=client -o yaml \
  --from-literal=GITLAB_OLD_BASE_URL="http://gitlab-old.example.com" \
  --from-file=GITLAB_OLD_TOKEN=/tmp/gb-old-token.txt

shred -u /tmp/gb-old-token.txt
```

<br/>

## Configuration changes

| File | Change |
|---|---|
| `k8s/configmap.yaml` | add `providers.gitlab-old`, add the `gitlab-migration-test` repo |
| `k8s/deployment.yaml` | `GITLAB_OLD_BASE_URL` / `GITLAB_OLD_TOKEN` env vars |
| `k8s/secret.yaml` | both keys |

There is only **one** `webhook.gitlab_secret`. Use the **same token** on both instances' webhooks.

Validate:

```bash
kubectl kustomize k8s > /dev/null   # manifest parses
make test                           # includes config load/validation
```

`.githooks/pre-commit` writes the image tag into the commit automatically. Without the hook, the
CI `image_tag` job catches the drift.

<br/>

## Webhook registration

Register on **both** repos — otherwise only one direction works.

| Field | Value |
|---|---|
| URL | `http://git-bridge.example.com/webhook/gitlab` |
| Secret token | value of `WEBHOOK_GITLAB_SECRET` (same on both instances) |
| Trigger | **Push events** + **Tag push events** (both) |
| SSL verification | off (HTTP) |

> 🔴 Legacy GitLab (13.12) **blocks webhooks to private ranges** by default. If the git-bridge host
> resolves to a private IP, *Allow requests to the local network from web hooks and services* must
> be enabled under Admin Area → Settings → Network → Outbound requests. Check with:
> ```bash
> GITLAB_HOST=gitlab-old.example.com glab api application/settings \
>   | python3 -c 'import json,sys;raw=sys.stdin.read();i=raw.find("{");d,_=json.JSONDecoder().raw_decode(raw[i:]);print(d["allow_local_requests_from_web_hooks_and_services"])'
> ```

<br/>

## Verification

After deploying, walk through:

1. **Startup** — `config loaded` shows the higher `repos` / `providers` counts
2. **Initial sync** — trigger the reconcile CronJob, or the retry API
3. **source → target** — push on gitlab-old, confirm it lands on the target
4. **target → source** — push on gitlab-main, confirm it lands on the source
5. **Echo terminates** — the reverse event ends in `already-up-to-date` (no infinite loop)
6. **Tags** — tag push in both directions
7. **Delete propagation** — branch and tag deletion reaches the other side
8. **Rewind guard** — diverge the two sides deliberately, confirm `held` / `destination-ahead`
9. **Slack** — success and failure notifications reach the intended channel

What to look for in the logs:

```bash
kubectl -n git-bridge logs deploy/git-bridge --since=10m \
  | grep -E "received gitlab push event|fetching from source|pushing to target|mirror sync done|nothing to push"
```

`fetching from source (incremental)` is the healthy line. Repeated
`cloning from source (initial)` means the cache is being discarded every time.

History:

```bash
kubectl -n git-bridge exec deploy/git-bridge -- \
  wget -q -O- 'http://127.0.0.1:8081/console/api/history?limit=10'
```

<br/>

## Known limitations

- **Identical paths on both instances depend on host narrowing** (constraint 1). It holds only if
  both providers carry a `base_url` and both instances put `project.web_url` in the payload. If
  either side fails to narrow, dispatch falls back to type matching, first-match comes back into
  play, and **the direction can invert**. Config validation does not catch this — the failure
  surfaces only as a `dispatching by provider type` warning at runtime, so check the log on the
  first push after building such a pair.
- **One path cannot participate in several mirror pairs** (constraint 2). Supporting that requires
  changing `SyncByTarget` / `SyncDeleteByTarget` from first-match-return to **fan-out over every
  match**, and relaxing that validation at the same time.
- **A push touching 4 or more refs delivers no webhook** (constraint 5). The reconcile CronJob
  catches up within the hour, but it is not real-time propagation.
- `allow_force_push: false` on a protected branch turns a divergence into a rejected push.

<br/>

## Teardown

1. Delete the webhooks on both repos
2. Remove the repo entry and the `gitlab-old` provider from `k8s/configmap.yaml`
3. Remove it from the reconcile CronJob's repo list, if you run one
4. Remove the two env vars from `k8s/deployment.yaml` and the two keys from `k8s/secret.yaml`
5. After deploying, delete the `gitlab-migration-test-*.git` caches from the PVC (nothing prunes them)
6. Remote cleanup — both repos, the `test` group, the `git-bridge` service account and its PAT

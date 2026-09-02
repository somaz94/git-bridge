# Slack App Setup

Git-Bridge supports two methods for Slack notifications:

1. **Incoming Webhook** (simple) — just a webhook URL, no app needed
2. **Slack App Bot** (advanced) — interactive notifications with richer features

<br/>

## Method 1: Incoming Webhook (Default)

The simplest way. No Slack App required.

1. Go to https://api.slack.com/apps → **Create New App** → **From scratch**
2. Navigate to **Incoming Webhooks** → Toggle **Activate**
3. Click **Add New Webhook to Workspace**
4. Select a channel → **Allow**
5. Copy the webhook URL

```
SLACK_WEBHOOK_URL: "https://hooks.slack.com/services/T.../B.../..."
```

That's it. Git-Bridge will POST sync notifications to this URL.

<br/>

## Method 2: Slack App Bot (Advanced)

Create a Slack App for more interactive and customizable notifications.

<br/>

### 1. Create Slack App

1. Go to https://api.slack.com/apps
2. Click **Create New App** → **From scratch**
3. App Name: `Git Bridge` (or any name you prefer)
4. Select your Workspace → **Create App**

<br/>

### 2. Configure Bot Token Scopes

Navigate to **OAuth & Permissions** → **Scopes** → **Bot Token Scopes**, and add:

| Scope | Description |
|-------|-------------|
| `chat:write` | Post messages to channels |
| `files:write` | Upload files (e.g. sync reports) |
| `incoming-webhook` | Post via incoming webhooks |
| `channels:read` | View public channel info |
| `groups:read` | View private channel info |
| `groups:write` | Manage private channels |

<br/>

### 3. Install App to Workspace

1. Navigate to **OAuth & Permissions**
2. Click **Install to Workspace** → **Allow**
3. Copy the **Bot User OAuth Token** (`xoxb-...`)

<br/>

### 4. Invite Bot to Channel

After installing, the bot needs to be added to the target channel:

1. Create or open the channel where you want notifications
2. Type `/invite @Git Bridge` (or your app name)
3. The bot is now ready to post to this channel

<br/>

### 5. Configure Git-Bridge

Use the **Incoming Webhook URL** from the app (not the Bot Token) in git-bridge config:

```yaml
# K8s Secret
SLACK_WEBHOOK_URL: "https://hooks.slack.com/services/T.../B.../..."
```

> **Note**: Git-Bridge currently uses the Incoming Webhook format for notifications.
> The Bot Token (`xoxb-...`) is for Slack API calls if you extend the notification system.

<br/>

### 6. Generate Incoming Webhook (via Slack App)

Even with a Slack App, you can generate Incoming Webhooks:

1. Go to your app settings → **Incoming Webhooks**
2. Toggle **Activate Incoming Webhooks** → On
3. Click **Add New Webhook to Workspace**
4. Select the channel → **Allow**
5. Copy the webhook URL and set it as `SLACK_WEBHOOK_URL`

<br/>

## Notification Format

Git-Bridge sends notifications in the following cases:

| Event | Title | Level | Description |
|-------|-------|-------|-------------|
| Mirror sync success | `Mirror Sync` | `success` (✅) | Repository mirrored successfully |
| Mirror sync failure | `Mirror Sync Failed` | `error` (❌) | Clone or push failed |
| Ref delete success | `Ref Deleted` | `success` (✅) | Branch/tag deleted from target. The body carries `Deleted tip: <sha>` plus a two-command `Restore if needed:` block. A delete is the one operation that leaves nothing behind to look up — afterwards the destination names neither the ref nor the commit — so this is the only record of what was removed. It rides on every successful delete, including routine cleanup of a merged branch, so the label stays conditional rather than reading like an incident |
| Ref delete failure | `Ref Delete Failed` | `error` (❌) | Failed to delete ref |
| Ref restore success | `Ref Restored` | `success` (✅) | A console click put back a ref a delete removed, at the tip that delete recorded. The body carries `Restored tip: <sha>` and `Restored by: <actor>`. The actor is on it because this writes to a real repository — unlike a webhook or SQS event, which has a pusher rather than an operator, someone chose to do this and the channel is where that becomes visible |
| Ref restore failure | `Ref Restore Failed` | `error` (❌) | The restore was refused or failed. The body carries `Requested by: <actor>` and `Error: <what>`. All three exits send the same shape — the ref already exists on the destination, git has garbage-collected the commit, or the push failed — so a refusal is as legible in the channel as a success. A refusal is the expected outcome when someone re-created the branch in the meantime, not an incident |
| Forced overwrite | `Forced Update` | `error` (❌) | The push succeeded, but at least one **branch** was overwritten non-fast-forward, so commits reachable only from the old tip are gone from the destination. The body lists each overwritten ref as `<ref>: <old> → <new>` plus a `git fetch <clone-url> <old>` recovery line per branch. It **replaces** the success notification for that push, rather than arriving alongside it — the two together would read as a contradiction — which is why it carries route, duration and target itself |

> No notification is sent when the push is already up-to-date (loop detection).
>
> A forced update that only moved **tags** is recorded in the history but sent no
> alert: a pipeline that reuses build tag names re-points them constantly, and an
> alert that fires on routine traffic is one people learn to ignore.
>
> The notifier also understands a `warning` (⚠️) level, but nothing currently
> emits it — the forced-overwrite alert is `error`, so it is not filtered out of a
> channel watching only failures.
>
> Every message is sent as a Slack **attachment** with a colour bar keyed to the
> level: `success` green (`#2eb886`), `warning` amber (`#daa038`), `error` red
> (`#a30200`); an unknown level falls back to green, matching the ✅ prefix.
> The colour is the point of using an attachment — Block Kit has no colour, and
> the emoji alone does not carry far enough when scrolling a channel of routine
> green syncs. Because the body lives inside the attachment, the top-level
> `text` is deliberately empty (filling it renders the same message twice); the
> lock-screen preview uses the attachment's `fallback`, which carries the title
> only, so a notification is not flooded with SHAs and URLs.
>
> Destination URLs are rendered as Slack links (`<url|label>`) rather than raw
> addresses. The attachment column is narrower than a plain message, and a raw
> CodeCommit console URL wrapped onto three lines there and pushed the message
> past Slack's collapse threshold, adding a "show more" control. A SHA on its
> own stays plain text — it is read and compared, not clicked — while the git
> commands in the delete alert sit in a code block, which is what earns them a
> copy button.

<br/>

### Example: Mirror Sync Success (Branch Push)

```
✅ Mirror Sync: my-repo
Action: branches + tags synced
Route: codecommit/my-repo → gitlab/team/my-repo
Duration: 1.234s
Target: gitlab/team/my-repo          ← link to https://gitlab.example.com/team/my-repo
Branch: main
Pushed by: alice
```

### Example: Mirror Sync Success (Tag Push)

```
✅ Mirror Sync: my-repo
Action: branches + tags synced
Route: codecommit/my-repo → gitlab/team/my-repo
Duration: 0.876s
Target: gitlab/team/my-repo          ← link to https://gitlab.example.com/team/my-repo
Tag: v1.0.0
Pushed by: alice
```

> **Pushed by** shows the commit author of the pushed ref, extracted via `git log -1 --format=%an <ref>` from the mirror directory after push. This works consistently across all providers (GitLab, GitHub, CodeCommit/SQS). If no ref info is available, this field is omitted.
>
> **Branch** or **Tag** is shown based on the pushed ref. For tag pushes, only the tag name is displayed (no branch). For SQS events, the ref is derived from the `referenceName` and `referenceType` fields.

<br/>

### Example: Ref Deleted

~~~
✅ Ref Deleted: git-bridge-test
Action: delete branch 'delete-tip-probe'
Route: gitlab/team/test-repo → codecommit/git-bridge-test
URL: codecommit/git-bridge-test      ← link to the CodeCommit console
Deleted tip: c70e089e6660bfe57cf28a6df84aafad0e7e2b69
Restore if needed:
```
git fetch <clone-url> c70e089e6660bfe57cf28a6df84aafad0e7e2b69
git push <clone-url> c70e089e6660bfe57cf28a6df84aafad0e7e2b69:refs/heads/delete-tip-probe
```
~~~

> **Deleted tip** is read from the destination with `git ls-remote` immediately
> before the delete. It is a full 40-character object name on purpose: a remote
> rejects an abbreviated one, so a short SHA would be a record nobody could act
> on. Substitute the destination's clone URL — named by the `Route:` line — for
> `<clone-url>`; the URL is never printed because it carries the credentials.
> git keeps the objects until it garbage-collects, so the window is real but
> not indefinite.
>
> **Both commands are needed.** The fetch only pulls the objects into a local
> clone; it leaves the destination exactly as deleted. The push is what
> re-creates the ref, which is why the full ref (`refs/heads/…` or `refs/tags/…`)
> is spelled out rather than left for the reader to assemble. They are rendered
> as a code block so Slack attaches a copy button and the inevitable wrapping —
> a `<clone-url>` plus a 40-character SHA plus a full ref does not fit the
> attachment column — reads as formatting rather than a broken line. The tip
> above stays plain text: it is compared against the history, not pasted.
>
> **The forced-update alert deliberately does not match this.** There, the ref
> still exists and points at something newer, so force-pushing the old tip back
> would destroy whatever legitimately landed in the meantime. It offers the
> `git fetch` alone — retrieve the commits and decide — because the restore is a
> judgement call, not a paste.
>
> Only a delete that actually removed something sends this. The echo coming back
> from the other side finds the ref already gone and ends as a silent no-op, so
> one deletion produces one message, not two.
>
> **The two commands are also a button in the console.** A delete row that
> recorded a tip carries a restore button that does exactly this, server-side —
> see the two examples below. The commands stay in the message because Slack is
> where the delete is noticed, and reading them is how someone decides whether
> the console is even worth opening.

<br/>

### Example: Ref Restored

```
✅ Ref Restored: git-bridge-test
Action: restore branch 'delete-tip-probe'
Route: gitlab/team/test-repo → codecommit/git-bridge-test
URL: codecommit/git-bridge-test      ← link to the CodeCommit console
Restored tip: c70e089e6660bfe57cf28a6df84aafad0e7e2b69
Restored by: alice
```

### Example: Ref Restore Failed

```
❌ Ref Restore Failed: git-bridge-test
Action: restore branch 'delete-tip-probe'
Route: gitlab/team/test-repo → codecommit/git-bridge-test
Requested by: alice
Error: refs/heads/delete-tip-probe already exists at 3d9a1c4f0b77e2a58c61d4b0f9e3a72c15d8b604 on codecommit/git-bridge-test
```

> **Restored tip** is the same SHA the delete message carried, which is the point
> of recording it: the restore does not guess a tip, it replays the one that was
> written down. It stays plain text like the deleted tip — read and compared, not
> pasted — and no code block appears here at all, because the whole reason this
> message exists is that nobody had to run those commands by hand.
>
> **Restored by / Requested by** is the portal identity of whoever clicked,
> carried into the history event's `actor` as well. A webhook or SQS event has a
> pusher; this has an operator, and the difference is worth seeing in the channel
> because a restore writes to a real repository.
>
> **The failure above is a refusal, not a fault.** The ref was back on the
> destination, so the restore declined rather than overwrite whoever put it
> there — the same accident it exists to undo, with a different victim. The other
> two exits send the identical shape with a different `Error:` line: the commit
> was garbage-collected (`is no longer available`), or the push itself failed.
> Keeping all three identical is deliberate, so a refusal is as legible as a
> success at a glance.
>
> A restore of a ref that is already at exactly the requested tip is a silent
> no-op — it sends nothing, the same way a delete of an already-absent ref does.

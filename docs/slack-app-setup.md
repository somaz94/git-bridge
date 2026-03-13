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

| Event | Level | Description |
|-------|-------|-------------|
| Mirror sync success | `success` | Repository mirrored successfully |
| Mirror sync failure | `error` | Clone or push failed |
| Ref delete success | `success` | Branch/tag deleted from target |
| Ref delete failure | `error` | Failed to delete ref |

> No notification is sent when the push is already up-to-date (loop detection).

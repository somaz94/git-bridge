# GitHub Webhook Setup Guide

This guide explains how to configure GitHub webhooks for Git-Bridge.

Webhook setup is **required** when the mirror direction is `target-to-source` or `bidirectional`. If you only use `source-to-target` with CodeCommit as source, SQS handles event delivery automatically and no webhook is needed.

<br/>

## Prerequisites

- GitHub repository with Admin access
- Git-Bridge deployed and accessible (e.g., `http://git-bridge.example.com`)
- `WEBHOOK_GITHUB_SECRET` configured in K8s Secret (optional but recommended)

<br/>

## Step-by-Step Setup

<br/>

### 1. Open Webhook Settings

Navigate to your GitHub repository:

```
Settings > Webhooks > Add webhook
```

<br/>

### 2. Configure Webhook

| Field | Value |
|-------|-------|
| **Payload URL** | `http://git-bridge.example.com/webhook/github` |
| **Content type** | `application/json` (MUST be JSON, not form-urlencoded) |
| **Secret** | Value of `WEBHOOK_GITHUB_SECRET` |
| **Which events would you like to trigger this webhook?** | Just the push event |
| **Active** | Checked |

<br/>

### 3. Select Events

Select **Just the push event**. GitHub's push event covers both branch pushes and tag pushes.

Do not select "Send me everything" — Git-Bridge only processes push events and will ignore all other event types.

<br/>

### 4. Save and Test

1. Click **Add webhook**
2. GitHub will send a `ping` event automatically (Git-Bridge will return 400 for ping — this is expected)
3. Push a commit to the repository to trigger a real push event
4. Go to **Settings > Webhooks > (your webhook) > Recent Deliveries** to verify HTTP 200 response

You can also verify by checking Git-Bridge logs:

```bash
kubectl logs -n git-bridge -l app=git-bridge -f
```

<br/>

## Secret (HMAC-SHA256)

GitHub uses HMAC-SHA256 to sign webhook payloads. This is different from GitLab's simple token comparison.

- Set `WEBHOOK_GITHUB_SECRET` in `k8s/secret.yaml` to any value you choose
- Use the same value in the GitHub webhook **Secret** field
- GitHub sends the signature in the `X-Hub-Signature-256` header
- Git-Bridge verifies the signature by computing HMAC-SHA256 of the payload with the shared secret
- If `WEBHOOK_GITHUB_SECRET` is empty, Git-Bridge skips signature verification

<br/>

## Per-Repository Setup

Each GitHub repository that acts as a **target** (in `target-to-source` or `bidirectional` direction) needs its own webhook configured. The same secret can be used across all repositories.

### Example

If your `configmap.yaml` has:

```yaml
repos:
  - name: app
    source: codecommit
    target: github
    target_path: org/app
    direction: bidirectional       # webhook required

  - name: lib
    source: codecommit
    target: github
    target_path: org/lib
    direction: source-to-target   # webhook NOT required

  - name: docs
    source: codecommit
    target: github
    target_path: org/docs
    direction: target-to-source   # webhook required
```

Then you need to configure webhooks on:
- `org/app` (bidirectional)
- `org/docs` (target-to-source)

No webhook is needed for `org/lib` (source-to-target).

<br/>

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| HTTP 401 Unauthorized | HMAC signature mismatch | Ensure `WEBHOOK_GITHUB_SECRET` matches the GitHub webhook secret |
| HTTP 401 Unauthorized | Missing `X-Hub-Signature-256` header | Ensure secret is set in both K8s Secret and GitHub webhook config |
| HTTP 405 Method Not Allowed | Wrong HTTP method | Verify webhook URL is correct and GitHub is sending POST |
| HTTP 400 Bad Request | Invalid payload or ping event | Ping events are expected to return 400; push events should return 200 |
| `parse failed: invalid character` | Content type is `application/x-www-form-urlencoded` | Change Content type to `application/json` in GitHub webhook settings |
| Mirror sync not triggered | Wrong direction | Verify the repo's direction is `target-to-source` or `bidirectional` |
| Mirror sync not triggered | Wrong `target_path` | Ensure `target_path` in config matches the GitHub repository's `full_name` (e.g., `org/repo`) |
| Push to source fails (403) | IAM permission denied | Add `codecommit:GitPush` to the IAM policy for the mirror user |

<br/>

## Verifying Webhook Delivery

In GitHub, go to:

```
Settings > Webhooks > (your webhook) > Recent Deliveries
```

Each delivery shows the request headers, payload, and response for debugging.

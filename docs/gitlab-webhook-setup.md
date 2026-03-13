# GitLab Webhook Setup Guide

This guide explains how to configure GitLab webhooks for Git-Bridge.

Webhook setup is **required** when the mirror direction is `target-to-source` or `bidirectional`. If you only use `source-to-target` with CodeCommit as source, SQS handles event delivery automatically and no webhook is needed.

<br/>

## Prerequisites

- GitLab project with Maintainer or Owner access
- Git-Bridge deployed and accessible (e.g., `http://git-bridge.example.com`)
- `WEBHOOK_GITLAB_SECRET` configured in K8s Secret (optional but recommended)

<br/>

## Step-by-Step Setup

<br/>

### 1. Open Webhook Settings

Navigate to your GitLab project:

```
Settings > Webhooks > Add new webhook
```

<br/>

### 2. Configure Webhook

| Field | Value |
|-------|-------|
| **URL** | `http://git-bridge.example.com/webhook/gitlab` |
| **Secret token** | Value of `WEBHOOK_GITLAB_SECRET` (e.g., `git-bridge-token`) |
| **Trigger** | Push events |
| **SSL verification** | Disable (if using HTTP, not HTTPS) |

<br/>

### 3. Select Events

Only **Push events** is needed. GitLab's push event covers both branch pushes and tag pushes.

Other events (Merge request, Issue, etc.) are not processed by Git-Bridge and can be left unchecked.

<br/>

### 4. Save and Test

1. Click **Add webhook**
2. Scroll down to the webhook list
3. Click **Test** > **Push events**
4. Verify the response returns HTTP 200

You can also verify by checking Git-Bridge logs:

```bash
kubectl logs -n git-bridge -l app=git-bridge -f
```

<br/>

## Secret Token

The secret token is used to verify that incoming webhook requests are genuinely from GitLab. This is optional but recommended for security.

- Set `WEBHOOK_GITLAB_SECRET` in `k8s/secret.yaml` to any value you choose
- Use the same value in the GitLab webhook **Secret token** field
- If `WEBHOOK_GITLAB_SECRET` is empty, Git-Bridge skips token verification

<br/>

## Per-Project Setup

Each GitLab project that acts as a **target** (in `target-to-source` or `bidirectional` direction) needs its own webhook configured. The same secret token can be used across all projects.

### Example

If your `configmap.yaml` has:

```yaml
repos:
  - name: repo-a
    source: codecommit
    target: gitlab
    target_path: server/repo-a
    direction: bidirectional       # webhook required

  - name: repo-b
    source: codecommit
    target: gitlab
    target_path: server/repo-b
    direction: source-to-target   # webhook NOT required

  - name: repo-c
    source: codecommit
    target: gitlab
    target_path: team/repo-c
    direction: target-to-source   # webhook required
```

Then you need to configure webhooks on:
- `server/repo-a` (bidirectional)
- `team/repo-c` (target-to-source)

No webhook is needed for `server/repo-b` (source-to-target).

<br/>

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| HTTP 401 Unauthorized | Secret token mismatch | Ensure `WEBHOOK_GITLAB_SECRET` matches the GitLab webhook secret token |
| HTTP 405 Method Not Allowed | Wrong HTTP method | Verify webhook URL is correct and GitLab is sending POST |
| HTTP 400 Bad Request | Invalid payload | Check GitLab webhook event type is set to Push events |
| Mirror sync not triggered | Wrong direction | Verify the repo's direction is `target-to-source` or `bidirectional` |
| Mirror sync not triggered | Wrong `target_path` | Ensure `target_path` in config matches the GitLab project's `path_with_namespace` |
| Push to source fails (403) | IAM permission denied | Add `codecommit:GitPush` to the IAM policy for the mirror user |

<br/>

## Verifying Webhook Delivery

In GitLab, go to:

```
Settings > Webhooks > (your webhook) > Recent events
```

This shows the delivery history with request/response details for debugging.

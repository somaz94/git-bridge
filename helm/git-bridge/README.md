# git-bridge Helm Chart

Helm chart for [git-bridge](https://github.com/somaz94/git-bridge) — a multi-provider, bidirectional Git repository mirroring service that syncs repositories across CodeCommit, GitLab, and GitHub.

## Install

```bash
helm install my-release ./helm/git-bridge \
  --namespace git-bridge \
  --create-namespace \
  -f my-values.yaml
```

## Uninstall

```bash
helm uninstall my-release -n git-bridge
```

## Values

Key values:

| Key | Description | Default |
|-----|-------------|---------|
| `image.repository` | Container image repo | `somaz940/git-bridge` |
| `image.tag` | Image tag | `v0.1.0` |
| `replicaCount` | Number of replicas | `1` |
| `config` | git-bridge config.yaml (rendered into ConfigMap) | `{}` |
| `secret.create` | Create chart-managed Secret | `true` |
| `secret.existingSecret` | Reference external Secret | `""` |
| `secret.data` | Key-value map of env vars referenced by config | `{}` |
| `persistence.enabled` | Persist git mirror cache via PVC | `false` |
| `persistence.size` | PVC size | `10Gi` |
| `service.type` | Service type | `ClusterIP` |
| `ingress.enabled` | Enable Ingress | `false` |

See `values.yaml` for the full reference and `examples/` for common scenarios.

## Configuration model

The chart renders `.Values.config` as `/etc/git-bridge/config.yaml` inside the pod. The config uses `${ENV_VAR}` placeholders that are expanded at runtime from environment variables — those env vars come from a Kubernetes Secret referenced via `envFrom.secretRef`.

You can either:

1. Let the chart create the Secret — set `secret.create=true` and populate `secret.data` (recommended for local testing / non-sensitive environments).
2. Reference an externally managed Secret (e.g., from External Secrets Operator or Sealed Secrets) — set `secret.create=false` and `secret.existingSecret=<name>`.

## Examples

- `examples/default.yaml` — minimal CodeCommit → GitLab mirror with managed secret
- `examples/webhook-only.yaml` — GitHub ↔ GitLab sync via webhooks only (no SQS)
- `examples/codecommit-multi-region.yaml` — multi-region CodeCommit consumers with existing secret

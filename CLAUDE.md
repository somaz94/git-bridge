# CLAUDE.md - git-bridge

Multi-provider, bidirectional Git repository mirroring tool (CodeCommit, GitLab, GitHub).

## Commit Guidelines

- Do not include `Co-Authored-By` lines in commit messages.
- Do not push to remote. Only commit. The user will push manually.
- Do not modify git config.

## Project Structure

- **Language**: Go 1.26+
- Syncs repos via SQS polling (CodeCommit events) and HTTP webhooks (GitLab/GitHub push events)
- Supports any-to-any mirroring: source-to-target, target-to-source, or bidirectional
- Features: loop detection, DLQ support, Slack notifications, persistent cache (PVC-backed)

## Key Directories

- `cmd/git-bridge/` — Entry point
- `internal/config/` — YAML config with env var expansion
- `internal/consumer/` — SQS & webhook event handlers
- `internal/mirror/` — Incremental git fetch/clone/push operations
- `internal/provider/` — Git provider abstraction (CodeCommit, GitLab, GitHub)
- `internal/notify/` — Slack webhook notifications
- `internal/server/` — HTTP server (health/webhook endpoints)
- `k8s/` — Kubernetes deployment manifests
- `examples/` — Example configs & deployment files

## Build & Test

```bash
make build          # Compile binary
make test           # Unit tests with coverage
make fmt            # Format code
make vet            # Run go vet
make docker-build   # Build Docker image
make docker-buildx  # Multi-arch build (linux/arm64,amd64)
make deploy         # Deploy to Kubernetes
```

## Deployment

```bash
kubectl apply -f k8s
```

## Language

- Communicate with the user in Korean.
- All documentation and code comments must be written in English.

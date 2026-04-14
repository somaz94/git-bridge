# Development

Guide for building, testing, and contributing to git-bridge.

<br/>

## Table of Contents

- [Prerequisites](#prerequisites)
- [Project Structure](#project-structure)
- [Build](#build)
- [Testing](#testing)
- [Linting](#linting)
- [Docker](#docker)
- [Helm Chart](#helm-chart)
- [Local Deploy](#local-deploy)
- [Kubernetes Deploy](#kubernetes-deploy)
- [Version Management](#version-management)
- [Workflow](#workflow)
- [CI/CD Workflows](#cicd-workflows)
- [Conventions](#conventions)

<br/>

## Prerequisites

- Go 1.26+
- Make
- Docker (for container builds)
- Helm 3 (for chart testing / Helm deploys)
- kubectl (for Kubernetes deploys)
- gh CLI (optional, for `make pr`)

<br/>

## Project Structure

```
.
├── cmd/
│   └── git-bridge/
│       └── main.go                   # Entry point (flag parsing, startup)
├── internal/
│   ├── config/                       # Configuration management
│   ├── consumer/                     # Event consumption (SQS, webhooks)
│   ├── mirror/                       # Git mirroring logic
│   ├── notify/                       # Notification handlers (Slack)
│   ├── provider/                     # Git providers (GitHub, GitLab, CodeCommit)
│   ├── server/                       # HTTP server (health, webhooks)
│   └── version/                      # Build-time version metadata (ldflags)
├── examples/
│   ├── config.yaml                   # Documented reference config
│   └── config.local.yaml             # Local-only override (gitignored)
├── k8s/                              # Kubernetes raw manifests
│   ├── namespace.yaml
│   ├── secret.yaml
│   ├── configmap.yaml
│   ├── pvc.yaml
│   └── deployment.yaml
├── helm/
│   └── git-bridge/                   # Helm chart
│       ├── Chart.yaml
│       ├── values.yaml
│       ├── templates/                # 8 templates + tests/
│       └── examples/                 # Scenario-specific values files
├── hack/                             # Build & deploy automation scripts
│   ├── bump-version.sh               # Version bumping across all files
│   ├── test-deploy.sh                # Smoke tests against running server
│   └── test-helm.sh                  # Helm lint + template render tests
├── scripts/
│   └── create-pr.sh                  # GitHub PR creation helper
├── docs/                             # Documentation
├── .github/workflows/                # CI/CD pipelines (10 workflows)
├── Dockerfile                        # Multi-stage build (alpine + git CLI)
├── cliff.toml                        # git-cliff changelog config
└── Makefile                          # Build, test, deploy (sectioned)
```

<br/>

### Key Directories

| Directory | Description |
|-----------|-------------|
| `cmd/git-bridge/` | Application entry point |
| `internal/config/` | YAML config loading, `${ENV_VAR}` expansion, validation |
| `internal/consumer/` | SQS long-polling and webhook event consumers |
| `internal/mirror/` | Core git mirroring logic (fetch/clone/push) |
| `internal/notify/` | Slack notification handlers |
| `internal/provider/` | GitHub, GitLab, CodeCommit provider implementations |
| `internal/server/` | HTTP server for `/health`, `/ready`, `/webhook/*` |
| `internal/version/` | Build-time version metadata injected via ldflags |
| `k8s/` | Kubernetes raw manifests for `make deploy-k8s` |
| `helm/git-bridge/` | Helm chart for production-grade deployments |
| `hack/` | Automation scripts invoked from Makefile |

<br/>

## Build

```bash
make build               # Build binary → bin/git-bridge (with ldflags)
make run                 # Build and run with examples/config.yaml
make install             # Copy binary to /usr/local/bin
make uninstall           # Remove binary from /usr/local/bin
make cross-build         # Build for linux/darwin × amd64/arm64 → dist/
make clean               # Remove bin/, dist/, coverage artifacts
make tidy                # Run go mod tidy
```

The binary is built with `ldflags` injecting:
- `git-bridge/internal/version.Version` — from Makefile `IMG` tag
- `git-bridge/internal/version.GitCommit` — from `git rev-parse --short HEAD`
- `git-bridge/internal/version.BuildDate` — UTC ISO 8601 timestamp

Verify with:

```bash
./bin/git-bridge -version
# git-bridge v0.3.0 (commit: 9c35281, built: 2026-04-14T01:43:38Z)
```

<br/>

## Testing

```bash
make test                # All tests with race detection + coverage
make test-unit           # Unit tests only (./internal/...)
make test-integration    # Integration tests only (TestIntegration*)
make test-helm           # Helm chart lint + template render tests
make cover               # HTML coverage report (coverage.html)
```

Coverage targets (see [testing rule](../CLAUDE.md) — 90%+ minimum, excluding `main.go` / signal handling):

| Package | Coverage | Description |
|---------|----------|-------------|
| `internal/version` | 100% | Build version metadata |
| `internal/config` | 100% | YAML loading, env expansion, validation |
| `internal/provider` | 100% | Provider interface (CodeCommit/GitLab/GitHub) |
| `internal/server` | 100% | HTTP server, health/webhook routing |
| `internal/consumer` | 99%+ | SQS polling, webhook verification |
| `internal/mirror` | 93%+ | Git fetch/clone/push logic |
| `internal/notify` | 90%+ | Slack notification |

<br/>

### Helm Chart Tests

`make test-helm` runs 10 scenarios via `hack/test-helm.sh`:

| Scenario | Description |
|----------|-------------|
| Lint | Chart structure + syntax validation |
| Default values | Renders with no overrides |
| Ingress enabled | Ingress resource rendered |
| Persistence enabled | PVC + volume mount |
| Managed secret | Chart-created Secret with `secret.data` |
| Existing secret | External Secret reference |
| Full options | Multiple overrides simultaneously |
| Example: default | CodeCommit → GitLab with managed secret |
| Example: webhook-only | GitHub ↔ GitLab via webhooks |
| Example: codecommit-multi-region | Multi-region CodeCommit |

<br/>

### Smoke Tests

`make deploy-smoke` runs `hack/test-deploy.sh` against a live server on `localhost:8080`:

- `/health`, `/ready` → 200
- `/webhook/github`, `/webhook/gitlab` (no signature) → 4xx
- `GET /webhook/*` → 405
- Unknown paths → 404

<br/>

## Linting

```bash
make fmt                 # go fmt ./...
make vet                 # go vet ./...
make lint                # golangci-lint run (auto-installs v2.1.6 → ./bin/)
make lint-fix            # golangci-lint run --fix
```

`golangci-lint` is downloaded into `./bin/golangci-lint` on first use — no global install required.

<br/>

## Docker

```bash
make docker-build                    # Build image (single arch) with build args
make docker-push                     # Push image
make docker-buildx-tag               # Multi-arch (arm64 + amd64), version tag
make docker-buildx-latest            # Multi-arch, latest tag
make docker-buildx                   # Both version + latest
```

Build args automatically injected:
- `VERSION` — from `IMG` tag
- `GIT_COMMIT` — `git rev-parse --short HEAD`
- `BUILD_DATE` — UTC ISO 8601

OCI image labels set from build args (`org.opencontainers.image.*`).

- **Builder**: `golang:1.26-alpine` with cache mounts for `/go/pkg/mod` + `/root/.cache/go-build`
- **Runtime**: `alpine:3.23` with `git`, `ca-certificates`, `tzdata` (git CLI is required for mirroring)
- **User**: non-root (`appuser`, UID 1000)
- **Port**: 8080

<br/>

## Helm Chart

Chart lives at `helm/git-bridge/`.

```bash
helm lint ./helm/git-bridge
helm template test-release ./helm/git-bridge
helm install git-bridge ./helm/git-bridge \
  --namespace git-bridge --create-namespace \
  -f helm/git-bridge/examples/default.yaml
```

Scenario examples under `helm/git-bridge/examples/`:

- `default.yaml` — CodeCommit → GitLab with chart-managed Secret
- `webhook-only.yaml` — GitHub ↔ GitLab bidirectional via webhooks (no SQS)
- `codecommit-multi-region.yaml` — Multi-region SQS + external Secret

Secret options:
- **Chart-managed** — set `secret.create=true` + `secret.data={...}` (testing / trivial envs)
- **External** — set `secret.create=false` + `secret.existingSecret=<name>` (ESO, Sealed Secrets, etc.)

<br/>

## Local Deploy

```bash
make deploy              # Build + run binary (uses examples/config.local.yaml if present)
make deploy-smoke        # Run hack/test-deploy.sh on localhost:8080
make deploy-all          # deploy + smoke in one step
make undeploy            # Stop local process

make deploy-docker       # Run Docker container (pulls if not local)
make undeploy-docker
```

For local smoke tests without real AWS / provider credentials, create `examples/config.local.yaml` (gitignored) with empty `consumers: []` and dummy providers — `make deploy` automatically picks it up over `examples/config.yaml`.

<br/>

## Kubernetes Deploy

### Raw manifests (`k8s/`)

```bash
make deploy-k8s          # Apply all manifests + wait for rollout
make undeploy-k8s        # Remove all manifests
make restart             # kubectl rollout restart
make logs                # Tail pod logs
```

### Helm chart (`helm/git-bridge/`)

```bash
helm install git-bridge ./helm/git-bridge -n git-bridge --create-namespace
helm upgrade git-bridge ./helm/git-bridge -n git-bridge -f my-values.yaml
helm uninstall git-bridge -n git-bridge
```

<br/>

## Version Management

See [docs/version.md](version.md) for full details.

Quick reference:

```bash
make version                          # Show current version across all files
make bump-version VERSION=v0.4.0      # Bump Makefile, Chart.yaml, values.yaml, k8s/, README
```

Tagging `vX.Y.Z` on `main` triggers `release.yml` + `helm-release.yml` workflows.

<br/>

## Workflow

```bash
make check-gh                                    # Verify gh CLI ready
make branch name=gitlab-provider                 # Create feat/gitlab-provider from main
make pr title="feat: add GitLab provider"        # Test → push → create PR
```

<br/>

## CI/CD Workflows

| Workflow | Trigger | Description |
|----------|---------|-------------|
| `test.yml` | push, PR | Run tests with Go version from go.mod |
| `lint.yml` | workflow_dispatch | golangci-lint v2.1.6 |
| `release.yml` | tag `v*.*.*` | Docker multi-arch build/push + GitHub release (git-cliff) |
| `helm-release.yml` | tag `v*.*.*`, dispatch | Package Helm chart + publish to gh-pages |
| `changelog-generator.yml` | PR merge, issue close | Auto-generate CHANGELOG.md |
| `contributors.yml` | after changelog | Auto-generate CONTRIBUTORS.md |
| `gitlab-mirror.yml` | push to main | Mirror to GitLab backup |
| `stale-issues.yml` | daily cron | Auto-close stale issues |
| `dependabot-auto-merge.yml` | PR (dependabot) | Auto-merge minor/patch updates |
| `issue-greeting.yml` | issue opened | Welcome message |

Required secrets: `DOCKERHUB_TOKEN`, `PAT_TOKEN`, `GITLAB_TOKEN`.

<br/>

## Conventions

- **Commits**: Conventional Commits (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `ci:`, `chore:`) — single concise line
- **Providers**: GitHub, GitLab, CodeCommit (any-to-any mirroring)
- **Events**: SQS (CodeCommit), Webhooks (GitHub/GitLab)
- **Naming**: `<TYPE>_<NAME>_<FIELD>` for env vars (see [naming-convention.md](naming-convention.md))
- **Docker**: Multi-stage alpine, multi-arch (linux/amd64, linux/arm64)
- **Logging**: Structured JSON via `slog.JSONHandler`

# Version Management & Release Process

<br/>

## Version Locations

Version is tracked in the following files:

| File | Field | Format |
|------|-------|--------|
| `Makefile` | `IMG` | `somaz940/git-bridge:v0.3.0` |
| `helm/git-bridge/Chart.yaml` | `version` | `0.3.0` (without `v`) |
| `helm/git-bridge/Chart.yaml` | `appVersion` | `v0.3.0` |
| `helm/git-bridge/values.yaml` | `image.tag` | `v0.3.0` |
| `k8s/deployment.yaml` | `image` | `somaz940/git-bridge:v0.3.0` |

At build time, the binary also embeds three ldflag-injected constants from `internal/version`:

- `Version` — from the Makefile `IMG` tag
- `GitCommit` — `git rev-parse --short HEAD`
- `BuildDate` — UTC ISO 8601

Check the embedded values:

```bash
./bin/git-bridge -version
# git-bridge v0.3.0 (commit: 9c35281, built: 2026-04-14T01:43:38Z)
```

<br/>

## Check Current Version

```bash
make version
```

Output:

```
Current version: v0.3.0

Version in each file:
  Makefile:                           v0.3.0
  Chart.yaml (version):               0.3.0
  Chart.yaml (appVersion):            v0.3.0
  values.yaml (image.tag):            v0.3.0
  k8s/deployment.yaml (image):        v0.3.0
```

If `k8s/deployment.yaml (image)` shows `<not pinned>`, the manifest is using `:latest` — pin it to the current version before the next bump so the script can track it.

<br/>

## Bump Version

Update all files at once:

```bash
make bump-version VERSION=v0.4.0
```

This updates:

- `Makefile` (IMG tag)
- `helm/git-bridge/Chart.yaml` (version + appVersion)
- `helm/git-bridge/values.yaml` (image.tag)
- `k8s/deployment.yaml` (image tag)
- `README.md` (version references)

The script validates the format (`vX.Y.Z`), is idempotent (does nothing if already at the target version), and skips files that are missing.

<br/>

## Release Process

### 1. Bump version and commit

```bash
make bump-version VERSION=v0.4.0
git diff                                    # review changes
git commit -am "chore: bump version to v0.4.0"
git push origin main
```

### 2. Build and push Docker image

```bash
make docker-buildx                          # multi-arch build + push (version + latest)
```

### 3. Create git tag

```bash
git tag v0.4.0
git push origin v0.4.0
```

This triggers the following CI workflows:

- **release.yml** — Docker multi-arch build+push → GitHub Release (git-cliff changelog)
- **helm-release.yml** — Package Helm chart → publish to `gh-pages`
- **changelog-generator.yml** — Update `CHANGELOG.md`
- **contributors.yml** — Update `CONTRIBUTORS.md`

### 4. Verify

```bash
# Docker image
docker pull somaz940/git-bridge:v0.4.0
docker inspect somaz940/git-bridge:v0.4.0 | grep -A2 Labels
# Expect: org.opencontainers.image.version=v0.4.0

# Helm chart (after gh-pages publishes)
helm repo add git-bridge https://somaz94.github.io/git-bridge/helm-repo
helm repo update
helm search repo git-bridge
```

<br/>

## Development Workflow

### Feature branch

```bash
make branch name=gitlab-provider            # creates feat/gitlab-provider
# ... develop ...
make pr title="feat: add GitLab provider"   # test + push + create PR
```

### Pre-flight checks

```bash
make test                                   # all tests pass (race + cover)
make test-helm                              # Helm chart lint + render
make lint                                   # golangci-lint
make version                                # versions consistent
```

<br/>

## Why the version bump script matters

All three delivery surfaces (Docker image, Helm chart, raw K8s manifests) must be pinned to the same version so users get consistent artifacts across install paths. Manual edits diverge easily — run `make bump-version` once and the script sed-updates every tracked file in a single atomic step, leaving just `git diff` for review.

#!/usr/bin/env bash
set -euo pipefail

# test-helm.sh - Lint and template-test the Helm chart
#
# Usage:
#   ./hack/test-helm.sh
#   make test-helm

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CHART_DIR="${ROOT_DIR}/helm/git-bridge"

echo "==> Helm chart tests"
echo ""

# 1. Lint
echo "--- Lint ---"
helm lint "${CHART_DIR}"
echo ""

# 2. Template render (default values)
echo "--- Template (default values) ---"
helm template test-release "${CHART_DIR}" > /dev/null
echo "  [OK] Default values render successfully"
echo ""

# 3. Template render (with ingress)
echo "--- Template (ingress enabled) ---"
helm template test-release "${CHART_DIR}" \
    --set ingress.enabled=true \
    --set "ingress.hosts[0].host=git-bridge.example.com" \
    --set "ingress.hosts[0].paths[0].path=/" \
    --set "ingress.hosts[0].paths[0].pathType=Prefix" > /dev/null
echo "  [OK] Ingress values render successfully"
echo ""

# 4. Template render (persistence enabled)
echo "--- Template (persistence enabled) ---"
helm template test-release "${CHART_DIR}" \
    --set persistence.enabled=true \
    --set persistence.storageClass=standard \
    --set persistence.size=20Gi > /dev/null
echo "  [OK] Persistence render successfully"
echo ""

# 5. Template render (secrets from values)
echo "--- Template (managed secret) ---"
helm template test-release "${CHART_DIR}" \
    --set secret.create=true \
    --set "secret.data.GITHUB_MAIN_TOKEN=test-token" \
    --set "secret.data.WEBHOOK_GITHUB_SECRET=test-secret" > /dev/null
echo "  [OK] Managed secret render successfully"
echo ""

# 6. Template render (existing secret reference)
echo "--- Template (existing secret) ---"
helm template test-release "${CHART_DIR}" \
    --set secret.create=false \
    --set secret.existingSecret=my-external-secret > /dev/null
echo "  [OK] Existing secret render successfully"
echo ""

# 7. Template render (full options)
echo "--- Template (full options) ---"
helm template test-release "${CHART_DIR}" \
    --set replicaCount=2 \
    --set revisionHistoryLimit=5 \
    --set service.type=NodePort \
    --set "extraEnv[0].name=CUSTOM" \
    --set "extraEnv[0].value=test" > /dev/null
echo "  [OK] Full options render successfully"
echo ""

# 8. Template render (all example files)
echo "--- Template (example values) ---"
for f in "${CHART_DIR}"/examples/*.yaml; do
    [ -f "$f" ] || continue
    name=$(basename "$f" .yaml)
    helm template test-release "${CHART_DIR}" -f "$f" > /dev/null
    echo "  [OK] ${name}"
done
echo ""

TOTAL=$((7 + $(ls "${CHART_DIR}"/examples/*.yaml 2>/dev/null | wc -l | tr -d ' ')))
echo "==> All Helm chart tests passed! (${TOTAL} scenarios)"

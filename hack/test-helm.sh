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

# 9. Reconcile CronJob: calls follow each repo's direction, and the guards fire
echo "--- Template (reconcile CronJob) ---"
REC_VALUES=(
    --set reconcile.enabled=true
    --set "secret.data.RETRY_API_TOKEN=test-token"
    --set "config.repos[0].name=app-a" --set "config.repos[0].direction=bidirectional"
    --set "config.repos[1].name=app-b" --set "config.repos[1].direction=source-to-target"
)
calls=$(helm template test-release "${CHART_DIR}" "${REC_VALUES[@]}" --show-only templates/cronjob-reconcile.yaml)
expected='app-a:source-to-target app-a:target-to-source app-b:source-to-target'
grep -q "value: \"${expected}\"" <<<"$calls" || { echo "  [FAIL] unexpected CALLS:"; grep -A1 'name: CALLS' <<<"$calls"; exit 1; }
if grep -q 'app.kubernetes.io/name: git-bridge$' <<<"$(sed -n '/template:/,$p' <<<"$calls")"; then
    echo "  [FAIL] reconcile pod carries the Service selector label"; exit 1
fi
if helm template test-release "${CHART_DIR}" --set reconcile.enabled=true --set "secret.data.RETRY_API_TOKEN=x" >/dev/null 2>&1; then
    echo "  [FAIL] rendered without any repo to reconcile"; exit 1
fi
if helm template test-release "${CHART_DIR}" --set reconcile.enabled=true \
    --set "config.repos[0].name=app-a" --set "config.repos[0].direction=bidirectional" >/dev/null 2>&1; then
    echo "  [FAIL] rendered without RETRY_API_TOKEN"; exit 1
fi
echo "  [OK] Reconcile CronJob renders and guards fire"
echo ""

TOTAL=$((8 + $(ls "${CHART_DIR}"/examples/*.yaml 2>/dev/null | wc -l | tr -d ' ')))
echo "==> All Helm chart tests passed! (${TOTAL} scenarios)"

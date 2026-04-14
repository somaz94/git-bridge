#!/usr/bin/env bash
set -euo pipefail

# test-deploy.sh - Smoke test against a running git-bridge instance
#
# Usage:
#   ./hack/test-deploy.sh [PORT]
#   make deploy-smoke
#   make deploy-all    # deploy + smoke in one step

PORT="${1:-8080}"
BASE="http://localhost:${PORT}"

PASS=0
FAIL=0

check() {
    local desc="$1" result="$2"
    if [ "$result" = "true" ]; then
        echo "  ✓ ${desc}"
        PASS=$((PASS + 1))
    else
        echo "  ✗ ${desc}"
        FAIL=$((FAIL + 1))
    fi
}

echo "=== Smoke Test: ${BASE} ==="
echo ""

# ---------------------------------------------------------------
# 1. Wait for server
# ---------------------------------------------------------------
echo "[1/4] Server health..."
for i in 1 2 3 4 5; do
    STATUS=$(curl -s -o /dev/null -w '%{http_code}' "${BASE}/health" 2>/dev/null) || true
    if [ "$STATUS" = "200" ]; then break; fi
    if [ "$i" = "5" ]; then
        echo "  ✗ Server not responding after 5 attempts"
        exit 1
    fi
    sleep 1
done
check "GET /health => 200" "true"

READY_STATUS=$(curl -s -o /dev/null -w '%{http_code}' "${BASE}/ready" 2>/dev/null) || true
check "GET /ready => 200" "$([ "$READY_STATUS" = "200" ] && echo true || echo false)"

# ---------------------------------------------------------------
# 2. Webhook endpoints (unauthenticated → 401/403)
# ---------------------------------------------------------------
echo "[2/4] Webhook endpoints reject unauthenticated requests..."

GITHUB_STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H 'Content-Type: application/json' \
    -H 'X-GitHub-Event: push' \
    -d '{}' \
    "${BASE}/webhook/github" 2>/dev/null) || true
check "POST /webhook/github (no sig) => 4xx" "$([ "${GITHUB_STATUS:0:1}" = "4" ] && echo true || echo false)"

GITLAB_STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H 'Content-Type: application/json' \
    -H 'X-Gitlab-Event: Push Hook' \
    -d '{}' \
    "${BASE}/webhook/gitlab" 2>/dev/null) || true
check "POST /webhook/gitlab (no sig) => 4xx" "$([ "${GITLAB_STATUS:0:1}" = "4" ] && echo true || echo false)"

# ---------------------------------------------------------------
# 3. HTTP method handling
# ---------------------------------------------------------------
echo "[3/4] HTTP method handling..."

GET_GITHUB=$(curl -s -o /dev/null -w '%{http_code}' "${BASE}/webhook/github" 2>/dev/null) || true
check "GET /webhook/github => 405" "$([ "$GET_GITHUB" = "405" ] && echo true || echo false)"

GET_GITLAB=$(curl -s -o /dev/null -w '%{http_code}' "${BASE}/webhook/gitlab" 2>/dev/null) || true
check "GET /webhook/gitlab => 405" "$([ "$GET_GITLAB" = "405" ] && echo true || echo false)"

# ---------------------------------------------------------------
# 4. Not found handling
# ---------------------------------------------------------------
echo "[4/4] Not found handling..."

NOT_FOUND=$(curl -s -o /dev/null -w '%{http_code}' "${BASE}/nonexistent-path" 2>/dev/null) || true
check "GET /nonexistent-path => 404" "$([ "$NOT_FOUND" = "404" ] && echo true || echo false)"

# ---------------------------------------------------------------
# Summary
# ---------------------------------------------------------------
echo ""
TOTAL=$((PASS + FAIL))
echo "=== Results: ${PASS}/${TOTAL} passed, ${FAIL} failed ==="

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi

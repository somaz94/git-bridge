#!/usr/bin/env bash
set -euo pipefail

# bump-version.sh - Automatically update version across all project files
#
# Usage:
#   ./hack/bump-version.sh <new-version>
#   ./hack/bump-version.sh --current
#   make bump-version VERSION=v0.2.0
#   make version

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

PROJECT_NAME="git-bridge"

# Extract current version from Makefile IMG
get_current_version() {
    grep -o "${PROJECT_NAME}:v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*" "${ROOT_DIR}/Makefile" \
        | head -1 \
        | cut -d: -f2
}

if [[ $# -ne 1 ]]; then
    echo "Usage: $0 <new-version>"
    echo "       $0 --current"
    echo "Example: $0 v0.2.0"
    exit 1
fi

# Show current version and exit
if [[ "$1" == "--current" ]]; then
    CURRENT="$(get_current_version)"
    if [[ -z "${CURRENT}" ]]; then
        echo "Error: Could not detect current version from Makefile"
        exit 1
    fi
    echo "Current version: ${CURRENT}"
    echo ""
    echo "Version in each file:"
    printf "  %-35s %s\n" "Makefile:" "${CURRENT}"
    if [[ -f "${ROOT_DIR}/helm/${PROJECT_NAME}/Chart.yaml" ]]; then
        printf "  %-35s %s\n" "Chart.yaml (version):" "$(grep '^version:' "${ROOT_DIR}/helm/${PROJECT_NAME}/Chart.yaml" | awk '{print $2}')"
        printf "  %-35s %s\n" "Chart.yaml (appVersion):" "$(grep '^appVersion:' "${ROOT_DIR}/helm/${PROJECT_NAME}/Chart.yaml" | awk -F'"' '{print $2}')"
    fi
    if [[ -f "${ROOT_DIR}/helm/${PROJECT_NAME}/values.yaml" ]]; then
        printf "  %-35s %s\n" "values.yaml (image.tag):" "$(grep 'tag:' "${ROOT_DIR}/helm/${PROJECT_NAME}/values.yaml" | head -1 | awk '{print $2}' | tr -d '\"')"
    fi
    if [[ -f "${ROOT_DIR}/k8s/deployment.yaml" ]]; then
        K8S_TAG="$(grep -o "${PROJECT_NAME}:v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*" "${ROOT_DIR}/k8s/deployment.yaml" | head -1 | cut -d: -f2 || true)"
        printf "  %-35s %s\n" "k8s/deployment.yaml (image):" "${K8S_TAG:-<not pinned>}"
    fi
    exit 0
fi

NEW_VERSION="$1"

# Validate version format (vX.Y.Z)
if [[ ! "${NEW_VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Error: Version must be in format vX.Y.Z (e.g., v0.2.0)"
    exit 1
fi

# Strip 'v' prefix for chart version
CHART_VERSION="${NEW_VERSION#v}"

# Detect current version
CURRENT_VERSION="$(get_current_version)"
if [[ -z "${CURRENT_VERSION}" ]]; then
    echo "Error: Could not detect current version from Makefile"
    exit 1
fi

CURRENT_CHART_VERSION="${CURRENT_VERSION#v}"

if [[ "${CURRENT_VERSION}" == "${NEW_VERSION}" ]]; then
    echo "Already at version ${NEW_VERSION}, nothing to do."
    exit 0
fi

echo "Bumping version: ${CURRENT_VERSION} -> ${NEW_VERSION}"
echo ""

update_file() {
    local file="$1"
    local pattern="$2"
    local replacement="$3"
    local label="$4"

    if [[ ! -f "${file}" ]]; then
        echo "  [SKIP] ${label} (not found)"
        return
    fi

    if ! grep -q -e "${pattern}" "${file}"; then
        echo "  [WARN] ${label} (pattern not found -- version drift?)" >&2
        WARN_COUNT=$(( ${WARN_COUNT:-0} + 1 ))
        return
    fi

    if sed --version >/dev/null 2>&1; then
        sed -i "s|${pattern}|${replacement}|g" "${file}"
    else
        sed -i '' "s|${pattern}|${replacement}|g" "${file}"
    fi
    echo "  [OK]   ${label}"
}

echo "==> Updating required files..."
update_file "${ROOT_DIR}/Makefile" \
    "${PROJECT_NAME}:${CURRENT_VERSION}" \
    "${PROJECT_NAME}:${NEW_VERSION}" \
    "Makefile (IMG)"

update_file "${ROOT_DIR}/helm/${PROJECT_NAME}/Chart.yaml" \
    "^version: ${CURRENT_CHART_VERSION}" \
    "version: ${CHART_VERSION}" \
    "Chart.yaml (version)"

update_file "${ROOT_DIR}/helm/${PROJECT_NAME}/Chart.yaml" \
    "^appVersion: \"${CURRENT_VERSION}\"" \
    "appVersion: \"${NEW_VERSION}\"" \
    "Chart.yaml (appVersion)"

update_file "${ROOT_DIR}/helm/${PROJECT_NAME}/values.yaml" \
    "tag: \"${CURRENT_VERSION}\"" \
    "tag: \"${NEW_VERSION}\"" \
    "values.yaml (image.tag)"

update_file "${ROOT_DIR}/k8s/deployment.yaml" \
    "${PROJECT_NAME}:${CURRENT_VERSION}" \
    "${PROJECT_NAME}:${NEW_VERSION}" \
    "k8s/deployment.yaml (image)"

echo ""
echo "==> Updating documentation files..."
update_file "${ROOT_DIR}/README.md" \
    "${CURRENT_VERSION}" \
    "${NEW_VERSION}" \
    "README.md"

echo ""
if [[ "${WARN_COUNT:-0}" -gt 0 ]]; then
    echo "" >&2
    echo "WARNING: ${WARN_COUNT} file(s) were NOT updated (pattern not found). Check for version drift above." >&2
fi

echo "Done! Version bumped to ${NEW_VERSION}"
echo ""
echo "Next steps:"
echo "  1. Review changes: git diff"
echo "  2. Commit: git commit -am \"chore: bump version to ${NEW_VERSION}\""
echo "  3. Push: git push origin main"
echo "  4. Build image: make docker-buildx"
echo "  5. Tag: git tag ${NEW_VERSION} && git push origin ${NEW_VERSION}"

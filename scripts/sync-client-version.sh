#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

CLI_VERSION=$(jq -r .version "$REPO_ROOT/cli/package.json")

echo "Syncing @hiver.sh/client to v$CLI_VERSION..."

jq --arg v "$CLI_VERSION" '.version = $v' \
  "$REPO_ROOT/client/typescript/package.json" > "$REPO_ROOT/client/typescript/package.json.tmp" \
  && mv "$REPO_ROOT/client/typescript/package.json.tmp" "$REPO_ROOT/client/typescript/package.json"

sed -i '' "s/^version = .*/version = \"$CLI_VERSION\"/" "$REPO_ROOT/client/python/pyproject.toml"

# Keep the Helm chart on the same version as the CLI/clients so it publishes to
# the chart repo (Artifact Hub) under the matching version. appVersion tracks the
# same release — the images are pinned to this version at release time.
CHART="$REPO_ROOT/deployment/k8s/chart/Chart.yaml"
sed -i '' "s/^version:.*/version: $CLI_VERSION/" "$CHART"
sed -i '' "s/^appVersion:.*/appVersion: \"$CLI_VERSION\"/" "$CHART"

echo "Done. Commit client/typescript/package.json, client/python/pyproject.toml, and deployment/k8s/chart/Chart.yaml."

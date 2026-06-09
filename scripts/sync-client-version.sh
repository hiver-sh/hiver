#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

CLI_VERSION=$(jq -r .version "$REPO_ROOT/cli/package.json")

echo "Syncing @hiver.sh/client to v$CLI_VERSION..."

jq --arg v "$CLI_VERSION" '.version = $v' \
  "$REPO_ROOT/client/typescript/package.json" > "$REPO_ROOT/client/typescript/package.json.tmp" \
  && mv "$REPO_ROOT/client/typescript/package.json.tmp" "$REPO_ROOT/client/typescript/package.json"

sed -i '' "s/^version = .*/version = \"$CLI_VERSION\"/" "$REPO_ROOT/client/python/pyproject.toml"

echo "Done. Commit client/typescript/package.json and client/python/pyproject.toml."

#!/usr/bin/env bash
# Rewrite chart/values.yaml image references to the immutable *versioned* tags the
# Release workflow publishes. Run at release time (CI) against a fresh checkout;
# the result is packaged into the released Helm chart so an installed chart runs
# exactly the images published under the same version — never a drifting `:latest`.
#
# Versioned tags are immutable (a new release = a new tag, never re-pushed), so
# they are not exposed to the GKE mirror.gcr.io stale-tag hazard that motivated
# the old @sha256 digest pinning — a plain tag reference is enough and readable.
#
# The versioned tag mirrors the naming in .github/workflows/release.yaml
# (push-runtime-images) and scripts/tag-sandbox-images.sh, for VERSION=X.Y.Z:
#   hiversh/controller:latest          -> hiversh/controller:X.Y.Z
#   hiversh/gateway:latest             -> hiversh/gateway:X.Y.Z
#   hiversh/claude:latest-microvm      -> hiversh/claude:X.Y.Z-microvm
#   hiversh/python:3.13-alpine-microvm -> hiversh/python:X.Y.Z-3.13-alpine-microvm
#   hiversh/node:alpine-microvm        -> hiversh/node:X.Y.Z-alpine-microvm
#
# Usage: deployment/k8s/pin-release-images.sh <version> [path/to/values.yaml]
set -euo pipefail

VERSION="${1:?usage: pin-release-images.sh <version> [values.yaml]}"
VALUES="${2:-$(dirname "$0")/chart/values.yaml}"

# Map a source tag to the versioned tag the release publishes:
#   (no tag)          -> X.Y.Z
#   contains "latest" -> substitute "latest" with X.Y.Z  (latest-microvm -> X.Y.Z-microvm)
#   otherwise         -> X.Y.Z-<tag>  (3.13-alpine-microvm -> X.Y.Z-3.13-alpine-microvm)
versioned_tag() {
  local tag="$1"
  if [[ -z "$tag" ]]; then echo "$VERSION"
  elif [[ "$tag" == *latest* ]]; then echo "${tag/latest/$VERSION}"
  else echo "${VERSION}-${tag}"
  fi
}

# Every distinct hiversh/... image reference in the file (tag optional; no digest).
# Each appears as the last token on an `image:` line, so the substitution is
# anchored to end-of-line ($) — that keeps a shorter ref (python:3.13-alpine)
# from also matching inside a longer one (python:3.13-alpine-microvm), and leaves
# the same string in a comment untouched. Process substitution (not a pipe) so a
# `set -e` failure aborts the whole script.
while read -r ref; do
  repo="${ref%%:*}"                       # hiversh/claude
  tag=""
  [[ "$ref" == *:* ]] && tag="${ref#*:}"  # latest-microvm | 3.13-alpine | (empty)
  new_ref="${repo}:$(versioned_tag "$tag")"
  esc="${ref//./\\.}"                      # escape dots for the regex match
  sed -i.bak "s|${esc}\$|${new_ref}|g" "$VALUES"
  printf ' ~ %-42s -> %s\n' "$ref" "$new_ref"
done < <(grep -oE 'hiversh/[A-Za-z0-9._/-]+(:[A-Za-z0-9._-]+)?' "$VALUES" | sort -u)

rm -f "${VALUES}.bak"

#!/usr/bin/env bash
# Return the tested commit only if the current remote tag still points at it.
set -euo pipefail

[[ ${GITHUB_REF_NAME:-} =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
[[ ${GITHUB_EVENT_NAME:-} == push && ${GITHUB_REF_TYPE:-} == tag ]]
[[ ${GITHUB_SHA:-} =~ ^[0-9a-f]{40}$ ]]
sha=$(git rev-parse --verify 'HEAD^{commit}')
ref="refs/tags/$GITHUB_REF_NAME"
refs=$(git ls-remote --exit-code origin "$ref" "$ref^{}")
tag_object=$(awk -v ref="$ref" '$2 == ref {print $1}' <<<"$refs")
commit_object=$(awk -v ref="$ref^{}" '$2 == ref {print $1}' <<<"$refs")
[[ ${commit_object:-$tag_object} == "$sha" &&
   ( $GITHUB_SHA == "$tag_object" || $GITHUB_SHA == "$sha" ) ]] ||
  { echo "Release tag does not match the checked-out commit and push event" >&2; exit 1; }
printf '%s\n' "$sha"

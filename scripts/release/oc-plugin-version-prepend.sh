#!/usr/bin/env bash
# Prepend <version> to ./oc-plugin.json versions[]: normalize v-prefixed,
# dedupe, sort semver-descending. Generates the manifest on first publish
# from ID/OWNER (PUBLISHER optional). Edits the file in place (cwd).
# Usage: oc-plugin-version-prepend.sh <version>   (bare or v-prefixed)
set -euo pipefail
: "${1:?version required}"
VER="v${1#v}"
FILE="oc-plugin.json"

if [ ! -s "$FILE" ]; then
  : "${ID:?ID required to generate manifest}" "${OWNER:?OWNER required to generate manifest}"
  jq -n --arg id "$ID" --arg pub "${PUBLISHER:-OpenCapital}" --arg ns "${OWNER}/plugins" '{
    schemaVersion: 1, pluginId: $id, publisher: $pub,
    registry: { host: "ghcr.io", namespace: $ns, publicURL: "https://ghcr.io" },
    versions: []
  }' > "$FILE"
fi

tmp="$(mktemp)"
jq --arg v "$VER" '.versions = (
  (((.versions // []) + [$v]) | map("v" + sub("^v";"")) | unique)
  | sort_by(sub("^v";"") | split(".") | map(split("-")[0] | tonumber? // 0))
  | reverse
)' "$FILE" > "$tmp"
mv "$tmp" "$FILE"

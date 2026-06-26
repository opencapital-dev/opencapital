#!/usr/bin/env bash
# Set .version in opencapital-app/src-tauri/tauri.conf.json (cwd-relative).
# Usage: write-tauri-version.sh <version>   (bare or v-prefixed)
set -euo pipefail
: "${1:?version required}"
VER="${1#v}"
FILE="opencapital-app/src-tauri/tauri.conf.json"
tmp="$(mktemp)"
jq --arg v "$VER" '.version = $v' "$FILE" > "$tmp"
mv "$tmp" "$FILE"

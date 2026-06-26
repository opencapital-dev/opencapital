#!/usr/bin/env bash
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$here/write-tauri-version.sh"
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT; cd "$work"
mkdir -p opencapital-app/src-tauri
echo '{"productName":"OpenCapital","version":"0.0.0","identifier":"dev.oc"}' > opencapital-app/src-tauri/tauri.conf.json
bash "$SCRIPT" v1.2.3
got="$(jq -r '.version' opencapital-app/src-tauri/tauri.conf.json)"
[ "$got" = "1.2.3" ] || { echo "FAIL: got $got want 1.2.3"; exit 1; }
# identifier preserved
[ "$(jq -r '.identifier' opencapital-app/src-tauri/tauri.conf.json)" = "dev.oc" ] || { echo "FAIL: clobbered other keys"; exit 1; }
echo "PASS"

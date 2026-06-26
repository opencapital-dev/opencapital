#!/usr/bin/env bash
# Offline test: append+sort, idempotent dedupe, bare-legacy normalization,
# and first-publish generate. No network.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$here/oc-plugin-version-prepend.sh"

work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT; cd "$work"

# append + sort semver-descending, normalize to v-prefixed
echo '{"schemaVersion":1,"pluginId":"x","publisher":"OpenCapital","registry":{"host":"ghcr.io","namespace":"o/plugins","publicURL":"https://ghcr.io"},"versions":["0.1.2","v0.1.10","0.1.3"]}' > oc-plugin.json
bash "$SCRIPT" v0.2.0
got="$(jq -c '.versions' oc-plugin.json)"
want='["v0.2.0","v0.1.10","v0.1.3","v0.1.2"]'
[ "$got" = "$want" ] || { echo "FAIL append+sort: got $got want $want"; exit 1; }

# idempotent: re-adding existing (bare vs v) does not duplicate
echo '{"versions":["v0.1.3","0.1.2"]}' > oc-plugin.json
bash "$SCRIPT" 0.1.3
got="$(jq -c '.versions' oc-plugin.json)"
want='["v0.1.3","v0.1.2"]'
[ "$got" = "$want" ] || { echo "FAIL idempotent: got $got want $want"; exit 1; }

# first publish: missing file generated from ID/OWNER, then version added
rm -f oc-plugin.json
ID=core-app OWNER=opencapital-dev bash "$SCRIPT" v0.1.0
got="$(jq -c '{p:.pluginId,ns:.registry.namespace,v:.versions}' oc-plugin.json)"
want='{"p":"core-app","ns":"opencapital-dev/plugins","v":["v0.1.0"]}'
[ "$got" = "$want" ] || { echo "FAIL generate: got $got want $want"; exit 1; }

echo "PASS"

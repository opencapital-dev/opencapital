#!/bin/sh
# Register all Avro schemas with Redpanda Schema Registry.
# Uses TopicNameStrategy only: subject = "<topic>-value".
# Driven entirely from schemas/TOPICS.tsv (one line per topic).
#
# Env:
#   REGISTRY_URL  - e.g. http://redpanda:8081
#   SCHEMAS_DIR   - path containing TOPICS.tsv and the typed schema tree

set -eu

REGISTRY_URL="${REGISTRY_URL:-http://localhost:8081}"
SCHEMAS_DIR="${SCHEMAS_DIR:-/work/schemas}"
TOPICS_FILE="${SCHEMAS_DIR}/TOPICS.tsv"

log() { echo "[register-schemas] $*"; }

post_schema() {
  subject="$1"
  file="$2"

  if [ ! -f "$file" ]; then
    log "MISSING $file"
    return 1
  fi

  schema_escaped=$(awk 'BEGIN{ORS=""}{gsub(/\\/,"\\\\"); gsub(/"/,"\\\""); print}' "$file")
  body="{\"schemaType\":\"AVRO\",\"schema\":\"${schema_escaped}\"}"

  log "registering ${subject} <- ${file}"
  http_code=$(
    printf '%s' "$body" | curl -s -o /tmp/reg.out -w '%{http_code}' \
      -X POST \
      -H "Content-Type: application/vnd.schemaregistry.v1+json" \
      --data-binary @- \
      "${REGISTRY_URL}/subjects/${subject}/versions"
  )
  if [ "$http_code" != "200" ] && [ "$http_code" != "409" ]; then
    log "FAILED ${subject} http=${http_code} body=$(cat /tmp/reg.out)"
    return 1
  fi
  log "ok ${subject} (http=${http_code})"
}

wait_for_registry() {
  i=0
  while [ $i -lt 30 ]; do
    if curl -fs "${REGISTRY_URL}/subjects" >/dev/null 2>&1; then
      return 0
    fi
    i=$((i + 1))
    sleep 2
  done
  log "schema registry not reachable at ${REGISTRY_URL}"
  return 1
}

wait_for_registry

if [ ! -f "$TOPICS_FILE" ]; then
  log "MISSING $TOPICS_FILE"
  exit 1
fi

# Strip comments/blank lines and register each topic's value schema.
grep -v '^[[:space:]]*#' "$TOPICS_FILE" | awk 'NF>=2{print $1" "$2}' | while read -r topic relpath; do
  [ -n "$topic" ] || continue
  post_schema "${topic}-value" "${SCHEMAS_DIR}/${relpath}"
done

log "done"

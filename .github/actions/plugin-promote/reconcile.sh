#!/usr/bin/env bash
#
# reconcile.sh -- reconcile the trusted GHCR plugin namespace to match
# infra/plugins/plugins.yaml.
#
# The manifest (plugins.yaml) is authoritative: for each plugin it lists the
# bare semver versions that should be present (and only those) in the trusted
# namespace. This script computes the delta against the live trusted namespace
# and, depending on MODE, either reports it (verify) or applies it (reconcile).
#
#   verify    -- compute the plan + verify every to-be-added version (cosign +
#                footprint collision). NO mutation. exit!=0 if anything fails.
#                This is the PR gate.
#   reconcile -- verify every to-be-added version; iff ALL pass, copy them
#                staging->trusted and delete every to-be-removed trusted tag.
#                Abort BEFORE any mutation on failure (no half-apply). This is
#                the merge action.
#
# Built from off-the-shelf tools only: cosign, oras, gh, yq, jq.
#
# Auth: assumes the caller already logged oras/cosign into GHCR and that the
# GH_TOKEN in the environment grants packages:write (gh api uses it). No login
# happens here.
#
# Configuration (env vars, with defaults):
#   MODE           verify | reconcile                 (default: verify)
#   OWNER          GHCR owner / github.repository_owner (default: opencapital-dev)
#   REGISTRY       OCI registry host                   (default: ghcr.io)
#   TRUSTED_NS     trusted namespace                   (default: plugins)
#   STAGING_NS     staging namespace                   (default: plugins-staging)
#   MANIFEST       path to plugins.yaml                (default: infra/plugins/plugins.yaml)
#   SIGNERS        path to signers.yaml                (default: infra/plugins/signers.yaml)
#
set -euo pipefail

MODE="${MODE:-verify}"
OWNER="${OWNER:-opencapital-dev}"
REGISTRY="${REGISTRY:-ghcr.io}"
TRUSTED_NS="${TRUSTED_NS:-plugins}"
STAGING_NS="${STAGING_NS:-plugins-staging}"
MANIFEST="${MANIFEST:-infra/plugins/plugins.yaml}"
SIGNERS="${SIGNERS:-infra/plugins/signers.yaml}"

log()  { printf '%s\n' "$*" >&2; }
fail() { log "ERROR: $*"; exit 1; }

# ---------------------------------------------------------------------------
# Pure helpers (no network). Exercised offline by test_reconcile.sh.
# ---------------------------------------------------------------------------

# manifest_plugins MANIFEST -> plugin ids, one per line.
manifest_plugins() {
  yq -r '.plugins | keys | .[]' "$1"
}

# validate_manifest MANIFEST -> exit 0 if the file is parseable YAML with a
# .plugins mapping, else fail loudly. Run ONCE up front so a malformed
# authoritative manifest (or a typo'd top-level key) can never fail-open into an
# empty plugin list -- which would read as a clean "0 to add, 0 to remove" or,
# worse, silently drive mass removal.
validate_manifest() {
  yq -e '.plugins | tag == "!!map"' "$1" >/dev/null 2>&1 \
    || fail "manifest is not parseable / has no .plugins map: $1"
}

# validate_signers SIGNERS -> exit 0 if the file is parseable YAML with a
# .signers sequence, else fail loudly. Same up-front guarantee as the manifest.
validate_signers() {
  yq -e '.signers | tag == "!!seq"' "$1" >/dev/null 2>&1 \
    || fail "signers is not parseable / has no .signers list: $1"
}

# desired_tags MANIFEST PLUGIN -> the manifest versions for one plugin as
# v<semver> tags, one per line (empty when the plugin lists no versions). A
# leading "v" in the manifest value is stripped first so both "0.1.5" and
# "v0.1.5" normalize to "v0.1.5" (no "vv0.1.5"). The manifest is validated up
# front, so a yq error here is a real fault and is NOT swallowed.
desired_tags() {
  yq -r ".plugins.\"$2\"[]? | \"v\" + (. | sub(\"^v\"; \"\"))" "$1"
}

# set_diff A_LINES B_LINES -> lines in A but not in B (A \ B), order-preserving,
# read from two newline-separated strings passed as args.
set_diff() {
  local a="$1" b="$2"
  [ -n "$a" ] || return 0
  comm -23 <(printf '%s\n' "$a" | sort -u) <(printf '%s\n' "$b" | sort -u)
}

# source_tables_of FOOTPRINT_JSON -> the source_table of every logical view,
# one per line. Empty footprint / no views -> nothing.
source_tables_of() {
  printf '%s' "$1" | jq -r '(.logical_views // [])[].source_table' 2>/dev/null || true
}

# collision OWNER_TABLES_JSON CANDIDATE_PLUGIN CANDIDATE_FOOTPRINT_JSON
# ownership map is a JSON object { "<source_table>": "<owning_plugin>" } built
# from every OTHER plugin's footprints. Prints the first conflicting line
# "<table> owned by <plugin>" and returns 1 on collision, else returns 0.
collision() {
  local owners="$1" cand="$2" fp="$3" t owner
  while IFS= read -r t; do
    [ -n "$t" ] || continue
    owner="$(printf '%s' "$owners" | jq -r --arg t "$t" '.[$t] // ""')"
    if [ -n "$owner" ] && [ "$owner" != "$cand" ]; then
      printf '%s owned by %s\n' "$t" "$owner"
      return 1
    fi
  done < <(source_tables_of "$fp")
  return 0
}

# ---------------------------------------------------------------------------
# Network helpers (overridable in tests by predefining the function).
# ---------------------------------------------------------------------------

trusted_ref() { printf '%s/%s/%s/%s:%s' "$REGISTRY" "$OWNER" "$TRUSTED_NS" "$1" "$2"; }
staging_ref() { printf '%s/%s/%s/%s:%s' "$REGISTRY" "$OWNER" "$STAGING_NS" "$1" "$2"; }
trusted_repo() { printf '%s/%s/%s/%s' "$REGISTRY" "$OWNER" "$TRUSTED_NS" "$1"; }
staging_repo() { printf '%s/%s/%s/%s' "$REGISTRY" "$OWNER" "$STAGING_NS" "$1"; }

# current_tags PLUGIN -> the trusted tags of a plugin, one per line. A repo that
# does not exist yet (never promoted) is empty, not an error. But ONLY genuine
# repo-not-found is empty: a transient registry 5xx / network error must NOT
# read as "no trusted tags" (in reconcile mode that mis-drives toAdd/toRemove),
# so any other non-zero oras exit fails loudly.
if ! declare -F current_tags >/dev/null; then
current_tags() {
  local out err rc
  err="$(mktemp)"
  if out="$(oras repo tags "$(trusted_repo "$1")" 2>"$err")"; then
    rm -f "$err"
    printf '%s\n' "$out" | grep -v '^[[:space:]]*$' || true
    return 0
  fi
  rc=$?
  local stderr; stderr="$(cat "$err")"; rm -f "$err"
  # Genuine repo-not-found (never promoted) -> empty set, no error. oras emits
  # "name unknown: repository name not known to registry" for an absent repo.
  if printf '%s' "$stderr" | grep -qiE 'name unknown|not known to registry|not found|NAME_UNKNOWN|404'; then
    return 0
  fi
  fail "oras repo tags failed for $(trusted_repo "$1") (exit $rc): ${stderr:-<no stderr>}"
}
fi

# footprint_json NAMESPACE PLUGIN TAG -> the JSON footprint (config blob) of an
# OCI artifact. Empty string when the artifact/repo is absent.
if ! declare -F footprint_json >/dev/null; then
footprint_json() {
  local ns="$1" plugin="$2" tag="$3" repo ref digest
  case "$ns" in
    "$TRUSTED_NS") repo="$(trusted_repo "$plugin")"; ref="$(trusted_ref "$plugin" "$tag")" ;;
    "$STAGING_NS") repo="$(staging_repo "$plugin")"; ref="$(staging_ref "$plugin" "$tag")" ;;
    *) fail "footprint_json: unknown namespace $ns" ;;
  esac
  digest="$(oras manifest fetch "$ref" 2>/dev/null | jq -r '.config.digest // empty')"
  [ -n "$digest" ] || return 0
  oras blob fetch --output - "${repo}@${digest}" 2>/dev/null || true
}
fi

# verify_signature STAGING_REF -> 0 if the artifact's cosign signature verifies
# under ANY signers.yaml entry, 1 otherwise. Uses the public Sigstore TUF root.
if ! declare -F verify_signature >/dev/null; then
verify_signature() {
  local ref="$1" n i issuer regexp
  n="$(yq -r '.signers | length' "$SIGNERS")"
  for (( i=0; i<n; i++ )); do
    issuer="$(yq -r ".signers[$i].issuer" "$SIGNERS")"
    regexp="$(yq -r ".signers[$i].identity_regexp" "$SIGNERS")"
    if cosign verify \
        --certificate-oidc-issuer "$issuer" \
        --certificate-identity-regexp "$regexp" \
        "$ref" >/dev/null 2>&1; then
      return 0
    fi
  done
  return 1
}
fi

# copy_to_trusted PLUGIN TAG -> oras cp staging->trusted, carrying referrers
# (the cosign signature). Idempotent.
if ! declare -F copy_to_trusted >/dev/null; then
copy_to_trusted() {
  oras cp --recursive "$(staging_ref "$1" "$2")" "$(trusted_ref "$1" "$2")"
}
fi

# delete_trusted_tag PLUGIN TAG -> delete the trusted GHCR package version that
# carries TAG, via the GitHub Packages REST API (mirrors GHCRDeleter). A tag not
# present is a no-op.
if ! declare -F delete_trusted_tag >/dev/null; then
delete_trusted_tag() {
  local plugin="$1" tag="$2" pkg esc vid ids
  pkg="${TRUSTED_NS}/${plugin}"
  esc="${pkg//\//%2F}"
  # Run the list FIRST and check its exit status BEFORE piping. An auth/network
  # failure must NOT masquerade as "tag already absent" (which would report
  # success while the stale trusted tag survives), so a non-zero gh exit fails
  # loudly. Only a successful list that yields no matching version is the
  # genuine "already absent" case.
  if ! ids="$(gh api --paginate \
      "/users/${OWNER}/packages/container/${esc}/versions?per_page=100" \
      --jq ".[] | select(.metadata.container.tags | index(\"${tag}\")) | .id")"; then
    fail "gh api version lookup failed for ${pkg}:${tag}; refusing to treat as absent"
  fi
  vid="$(printf '%s\n' "$ids" | grep -v '^[[:space:]]*$' | head -1 || true)"
  if [ -z "$vid" ]; then
    log "    (already absent) ${pkg}:${tag}"
    return 0
  fi
  gh api --method DELETE \
    "/users/${OWNER}/packages/container/${esc}/versions/${vid}" >/dev/null
}
fi

# ---------------------------------------------------------------------------
# Reconciliation
# ---------------------------------------------------------------------------

# The plugin universe is the manifest's keys -- the manifest is authoritative.
# Every plugin keeps a key; an empty list (`plugin: []`) means "no validated
# versions", which still drives removal of any tags that plugin has in trusted
# (current_tags finds them, set_diff puts them in toRemove). To fully retire a
# plugin, set its list to [] (do NOT delete the key, or its trusted tags are no
# longer reconciled). We deliberately do NOT enumerate the owner's GHCR packages
# via the GitHub API: that endpoint is unavailable to the built-in GITHUB_TOKEN
# the PR-check job runs with (and to cross-repo-PR runs, which get no secrets),
# and the registry has no catalog API. The manifest is the single source of
# truth for which plugins exist.

main() {
  case "$MODE" in
    verify|reconcile) ;;
    *) fail "MODE must be 'verify' or 'reconcile', got '$MODE'" ;;
  esac
  [ -f "$MANIFEST" ] || fail "manifest not found: $MANIFEST"
  [ -f "$SIGNERS" ]  || fail "signers not found: $SIGNERS"
  # Authoritative inputs must parse before we compute any delta; a malformed
  # file must fail loudly, not fail-open into an empty plan.
  validate_manifest "$MANIFEST"
  validate_signers "$SIGNERS"

  log "plugin-promote: mode=$MODE registry=$REGISTRY owner=$OWNER trusted=$TRUSTED_NS staging=$STAGING_NS"
  log "manifest=$MANIFEST signers=$SIGNERS"
  log ""

  local plugins
  plugins="$(manifest_plugins "$MANIFEST")"

  # Per-plugin plan, accumulated for the summary + (in reconcile) the apply pass.
  # Parallel arrays keyed by index; entries are "plugin\ttag".
  local -a add_list=() remove_list=()
  local failed=0

  # ---- Pass 1: plan + verify every toAdd ---------------------------------
  local plugin desired current to_add to_remove tag
  while IFS= read -r plugin; do
    [ -n "$plugin" ] || continue
    desired="$(desired_tags "$MANIFEST" "$plugin")"
    current="$(current_tags "$plugin")"
    to_add="$(set_diff "$desired" "$current")"
    to_remove="$(set_diff "$current" "$desired")"

    log "plugin: $plugin"
    log "  desired:  $(printf '%s' "${desired:-<none>}" | tr '\n' ' ')"
    log "  current:  $(printf '%s' "${current:-<none>}" | tr '\n' ' ')"

    while IFS= read -r tag; do
      [ -n "$tag" ] || continue
      add_list+=("${plugin}"$'\t'"${tag}")
      log "  + verify $plugin:$tag (staging)"
      if verify_one "$plugin" "$tag"; then
        log "      OK"
      else
        log "      FAIL"
        failed=1
      fi
    done <<< "$to_add"

    while IFS= read -r tag; do
      [ -n "$tag" ] || continue
      remove_list+=("${plugin}"$'\t'"${tag}")
      log "  - remove $plugin:$tag (not in manifest)"
    done <<< "$to_remove"
    log ""
  done <<< "$plugins"

  # ---- Summary -----------------------------------------------------------
  log "plan: ${#add_list[@]} to add, ${#remove_list[@]} to remove"

  if [ "$failed" -ne 0 ]; then
    fail "one or more candidate versions failed verification; no changes applied"
  fi

  if [ "$MODE" = "verify" ]; then
    log "verify: all candidate versions passed; no mutation performed"
    return 0
  fi

  # ---- Pass 2 (reconcile only): apply -----------------------------------
  # Verification already passed for every toAdd, so mutate now. Copies first
  # (additive), then removals.
  local entry
  for entry in "${add_list[@]:-}"; do
    [ -n "$entry" ] || continue
    plugin="${entry%%$'\t'*}"; tag="${entry##*$'\t'}"
    log "apply: copy $plugin:$tag staging->trusted"
    copy_to_trusted "$plugin" "$tag"
  done
  for entry in "${remove_list[@]:-}"; do
    [ -n "$entry" ] || continue
    plugin="${entry%%$'\t'*}"; tag="${entry##*$'\t'}"
    log "apply: delete trusted $plugin:$tag"
    delete_trusted_tag "$plugin" "$tag"
  done

  log "reconcile: applied ${#add_list[@]} adds, ${#remove_list[@]} removes"
}

# verify_one PLUGIN TAG -> 0 if the staged candidate passes BOTH the signature
# check and the footprint-collision check, 1 otherwise.
#
# Collision: the candidate's source_tables must not be owned by any OTHER
# plugin. Ownership is built from every other plugin's footprint — their
# candidate version from staging plus their already-trusted versions — so a
# table claimed by a different plugin (in the manifest or already promoted)
# blocks the candidate.
verify_one() {
  local plugin="$1" tag="$2"
  local sref fp owners other other_tag other_fp ot conflict owner_map_err
  local other_desired other_current other_src

  sref="$(staging_ref "$plugin" "$tag")"
  if ! verify_signature "$sref"; then
    log "      signature did not verify under any signer"
    return 1
  fi

  fp="$(footprint_json "$STAGING_NS" "$plugin" "$tag")"
  if [ -z "$fp" ]; then
    log "      could not read candidate footprint"
    return 1
  fi

  # Build ownership map {table: plugin} from every OTHER plugin.
  #
  # Every tag we iterate here is one that SHOULD have a footprint: it is either
  # a manifest-desired candidate in staging or an already-trusted version. So an
  # empty/errored footprint for such a tag is NOT "nothing to add to the map" --
  # it means we cannot see that plugin's source_tables, and a colliding
  # candidate could slip through. Fail closed, exactly like the candidate's own
  # footprint above, instead of silently dropping the plugin from the map.
  owners='{}'
  owner_map_err=0
  while IFS= read -r other; do
    [ -n "$other" ] || continue
    [ "$other" != "$plugin" ] || continue
    # The other plugin's candidate (manifest desired, highest) from staging,
    # plus all its already-trusted versions.
    other_desired="$(desired_tags "$MANIFEST" "$other")"
    other_current="$(current_tags "$other")"
    while IFS= read -r other_tag; do
      [ -n "$other_tag" ] || continue
      if printf '%s\n' "$other_desired" | grep -qx "$other_tag"; then
        other_src="$STAGING_NS"
      else
        other_src="$TRUSTED_NS"
      fi
      other_fp="$(footprint_json "$other_src" "$other" "$other_tag")"
      if [ -z "$other_fp" ]; then
        log "      could not read footprint for $other:$other_tag ($other_src) -- failing closed"
        owner_map_err=1
        continue
      fi
      while IFS= read -r ot; do
        [ -n "$ot" ] || continue
        owners="$(printf '%s' "$owners" | jq -c --arg t "$ot" --arg p "$other" '.[$t] = $p')"
      done < <(source_tables_of "$other_fp")
    done < <(printf '%s\n%s\n' "$other_desired" "$other_current" | grep -v '^[[:space:]]*$' | sort -u)
  done < <(manifest_plugins "$MANIFEST")

  # If any other-plugin footprint that should exist was unreadable, the
  # ownership map is incomplete and the collision check below cannot be trusted.
  if [ "$owner_map_err" -ne 0 ]; then
    log "      ownership map incomplete (a sibling footprint was unreadable)"
    return 1
  fi

  # collision prints the conflict and returns 1 when the candidate claims a
  # source_table owned by another plugin.
  if ! conflict="$(collision "$owners" "$plugin" "$fp")"; then
    log "      footprint collision: $conflict"
    return 1
  fi
  return 0
}

# Allow the test harness to source this file for its pure functions without
# running main.
if [ "${PLUGIN_PROMOTE_LIB_ONLY:-0}" != "1" ]; then
  main "$@"
fi

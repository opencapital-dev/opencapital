# Direct-to-production plugin publishing — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish plugin OCI images straight to the single `plugins` namespace on a git tag — no staging, no namespace copy, no catalog PR, no preview channel — and have the desktop app resolve them from per-plugin manifests that the publish CI maintains in each plugin's own repo.

**Architecture:** The retained `oc-plugin-publish-action` pushes the artifact to `ghcr.io/<owner>/plugins/<id>`, cosign-signs it, and appends the tag to the caller repo's own `oc-plugin.json` `versions[]` via the built-in `GITHUB_TOKEN` (own-repo write, no PAT, no PR). `plugins.json` stays central as a curated list of per-plugin manifest URLs, repointed to the plugin repos. The Tauri app reads `versions[]` over anonymous HTTP (it cannot enumerate GHCR tags) and loses all staging/preview machinery.

**Tech Stack:** Go (the `assemble` OCI packer), GitHub Actions composite action + bash/jq/gh, Rust (Tauri `src-tauri`, `reqwest`), React/TypeScript (`@grafana/ui`).

## Global Constraints

- Single namespace only: `ghcr.io/opencapital-dev/plugins/<id>`. No `plugins-staging`. No image copy between namespaces.
- No provisioned secrets in CI: push + manifest commit both use the workflow's built-in `GITHUB_TOKEN`. `CATALOG_PR_TOKEN` is removed.
- No PR, no preview channel, no `tags/list` enumeration. The app reads versions only from the manifest JSON over anonymous HTTP.
- cosign signing is **kept**, retargeted to the `plugins` namespace. App-side signature verification is **out of scope** (follow-up).
- Per-plugin manifest path: repo-root `oc-plugin.json`. Versions are stored **v-prefixed, semver-descending**.
- `manifest-bump.sh` runs only in GitHub Actions (ubuntu, bash 5) — bash 4+ features are fine here (this is NOT a bundled-app script).
- Repos: action repo `oc-plugin-publish-action` (clone to `/Users/ignacioballester/trading-code/oc-plugin-publish-action`); plugin repos are siblings `oc-plugin-core-app`, `oc-plugin-core-datasource`, `oc-plugin-yfinance-app`; app repo is `/Users/ignacioballester/trading-code/opencapital`.
- Spec: `docs/superpowers/specs/2026-06-24-direct-to-production-plugins-design.md`.

---

## Phase 1 — Publish pipeline (action repo + plugin repos)

### Task 1: Action — retarget push + cosign to the `plugins` namespace

**Files:**
- Modify: `oc-plugin-publish-action/assemble/oras.go` (the `repoRef` line + its doc comment)
- Modify: `oc-plugin-publish-action/action.yml` (the cosign sign step)

**Interfaces:**
- Produces: images + signatures at `ghcr.io/<owner>/plugins/<id>:<version>` (consumed by the app's `resolve_artifact` in Phase 2 and by Task 10's e2e check).

- [ ] **Step 1: Clone the action repo**

```bash
cd /Users/ignacioballester/trading-code
gh repo clone opencapital-dev/oc-plugin-publish-action
cd oc-plugin-publish-action
git checkout -b feat/direct-to-production
```

- [ ] **Step 2: Retarget the push in `assemble/oras.go`**

Replace the `repoRef` construction (currently `ghcr.io/%s/plugins-staging/%s`):

```go
	repoRef := fmt.Sprintf("ghcr.io/%s/plugins/%s", owner, id)
```

Also update the function doc comment above `run(...)` from
`// ... pushes it to ghcr.io/<owner>/plugins-staging/<id>:<version>.` to
`// ... pushes it to ghcr.io/<owner>/plugins/<id>:<version>.`

- [ ] **Step 3: Build to verify it compiles**

Run: `cd /Users/ignacioballester/trading-code/oc-plugin-publish-action/assemble && go build ./...`
Expected: no output, exit 0.

- [ ] **Step 4: Retarget the cosign sign step in `action.yml`**

In the cosign sign step, change the signed reference from
`ghcr.io/${{ inputs.owner }}/plugins-staging/${{ inputs.id }}@${DIGEST}` to:

```yaml
        cosign sign --yes --new-bundle-format \
          "ghcr.io/${{ inputs.owner }}/plugins/${{ inputs.id }}@${DIGEST}"
```

(Leave the cosign v3.0.6 install step and `docker/login-action` unchanged.)

- [ ] **Step 5: Verify no `plugins-staging` remains in the push/sign path**

Run: `cd /Users/ignacioballester/trading-code/oc-plugin-publish-action && grep -rn "plugins-staging" assemble/oras.go action.yml`
Expected: no matches (exit 1).

- [ ] **Step 6: Commit**

```bash
cd /Users/ignacioballester/trading-code/oc-plugin-publish-action
git add assemble/oras.go action.yml
git commit -m "feat: push + sign to the single plugins namespace (drop staging)"
```

---

### Task 2: Action — replace the catalog-PR bridge with own-repo manifest bump

**Files:**
- Create: `oc-plugin-publish-action/manifest-bump.sh`
- Create: `oc-plugin-publish-action/manifest-bump-merge.test.sh` (offline jq-filter test)
- Delete: `oc-plugin-publish-action/catalog-pr.sh`
- Modify: `oc-plugin-publish-action/action.yml` (drop catalog inputs + PR step; add bump step; update name/description)
- Modify: `oc-plugin-publish-action/README.md`

**Interfaces:**
- Consumes: `github.repository`, `github.token`, `inputs.id`, `inputs.owner`, `inputs.version` (from `action.yml`).
- Produces: a commit to the caller repo's `main` updating `oc-plugin.json` `versions[]` (consumed by the app via the manifest URL; verified in Task 10).

- [ ] **Step 1: Write the offline test for the version-merge jq filter**

Create `oc-plugin-publish-action/manifest-bump-merge.test.sh`:

```bash
#!/usr/bin/env bash
# Offline test for the versions[] merge filter used by manifest-bump.sh.
# No network/gh — just exercises the jq transform.
set -euo pipefail

# The merge filter: normalize all to v-prefixed, dedupe, sort semver-descending.
FILTER='.versions = (
  (((.versions // []) + [$v]) | map("v" + sub("^v";"")) | unique)
  | sort_by(sub("^v";"") | split(".") | map(split("-")[0] | tonumber? // 0))
  | reverse
)'

got="$(echo '{"versions":["0.1.2","v0.1.10","0.1.3"]}' \
  | jq -c --arg v "v0.2.0" "$FILTER")"
want='{"versions":["v0.2.0","v0.1.10","v0.1.3","v0.1.2"]}'
[ "$got" = "$want" ] || { echo "FAIL append+sort: got $got want $want"; exit 1; }

# Idempotent: re-adding an existing version (bare vs v-prefixed) does not duplicate.
got="$(echo '{"versions":["v0.1.3","0.1.2"]}' \
  | jq -c --arg v "0.1.3" "$FILTER")"
want='{"versions":["v0.1.3","v0.1.2"]}'
[ "$got" = "$want" ] || { echo "FAIL idempotent: got $got want $want"; exit 1; }

echo "PASS"
```

- [ ] **Step 2: Run the test to verify it fails (no script under test yet is fine — this tests the filter inline, so it should PASS once jq is present)**

Run: `bash /Users/ignacioballester/trading-code/oc-plugin-publish-action/manifest-bump-merge.test.sh`
Expected: `PASS`. (If `FAIL`, the jq filter in this step is wrong — fix it here before continuing; `manifest-bump.sh` in Step 3 must use the identical filter.)

- [ ] **Step 3: Write `manifest-bump.sh`**

Create `oc-plugin-publish-action/manifest-bump.sh`:

```bash
#!/usr/bin/env bash
# manifest-bump.sh — append <VERSION> to the CALLER repo's own oc-plugin.json
# versions[] on main, after the signed image is in ghcr.io/<owner>/plugins/<id>.
# Own-repo write via GITHUB_TOKEN: no PAT, no PR, no cross-repo. Idempotent.
# CI-only (GitHub Actions ubuntu). Generates the manifest on first publish.
#
# Env (set by action.yml):
#   REPO       owner/repo of the caller (github.repository)
#   ID         plugin id (inputs.id)
#   OWNER      github owner (inputs.owner) — registry.namespace = <owner>/plugins
#   VERSION    tag, e.g. v0.1.4 (bare 0.1.4 tolerated)
#   PUBLISHER  publisher string for first-publish generate (default OpenCapital)
#   GH_TOKEN   GITHUB_TOKEN with contents:write on REPO
set -euo pipefail
: "${REPO:?REPO required}" "${ID:?ID required}" "${OWNER:?OWNER required}" "${VERSION:?VERSION required}"
: "${GH_TOKEN:?GH_TOKEN required}"
export GH_TOKEN
PUBLISHER="${PUBLISHER:-OpenCapital}"
FILE="oc-plugin.json"
VER="v${VERSION#v}"

# Current manifest + its blob sha on main (empty on first publish).
if cur="$(gh api "repos/${REPO}/contents/${FILE}?ref=main" --jq '.content' 2>/dev/null | base64 -d)" && [ -n "$cur" ]; then
  fsha="$(gh api "repos/${REPO}/contents/${FILE}?ref=main" --jq '.sha')"
else
  cur=""
  fsha=""
fi

if [ -z "$cur" ]; then
  cur="$(jq -n --arg id "$ID" --arg pub "$PUBLISHER" --arg ns "${OWNER}/plugins" '{
    schemaVersion: 1, pluginId: $id, publisher: $pub,
    registry: { host: "ghcr.io", namespace: $ns, publicURL: "https://ghcr.io" },
    versions: []
  }')"
fi

new="$(printf '%s' "$cur" | jq --arg v "$VER" '.versions = (
  (((.versions // []) + [$v]) | map("v" + sub("^v";"")) | unique)
  | sort_by(sub("^v";"") | split(".") | map(split("-")[0] | tonumber? // 0))
  | reverse
)')"

if [ "$(printf '%s' "$cur" | jq -S .)" = "$(printf '%s' "$new" | jq -S .)" ]; then
  echo "::notice::${ID} ${VER} already in versions[]; no commit"
  exit 0
fi

args=(-f "message=release: ${ID} ${VER}" -f "content=$(printf '%s\n' "$new" | base64 -w0)" -f "branch=main")
[ -n "$fsha" ] && args+=(-f "sha=${fsha}")
gh api "repos/${REPO}/contents/${FILE}" -X PUT "${args[@]}" >/dev/null
echo "::notice::committed ${ID} ${VER} to ${REPO}@main:${FILE}"
```

Make it executable:

```bash
chmod +x /Users/ignacioballester/trading-code/oc-plugin-publish-action/manifest-bump.sh
```

- [ ] **Step 4: Verify the script parses and re-run the filter test**

Run: `cd /Users/ignacioballester/trading-code/oc-plugin-publish-action && bash -n manifest-bump.sh && echo syntax-ok && bash manifest-bump-merge.test.sh`
Expected: `syntax-ok` then `PASS`. Eyeball that the `.versions = ( ... )` filter inside `manifest-bump.sh` is byte-identical to the one in `manifest-bump-merge.test.sh`.

- [ ] **Step 5: Delete the obsolete catalog-PR script**

```bash
cd /Users/ignacioballester/trading-code/oc-plugin-publish-action
git rm catalog-pr.sh
```

- [ ] **Step 6: Rewire `action.yml` — drop catalog inputs + PR step, add the bump step, fix name/description**

In `action.yml`:
- Change the top `name`/`description`:

```yaml
name: "OpenCapital plugin publish"
description: "Assemble + sign + push a Grafana plugin to GHCR plugins, then record the version in the plugin's own manifest"
```

- Delete the three inputs `catalog-repo`, `catalog-channel`, `catalog-token` entirely.
- Delete the final `- name: open catalog PR` step (the one running `catalog-pr.sh`).
- Append a new last step:

```yaml
    - name: bump manifest versions[]
      shell: bash
      env:
        REPO: ${{ github.repository }}
        ID: ${{ inputs.id }}
        OWNER: ${{ inputs.owner }}
        VERSION: ${{ inputs.version }}
        GH_TOKEN: ${{ github.token }}
      run: bash "${{ github.action_path }}/manifest-bump.sh"
```

- [ ] **Step 7: Verify the action manifest is valid YAML and catalog refs are gone**

Run:
```bash
cd /Users/ignacioballester/trading-code/oc-plugin-publish-action
python3 -c "import yaml,sys; yaml.safe_load(open('action.yml')); print('yaml-ok')"
grep -n "catalog" action.yml || echo "no-catalog-refs"
```
Expected: `yaml-ok` then `no-catalog-refs`.

- [ ] **Step 8: Update `README.md`**

Rewrite the README so it states the artifact is pushed to `ghcr.io/<owner>/plugins/<id>:<version>` (not `plugins-staging`), describes the new "bump manifest versions[]" step (own-repo `oc-plugin.json` commit via `GITHUB_TOKEN`, no catalog PR), removes the `catalog-*` inputs from the inputs table, and notes the caller must grant `contents: write` in addition to `packages: write` + `id-token: write`.

- [ ] **Step 9: Commit**

```bash
cd /Users/ignacioballester/trading-code/oc-plugin-publish-action
git add manifest-bump.sh manifest-bump-merge.test.sh action.yml README.md
git commit -m "feat: bump caller's own oc-plugin.json versions[] (drop catalog PR)"
```

---

### Task 3: Action — ship (push branch, merge, move `v1`)

**Files:** none (release operations on `oc-plugin-publish-action`).

**Interfaces:**
- Produces: `oc-plugin-publish-action@v1` now points at the direct-to-production behavior (consumed by every plugin repo's `publish.yml`).

- [ ] **Step 1: Push the branch and open/merge a PR**

```bash
cd /Users/ignacioballester/trading-code/oc-plugin-publish-action
git push -u origin feat/direct-to-production
gh pr create --fill --base main
```
Merge it (squash) once green:
```bash
gh pr merge --squash --delete-branch
```

- [ ] **Step 2: Move the `v1` tag to the merged commit**

```bash
cd /Users/ignacioballester/trading-code/oc-plugin-publish-action
git checkout main && git pull
git tag -f v1
git push origin v1 --force
```

- [ ] **Step 3: Verify `v1` resolves to the new behavior**

Run: `gh api repos/opencapital-dev/oc-plugin-publish-action/contents/manifest-bump.sh?ref=v1 --jq '.name'`
Expected: `manifest-bump.sh` (the file exists at the `v1` tag).

---

### Task 4: Plugin repos — add `oc-plugin.json` + grant `contents: write`

**Files (per plugin repo, ×3):**
- Create: `<plugin-repo>/oc-plugin.json`
- Modify: `<plugin-repo>/.github/workflows/publish.yml`

Plugin repos and their seed versions:
- `oc-plugin-core-app` → id `core-app`, versions `["0.1.3","0.1.2"]`
- `oc-plugin-core-datasource` → id `core-datasource`, versions `["0.1.7"]`
- `oc-plugin-yfinance-app` → id `yfinance-app`, versions `["0.1.3","0.1.2"]`

**Interfaces:**
- Produces: `https://raw.githubusercontent.com/opencapital-dev/<plugin-repo>/main/oc-plugin.json` (consumed by `plugins.json` in Task 5 and by the app's manifest fetch).

- [ ] **Step 1: Create `oc-plugin.json` in each plugin repo**

`oc-plugin-yfinance-app/oc-plugin.json` (mirror for the other two with the id + versions above):

```json
{
  "schemaVersion": 1,
  "pluginId": "yfinance-app",
  "publisher": "OpenCapital",
  "registry": {
    "host": "ghcr.io",
    "namespace": "opencapital-dev/plugins",
    "publicURL": "https://ghcr.io"
  },
  "versions": ["0.1.3", "0.1.2"]
}
```

- [ ] **Step 2: Verify each manifest is valid JSON**

Run:
```bash
for r in oc-plugin-core-app oc-plugin-core-datasource oc-plugin-yfinance-app; do
  jq -e '.pluginId and .registry.namespace=="opencapital-dev/plugins" and (.versions|length>0) and (has("stagingNamespace")|not) and (has("preview")|not)' \
    /Users/ignacioballester/trading-code/$r/oc-plugin.json >/dev/null \
    && echo "$r ok" || echo "$r BAD"
done
```
Expected: three `... ok` lines.

- [ ] **Step 3: Grant `contents: write` and drop `catalog-token` in each `publish.yml`**

In each `<plugin-repo>/.github/workflows/publish.yml`:
- Change the top-level `permissions:` block to:

```yaml
permissions:
  contents: write
  packages: write
  id-token: write
```

- Remove the `catalog-token: ${{ secrets.CATALOG_PR_TOKEN }}` line from the `uses: opencapital-dev/oc-plugin-publish-action@v1` `with:` block. (Keep `dir`, `id`, `owner`, `version`, `platforms`.)

- [ ] **Step 4: Verify the workflow edits**

Run:
```bash
for r in oc-plugin-core-app oc-plugin-core-datasource oc-plugin-yfinance-app; do
  f=/Users/ignacioballester/trading-code/$r/.github/workflows/publish.yml
  grep -q "contents: write" "$f" && ! grep -q "catalog-token" "$f" && echo "$r ok" || echo "$r BAD"
done
```
Expected: three `... ok` lines.

- [ ] **Step 5: Commit in each plugin repo**

```bash
for r in oc-plugin-core-app oc-plugin-core-datasource oc-plugin-yfinance-app; do
  ( cd /Users/ignacioballester/trading-code/$r \
    && git add oc-plugin.json .github/workflows/publish.yml \
    && git commit -m "feat: own oc-plugin.json manifest; publish direct to production" )
done
```

(Push happens with the real release in Task 10; or push now if these repos auto-deploy nothing on push.)

---

## Phase 2 — Desktop app + central catalog (`opencapital` repo)

> Tasks 6–9 are pure `opencapital` code changes and do **not** depend on Phase 1. Task 5's URL-liveness check and Task 10's e2e require Phase 1 shipped + plugin repos pushed.

### Task 5: Repoint `plugins.json`, delete the central per-plugin manifests

**Files:**
- Modify: `plugins.json`
- Delete: `plugins/core-app.json`, `plugins/core-datasource.json`, `plugins/yfinance-app.json`

- [ ] **Step 1: Repoint `plugins.json` to the plugin-repo manifest URLs**

Replace `plugins.json` with:

```json
{
  "schemaVersion": 1,
  "plugins": [
    "https://raw.githubusercontent.com/opencapital-dev/oc-plugin-core-app/main/oc-plugin.json",
    "https://raw.githubusercontent.com/opencapital-dev/oc-plugin-core-datasource/main/oc-plugin.json",
    "https://raw.githubusercontent.com/opencapital-dev/oc-plugin-yfinance-app/main/oc-plugin.json"
  ]
}
```

- [ ] **Step 2: Delete the central per-plugin manifests**

```bash
cd /Users/ignacioballester/trading-code/opencapital
git rm plugins/core-app.json plugins/core-datasource.json plugins/yfinance-app.json
```

- [ ] **Step 3: Verify `plugins.json` shape**

Run: `jq -e '.plugins | length == 3 and (map(test("oc-plugin-.*/main/oc-plugin.json")) | all)' /Users/ignacioballester/trading-code/opencapital/plugins.json`
Expected: `true`.

- [ ] **Step 4: Commit**

```bash
cd /Users/ignacioballester/trading-code/opencapital
git add plugins.json plugins/
git commit -m "feat(catalog): point plugins.json at plugin-repo manifests; drop central copies"
```

---

### Task 6: Rust catalog — strip staging + preview; versions come from `versions[]`

**Files:**
- Modify: `opencapital-app/src-tauri/src/catalog/manifest.rs`
- Modify: `opencapital-app/src-tauri/src/catalog/registry.rs`
- Modify: `opencapital-app/src-tauri/src/catalog/mod.rs`
- Modify: `opencapital-app/src-tauri/src/catalog/sources.rs`
- Modify: `opencapital-app/src-tauri/src/kinde.rs` (the `plugin_versions` command — sole external caller of the removed `versions_with_status`)

**Interfaces:**
- Produces: `PluginRef { manifest_url, plugin_id, publisher, verified, reg: RegistryCoords { host, namespace, public_url }, versions: Vec<String> }`; `pick_version(&PluginRef) -> Option<(String, String)>`; `resolve_artifact(...)` resolving against `reg.namespace` only; `plugin_versions(...) -> Result<Vec<String>, String>`.
- Consumes: existing `sort_semver_desc`, `fetch_oci_manifest`, `blob_url`, `tag_forms`, `PLATFORM_ANNOTATION`.

These structs are interdependent, so they change and compile together as one task; the test cycle is `cargo test`.

- [ ] **Step 1: Update the tests to the new behavior (they will fail to compile first)**

In `registry.rs`, delete the `versions_with_status_union_and_dedup` test entirely (it relies on `validated`/`preview`/`versions_with_status`, all removed).

In `mod.rs` tests, replace the three `pick_version_*` tests with:

```rust
    #[test]
    fn pick_version_picks_highest() {
        let r = PluginRef {
            manifest_url: "http://example.com/m.json".into(),
            plugin_id: "test".into(),
            publisher: "pub".into(),
            verified: true,
            reg: RegistryCoords {
                host: "ghcr.io".into(),
                namespace: "ns/plugins".into(),
                public_url: String::new(),
            },
            versions: vec!["v1.1.0".into(), "v1.0.0".into()],
        };
        let (ver, ns) = pick_version(&r).unwrap();
        assert_eq!(ver, "v1.1.0");
        assert_eq!(ns, "ns/plugins");
    }

    #[test]
    fn pick_version_none_when_empty() {
        let r = PluginRef {
            manifest_url: "http://example.com/m.json".into(),
            plugin_id: "test".into(),
            publisher: "pub".into(),
            verified: true,
            reg: RegistryCoords::default(),
            versions: vec![],
        };
        assert!(pick_version(&r).is_none());
    }
```

In `manifest.rs`, update `validate_plugin_rejects_missing_fields` — remove the `preview` field from the struct literal and delete the final two assertions about preview/stagingNamespace:

```rust
    #[test]
    fn validate_plugin_rejects_missing_fields() {
        let mut m = PluginManifest {
            schema_version: 1,
            plugin_id: "".into(),
            publisher: "".into(),
            registry: RegistrySpec::default(),
            versions: vec![],
        };
        assert!(validate_plugin(&m).is_err()); // missing pluginId
        m.plugin_id = "test".into();
        assert!(validate_plugin(&m).is_err()); // missing registry.host
        m.registry.host = "ghcr.io".into();
        assert!(validate_plugin(&m).is_err()); // missing registry.namespace
        m.registry.namespace = "ns/p".into();
        assert!(validate_plugin(&m).is_ok()); // OK
    }
```

- [ ] **Step 2: Run tests to confirm they fail to compile**

Run: `cd /Users/ignacioballester/trading-code/opencapital/opencapital-app/src-tauri && cargo test --lib catalog 2>&1 | tail -20`
Expected: compile errors (fields `validated`/`preview`/`staging_namespace` no longer match, etc.). This is expected — implement next.

- [ ] **Step 3: `manifest.rs` — drop `stagingNamespace` + `preview`, simplify validation**

`RegistrySpec`:

```rust
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct RegistrySpec {
    pub host: String,
    pub namespace: String,
    #[serde(rename = "publicURL", default)]
    pub public_url: String,
}
```

`PluginManifest` (remove the `preview` field):

```rust
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PluginManifest {
    #[serde(rename = "schemaVersion", default)]
    pub schema_version: u32,
    #[serde(rename = "pluginId")]
    pub plugin_id: String,
    #[serde(default)]
    pub publisher: String,
    pub registry: RegistrySpec,
    #[serde(default)]
    pub versions: Vec<String>,
}
```

`validate_plugin` (drop the preview/staging rule):

```rust
pub fn validate_plugin(m: &PluginManifest) -> Result<(), String> {
    if m.plugin_id.is_empty() {
        return Err("pluginId required".into());
    }
    if m.registry.host.is_empty() {
        return Err("registry.host required".into());
    }
    if m.registry.namespace.is_empty() {
        return Err("registry.namespace required".into());
    }
    Ok(())
}
```

- [ ] **Step 4: `registry.rs` — drop staging from coords/ref, single-namespace resolve, remove `VersionStatus`/`versions_with_status`**

`RegistryCoords`:

```rust
#[derive(Debug, Clone, Default)]
pub struct RegistryCoords {
    pub host: String,
    pub namespace: String,
    pub public_url: String,
}
```

`PluginRef` (replace `validated` + `preview` with one list):

```rust
#[derive(Debug, Clone)]
pub struct PluginRef {
    pub manifest_url: String,
    pub plugin_id: String,
    pub publisher: String,
    pub verified: bool,
    pub reg: RegistryCoords,
    /// Published versions, semver-desc.
    pub versions: Vec<String>,
}
```

Delete the `VersionStatus` struct (the `#[derive(...)] pub struct VersionStatus { version, validated }`) and the entire `versions_with_status` function.

`resolve_artifact` (single namespace, no fallback loop):

```rust
/// resolve_artifact returns the per-platform tarball blob for (id, version)
/// from the plugin's namespace. Returns None when the namespace has no layer
/// for that platform.
pub async fn resolve_artifact(
    client: &reqwest::Client,
    plugin_ref: &PluginRef,
    version: &str,
    platform: &str,
) -> Result<Option<Artifact>, String> {
    let reg = &plugin_ref.reg;
    for tag in tag_forms(version) {
        let Some(man) =
            fetch_oci_manifest(client, &reg.host, &reg.namespace, &plugin_ref.plugin_id, &tag).await?
        else {
            continue;
        };
        for layer in &man.layers {
            if layer.annotations.get(PLATFORM_ANNOTATION).map(String::as_str) == Some(platform) {
                let digest = &layer.digest;
                let sha256 = digest.strip_prefix("sha256:").unwrap_or(digest).to_string();
                return Ok(Some(Artifact {
                    download_url: blob_url(&reg.public_base(), &reg.namespace, &plugin_ref.plugin_id, digest),
                    sha256,
                    size_bytes: layer.size,
                }));
            }
        }
    }
    Ok(None)
}
```

- [ ] **Step 5: `mod.rs` — simplify `pick_version`, `ref_to_plugin`, exports**

Update the `pub use` (drop `VersionStatus` and `versions_with_status`):

```rust
pub use registry::{
    Artifact, Plugin, PluginRef, RegistryCoords, SourceInfo,
    resolve_artifact, sort_semver_desc,
};
```

Replace `pick_version`:

```rust
/// pick_version returns (highest version, namespace), or None when the plugin
/// has no published versions.
fn pick_version(r: &PluginRef) -> Option<(String, String)> {
    r.versions.first().map(|v| (v.clone(), r.reg.namespace.clone()))
}
```

In `ref_to_plugin`, change the destructuring and the emitted version (remove `preview_only` and the version-blanking):

```rust
    let (version, namespace) = pick_version(r)?;
```

and at the `Some(Plugin { ... })` construction use `version` directly:

```rust
    Some(Plugin {
        footprint,
        required: required_set.contains(r.plugin_id.as_str()),
        version,
        platforms,
        source: SourceInfo {
            url: r.manifest_url.clone(),
            publisher: r.publisher.clone(),
            verified: r.verified,
        },
    })
```

Delete the now-unused `emitted_version`/`preview_only` lines.

- [ ] **Step 6: `sources.rs` — `manifest_to_ref` drops staging/preview**

```rust
fn manifest_to_ref(manifest_url: String, verified: bool, m: PluginManifest) -> PluginRef {
    PluginRef {
        manifest_url,
        plugin_id: m.plugin_id,
        publisher: m.publisher,
        verified,
        reg: RegistryCoords {
            host: m.registry.host,
            namespace: m.registry.namespace,
            public_url: m.registry.public_url,
        },
        versions: sort_semver_desc(&m.versions),
    }
}
```

- [ ] **Step 7: `kinde.rs` — `plugin_versions` returns `Vec<String>`**

Delete the local `VersionStatus` struct (`#[derive(serde::Serialize)] pub struct VersionStatus { version, validated }`) and rewrite the command:

```rust
/// plugin_versions lists the published versions of a plugin, newest first,
/// served in-process from the federated catalog (versions[] in the manifest).
#[tauri::command]
pub async fn plugin_versions(
    plugin_id: String,
    cfg: State<'_, AppConfig>,
    session: State<'_, Session>,
) -> Result<Vec<String>, String> {
    let _ = session; // auth check dropped for in-process call
    let user_sources = crate::catalog::sources::read_sources_in(&cfg.base_dir())?;
    let refs = build_refs_cached(&cfg.plugin_list_url, &user_sources).await;
    match refs.iter().find(|r| r.plugin_id == plugin_id) {
        None => Ok(vec![]),
        Some(r) => Ok(r.versions.clone()),
    }
}
```

- [ ] **Step 8: Run tests to verify they pass**

Run: `cd /Users/ignacioballester/trading-code/opencapital/opencapital-app/src-tauri && cargo test --lib catalog && cargo test --lib kinde 2>&1 | tail -15`
Expected: all tests pass; no warnings about unused `staging_namespace`/`validated`/`preview`.

- [ ] **Step 9: Full crate compile check**

Run: `cd /Users/ignacioballester/trading-code/opencapital/opencapital-app/src-tauri && cargo build 2>&1 | tail -15`
Expected: `Finished` (no errors). If `grafana.rs:330` (the `resolve_artifact` caller) errors, it should not — the signature is unchanged.

- [ ] **Step 10: Commit**

```bash
cd /Users/ignacioballester/trading-code/opencapital
git add opencapital-app/src-tauri/src/catalog opencapital-app/src-tauri/src/kinde.rs
git commit -m "feat(catalog): single namespace, versions[] only; drop staging + preview"
```

---

### Task 7: Rust — remove the `show_preview` toggle (command, handler, config)

**Files:**
- Modify: `opencapital-app/src-tauri/src/kinde.rs` (remove `get_show_preview` / `set_show_preview`)
- Modify: `opencapital-app/src-tauri/src/lib.rs:131-132` (remove the two handlers)
- Modify: `opencapital-app/src-tauri/src/config.rs` (remove `read_show_preview_in` / `set_show_preview_in` + their two tests)

- [ ] **Step 1: Remove the two commands from `kinde.rs`**

Delete the `get_show_preview` and `set_show_preview` `#[tauri::command]` functions (the block at ~563–573).

- [ ] **Step 2: Remove the two handlers from `lib.rs`**

In the `tauri::generate_handler![ ... ]` list, delete the lines:

```rust
            kinde::get_show_preview,
            kinde::set_show_preview,
```

- [ ] **Step 3: Remove the config helpers + their tests from `config.rs`**

Delete `read_show_preview_in` and `set_show_preview_in` (the block at ~400–422), and delete the two tests `show_preview_defaults_false_and_roundtrips` and `set_show_preview_preserves_other_settings_keys` (~488–507).

- [ ] **Step 4: Verify no `show_preview` references remain**

Run: `cd /Users/ignacioballester/trading-code/opencapital && grep -rn "show_preview" opencapital-app/src-tauri/src/ || echo "clean"`
Expected: `clean`.

- [ ] **Step 5: Build + test**

Run: `cd /Users/ignacioballester/trading-code/opencapital/opencapital-app/src-tauri && cargo build && cargo test --lib config 2>&1 | tail -10`
Expected: `Finished` and config tests pass.

- [ ] **Step 6: Commit**

```bash
cd /Users/ignacioballester/trading-code/opencapital
git add opencapital-app/src-tauri/src/kinde.rs opencapital-app/src-tauri/src/lib.rs opencapital-app/src-tauri/src/config.rs
git commit -m "feat: remove show-preview toggle (no preview channel)"
```

---

### Task 8: Frontend — drop preview UX; versions are plain strings

**Files:**
- Modify: `opencapital-app/src/types.ts`
- Modify: `opencapital-app/src/api.ts`
- Modify: `opencapital-app/src/components/PluginsView.tsx`

- [ ] **Step 1: `types.ts` — remove `VersionStatus`**

Delete the line `export type VersionStatus = { version: string; validated: boolean };`. Leave `CatalogEntry.latest_validated_version?` as-is (now means "latest version").

- [ ] **Step 2: `api.ts` — `pluginVersions` returns `string[]`; drop preview calls**

- Change the import on line 2 to drop `VersionStatus`:

```ts
import type { Catalog, KindeProfile, PluginSource } from "./types";
```

- Change `pluginVersions`:

```ts
  pluginVersions: (pluginId: string) =>
    invoke<string[]>("plugin_versions", { pluginId }),
```

- Delete the two lines:

```ts
  getShowPreview: () => invoke<boolean>("get_show_preview"),
  setShowPreview: (on: boolean) => invoke<void>("set_show_preview", { on }),
```

- [ ] **Step 3: `PluginsView.tsx` — remove preview state/toggle/badge, treat versions as strings**

- Import (line 22): drop `VersionStatus`:

```tsx
import type { CatalogEntry } from "../types";
```

- `versions` state (line ~39):

```tsx
  const [versions, setVersions] = useState<Record<string, string[]>>({});
```

- Delete the `showPreview` state line (`const [showPreview, setShowPreview] = useState(false);`).
- In the `useEffect` (line ~83) delete `api.getShowPreview().then(setShowPreview).catch(() => {});`.
- Delete the whole `handleShowPreview` function (lines ~137–140).
- Delete the "Show preview versions" control in the header (the `<InlineField label="Show preview versions" ...><Switch .../></InlineField>` block, ~166–169). (`InlineField` is still used elsewhere; keep the import.)
- Replace the version-list block (lines ~201–227) with string handling:

```tsx
            const vsList = versions[p.plugin_id] ?? [];
            const loaded = versions[p.plugin_id] !== undefined;

            // "Latest" is the recommended default; specific versions pin (freeze)
            // the plugin. Always include the current pin so the control shows it
            // even before the version list has loaded.
            const versionOptions: Array<SelectableValue<string>> = [
              {
                label: "Latest",
                value: LATEST,
                description: "Auto-updates to the newest published build",
                icon: "arrow-up",
              },
            ];
            const seen = new Set<string>();
            vsList.forEach((v) => {
              seen.add(v);
              versionOptions.push({ label: fmtVersion(v)!, value: v });
            });
            if (pin && !seen.has(pin)) {
              versionOptions.push({ label: fmtVersion(pin)!, value: pin });
            }
```

(`loadVersions` already does `const vs = await api.pluginVersions(...)` and `setVersions(... vs)`; with the new `string[]` return type it now stores strings — no change needed there.)

- [ ] **Step 4: Type-check + lint + build the frontend**

Run:
```bash
cd /Users/ignacioballester/trading-code/opencapital/opencapital-app
npm run typecheck 2>/dev/null || npx tsc --noEmit
npm run lint
npm run build
```
Expected: tsc clean (no `VersionStatus`/`validated`/`showPreview` errors), lint clean, build succeeds.

- [ ] **Step 5: Verify no preview references remain in the frontend**

Run: `cd /Users/ignacioballester/trading-code/opencapital && grep -rn "showPreview\|ShowPreview\|VersionStatus\|validated" opencapital-app/src/ || echo "clean"`
Expected: `clean`.

- [ ] **Step 6: Commit**

```bash
cd /Users/ignacioballester/trading-code/opencapital
git add opencapital-app/src/types.ts opencapital-app/src/api.ts opencapital-app/src/components/PluginsView.tsx
git commit -m "feat(ui): drop preview toggle/badge; versions are plain strings"
```

---

### Task 9: Docs — purge staging / promotion / preview language

**Files:**
- Modify: `docs/superpowers/specs/2026-06-14-federated-plugin-sources-design.md`
- Modify: `docs/superpowers/plans/2026-06-14-federated-plugin-sources-*.md`
- Modify: any `reference/` doc mentioning `plugins-staging` / promotion / preview

- [ ] **Step 1: Find every stale reference**

Run:
```bash
cd /Users/ignacioballester/trading-code/opencapital
grep -rln "plugins-staging\|stagingNamespace\|promote\|promotion\|preview" docs/ reference/ 2>/dev/null
```

- [ ] **Step 2: Update each hit to the new model**

For each file, edit prose so it describes: a single `plugins` namespace; per-plugin manifests living in each plugin repo (`oc-plugin.json`); `plugins.json` as a curated list of those manifest URLs; tag-publish appends `versions[]` via the action using `GITHUB_TOKEN`; no staging, no namespace copy, no catalog PR, no preview channel. Add a one-line "Superseded by `docs/superpowers/specs/2026-06-24-direct-to-production-plugins-design.md`" note at the top of the 2026-06-14 federated-sources docs rather than rewriting them wholesale.

- [ ] **Step 3: Verify no stale registry references in docs (preview as an English word elsewhere is fine — scope to the catalog docs)**

Run:
```bash
cd /Users/ignacioballester/trading-code/opencapital
grep -rn "plugins-staging\|stagingNamespace" docs/ reference/ 2>/dev/null || echo "clean"
```
Expected: `clean`.

- [ ] **Step 4: Commit**

```bash
cd /Users/ignacioballester/trading-code/opencapital
git add docs/ reference/ 2>/dev/null
git commit -m "docs: describe single-namespace federated publishing"
```

---

## Phase 3 — End-to-end verification

### Task 10: Real publish + app catalog smoke test

**Files:** `oc-plugin-yfinance-app/package.json` (version bump) — and a git tag.

> This publishes a REAL new version to the production namespace. Do it only after Tasks 1–9 are merged and the plugin repos are pushed. Confirm with the user before pushing the tag.

- [ ] **Step 1: Push the Phase-1 plugin-repo commits**

```bash
for r in oc-plugin-core-app oc-plugin-core-datasource oc-plugin-yfinance-app; do
  ( cd /Users/ignacioballester/trading-code/$r && git push )
done
```

- [ ] **Step 2: Bump + tag yfinance to a new patch**

```bash
cd /Users/ignacioballester/trading-code/oc-plugin-yfinance-app
# set package.json version to the next patch, e.g. 0.1.4
npm version 0.1.4 --no-git-tag-version
git add package.json package-lock.json && git commit -m "release: v0.1.4"
git push
git tag v0.1.4 && git push origin v0.1.4
```

- [ ] **Step 3: Watch the publish workflow**

Run: `gh run watch --repo opencapital-dev/oc-plugin-yfinance-app` (or `gh run list --repo opencapital-dev/oc-plugin-yfinance-app`).
Expected: the `publish` workflow succeeds.

- [ ] **Step 4: Verify the image is in production (and absent from staging)**

Run:
```bash
oras manifest fetch ghcr.io/opencapital-dev/plugins/yfinance-app:v0.1.4 >/dev/null && echo "in plugins"
oras manifest fetch ghcr.io/opencapital-dev/plugins-staging/yfinance-app:v0.1.4 2>/dev/null && echo "STILL IN STAGING (bad)" || echo "not in staging (good)"
```
Expected: `in plugins` then `not in staging (good)`.

- [ ] **Step 5: Verify the manifest bump committed (no PR)**

Run:
```bash
gh api repos/opencapital-dev/oc-plugin-yfinance-app/contents/oc-plugin.json --jq '.content' | base64 -d | jq '.versions'
gh pr list --repo opencapital-dev/oc-plugin-yfinance-app --state open
```
Expected: `versions` contains `"v0.1.4"`; no catalog PR was opened.

- [ ] **Step 6: Verify the cosign signature exists on the production image**

Run: `cosign triangulate ghcr.io/opencapital-dev/plugins/yfinance-app:v0.1.4`
Expected: prints the signature tag reference (a `sha256-...sig` ref exists).

- [ ] **Step 7: App catalog smoke test (no GitHub token in env)**

```bash
cd /Users/ignacioballester/trading-code/opencapital
env -u GITHUB_TOKEN -u GH_TOKEN make app
```
In the app: open Plugins → confirm `yfinance-app` shows `v0.1.4`, there is **no** "Show preview versions" toggle and **no** "preview" badge; pin `v0.1.4`; Launch and confirm the plugin installs and its `dashboards/` + `library-panels/` provision into Grafana.

- [ ] **Step 8: Final repo-wide staging sweep in the app repo**

Run: `cd /Users/ignacioballester/trading-code/opencapital && grep -rn "plugins-staging\|stagingNamespace\|staging_namespace\|show_preview\|VersionStatus" opencapital-app plugins.json plugins 2>/dev/null || echo "clean"`
Expected: `clean`.

---

## Self-review notes (coverage map spec → tasks)

- Spec §A (action: oras.go, action.yml cosign, drop catalog inputs/step, manifest-bump.sh, README, re-tag v1) → Tasks 1–3.
- Spec §B (plugin repos: oc-plugin.json seed, publish.yml permissions + drop catalog-token) → Task 4.
- Spec §C manifests/list (repoint plugins.json, delete central) → Task 5.
- Spec §C Rust catalog (manifest/registry/mod/sources, + plugin_versions) → Task 6.
- Spec §C Rust commands/config (show_preview removal, lib.rs handlers) → Task 7.
- Spec §C frontend (types/api/PluginsView) → Task 8.
- Spec §D docs → Task 9.
- Spec verification (cargo test, make app, e2e publish, anonymous resolution) → Tasks 6/7/8 (unit) + Task 10 (e2e).

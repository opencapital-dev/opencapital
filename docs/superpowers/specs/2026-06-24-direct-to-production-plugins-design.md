# Direct-to-production plugin publishing — remove staging, promotion, preview

Date: 2026-06-24
Status: approved design, pre-implementation
Branch base: `feat/federated-plugin-sources`

## Context

Plugin OCI images today flow through a two-namespace promotion pipeline:

1. The external composite action `opencapital-dev/oc-plugin-publish-action@v1`
   (called by each plugin repo's `publish.yml`) assembles the OCI artifact and
   pushes it to the **staging** namespace `ghcr.io/opencapital-dev/plugins-staging/<id>`,
   cosign-signs it, then opens a "catalog PR" on the opencapital repo.
2. Merging that PR was meant to trigger `plugin-promote-reconcile`, which copied
   (`oras cp`) the image **staging → trusted** (`ghcr.io/opencapital-dev/plugins`)
   after a cosign + footprint-collision gate.

That promotion machinery was already half-dismantled by the federated-sources
migration: the promote workflows, the `plugin-promote` composite action, and the
Go control-plane are deleted (serviceless); `plugins.json` is now a federated
list of per-plugin manifest URLs. What remains is **orphaned and broken**: the
publish action still pushes to `plugins-staging` and still runs `catalog-pr.sh`,
which writes the *old* `plugins.json` map format and promises a reconcile
workflow that no longer exists. Meanwhile the app and manifests still carry the
staging namespace, a preview channel, and a "show preview" toggle.

**Goal:** collapse to a single production namespace. Publishing a git tag in a
plugin repo is the *only* step — it pushes the image directly to
`ghcr.io/opencapital-dev/plugins/<id>` and records the version in that plugin's
own manifest, with **no staging, no image copy between namespaces, no promotion,
no cross-repo PR, no preview channel, and no provisioned token**. The desktop
app continues to read versions from a manifest over plain anonymous HTTP.

### Key constraint (why we keep `versions[]` in a manifest)

The Tauri app pulls from GHCR anonymously. Anonymous GHCR auth permits
manifest-by-tag and blob GETs (which the app already does via the token dance in
`registry.rs::fetch_oci_manifest` / `ghcr_authed_get`) but **not** tag
enumeration (`/v2/<ns>/<id>/tags/list` requires a real token). We will not
provision a token in the app. Therefore the set of published versions must be
served as static JSON — the per-plugin manifest's `versions[]` array — not
discovered from the registry.

### Key constraint (why manifests move into plugin repos)

To make "publish the tag" sufficient, the publish CI must append the new version
to `versions[]` itself. CI in a plugin repo can write its **own** repo with the
built-in `GITHUB_TOKEN` (no provisioned secret), but cannot write the opencapital
repo without a cross-repo PAT (rejected). So each per-plugin manifest must live
**in its own plugin repo**, where its CI can commit it.

## Target architecture

- **`plugins.json`** (curated list, stays in opencapital) — 3 pointer URLs,
  repointed from `opencapital/.../plugins/<id>.json` to each plugin repo's raw
  manifest URL. Holds no versions.
- **Per-plugin manifest** (relocated into each plugin repo, e.g. repo-root
  `oc-plugin.json`) — `schemaVersion`, `pluginId`, `publisher`,
  `registry {host, namespace, publicURL}`, `versions[]`. No `stagingNamespace`,
  no `preview`. The publish CI generates it if absent and appends each tag to
  `versions[]`.
- **Single namespace** `ghcr.io/opencapital-dev/plugins/<id>`. The publish CI
  pushes directly here.
- **Publish action retained, modified.** `oc-plugin-publish-action@v1` keeps the
  proven multi-platform packer and cosign signing; it is changed to push to the
  single namespace, drop the catalog-PR bridge, and append the tag to the
  calling repo's own manifest `versions[]`. The plugin manifest gets the OCI
  image's cosign signature; the app is the intended verifier (deferred — see
  out-of-scope).

## Changes by repo

### A. `oc-plugin-publish-action` — keep, modify

Retain the external composite action (the proven multi-platform OCI packer in
`assemble/`). Changes:

1. `assemble/oras.go` — change the push target from
   `ghcr.io/%s/plugins-staging/%s` → `ghcr.io/%s/plugins/%s` (and the comment).
2. `action.yml`:
   - cosign sign step → target `ghcr.io/${{ inputs.owner }}/plugins/${{ inputs.id }}@${DIGEST}`
     (signing **kept**; install dance for cosign v3.0.6 unchanged).
   - Remove the `catalog-repo`, `catalog-channel`, `catalog-token` inputs and the
     "open catalog PR" step.
   - Add a **manifest-bump** step running the rewritten script with
     `REPO=${{ github.repository }}`, `GH_TOKEN=${{ github.token }}`,
     `ID=${{ inputs.id }}`, `VERSION=${{ inputs.version }}` — writes the **calling
     repo's own** manifest, so no PAT and no cross-repo write.
   - Update `name`/`description` (drop "plugins-staging").
3. `catalog-pr.sh` → `manifest-bump.sh`: `gh api` GET `oc-plugin.json` on the
   caller repo's `main`; `jq`-append the v-normalized tag to `.versions`
   (`unique | semver-desc`, no-op if already present); generate the full manifest
   from constants + `pluginId` if the file is absent (first publish); `gh api` PUT
   back to `main`. No PR, no channel, no cross-repo.
4. README: drop staging / promotion / catalog-PR language.
5. Re-tag `v1` so the plugin repos pick up the new behavior.

### B. Plugin sibling repos (`oc-plugin-core-app`, `oc-plugin-core-datasource`, `oc-plugin-yfinance-app`)

Per repo:

1. **Add the relocated manifest** (e.g. `oc-plugin.json` at repo root), seeded
   from the current `opencapital/plugins/<id>.json` minus `stagingNamespace`:
   ```json
   {
     "schemaVersion": 1,
     "pluginId": "yfinance-app",
     "publisher": "OpenCapital",
     "registry": { "host": "ghcr.io", "namespace": "opencapital-dev/plugins", "publicURL": "https://ghcr.io" },
     "versions": ["0.1.3", "0.1.2"]
   }
   ```
   (The CI maintains `versions[]` going forward; the seed preserves history.)
2. **Edit `.github/workflows/publish.yml`** — keep the
   `uses: opencapital-dev/oc-plugin-publish-action@v1` step (it now pushes to
   production and bumps the manifest itself). Required changes:
   - `permissions:` add `contents: write` (was `contents: read`) so the action's
     manifest-bump step can commit to this repo's own `main`. (`packages: write`
     and `id-token: write` stay.)
   - Drop the `catalog-token: ${{ secrets.CATALOG_PR_TOKEN }}` input (the input no
     longer exists; the `CATALOG_PR_TOKEN` secret is no longer needed).
   - The existing native-runner backend build matrix and frontend build stay.

Push auth + manifest-bump both use the workflow's built-in `GITHUB_TOKEN` (which
has `packages: write` for the push and `contents: write` for the commit) — no
provisioned secret.

### C. `opencapital` repo

#### Manifests / list
- `plugins.json`: repoint the 3 URLs to the plugin repos' raw manifest URLs, e.g.
  `https://raw.githubusercontent.com/opencapital-dev/oc-plugin-yfinance-app/main/oc-plugin.json`.
- Delete `plugins/core-app.json`, `plugins/core-datasource.json`,
  `plugins/yfinance-app.json`.

#### Rust catalog (`opencapital-app/src-tauri/src/catalog/`)
Remove staging + preview throughout; keep `versions[]` as the version source.

- `manifest.rs`:
  - `RegistrySpec` — drop `staging_namespace` field (`stagingNamespace`).
  - `PluginManifest` — drop `preview` field (keep `versions`).
  - `validate_plugin` — drop the `preview`/`stagingNamespace` rule (lines ~58–60);
    keep `pluginId`/`host`/`namespace` required. Update the unit test at ~265–285.
- `registry.rs`:
  - `RegistryCoords` — drop `staging_namespace` (lines ~96).
  - `PluginRef` — replace `validated` + `preview` with one `versions: Vec<String>`.
  - `resolve_artifact` — drop the staging-fallback loop (lines ~448–452); resolve
    against the single production namespace only.
  - Delete `VersionStatus` struct + `versions_with_status` (lines ~65–72, ~478–513);
    callers use the plain sorted `versions` list instead. Update tests
    (`versions_with_status_union_and_dedup` removed; keep semver/blob_url tests).
- `mod.rs`:
  - `pick_version` → return `(highest version, production namespace)`; drop the
    preview branch and the `preview_only` bool (lines ~155–168).
  - `ref_to_plugin` — drop the `preview_only` version-blanking (lines ~138–140);
    emit the resolved version. Update `pick_version_*` tests (~198–254).
  - Update `pub use` exports (drop `VersionStatus`).
- `sources.rs`:
  - `manifest_to_ref` — drop `staging_namespace`; set `versions =
    sort_semver_desc(&m.versions)` (drop `preview`). Lines ~163–178.

#### Rust commands / config
- `kinde.rs`: `plugin_versions` returns `Vec<String>` (drop the local
  `VersionStatus` struct + `validated`); delete `get_show_preview` /
  `set_show_preview` (lines ~482–519, ~563–573).
- `lib.rs`: remove `kinde::get_show_preview` and `kinde::set_show_preview` from
  the `invoke_handler` list (lines ~131–132).
- `config.rs`: delete `read_show_preview_in` / `set_show_preview_in` and the
  `show_preview` settings key + their tests (lines ~400–418, ~489–506).

#### Frontend (`opencapital-app/src/`)
- `types.ts`: `VersionStatus` → `string` (drop `validated`); the catalog entry's
  `latest_validated_version` keeps its name for now (it now means "latest
  version" — optional cosmetic rename).
- `api.ts`: drop `getShowPreview` / `setShowPreview`; `pluginVersions` returns
  `string[]`.
- `PluginsView.tsx`: remove `showPreview` state, `handleShowPreview`, and the
  "Show preview versions" `Switch` (lines ~45, 83, 137–140, 166–169); `versions`
  state becomes `Record<string, string[]>`; `visibleVersions` = all versions
  (keep the "always include current pin" rule); drop the `"preview"` option
  description (line ~225); reword "Latest" copy from "newest validated build" →
  "newest published build".

### D. Docs
Update to drop staging / promotion / preview language:
- `docs/superpowers/specs/2026-06-14-federated-plugin-sources-design.md`
- `docs/superpowers/plans/2026-06-14-federated-plugin-sources-*.md`
- Any `reference/` catalog docs that mention `plugins-staging` or promotion.

## Artifact contract (what the publish action must produce)

The app's resolver (`registry.rs::resolve_artifact`, `mod.rs::ref_to_plugin`)
reads, per `<id>:<tag>` in the production namespace:
- an OCI image manifest with a **config blob** (the footprint JSON) — read via
  `man.config.digest`;
- one **layer per platform**, each annotated `io.opencapital.platform=<os-arch>`
  (`darwin-amd64`, `darwin-arm64`, `windows-amd64`), each a `.tar.gz` of `dist/`
  containing that platform's `gpx_*` backend binary plus all shared frontend
  assets and the staged repo-root `dashboards/` + `library-panels/` trees.

The footprint JSON is derived from `dist/plugin.json`: `type`, `id`→`grafana_slug`,
`name`→`display_name`, `info.description`, and the `opencapital.{plugin_id,
platform_plugin, logical_views, query_entities}` block (see the current
`assemble/footprint.go` for the exact mapping). The app does not filter on OCI
media types, so media-type strings are not load-bearing for resolution — but the
platform annotation key and the per-platform tarball contents are.

## Verification

1. **Rust unit tests:** `cargo test` in `opencapital-app/src-tauri` — catalog
   tests updated for single-namespace / no-preview; `config.rs` preview tests
   removed.
2. **App build + frontend:** `make app` (single bundled path); `npm run build` +
   lint in `opencapital-app/src` clean.
3. **End-to-end publish (real):** in `oc-plugin-yfinance-app`, bump to a new patch
   and push tag `vX.Y.Z`. Confirm:
   - image present at `ghcr.io/opencapital-dev/plugins/yfinance-app:vX.Y.Z` (and
     **absent** from `plugins-staging`);
   - the plugin repo's `oc-plugin.json` on `main` gained `vX.Y.Z` in `versions[]`
     via the CI commit (no PR);
   - the cosign signature referrer is present on the production image;
   - no `CATALOG_PR_TOKEN` / cross-repo write occurred.
4. **App catalog E2E:** launch the app → Plugins view shows `yfinance-app` at the
   new version, no "Show preview versions" toggle, no "preview" badges; pin the
   new version; launch Grafana and confirm the plugin installs and its
   dashboards/library-panels provision.
5. **Anonymous resolution:** verify the catalog loads with no GitHub token in the
   app environment (manifest over raw HTTP + anonymous GHCR manifest/blob pulls).

## Out of scope / risks

- **GHCR tag pagination** — not relevant; versions come from the manifest, not
  `tags/list`.
- **First publish of a brand-new plugin** — the bump step must generate the full
  `oc-plugin.json` when absent (constants + `pluginId` from `src/plugin.json`),
  not just append.
- **CI commits to `main`** — the manifest-bump pushes to the plugin repo's `main`
  on every release. Acceptable (release-bot pattern, own repo, `GITHUB_TOKEN`).
- **App-side cosign verification** — deferred to a follow-up spec. cosign signing
  is **retained** (retargeted to the `plugins` namespace) so the signature exists
  to verify later. This is the intended replacement for the deleted control-plane
  `signers.yaml` gate: the app fetches the signature referrer and verifies the
  keyless sigstore bundle against an allowlist of OIDC identities. Out of scope
  here to keep this change focused.

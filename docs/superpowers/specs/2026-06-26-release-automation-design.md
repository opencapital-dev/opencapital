# Release automation via commit-to-main + semantic-release

**Date:** 2026-06-26
**Status:** Design approved, pending spec review
**Scope:** OpenCapital desktop app (this monorepo) + all `oc-plugin-*` repos + the shared `oc-plugin-publish-action`.

## Problem

Releasing the app or a plugin today is manual: a human bumps the version
(`tauri.conf.json` for the app, the git tag for plugins) and pushes a tag
(`opencapital-v*` / `v*`), which triggers the build/publish workflow. The plugin
publish then records the version into `oc-plugin.json` `versions[]` via
`manifest-bump.sh` as a second commit to `main`.

We want releases to be cut automatically from a normal merge to `main`, with the
version derived from Conventional Commits — no manual version bump, no manual
tag.

## Goal

One merge to `main` → the release pipeline computes the next semantic version
from the Conventional Commit messages since the last tag, builds + publishes,
creates the tag, and commits the version bump back to `main` with `[skip ci]` so
it does not loop.

Version rules:
- `fix:` → **patch**
- `feat:` → **minor**
- `feat!:` / `fix!:` / a `BREAKING CHANGE:` footer → **major**
- only non-releasing types (`chore`, `docs`, `refactor`, `test`, `ci`, `build`,
  `style`) since the last tag → **no release** (pipeline is a no-op)

We are on `0.x` today (app `0.1.11`, core-app `0.1.6`). The breaking rule is
honored literally: the first `feat!`/`BREAKING CHANGE` jumps `0.1.x → 1.0.0`.

## Decisions (locked)

| Decision | Choice |
|---|---|
| Release model | Auto on every relevant merge to `main` (no release-PR gate) |
| Versioning tool | `semantic-release` (npm) |
| Topology | Single workflow per repo, 3 jobs (`version` → `build` → `publish`), no PAT |
| Loop prevention | Bump commit carries `[skip ci]` (+ `GITHUB_TOKEN` pushes never re-trigger) |
| `0.x` breaking | Breaking → major (`0.1.x → 1.0.0`) |
| App tags | Collapse the dual `opencapital-v*` / `v*` scheme to a single `opencapital-v*` tag |
| Plugin workflow | One reusable workflow (`workflow_call`) hosted in the `opencapital` repo; each plugin keeps a tiny caller |

## Architecture

### Shared 3-job shape (`on: push: branches: [main]`)

```
job version  (ubuntu)
  - checkout fetch-depth: 0   (full history + tags — semantic-release needs them)
  - setup-node
  - npx --yes semantic-release@24 --dry-run
      via @semantic-release/exec verifyReleaseCmd:
        echo "version=${nextRelease.version}" >> $GITHUB_OUTPUT
        echo "released=true"                  >> $GITHUB_OUTPUT
  - outputs: version, released   (released defaults to 'false' if the cmd never runs)

job build    (matrix, native)   if: needs.version.outputs.released == 'true'
  - build artifacts using needs.version.outputs.version
  - upload-artifact

job publish  (ubuntu)           if: needs.version.outputs.released == 'true'
  - download-artifact
  - push artifacts (GHCR for plugins / collect bundles for app)   <-- BEFORE semantic-release
  - npx --yes semantic-release@24   (real run)
      analyze → prepare (write version files + changelog)
      → commit "chore(release): X.Y.Z [skip ci]" + tag → GH release
```

**Why `version` job dry-run + `verifyReleaseCmd`:** `verifyReleaseCmd` runs
during analyze/verify (which execute in `--dry-run`), so it is the reliable hook
to export the computed version and a `released=true` flag to `$GITHUB_OUTPUT`.
When there is no release, the command never fires and `released` stays `false`,
gating the expensive native matrix build off.

**Why GHCR push / asset collection happens BEFORE the real semantic-release:**
semantic-release pushes the bump commit + tag during `prepare`/tag, *before* its
`publish` step. Pushing the artifacts first preserves the invariant that a
version is recorded in `oc-plugin.json` / a GitHub Release only after the
artifact actually exists in the registry. The real semantic-release run then only
finalizes: version-file commit, tag, GitHub Release, changelog.

**Determinism:** the dry-run (`version` job) and the real run (`publish` job)
analyze the same commit range and produce the same version, provided no new
commit lands on `main` mid-run. Acceptable for this team's cadence.

### Loop prevention

The bump commit message is `chore(release): ${nextRelease.version} [skip ci]`.
Two independent guards stack:
1. GitHub skips workflow runs for `push` events whose head commit message
   contains `[skip ci]`.
2. Commits and tags pushed with the default `GITHUB_TOKEN` never trigger another
   workflow run (GitHub recursion guard).

The pipeline does **not** depend on a tag-push trigger, so the
`GITHUB_TOKEN`-pushed tag triggering nothing is the desired behavior, not a
problem.

## `.releaserc` (semantic-release config)

Shared plugin set:
- `@semantic-release/commit-analyzer` with explicit release rules:
  `{breaking: true → major}`, `{type: feat → minor}`, `{type: fix → patch}`.
- `@semantic-release/release-notes-generator`
- `@semantic-release/exec` — `verifyReleaseCmd` (export version), and
  `prepareCmd` to write the per-target version file.
- `@semantic-release/git` — commit the version file(s) with the `[skip ci]`
  message.
- `@semantic-release/github` — create the GitHub Release (+ upload assets for the
  app).

Per-target differences:
- **Plugins:** `tagFormat: "v${version}"`. `prepareCmd` prepends the new version
  to `oc-plugin.json` `versions[]` (kept v-prefixed, sorted semver-descending —
  the logic currently in `manifest-bump.sh`). `git.assets: ["oc-plugin.json"]`.
- **App:** `tagFormat: "opencapital-v${version}"`. `prepareCmd` writes the
  version into `opencapital-app/src-tauri/tauri.conf.json`.
  `git.assets: ["opencapital-app/src-tauri/tauri.conf.json"]`. `github.assets`
  uploads the bundles + `latest.json`.

The reusable plugin workflow **generates** the plugin `.releaserc` at runtime
from its `id`/`owner` inputs, so a new plugin needs no committed config. The app
commits its own `.releaserc` (app-specific `prepareCmd`, assets, `paths`).

## Plugins

### Reusable workflow

`.github/workflows/plugin-release.yml` in the `opencapital` repo, with
`on: workflow_call` and inputs `id`, `owner`, `platforms`. It implements the
3-job shape, generates `.releaserc`, and runs the publish step. Permissions
declared in the reusable workflow: `contents: write`, `packages: write`,
`id-token: write`.

Each plugin repo's `publish.yml` shrinks to a caller:

```yaml
name: release
on:
  push:
    branches: [main]
jobs:
  release:
    uses: opencapital-dev/opencapital/.github/workflows/plugin-release.yml@main
    with:
      id: core-app
      owner: opencapital-dev
      platforms: darwin-amd64,darwin-arm64,windows-amd64
    permissions:
      contents: write
      packages: write
      id-token: write
    secrets: inherit
```

A brand-new plugin is onboarded by adding this caller file (+ still manually
adding its manifest URL to `plugins.json` in the `opencapital` repo — that
registry edit stays a deliberate manual step, out of scope here).

GHCR push + cosign signing use the **caller** repo's `GITHUB_TOKEN` and OIDC
identity, so no shared secrets are needed for plugins.

### Build job (plugin)

Mirrors the current `build-backend` matrix (native macOS for darwin arm64/amd64,
native Windows for windows-amd64) plus the frontend build. Before
`npm run build`, set the source plugin version to the computed version
(`npm pkg set version=X.Y.Z`) so the built `dist/plugin.json` Footprint version
matches the release. Upload `dist/` (frontend + backend binaries) as the
artifact.

### Publish job (plugin)

Download `dist/`, then call `oc-plugin-publish-action` to assemble → push to
`ghcr.io/<owner>/plugins/<id>:vX.Y.Z` → cosign sign. Then run the real
semantic-release, which commits the `oc-plugin.json` `versions[]` bump
(`[skip ci]`), tags `vX.Y.Z`, and creates the GitHub Release with the generated
changelog (plugins have no Release today — this is a free addition).

### Shared action change (`oc-plugin-publish-action`)

Remove the `bump manifest versions[]` step (and `manifest-bump.sh`) from
`action.yml`. semantic-release now owns the `oc-plugin.json` write via
`prepareCmd` + `@semantic-release/git`. The action is reduced to: assemble →
GHCR push → cosign sign. Its `version` input is the semantic-release-computed
version, not `github.ref_name`. `manifest-bump.sh` and its test are deleted.

This change is backward-safe to land first: it breaks nothing until callers are
migrated to the new reusable workflow.

## App (this monorepo)

### Workflow

Replace `opencapital-release.yml`'s `on: push: tags: ["opencapital-v*"]` with
`on: push: branches: [main]` + a `paths:` filter (see Gating). Same 3-job shape.

- **version job:** dry-run → `version`, `released`.
- **build job (matrix macOS + Windows):** write the computed version into
  `tauri.conf.json` (replacing the current "tag matches tauri.conf.json" assert,
  which no longer applies — there is no pre-existing tag at build time), then run
  the existing `make`/tauri build path unchanged. Upload bundles.
- **publish job:** download bundles, build `latest.json`, then run real
  semantic-release → commit `tauri.conf.json` bump (`[skip ci]`), tag
  `opencapital-vX.Y.Z`, create the GitHub Release, upload bundles + `latest.json`
  as Release assets.

### Single-tag collapse

Today the trigger tag is `opencapital-v*` but the published Release / updater
download tag is `v*` (`publish` job: `tag="v${version}"`; `latest.json` URLs
point at `releases/download/v${version}/`). That split existed only to keep the
`v*` Release tag from re-triggering the `opencapital-v*` build. With a
branch-push trigger there is no tag trigger to avoid, so collapse to a single
`opencapital-v*` tag:
- semantic-release `tagFormat: "opencapital-v${version}"` creates the tag.
- The GitHub Release is created at the same `opencapital-v*` tag.
- `latest.json` `url` fields and the updater's release base are repointed to
  `releases/download/opencapital-v${version}/`.

**Verify before implementation:** confirm no external consumer (the Tauri
updater endpoint config, landing-page download links) is hard-pinned to the `v*`
tag namespace. If one is, that consumer is updated in the same change.

### Monorepo gating

The repo also holds `dataplane/`, `services/` (non-compute), `schemas/`, `docs/`.
The trigger `paths:` filter restricts app releases to app-relevant changes:
`opencapital-app/**`, `services/compute/**`, `dataplane/**`, `plugins.json`,
`Makefile`, and the workflow file itself. A `dataplane/`-only fix will not cut an
app release.

**Known limitation:** `paths:` gates whether the workflow *runs*; semantic-release
still analyzes *all* commits in the range. An unrelated `fix:` that accumulated
since the last tag can ride along and bump the patch version when the next
app-path change triggers a run. This is accepted as minor version noise.
`semantic-release-monorepo` (path-scoped analysis) is the escape hatch if it
becomes a problem.

## Components / boundaries

| Unit | Responsibility | Depends on |
|---|---|---|
| `version` job | Compute next version + `released` flag from commits | git history/tags, semantic-release dry-run |
| `build` job | Produce signed/unsigned artifacts for a known version | version output |
| `publish` job | Push artifacts, then finalize (commit + tag + Release) | artifacts, version, semantic-release real run |
| reusable `plugin-release.yml` | The whole 3-job shape for any plugin | inputs `id`/`owner`/`platforms`, `oc-plugin-publish-action` |
| `oc-plugin-publish-action` | Assemble → GHCR push → cosign sign | computed version, `dist/` |
| app `.releaserc` | App version-file write + asset upload + tagFormat | computed version, bundles |

## Error handling / edge cases

- **No releasable commits:** `released=false`, build + publish skipped. Clean
  no-op on every chore/docs merge.
- **GHCR push fails:** happens before the tag/commit in the publish job → no tag,
  no `oc-plugin.json` bump recorded → safe to re-run.
- **semantic-release tag/commit fails after GHCR push:** the GHCR image exists
  but the version is not yet recorded → harmless; a re-run records it
  (idempotent prepend).
- **First release in a repo with no tags:** semantic-release starts from the
  configured base; with existing `v0.1.x` / `opencapital-v0.1.x` tags present it
  continues from the latest matching `tagFormat`.
- **Two merges land close together:** each push triggers its own run; the second
  run's dry-run sees the first's tag and computes the next version. The
  `[skip ci]` bump commit does not start a run.
- **Mid-run new commit on main:** small risk the real run computes a different
  version than the dry-run. Accepted given cadence; revisit if observed.

## Testing / verification

- `manifest-bump-merge.test.sh` logic (version-list prepend + sort) is ported
  into the plugin `prepareCmd` and re-covered by a small unit test there.
- End-to-end smoke on **one** plugin (core-app) first: merge a `fix:` commit,
  confirm patch release (GHCR tag pushed + signed, `oc-plugin.json` bumped with
  `[skip ci]`, GitHub Release created, no loop). Then a `feat:` → minor.
- App smoke: merge an app-path `fix:`, confirm `opencapital-vX.Y.Z` tag,
  `tauri.conf.json` bumped, `latest.json` points at the `opencapital-v*` tag,
  updater can resolve it.

## Rollout order

1. `oc-plugin-publish-action`: drop `manifest-bump.sh` + step, switch `version`
   input to the passed value. (Safe — no caller uses the new path yet.)
2. Add the reusable `plugin-release.yml` + generated `.releaserc` to the
   `opencapital` repo.
3. Migrate `oc-plugin-core-app` to the tiny caller; run the end-to-end smoke.
4. Roll out the caller to `oc-plugin-core-datasource` and
   `oc-plugin-basic-data-app`.
5. Rewrite the app `opencapital-release.yml` to the 3-job shape + single-tag
   collapse + `paths` gating; run the app smoke.

## Out of scope

- Registering a brand-new plugin's manifest URL in `plugins.json` (stays a manual
  deliberate edit).
- The Grafana overlay fork's own release (pinned via `grafana-overlay.pin`,
  unchanged).
- Path-scoped monorepo version analysis (`semantic-release-monorepo`) — noted as
  a future option only.
```

# Release Automation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cut app and plugin releases automatically from a merge to `main`, with the version derived from Conventional Commits via semantic-release.

**Architecture:** Each releasable repo runs one workflow on `push: main` with 3 jobs — `version` (semantic-release `--dry-run` → next version + `released` flag), `build` (native matrix, gated on `released`), `publish` (push artifacts, then real semantic-release tags + commits the bump with `[skip ci]` + creates a GitHub Release). Plugins share one reusable workflow hosted in the `opencapital` repo; the app has its own.

**Tech Stack:** GitHub Actions, semantic-release@24 (`commit-analyzer`/`release-notes-generator`/`exec`/`git`/`github` + `conventional-changelog-conventionalcommits`), `jq`, bash, Go (oras assemble action), cosign.

## Global Constraints

- Version rules (verbatim): `fix:` → patch, `feat:` → minor, breaking (`feat!`/`fix!`/`BREAKING CHANGE:`) → major.
- On `0.x` today (app `0.1.11`, core-app `0.1.6`); breaking → major means `0.1.x → 1.0.0`. Honored literally.
- Bump commit message MUST be `chore(release): ${nextRelease.version} [skip ci]`.
- `versions[]` in `oc-plugin.json` is **v-prefixed** (`"v0.1.7"`); the OCI tag pushed to GHCR is the same v-prefixed string. The jq filter normalizes any bare legacy entries to v-prefixed.
- Plugin tag format: `v${version}`. App tag format: `opencapital-v${version}` (single tag — the old `v*` download-tag split is removed).
- Plugin ids / owner: `core-app`, `core-datasource`, `basic-data-app`; owner `opencapital-dev`. Plugin platforms: `darwin-amd64,darwin-arm64,windows-amd64`.
- Local sibling folder `oc-plugin-yfinance-app` is the repo `oc-plugin-basic-data-app` (renamed; folder not yet renamed locally).
- semantic-release invoked via npx, no per-repo devDeps: `npx --yes -p semantic-release@24 -p @semantic-release/exec -p @semantic-release/git -p conventional-changelog-conventionalcommits semantic-release [--dry-run]`.
- Repos are siblings under `/Users/ignacioballester/trading-code/`: `opencapital`, `oc-plugin-publish-action`, `oc-plugin-core-app`, `oc-plugin-core-datasource`, `oc-plugin-yfinance-app`.

---

## File Structure

In `opencapital` repo:
- `scripts/release/oc-plugin-version-prepend.sh` — prepend a version to `./oc-plugin.json` `versions[]` (+ first-publish generate). Tested.
- `scripts/release/oc-plugin-version-prepend.test.sh` — offline unit test.
- `scripts/release/write-tauri-version.sh` — set version in `tauri.conf.json`. Tested.
- `scripts/release/write-tauri-version.test.sh` — offline unit test.
- `scripts/release/plugin.releaserc.json` — static semantic-release config for any plugin.
- `scripts/release/app.releaserc.json` — static semantic-release config for the app.
- `.github/workflows/plugin-release.yml` — reusable (`workflow_call`) 3-job plugin pipeline.
- `.github/workflows/opencapital-release.yml` — rewritten to 3-job branch-triggered app pipeline.

In `oc-plugin-publish-action` repo:
- `action.yml` — gate then later remove the manifest-bump step; add/remove `record_manifest` input.
- `manifest-bump.sh`, `manifest-bump-merge.test.sh` — deleted in the final cleanup task.
- `README.md` — updated.

In each plugin repo:
- `.github/workflows/publish.yml` — replaced by a small caller of the reusable workflow.

---

## Task 1: Release scripts + unit tests (opencapital repo)

**Files:**
- Create: `scripts/release/oc-plugin-version-prepend.sh`
- Create: `scripts/release/oc-plugin-version-prepend.test.sh`
- Create: `scripts/release/write-tauri-version.sh`
- Create: `scripts/release/write-tauri-version.test.sh`

**Interfaces:**
- Produces: `oc-plugin-version-prepend.sh <version>` — edits `./oc-plugin.json` in cwd in place; reads `ID`/`OWNER`/`PUBLISHER` env only when generating a missing manifest. `write-tauri-version.sh <version>` — sets `.version` in `opencapital-app/src-tauri/tauri.conf.json` (path relative to cwd).

- [ ] **Step 1: Write the failing test for the version-prepend filter**

Create `scripts/release/oc-plugin-version-prepend.test.sh`:

```bash
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bash scripts/release/oc-plugin-version-prepend.test.sh`
Expected: FAIL — `oc-plugin-version-prepend.sh` does not exist (`No such file or directory`).

- [ ] **Step 3: Implement `oc-plugin-version-prepend.sh`**

Create `scripts/release/oc-plugin-version-prepend.sh`:

```bash
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
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `bash scripts/release/oc-plugin-version-prepend.test.sh`
Expected: `PASS`

- [ ] **Step 5: Write the failing test for the tauri version writer**

Create `scripts/release/write-tauri-version.test.sh`:

```bash
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
```

- [ ] **Step 6: Run the test to verify it fails**

Run: `bash scripts/release/write-tauri-version.test.sh`
Expected: FAIL — `write-tauri-version.sh` does not exist.

- [ ] **Step 7: Implement `write-tauri-version.sh`**

Create `scripts/release/write-tauri-version.sh`:

```bash
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
```

- [ ] **Step 8: Run the test to verify it passes**

Run: `bash scripts/release/write-tauri-version.test.sh`
Expected: `PASS`

- [ ] **Step 9: Make scripts executable and commit**

```bash
chmod +x scripts/release/*.sh
git add scripts/release/
git commit -m "feat(release): version-prepend + tauri-version scripts with tests"
```

---

## Task 2: semantic-release configs (opencapital repo)

**Files:**
- Create: `scripts/release/plugin.releaserc.json`
- Create: `scripts/release/app.releaserc.json`

**Interfaces:**
- Consumes: `bash scripts/release/oc-plugin-version-prepend.sh` and `write-tauri-version.sh` from Task 1 (referenced from the workflow at `.oc-release/scripts/release/...`). `$GITHUB_OUTPUT` env (set by Actions).
- Produces: two config files copied into the working dir at runtime as `.releaserc.json`.

- [ ] **Step 1: Create the plugin config**

Create `scripts/release/plugin.releaserc.json`:

```json
{
  "branches": ["main"],
  "tagFormat": "v${version}",
  "plugins": [
    ["@semantic-release/commit-analyzer", {
      "preset": "conventionalcommits",
      "releaseRules": [
        { "breaking": true, "release": "major" },
        { "type": "feat", "release": "minor" },
        { "type": "fix", "release": "patch" }
      ]
    }],
    ["@semantic-release/release-notes-generator", { "preset": "conventionalcommits" }],
    ["@semantic-release/exec", {
      "verifyReleaseCmd": "echo \"version=${nextRelease.version}\" >> \"$GITHUB_OUTPUT\"; echo \"released=true\" >> \"$GITHUB_OUTPUT\"",
      "prepareCmd": "bash .oc-release/scripts/release/oc-plugin-version-prepend.sh ${nextRelease.version}"
    }],
    ["@semantic-release/git", {
      "assets": ["oc-plugin.json"],
      "message": "chore(release): ${nextRelease.version} [skip ci]"
    }],
    "@semantic-release/github"
  ]
}
```

- [ ] **Step 2: Create the app config**

Create `scripts/release/app.releaserc.json`:

```json
{
  "branches": ["main"],
  "tagFormat": "opencapital-v${version}",
  "plugins": [
    ["@semantic-release/commit-analyzer", {
      "preset": "conventionalcommits",
      "releaseRules": [
        { "breaking": true, "release": "major" },
        { "type": "feat", "release": "minor" },
        { "type": "fix", "release": "patch" }
      ]
    }],
    ["@semantic-release/release-notes-generator", { "preset": "conventionalcommits" }],
    ["@semantic-release/exec", {
      "verifyReleaseCmd": "echo \"version=${nextRelease.version}\" >> \"$GITHUB_OUTPUT\"; echo \"released=true\" >> \"$GITHUB_OUTPUT\"",
      "prepareCmd": "bash scripts/release/write-tauri-version.sh ${nextRelease.version}"
    }],
    ["@semantic-release/git", {
      "assets": ["opencapital-app/src-tauri/tauri.conf.json"],
      "message": "chore(release): ${nextRelease.version} [skip ci]"
    }],
    ["@semantic-release/github", {
      "assets": [
        { "path": "artifacts/*.dmg" },
        { "path": "artifacts/*.app.tar.gz" },
        { "path": "artifacts/*.app.tar.gz.sig" },
        { "path": "artifacts/*-setup.exe" },
        { "path": "artifacts/*-setup.exe.sig" },
        { "path": "artifacts/latest.json" }
      ]
    }]
  ]
}
```

- [ ] **Step 3: Validate both are well-formed JSON**

Run: `jq -e . scripts/release/plugin.releaserc.json >/dev/null && jq -e . scripts/release/app.releaserc.json >/dev/null && echo OK`
Expected: `OK`

- [ ] **Step 4: Commit**

```bash
git add scripts/release/plugin.releaserc.json scripts/release/app.releaserc.json
git commit -m "feat(release): semantic-release configs for plugin + app"
```

---

## Task 3: `record_manifest` input gate on the publish action (oc-plugin-publish-action repo)

Make the manifest-bump step opt-out so the new reusable workflow (which records `oc-plugin.json` via semantic-release) and any old tag-triggered workflow can coexist without double-writing. Final removal is Task 8.

**Files:**
- Modify: `oc-plugin-publish-action/action.yml`
- Modify: `oc-plugin-publish-action/README.md`

**Interfaces:**
- Produces: new action input `record_manifest` (default `"true"`). When `"false"`, the `bump manifest versions[]` step is skipped.

- [ ] **Step 1: Add the input**

In `oc-plugin-publish-action/action.yml`, under `inputs:` (after the `platforms:` block, before `outputs:`), add:

```yaml
  record_manifest:
    required: false
    default: "true"
    description: "When false, skip the oc-plugin.json versions[] bump (the caller records it elsewhere, e.g. semantic-release)"
```

- [ ] **Step 2: Gate the bump step**

In `oc-plugin-publish-action/action.yml`, change the final step from:

```yaml
    - name: bump manifest versions[]
      shell: bash
```

to:

```yaml
    - name: bump manifest versions[]
      if: inputs.record_manifest == 'true'
      shell: bash
```

- [ ] **Step 3: Verify the action parses and the gate is present**

Run:
```bash
cd /Users/ignacioballester/trading-code/oc-plugin-publish-action
python3 -c "import yaml,sys; yaml.safe_load(open('action.yml')); print('yaml-ok')"
grep -q "record_manifest == 'true'" action.yml && grep -q "record_manifest:" action.yml && echo gate-ok
```
Expected: `yaml-ok` then `gate-ok`

- [ ] **Step 4: Note the input in README**

In `oc-plugin-publish-action/README.md`, add a row to the Inputs table:

```markdown
| `record_manifest` | no | `true` | When `false`, skip the `oc-plugin.json` `versions[]` bump (caller records it) |
```

- [ ] **Step 5: Commit and move the `v1` tag**

The action is consumed as `@v1`; move the floating major tag so callers pick up the change.

```bash
cd /Users/ignacioballester/trading-code/oc-plugin-publish-action
git add action.yml README.md
git commit -m "feat: record_manifest input to opt out of oc-plugin.json bump"
git push origin main
git tag -f v1 && git push -f origin v1
```
Expected: push succeeds; `v1` now points at this commit.

---

## Task 4: Reusable plugin release workflow (opencapital repo)

**Files:**
- Create: `.github/workflows/plugin-release.yml`

**Interfaces:**
- Consumes: `scripts/release/plugin.releaserc.json` + `scripts/release/oc-plugin-version-prepend.sh` (Tasks 1–2, fetched via sparse checkout to `.oc-release/`); `oc-plugin-publish-action@v1` with `record_manifest: false` (Task 3).
- Produces: a `workflow_call` workflow with inputs `id`, `owner`, `platforms`; callable as `opencapital-dev/opencapital/.github/workflows/plugin-release.yml@main`.

- [ ] **Step 1: Create the reusable workflow**

Create `.github/workflows/plugin-release.yml`:

```yaml
name: plugin-release

# Reusable 3-job release pipeline for any oc-plugin-* repo. Called from each
# plugin's tiny publish.yml on push:main. semantic-release computes the version
# from Conventional Commits, the matrix builds the Go backend natively, the
# publish job pushes+signs to GHCR (BEFORE semantic-release, so oc-plugin.json
# only ever lists published versions), then semantic-release tags + bumps
# oc-plugin.json [skip ci] + cuts a GitHub Release.
on:
  workflow_call:
    inputs:
      id: { required: true, type: string }
      owner: { required: true, type: string }
      platforms: { required: true, type: string }

jobs:
  version:
    runs-on: ubuntu-latest
    permissions: { contents: read }
    outputs:
      version: ${{ steps.sr.outputs.version }}
      released: ${{ steps.sr.outputs.released }}
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: actions/checkout@v4
        with:
          repository: opencapital-dev/opencapital
          path: .oc-release
          sparse-checkout: scripts/release
      - uses: actions/setup-node@v4
        with: { node-version: 22 }
      - name: compute next version (dry-run)
        id: sr
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          ID: ${{ inputs.id }}
          OWNER: ${{ inputs.owner }}
        run: |
          cp .oc-release/scripts/release/plugin.releaserc.json .releaserc.json
          npx --yes -p semantic-release@24 -p @semantic-release/exec \
            -p @semantic-release/git -p conventional-changelog-conventionalcommits \
            semantic-release --dry-run

  build:
    needs: version
    if: needs.version.outputs.released == 'true'
    strategy:
      fail-fast: false
      matrix:
        include:
          - os: macos-latest
            targets: "build:darwinARM64 build:darwin"
          - os: windows-latest
            targets: "build:windows"
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: "1.26" }
      - uses: magefile/mage-action@v3
        with: { install-only: true }
      - name: build backend
        shell: bash
        run: |
          for t in ${{ matrix.targets }}; do mage $t; done
          ls -la dist/gpx_*
      - uses: actions/upload-artifact@v4
        with:
          name: backend-${{ matrix.os }}
          path: dist/gpx_*
          if-no-files-found: error

  publish:
    needs: [version, build]
    if: needs.version.outputs.released == 'true'
    runs-on: ubuntu-latest
    permissions: { contents: write, packages: write, id-token: write }
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: actions/checkout@v4
        with:
          repository: opencapital-dev/opencapital
          path: .oc-release
          sparse-checkout: scripts/release
      - uses: actions/setup-node@v4
        with: { node-version: 22 }
      - name: build frontend (produces dist/plugin.json + assets)
        run: |
          npm pkg set version=${{ needs.version.outputs.version }}
          npm ci && npm run build
      - uses: actions/download-artifact@v4
        with: { path: /tmp/backends }
      - name: collect backend binaries into dist/
        run: |
          cp /tmp/backends/*/gpx_* dist/
          ls -la dist/
      # GHCR push + sign FIRST so oc-plugin.json only records published versions.
      - name: push + sign (GHCR)
        uses: opencapital-dev/oc-plugin-publish-action@v1
        with:
          dir: .
          id: ${{ inputs.id }}
          owner: ${{ inputs.owner }}
          version: v${{ needs.version.outputs.version }}
          platforms: ${{ inputs.platforms }}
          record_manifest: "false"
      # Drop the ephemeral package.json bump so semantic-release sees a clean tree.
      - name: finalize (tag + oc-plugin.json bump + GitHub Release)
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          ID: ${{ inputs.id }}
          OWNER: ${{ inputs.owner }}
        run: |
          git checkout -- package.json package-lock.json 2>/dev/null || true
          cp .oc-release/scripts/release/plugin.releaserc.json .releaserc.json
          npx --yes -p semantic-release@24 -p @semantic-release/exec \
            -p @semantic-release/git -p conventional-changelog-conventionalcommits \
            semantic-release
```

- [ ] **Step 2: Lint the workflow**

Run: `actionlint .github/workflows/plugin-release.yml`
Expected: no output (exit 0). Fix any reported issues.

- [ ] **Step 3: Commit and push (the reusable workflow must exist on `main` to be callable)**

```bash
git add .github/workflows/plugin-release.yml
git commit -m "feat(release): reusable plugin-release workflow (commit-to-main + semantic-release)"
git push origin main
```

Note: the working branch here is `release/opencapital-v0.1.11`. `workflow_call` referenced as `@main` requires this on `main` — open a PR / merge to `main` (or temporarily reference `@<branch>` in the Task 5 smoke caller and switch to `@main` after merge).

---

## Task 5: Migrate core-app + end-to-end smoke (oc-plugin-core-app repo)

This task proves the whole pipeline on one real plugin before rolling out.

**Files:**
- Replace: `oc-plugin-core-app/.github/workflows/publish.yml`

**Interfaces:**
- Consumes: the reusable workflow from Task 4.

- [ ] **Step 1: Replace publish.yml with a caller**

Overwrite `oc-plugin-core-app/.github/workflows/publish.yml`:

```yaml
name: release
on:
  push:
    branches: [main]
jobs:
  release:
    uses: opencapital-dev/opencapital/.github/workflows/plugin-release.yml@main
    permissions:
      contents: write
      packages: write
      id-token: write
    secrets: inherit
    with:
      id: core-app
      owner: opencapital-dev
      platforms: darwin-amd64,darwin-arm64,windows-amd64
```

- [ ] **Step 2: Lint**

Run: `cd /Users/ignacioballester/trading-code/oc-plugin-core-app && actionlint .github/workflows/publish.yml`
Expected: no output (exit 0).

- [ ] **Step 3: Commit + push a `fix:` to trigger a real release**

```bash
cd /Users/ignacioballester/trading-code/oc-plugin-core-app
git add .github/workflows/publish.yml
git commit -m "ci: release via commit-to-main + semantic-release (reusable workflow)

fix: switch core-app to auto-release on merge to main"
git push origin main
```

- [ ] **Step 4: Verify the end-to-end run (manual observation)**

Watch: `gh run watch --repo opencapital-dev/oc-plugin-core-app` (or the Actions tab).
Expected, in order:
1. `version` job logs `The next release version is 0.1.7` and sets `released=true`.
2. `build` matrix produces `gpx_*` for macOS + Windows.
3. `publish` pushes `ghcr.io/opencapital-dev/plugins/core-app:v0.1.7` + cosign signature.
4. semantic-release creates tag `v0.1.7`, commits `chore(release): 0.1.7 [skip ci]` touching `oc-plugin.json` (now `["v0.1.7", ...]`), and creates a GitHub Release.

Then confirm no loop:
Run: `gh run list --repo opencapital-dev/oc-plugin-core-app --limit 3`
Expected: the `[skip ci]` bump commit did NOT start a new run.

Verify registry + manifest:
```bash
gh api /orgs/opencapital-dev/packages/container/plugins%2Fcore-app/versions --jq '.[].metadata.container.tags[]' | head
gh api repos/opencapital-dev/oc-plugin-core-app/contents/oc-plugin.json --jq '.content' | base64 -d | jq '.versions[0]'
```
Expected: GHCR lists tag `v0.1.7`; `versions[0]` is `"v0.1.7"`.

- [ ] **Step 5: Verify a `feat:` cuts a minor (second smoke)**

Push a trivial `feat:` commit (e.g. a comment with `feat: ...` subject), watch the run, confirm next version is `0.2.0` (minor bump from `0.1.7`). This confirms the bump rules end-to-end.

---

## Task 6: Roll out to remaining plugins (oc-plugin-core-datasource, oc-plugin-yfinance-app)

**Files:**
- Replace: `oc-plugin-core-datasource/.github/workflows/publish.yml`
- Replace: `oc-plugin-yfinance-app/.github/workflows/publish.yml` (repo `oc-plugin-basic-data-app`)

**Interfaces:**
- Consumes: reusable workflow (Task 4), proven by Task 5.

- [ ] **Step 1: core-datasource caller**

Overwrite `oc-plugin-core-datasource/.github/workflows/publish.yml`:

```yaml
name: release
on:
  push:
    branches: [main]
jobs:
  release:
    uses: opencapital-dev/opencapital/.github/workflows/plugin-release.yml@main
    permissions:
      contents: write
      packages: write
      id-token: write
    secrets: inherit
    with:
      id: core-datasource
      owner: opencapital-dev
      platforms: darwin-amd64,darwin-arm64,windows-amd64
```

- [ ] **Step 2: basic-data-app caller**

Overwrite `oc-plugin-yfinance-app/.github/workflows/publish.yml`:

```yaml
name: release
on:
  push:
    branches: [main]
jobs:
  release:
    uses: opencapital-dev/opencapital/.github/workflows/plugin-release.yml@main
    permissions:
      contents: write
      packages: write
      id-token: write
    secrets: inherit
    with:
      id: basic-data-app
      owner: opencapital-dev
      platforms: darwin-amd64,darwin-arm64,windows-amd64
```

- [ ] **Step 3: Lint both**

Run:
```bash
actionlint /Users/ignacioballester/trading-code/oc-plugin-core-datasource/.github/workflows/publish.yml
actionlint /Users/ignacioballester/trading-code/oc-plugin-yfinance-app/.github/workflows/publish.yml
```
Expected: no output for either.

- [ ] **Step 4: Commit + push each, observe one release per repo**

```bash
cd /Users/ignacioballester/trading-code/oc-plugin-core-datasource
git add .github/workflows/publish.yml
git commit -m "ci: auto-release on merge to main (reusable workflow)

fix: switch core-datasource to commit-to-main releases"
git push origin main

cd /Users/ignacioballester/trading-code/oc-plugin-yfinance-app
git add .github/workflows/publish.yml
git commit -m "ci: auto-release on merge to main (reusable workflow)

fix: switch basic-data-app to commit-to-main releases"
git push origin main
```
Expected: each repo cuts a patch release; GHCR tag + `oc-plugin.json` `versions[0]` updated; no loop.

---

## Task 7: App release workflow rewrite (opencapital repo)

Rewrite `.github/workflows/opencapital-release.yml` into the 3-job branch-triggered shape, collapse to the single `opencapital-v*` tag, and let semantic-release tag + bump `tauri.conf.json` + create the Release.

**Files:**
- Modify: `.github/workflows/opencapital-release.yml`

**Interfaces:**
- Consumes: `scripts/release/app.releaserc.json`, `scripts/release/write-tauri-version.sh` (Tasks 1–2).

- [ ] **Step 1: Verify the single-tag collapse is safe**

Run:
```bash
cd /Users/ignacioballester/trading-code/opencapital
grep -rn 'releases/download/v\|opencapital-v\|latest.json' opencapital-app/src-tauri/tauri.conf.json opencapital-app/src-tauri/src 2>/dev/null
grep -rn 'endpoints\|updater' opencapital-app/src-tauri/tauri.conf.json
```
Expected: identify the updater endpoint + any hard-coded `releases/download/v${version}` base. Confirm nothing external is pinned to the `v*` tag namespace that this change would break. If the updater endpoint references a GitHub `latest.json` asset URL, note it points at the Release created in Step 4 (tag `opencapital-v*`).

- [ ] **Step 2: Replace the trigger + add paths gating**

In `.github/workflows/opencapital-release.yml`, replace:

```yaml
on:
  push:
    tags: ["opencapital-v*"]

permissions:
  contents: write
```

with:

```yaml
on:
  push:
    branches: [main]
    paths:
      - "opencapital-app/**"
      - "services/compute/**"
      - "dataplane/**"
      - "plugins.json"
      - "Makefile"
      - ".github/workflows/opencapital-release.yml"

permissions:
  contents: write
```

- [ ] **Step 3: Add the `version` job at the top of `jobs:`**

Insert as the first job under `jobs:` (before `build:`):

```yaml
  version:
    runs-on: ubuntu-latest
    outputs:
      version: ${{ steps.sr.outputs.version }}
      released: ${{ steps.sr.outputs.released }}
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: actions/setup-node@v4
        with: { node-version: 20 }
      - name: compute next version (dry-run)
        id: sr
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          cp scripts/release/app.releaserc.json .releaserc.json
          npx --yes -p semantic-release@24 -p @semantic-release/exec \
            -p @semantic-release/git -p conventional-changelog-conventionalcommits \
            semantic-release --dry-run
```

- [ ] **Step 4: Gate the build job + write the version into tauri.conf.json before building**

In the `build:` job, add `needs` + `if`, and replace the "Assert tag matches tauri.conf.json version" step with a write step.

Change the job header from:

```yaml
  build:
    strategy:
```

to:

```yaml
  build:
    needs: version
    if: needs.version.outputs.released == 'true'
    strategy:
```

Replace the assert step:

```yaml
      - name: Assert tag matches tauri.conf.json version
        shell: bash
        run: |
          tag="${GITHUB_REF_NAME#opencapital-v}"
          cfg=$(node -p "require('./opencapital-app/src-tauri/tauri.conf.json').version")
          echo "tag=$tag config=$cfg"
          if [ "$tag" != "$cfg" ]; then
            echo "::error::tag $tag != tauri.conf.json version $cfg"; exit 1
          fi
```

with:

```yaml
      - name: Write release version into tauri.conf.json
        shell: bash
        run: bash scripts/release/write-tauri-version.sh "${{ needs.version.outputs.version }}"
```

- [ ] **Step 5: Rewrite the `publish` job to finalize via semantic-release**

Replace the entire existing `publish:` job with:

```yaml
  publish:
    needs: [version, build]
    if: needs.version.outputs.released == 'true'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: actions/setup-node@v4
        with: { node-version: 20 }
      - uses: actions/download-artifact@v4
        with:
          path: artifacts
          merge-multiple: true
      - name: Build latest.json
        env:
          RELEASES_REPO: ${{ github.repository }}
          VERSION: ${{ needs.version.outputs.version }}
        shell: bash
        run: |
          set -euo pipefail
          version="$VERSION"
          tag="opencapital-v${version}"
          base="https://github.com/${RELEASES_REPO}/releases/download/${tag}"
          cd artifacts
          mac_bundle=$(ls *.app.tar.gz 2>/dev/null | head -n1 || true)
          mac_sig=$(ls *.app.tar.gz.sig 2>/dev/null | head -n1 || true)
          win_bundle=$(ls *-setup.exe 2>/dev/null | head -n1 || true)
          win_sig=$(ls *-setup.exe.sig 2>/dev/null | head -n1 || true)
          platforms="{}"
          if [ -n "$mac_bundle" ] && [ -n "$mac_sig" ]; then
            platforms=$(echo "$platforms" | jq \
              --arg sig "$(cat "$mac_sig")" --arg url "${base}/${mac_bundle}" \
              '. + {"darwin-aarch64": {signature: $sig, url: $url}}')
          fi
          if [ -n "$win_bundle" ] && [ -n "$win_sig" ]; then
            platforms=$(echo "$platforms" | jq \
              --arg sig "$(cat "$win_sig")" --arg url "${base}/${win_bundle}" \
              '. + {"windows-x86_64": {signature: $sig, url: $url}}')
          fi
          jq -n --arg v "$version" --arg notes "OpenCapital $version" \
                --arg date "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --argjson p "$platforms" \
            '{version: $v, notes: $notes, pub_date: $date, platforms: $p}' > latest.json
          cat latest.json
      - name: Finalize (tag + tauri bump + GitHub Release w/ assets)
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          cp scripts/release/app.releaserc.json .releaserc.json
          npx --yes -p semantic-release@24 -p @semantic-release/exec \
            -p @semantic-release/git -p conventional-changelog-conventionalcommits \
            semantic-release
```

Note: the `github.assets` globs in `app.releaserc.json` (Task 2) reference `artifacts/*` and `artifacts/latest.json`, both present in this job's checkout. semantic-release creates the tag `opencapital-v${version}` and the Release in one step and uploads them — replacing the old manual `gh release create`.

- [ ] **Step 6: Lint the rewritten workflow**

Run: `actionlint .github/workflows/opencapital-release.yml`
Expected: no output (exit 0). Fix any issues.

- [ ] **Step 7: Commit**

```bash
git add .github/workflows/opencapital-release.yml
git commit -m "feat(release): app auto-releases on merge to main (3-job, single tag)"
```

- [ ] **Step 8: App smoke (after merge to main)**

Merge to `main`, then push an app-path `fix:` commit. Watch the run:
Expected:
1. `version` → next version (e.g. `0.1.12`), `released=true`.
2. `build` writes `tauri.conf.json` = `0.1.12`, builds macOS + Windows bundles.
3. `publish` builds `latest.json` with `url` = `.../releases/download/opencapital-v0.1.12/...`; semantic-release tags `opencapital-v0.1.12`, commits `chore(release): 0.1.12 [skip ci]` (tauri.conf.json), creates the Release with bundles + `latest.json` attached.
4. No loop (the `[skip ci]` bump starts no run).

Verify:
```bash
gh release view opencapital-v0.1.12 --repo opencapital-dev/opencapital --json tagName,assets --jq '{tag:.tagName, assets:[.assets[].name]}'
```
Expected: tag `opencapital-v0.1.12`; assets include the dmg/app.tar.gz/.sig/setup.exe/latest.json. Confirm the Tauri updater resolves the new `latest.json`.

---

## Task 8: Remove manifest-bump from the publish action (oc-plugin-publish-action repo)

Cleanup after all plugins are on the reusable workflow (Tasks 5–6). semantic-release now owns `oc-plugin.json`, so the action's bump step + `record_manifest` input are dead.

**Files:**
- Modify: `oc-plugin-publish-action/action.yml`
- Delete: `oc-plugin-publish-action/manifest-bump.sh`
- Delete: `oc-plugin-publish-action/manifest-bump-merge.test.sh`
- Modify: `oc-plugin-publish-action/README.md`

- [ ] **Step 1: Remove the bump step + input**

In `action.yml`, delete the entire `- name: bump manifest versions[]` step (and its `if:`, `env:`, `run:`) and delete the `record_manifest:` input block added in Task 3.

- [ ] **Step 2: Delete the scripts**

```bash
cd /Users/ignacioballester/trading-code/oc-plugin-publish-action
git rm manifest-bump.sh manifest-bump-merge.test.sh
```

- [ ] **Step 3: Update README**

Remove the `record_manifest` row and the paragraph describing the `oc-plugin.json` commit (the action no longer writes it). State that version recording is the caller's responsibility (semantic-release).

- [ ] **Step 4: Verify the action still parses and has no manifest-bump references**

Run:
```bash
cd /Users/ignacioballester/trading-code/oc-plugin-publish-action
python3 -c "import yaml; yaml.safe_load(open('action.yml')); print('yaml-ok')"
grep -c "manifest-bump\|record_manifest" action.yml README.md || true
```
Expected: `yaml-ok`; grep count `0`.

- [ ] **Step 5: Commit + move `v1`**

```bash
git add -A
git commit -m "refactor: remove oc-plugin.json bump (semantic-release owns it now)"
git push origin main
git tag -f v1 && git push -f origin v1
```

- [ ] **Step 6: Confirm no regression**

Push a `fix:` to one plugin (e.g. core-datasource), watch the release: GHCR push + sign succeed, `oc-plugin.json` still bumped (by semantic-release), no double commit, no error.

---

## Self-Review

**Spec coverage:**
- Auto-on-merge 3-job topology → Tasks 4 (plugins), 7 (app). ✓
- semantic-release version rules (fix/feat/breaking) → Task 2 configs; verified in Task 5 Step 5. ✓
- `verifyReleaseCmd` version extraction + `released` gate → Task 2 + jobs in Tasks 4/7. ✓
- GHCR push before tag (record-only-published) → Task 4 publish job ordering. ✓
- Loop prevention `[skip ci]` → Task 2 `git.message`; verified in Task 5 Step 4. ✓
- Reusable workflow in opencapital + tiny callers → Tasks 4, 5, 6. ✓
- `oc-plugin.json` v-prefixed, self-healing filter → Task 1. ✓
- manifest-bump removal (staged: gate then delete) → Tasks 3, 8. ✓
- App single-tag collapse + latest.json repoint + paths gating + drop assert → Task 7. ✓
- New-plugin onboarding (caller + manual plugins.json) → noted in spec Out of Scope; caller shape in Tasks 5/6. ✓

**Placeholder scan:** No TBD/TODO; every code/YAML/test step shows full content. ✓

**Type/name consistency:** `oc-plugin-version-prepend.sh`, `write-tauri-version.sh`, config filenames `plugin.releaserc.json`/`app.releaserc.json`, input `record_manifest`, job outputs `version`/`released`, and `.oc-release/scripts/release/...` path used identically across Tasks 1, 2, 4, 7. ✓

**Open verification carried into tasks (not assumptions):** single-tag-collapse safety (Task 7 Step 1); `@main` reusability requires the reusable workflow merged to `main` (Task 4 Step 3 note).

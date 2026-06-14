# OpenCapital release runbook

Auto-update design: `docs/superpowers/specs/2026-06-05-opencapital-updates-design.md`.

## One-time setup

### 1. Production minisign keypair
The dev key checked into `tauri.conf.json` is for local testing only. Generate the
real one and back it up — losing it orphans every install.
```bash
cd opencapital-app
npm run tauri signer generate -- -w opencapital.key
```
- Put the **public key** in `src-tauri/tauri.conf.json` → `plugins.updater.pubkey` (replaces the dev placeholder).
- Store the **private key** + password as Actions secrets `TAURI_SIGNING_PRIVATE_KEY` / `TAURI_SIGNING_PRIVATE_KEY_PASSWORD` in `portfolio-management-v2`.

### 2. Public releases repo
Create `opencapital-dev/opencapital` (public, empty). It only holds Releases + `latest.json`.

### 3. Actions secrets (in `portfolio-management-v2`)
- `RELEASES_REPO_TOKEN` — fine-grained PAT with `contents:write` on `opencapital`.
- `TAURI_SIGNING_PRIVATE_KEY`, `TAURI_SIGNING_PRIVATE_KEY_PASSWORD` — minisign (required).
- macOS code-signing (optional, layer in later): `APPLE_CERTIFICATE`, `APPLE_CERTIFICATE_PASSWORD`, `APPLE_SIGNING_IDENTITY`, `APPLE_API_KEY`, `APPLE_API_ISSUER`, `APPLE_API_KEY_ID`.
- Windows code-signing (optional, pending EU-vs-Switzerland residency).

### 4. config.json endpoints
Once the Caddy edge is up, set the real hostnames in `src-tauri/resources/config.json`.

## Build model (what CI does)

- **Targets:** macOS Apple Silicon (`darwin-aarch64`) + Windows x64 (`windows-x86_64`). No Intel mac (hosted Intel runners sunset ~Aug 2027).
- **Grafana overlay** is built in the **fork repo** (`opencapital-dev/grafana`, lives at `../grafana`, default branch `main` = customized 13.0.2; `upstream-main` mirrors real Grafana). Its `opencapital-overlay` workflow publishes `grafana-overlay.tar.gz` as an `overlay-v*` release. The app release CI does **not** build the frontend — it downloads the overlay **pinned** by `opencapital-app/grafana-overlay.pin`.
- **Compute sidecar** is frozen per-platform in the release CI (`pip install polars pyinstaller` → pyinstaller). Not cross-compilable, so each runner builds its own.

### Bumping the overlay
When the fork frontend changes: push an `overlay-v<n>` tag to the fork (or run its `opencapital-overlay` workflow) → it publishes the overlay release → set `opencapital-app/grafana-overlay.pin` to `overlay-v<n>` in a PR. Keep the fork's `main` Grafana version in sync with `grafana_download_url` in `config.rs`.

## Cutting a release
1. Bump `version` in `src-tauri/tauri.conf.json` (the source of truth; `Cargo.toml`/`package.json` mirror it).
2. Ensure `grafana-overlay.pin` points at the desired fork overlay release.
3. Tag and push: `git tag opencapital-v0.1.1 && git push origin opencapital-v0.1.1`.
4. CI builds arm64-mac + win-x64 (pulling the pinned overlay, freezing the compute sidecar), signs with minisign, and publishes a Release + `latest.json` (keys `darwin-aarch64`, `windows-x86_64`) to `opencapital`.

## Interim (no OS certs)
Until Apple/Windows certs exist, builds are unsigned but still auto-update (minisign verifies them):
- macOS: first launch + post-update relaunch are quarantined — right-click → Open once, or `xattr -dr com.apple.quarantine /Applications/opencapital-app.app`.
- Windows: SmartScreen "unknown publisher" → More info → Run anyway.

## Dry run before the first real tag
Push a throwaway tag, let `publish` create the cross-repo release, and verify the asset set + `latest.json` shape. Delete the test release afterward.

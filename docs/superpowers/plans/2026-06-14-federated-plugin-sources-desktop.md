# Federated Plugin Sources — Desktop UI Implementation Plan (Plan 2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the desktop "Sources" management screen (list/add-by-URL/remove user-added plugin manifest URLs), badge marketplace cards by source (verified vs third-party), warn before selecting third-party plugins, and validate the whole federated path end-to-end by tagging one plugin to production.

**Architecture:** Plan 1 (control-plane backend) is done on this same branch (`feat/federated-plugin-sources`): `/v1/sources` (global, Kinde-authed) CRUD on user-added per-plugin manifest URLs, and the marketplace catalog now returns a `source {url, publisher, verified}` per entry. This plan is the Tauri/React front: 3 new Rust commands proxying `/v1/sources`, a `SourcesView`, a nav entry, a source badge + third-party confirm on `PluginsView`. The catalog command already returns `source` (it proxies raw JSON) — only the TS type + rendering need it.

**Tech Stack:** Tauri (Rust commands in `opencapital-app/src-tauri/src/`), React + TypeScript (`opencapital-app/src/`), `@grafana/ui` + `@emotion/css` styling via `useStyles2(getStyles)`. No frontend test harness exists → verification is typecheck (`npm run build`) + `cargo check` + a manual run.

**Reference spec:** `docs/superpowers/specs/2026-06-14-federated-plugin-sources-design.md` (§5 trust badges, §7 API, §8 desktop UX).

---

## File Structure

**Create:**
- `opencapital-app/src/components/SourcesView.tsx` — the Sources management screen.

**Modify:**
- `opencapital-app/src/types.ts` — `SourceInfo` type, `CatalogEntry.source`, `PluginSource` type.
- `opencapital-app/src-tauri/src/kinde.rs` — `list_sources`/`add_source`/`remove_source` commands.
- `opencapital-app/src-tauri/src/lib.rs` — register the 3 commands in `tauri::generate_handler!`.
- `opencapital-app/src/api.ts` — `listSources`/`addSource`/`removeSource`.
- `opencapital-app/src/components/AppShell.tsx` — `NavKey` + `NAV` gain `sources`.
- `opencapital-app/src/App.tsx` — render `SourcesView` on `nav === "sources"`.
- `opencapital-app/src/components/PluginsView.tsx` — source badge + third-party select confirm.

**Validate (no code):** `plugins/<id>.json` version bump + a real GHCR tag (Task 7).

---

## Task 1: Types

**Files:**
- Modify: `opencapital-app/src/types.ts`

- [ ] **Step 1: Add the types**

After the `VersionStatus` type, add `SourceInfo`; add `source` to `CatalogEntry`; add `PluginSource`:

```ts
export type SourceInfo = {
  url: string;
  publisher: string;
  verified: boolean;
};

export type CatalogEntry = {
  plugin_id: string;
  display_name: string;
  description: string;
  type: 'app' | 'datasource' | 'panel';
  required: boolean;
  installed: boolean;
  latest_validated_version?: string;
  source?: SourceInfo;
};

// A user-added plugin manifest URL (GET /v1/sources). The official set is not
// listed here — it comes from the curated plugins.json and shows as verified in
// the catalog.
export type PluginSource = {
  manifest_url: string;
  publisher: string;
  enabled: boolean;
};
```

(Replace the existing `CatalogEntry` definition with the one above — only the `source?` line is new.)

- [ ] **Step 2: Typecheck**

Run: `cd opencapital-app && npm run build`
Expected: compiles (no type errors). If `npm run build` is slow, `npx tsc --noEmit` is sufficient for this step.

- [ ] **Step 3: Commit**

```bash
git add opencapital-app/src/types.ts
git commit -m "feat(desktop): SourceInfo + CatalogEntry.source + PluginSource types

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Rust commands for /v1/sources

**Files:**
- Modify: `opencapital-app/src-tauri/src/kinde.rs`
- Modify: `opencapital-app/src-tauri/src/lib.rs`

The backend routes are global (not org-scoped), Kinde-authed: `GET /v1/sources`, `POST /v1/sources {manifest_url}`, `DELETE /v1/sources?manifest_url=...`. Mirror the existing `marketplace_catalog`/`me_orgs` command style (`current_token`, `reqwest::Client::new`, `read_json`, base `cfg.control_plane_url`).

- [ ] **Step 1: Add the three commands to `kinde.rs`** (place them near `marketplace_catalog`)

```rust
/// list_sources returns the user-added plugin manifest URLs (GET /v1/sources).
/// Global (not org-scoped); the official set is implicit in the catalog.
#[tauri::command]
pub async fn list_sources(
    cfg: State<'_, AppConfig>,
    session: State<'_, Session>,
) -> Result<serde_json::Value, String> {
    let token = current_token(&session)?;
    let resp = reqwest::Client::new()
        .get(format!("{}/v1/sources", cfg.control_plane_url))
        .bearer_auth(token)
        .send()
        .await
        .map_err(|e| format!("call GET /v1/sources: {e}"))?;
    read_json(resp, "GET /v1/sources").await
}

#[derive(serde::Serialize)]
struct AddSourceRequest<'a> {
    manifest_url: &'a str,
}

/// add_source validates + persists a user-added per-plugin manifest URL
/// (POST /v1/sources). Surfaces the control-plane validation error (422) verbatim
/// so the UI can show "manifest unreachable or invalid: …".
#[tauri::command]
pub async fn add_source(
    manifest_url: String,
    cfg: State<'_, AppConfig>,
    session: State<'_, Session>,
) -> Result<serde_json::Value, String> {
    let token = current_token(&session)?;
    let resp = reqwest::Client::new()
        .post(format!("{}/v1/sources", cfg.control_plane_url))
        .bearer_auth(token)
        .json(&AddSourceRequest { manifest_url: &manifest_url })
        .send()
        .await
        .map_err(|e| format!("call POST /v1/sources: {e}"))?;
    read_json(resp, "POST /v1/sources").await
}

/// remove_source deletes a user-added source (DELETE /v1/sources?manifest_url=…).
#[tauri::command]
pub async fn remove_source(
    manifest_url: String,
    cfg: State<'_, AppConfig>,
    session: State<'_, Session>,
) -> Result<(), String> {
    let token = current_token(&session)?;
    let resp = reqwest::Client::new()
        .delete(format!("{}/v1/sources", cfg.control_plane_url))
        .query(&[("manifest_url", &manifest_url)])
        .bearer_auth(token)
        .send()
        .await
        .map_err(|e| format!("call DELETE /v1/sources: {e}"))?;
    let status = resp.status();
    if !status.is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(format!("DELETE /v1/sources {status}: {body}"));
    }
    Ok(())
}
```

(`remove_source` returns `()` because the backend replies `204 No Content` — `read_json` would fail on an empty body.)

- [ ] **Step 2: Register the commands in `lib.rs`**

Read `opencapital-app/src-tauri/src/lib.rs`, find the `tauri::generate_handler![ … ]` macro, and add three lines among the existing `kinde::` entries:

```rust
            kinde::list_sources,
            kinde::add_source,
            kinde::remove_source,
```

- [ ] **Step 3: Compile**

Run: `cd opencapital-app/src-tauri && cargo check 2>&1 | tail -8`
Expected: compiles (allow several minutes on first run).

- [ ] **Step 4: Commit**

```bash
git add opencapital-app/src-tauri/src/kinde.rs opencapital-app/src-tauri/src/lib.rs
git commit -m "feat(desktop): list/add/remove plugin source Tauri commands

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: api.ts wrappers

**Files:**
- Modify: `opencapital-app/src/api.ts`

- [ ] **Step 1: Add the imports + methods**

Add `PluginSource` to the type import on line 2, and add three methods to the `api` object (e.g. after `pluginVersions`):

```ts
  listSources: () => invoke<PluginSource[]>("list_sources"),
  addSource: (manifestUrl: string) =>
    invoke<PluginSource>("add_source", { manifestUrl }),
  removeSource: (manifestUrl: string) =>
    invoke<void>("remove_source", { manifestUrl }),
```

(Tauri maps the JS arg key `manifestUrl` → the Rust snake_case param `manifest_url` automatically.)

- [ ] **Step 2: Typecheck**

Run: `cd opencapital-app && npm run build`
Expected: compiles.

- [ ] **Step 3: Commit**

```bash
git add opencapital-app/src/api.ts
git commit -m "feat(desktop): api wrappers for plugin sources

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Nav entry + route

**Files:**
- Modify: `opencapital-app/src/components/AppShell.tsx`
- Modify: `opencapital-app/src/App.tsx`

- [ ] **Step 1: `AppShell.tsx` — extend `NavKey` + `NAV`**

Change the `NavKey` type (line 9):

```ts
export type NavKey = "launch" | "plugins" | "sources" | "settings";
```

Add a `NAV` entry between plugins and settings (line 24-28):

```ts
const NAV: Array<{ key: NavKey; label: string; icon: IconName }> = [
  { key: "launch", label: "Launch", icon: "rocket" },
  { key: "plugins", label: "Plugins", icon: "apps" },
  { key: "sources", label: "Sources", icon: "link" },
  { key: "settings", label: "Settings", icon: "cog" },
];
```

- [ ] **Step 2: `App.tsx` — import + render**

Add the import (after the `PluginsView` import, line 7):

```ts
import { SourcesView } from "./components/SourcesView";
```

Add the route between the plugins and settings blocks (after line 110):

```tsx
        {selectedOrg && nav === "sources" && <SourcesView />}
```

(Sources are global/per-machine, so `SourcesView` takes no `org` prop; the `selectedOrg &&` guard only mirrors the other tabs' "needs a workspace context to be visible" gating.)

- [ ] **Step 3: Typecheck**

Run: `cd opencapital-app && npm run build`
Expected: FAILS only because `SourcesView` doesn't exist yet (Task 5). If you want this task green in isolation, do Task 5 first; otherwise proceed to Task 5 and typecheck there.

- [ ] **Step 4: Commit** (fold with Task 5 if you prefer a single green commit)

```bash
git add opencapital-app/src/components/AppShell.tsx opencapital-app/src/App.tsx
git commit -m "feat(desktop): Sources nav entry + route

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: SourcesView component

**Files:**
- Create: `opencapital-app/src/components/SourcesView.tsx`

- [ ] **Step 1: Write the component** (match `PluginsView`/`SettingsView` conventions)

```tsx
import { useEffect, useState } from "react";
import { css } from "@emotion/css";
import { GrafanaTheme2 } from "@grafana/data";
import {
  Alert,
  Badge,
  Button,
  Card,
  EmptyState,
  Field,
  Input,
  Spinner,
  Stack,
  Text,
  useStyles2,
} from "@grafana/ui";
import { api, errMsg } from "../api";
import type { PluginSource } from "../types";

export function SourcesView() {
  const styles = useStyles2(getStyles);
  const [sources, setSources] = useState<PluginSource[]>([]);
  const [loading, setLoading] = useState(true);
  const [url, setUrl] = useState("");
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState("");

  async function load() {
    setLoading(true);
    setError("");
    try {
      setSources((await api.listSources()) ?? []);
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    load();
  }, []);

  async function add() {
    const u = url.trim();
    if (!u) return;
    setAdding(true);
    setError("");
    try {
      await api.addSource(u);
      setUrl("");
      await load();
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setAdding(false);
    }
  }

  async function remove(manifestUrl: string) {
    setError("");
    try {
      await api.removeSource(manifestUrl);
      await load();
    } catch (e) {
      setError(errMsg(e));
    }
  }

  return (
    <div className={styles.wrap}>
      <header>
        <Text element="h1" variant="h3">
          Plugin sources
        </Text>
        <Text color="secondary">
          Add third-party plugins by their manifest URL. Official plugins are
          always available and don't need a source.
        </Text>
      </header>

      <Alert title="Only add sources you trust" severity="warning">
        Installing a plugin runs its code on your machine. A source URL you add is
        unverified — it can point at any registry. Add only manifests from authors
        you trust.
      </Alert>

      <Card>
        <Card.Heading>Add a source</Card.Heading>
        <Card.Description>
          <Field label="Per-plugin manifest URL">
            <Stack direction="row" gap={1}>
              <Input
                value={url}
                width={60}
                placeholder="https://example.com/my-plugin.json"
                onChange={(e) => setUrl(e.currentTarget.value)}
                onKeyDown={(e) => e.key === "Enter" && add()}
              />
              <Button icon="plus" disabled={adding || !url.trim()} onClick={add}>
                {adding ? "Validating…" : "Add"}
              </Button>
            </Stack>
          </Field>
        </Card.Description>
      </Card>

      {error && (
        <Alert title="Source error" severity="error" onRemove={() => setError("")}>
          {error}
        </Alert>
      )}

      {loading ? (
        <div className={styles.center}>
          <Spinner size="lg" />
        </div>
      ) : sources.length === 0 ? (
        <EmptyState variant="completed" message="No third-party sources added." />
      ) : (
        <Stack direction="column" gap={1}>
          {sources.map((s) => (
            <Card key={s.manifest_url}>
              <Card.Heading>{s.publisher || "Unknown publisher"}</Card.Heading>
              <Card.Meta>{s.manifest_url}</Card.Meta>
              <Card.Tags>
                <Badge text="Third-party" color="orange" icon="exclamation-triangle" />
              </Card.Tags>
              <Card.Actions>
                <Button
                  variant="destructive"
                  fill="outline"
                  icon="trash-alt"
                  onClick={() => remove(s.manifest_url)}
                >
                  Remove
                </Button>
              </Card.Actions>
            </Card>
          ))}
        </Stack>
      )}
    </div>
  );
}

const getStyles = (theme: GrafanaTheme2) => ({
  wrap: css({
    display: "flex",
    flexDirection: "column",
    gap: theme.spacing(2),
    width: "100%",
    maxWidth: 820,
    margin: "0 auto",
  }),
  center: css({
    display: "flex",
    justifyContent: "center",
    padding: theme.spacing(6),
  }),
});
```

- [ ] **Step 2: Typecheck**

Run: `cd opencapital-app && npm run build`
Expected: compiles (Task 4's reference to `SourcesView` now resolves). If any `@grafana/ui` import (e.g. `Field`, `Input`, `EmptyState` variant) doesn't exist in the installed version, adapt to the available component (check `PluginsView.tsx`/`SettingsView.tsx` for what's used) and note it.

- [ ] **Step 3: Commit**

```bash
git add opencapital-app/src/components/SourcesView.tsx
git commit -m "feat(desktop): SourcesView — add/list/remove plugin manifest URLs

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Source badge + third-party select confirm on PluginsView

**Files:**
- Modify: `opencapital-app/src/components/PluginsView.tsx`

- [ ] **Step 1: Add a source badge** in the `Card.Tags` `Stack` (after the `Installed` badge, around line 235)

```tsx
                    {p.source && !p.source.verified && (
                      <Badge
                        text={`Third-party · ${p.source.publisher || "unknown"}`}
                        color="orange"
                        icon="exclamation-triangle"
                      />
                    )}
```

- [ ] **Step 2: Add a confirm before SELECTING a third-party plugin.** Import `ConfirmModal` from `@grafana/ui` (add to the existing import block). Add state + gate the `toggle` for unverified sources:

```tsx
  const [confirm, setConfirm] = useState<CatalogEntry | null>(null);
```

Change `toggle` so selecting ON an unverified-source plugin opens the confirm instead of immediately selecting:

```tsx
  async function toggle(p: CatalogEntry) {
    if (p.required) return;
    const turningOn = !selection.has(p.plugin_id);
    if (turningOn && p.source && !p.source.verified) {
      setConfirm(p); // gate third-party opt-in behind a trust confirm
      return;
    }
    await applyToggle(p);
  }

  async function applyToggle(p: CatalogEntry) {
    const next = !selection.has(p.plugin_id);
    setError("");
    try {
      await api.setPluginSelection(org.org_id, p.plugin_id, next);
      setSelection((prev) => {
        const s = new Set(prev);
        if (next) s.add(p.plugin_id);
        else s.delete(p.plugin_id);
        return s;
      });
      setDirty(true);
    } catch (e) {
      setError(errMsg(e));
    }
  }
```

Render the modal near the end of the returned JSX (before the closing `</div>` of `styles.wrap`):

```tsx
      {confirm && (
        <ConfirmModal
          isOpen
          title="Install third-party plugin?"
          body={`"${confirm.display_name}" comes from an unverified source (${confirm.source?.publisher || "unknown"}). Installing runs its code on your machine on next launch. Continue?`}
          confirmText="Select"
          onConfirm={async () => {
            const p = confirm;
            setConfirm(null);
            await applyToggle(p);
          }}
          onDismiss={() => setConfirm(null)}
        />
      )}
```

- [ ] **Step 3: Typecheck**

Run: `cd opencapital-app && npm run build`
Expected: compiles.

- [ ] **Step 4: Commit**

```bash
git add opencapital-app/src/components/PluginsView.tsx
git commit -m "feat(desktop): source badge + third-party install confirm on plugins view

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: End-to-end validation — tag a plugin to production

Validate the full federated path: a new production version of one official plugin appears in the desktop marketplace. This is operational (needs a real GHCR tag); run it, capture evidence, do NOT fake it.

**Plugin under test:** `yfinance-app` (currently `versions: ["0.1.2"]` in `plugins/yfinance-app.json`). Target: add `0.1.3` to production and see it in the app.

- [ ] **Step 1: Publish the new version artifact to the TRUSTED namespace**

The artifact for `0.1.3` must exist at `ghcr.io/opencapital-dev/plugins/yfinance-app:v0.1.3` (the per-plugin manifest's `namespace`, which validated `versions` resolve from). Two paths:

- **Preferred (real publish):** in `~/trading-code/oc-plugin-yfinance-app`, bump `package.json` to `0.1.3`, push tag `v0.1.3` → the publish action builds + pushes `ghcr.io/opencapital-dev/plugins-staging/yfinance-app:v0.1.3` + cosign-signs. Then, because central promotion is removed, copy it into the trusted namespace:

  ```bash
  oras cp ghcr.io/opencapital-dev/plugins-staging/yfinance-app:v0.1.3 \
          ghcr.io/opencapital-dev/plugins/yfinance-app:v0.1.3
  ```
  (Requires `oras login ghcr.io` with a PAT carrying `write:packages` on opencapital-dev. The cosign signature referrer copies with `--recursive` if you also want the signature in trusted: add `-r`.)

- **If you cannot publish a real build,** confirm an existing `0.1.3` artifact is already in the trusted namespace before proceeding (`oras repo tags ghcr.io/opencapital-dev/plugins/yfinance-app`). If neither, STOP — do not invent a version that has no artifact (the reconciler's sha256 verify would fail at install).

Verify the tag exists:
```bash
oras repo tags ghcr.io/opencapital-dev/plugins/yfinance-app | grep -E '^v?0\.1\.3$'
```
Expected: prints the tag.

- [ ] **Step 2: Add the version to the per-plugin manifest**

Edit `plugins/yfinance-app.json` → put `"0.1.3"` first in `versions`:
```json
  "versions": ["0.1.3", "0.1.2"],
```
Commit + push so the live manifest resolves it:
```bash
git add plugins/yfinance-app.json
git commit -m "catalog: yfinance-app 0.1.3 to production

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
git push
```

NOTE on the manifest URL: the desktop's `PLUGINS_MANIFEST_URL` points at `…/opencapital/main/plugins.json`, whose pointer resolves `…/main/plugins/yfinance-app.json`. So the change must be on **`main`** to be live. To validate from the branch WITHOUT merging, temporarily run the local control-plane with `PLUGINS_MANIFEST_URL` overridden to the branch ref (`…/opencapital/feat/federated-plugin-sources/plugins.json`) — edit the env in `opencapital-app/src-tauri/src/dataplane.rs` for the test run only, then revert. State which path you took.

- [ ] **Step 3: Run the app and verify the new version surfaces**

Launch the desktop (use the `run` skill, or `make app`). Then:
1. Log in, pick a workspace, open **Plugins**.
2. Find `yfinance-app`. Open its **Version** dropdown (it loads `/v1/marketplace/plugins/yfinance-app/versions`).
3. **Verify `v0.1.3` appears and is marked validated** (not preview).
4. Select/keep it, go to **Launch**, relaunch Grafana, and confirm the reconciler installs `yfinance-app` at `0.1.3` (watch the `reconcile-progress` events / control-plane log; the install does a sha256-verified pull of the `:v0.1.3` blob).

- [ ] **Step 4: Capture evidence + record the result**

Note in the PR/commit description: the `oras repo tags` output, the version dropdown showing `v0.1.3 (validated)`, and the successful launch-time install log line. If you overrode `PLUGINS_MANIFEST_URL` for the test, REVERT that edit and confirm `dataplane.rs` is back to the `main` URL before finishing.

- [ ] **Step 5: Revert the test version if it was a throwaway**

If `0.1.3` was published only to validate the path (not a real release), revert `plugins/yfinance-app.json` to `["0.1.2"]` and (optionally) delete the throwaway tags, so production isn't left advertising an unintended version. If `0.1.3` is a genuine release, keep it.

---

## Final verification

- [ ] **Typecheck + Rust check**

Run: `cd opencapital-app && npm run build` and `cd opencapital-app/src-tauri && cargo check`
Expected: both clean.

- [ ] **Manual smoke** (Sources screen)

Launch the app → **Sources** tab. Add a known-good per-plugin manifest URL → it appears in the list and its plugin shows in **Plugins** with a "Third-party" badge; selecting it triggers the trust confirm. Remove it → it disappears from both. (For a quick test fixture, host a minimal per-plugin manifest, e.g. a gist raw URL, pointing at a public registry plugin.)

---

## Notes / follow-ups

- The plugin **publish action** (`oc-plugin-publish-action`, separate repo) still targets the old staging + catalog-PR flow and does not yet emit per-plugin manifests or push to the trusted namespace. Until it's updated, Task 7's "publish to trusted" needs the manual `oras cp`, and official per-plugin manifests are hand-maintained under `plugins/`. Tracked in the `opencapital-plugin-publish-promote` memory.
- Per-source auth (private registries), digest pinning, and control-plane-side signature verification remain future hardening (spec §10/§11).

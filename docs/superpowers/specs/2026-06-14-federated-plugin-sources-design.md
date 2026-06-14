# Federated plugin sources — design

- **Date:** 2026-06-14
- **Status:** Approved design (pre-implementation)
- **Topic:** Decentralized, federated plugin discovery for the OpenCapital marketplace

## Problem

Today the marketplace catalog reads a single public `plugins.json` (ids + versions)
and pulls every artifact from one GHCR registry whose coordinates are static
control-plane env (`REGISTRY_INTERNAL_URL/PUBLIC_URL/NAMESPACE/STAGING_NAMESPACE`).
`plugins`/`plugins-staging` are the official OpenCapital namespaces, published from
each opencapital repo's CI. There is no way for:

1. OpenCapital to **curate/feature** plugins it did not build (hosted in other
   namespaces/registries) into the marketplace, nor
2. users to **discover and install** third-party plugins from sources OpenCapital
   has never seen.

Goal: a decentralized plugin ecosystem where each plugin owns its own
self-describing manifest (versions + registry coordinates), the official catalog
is a curated list of pointers to those manifests, and users can add unknown
plugins by URL — without a central registrar and without registry catalog
enumeration (which GHCR does not support anonymously anyway).

## Decisions (locked)

| Decision | Choice | Why |
|---|---|---|
| Governance | **User-added source URLs** (Helm/apt model), no central index | No review burden; fully decentralized. App ships knowing only the official list. |
| Trust | **Trust-on-add + badge** | Adding a URL = the user vouches. Official = verified; everything else badged by origin. Signature verification is a later hook, not v0. |
| Identity | **Per-plugin manifest URL is the key** | No central gatekeeper exists to enforce id uniqueness, so the manifest URL is the namespace (apt/Helm/Docker model). Prevents impersonation of `core-app` etc. |
| Manifest unit | **Per-plugin manifest** | Each plugin's own repo publishes one self-describing manifest (registry coords + versions). `plugins.json` is just a curated list of pointers — no duplication. |
| Version ownership | **In each plugin's manifest, explicit** | Decentralized: each plugin repo owns its version list. Read explicitly (not live registry tags) so validated-vs-preview stays controllable with no extra round-trip. |
| Aggregation | **Server-side (control-plane)** | control-plane is already the catalog authority, does anonymous OCI pulls, and runs per-user locally. The desktop stays thin. |
| Artifact fetch | **Direct, per-plugin registry URL** | control-plane hands the reconciler a direct blob URL on the plugin's own registry host; reconciler pulls direct (as today). No proxy in v0. |

### Why not registry catalog enumeration

GHCR does not serve `/v2/_catalog` (or per-owner repo listing) with an anonymous
token; pulling a *known* artifact anonymously works (the `ghcr.io/token` noop-token
dance, which `oras`/`registry.Client` already do). Therefore **the manifest URLs ARE
the catalog** — discovery never enumerates a registry. Federation = "more
manifests," not "enumerate more registries."

## Architecture: control-plane (brain) vs. reconciler (hands)

Discovery and install are split by responsibility, not duplicated — and federation
is contained entirely to the brain side.

- **control-plane (brain):** discovery (which plugin manifests to show + their
  versions → catalog), decision (records the org's desired install set in
  `control_db`), and resolution (`/v1/internal/orgs/{org}/plugins` returns desired
  plugins + metadata + a resolved `artifact{download_url, sha256, size}` per host
  platform). **All** manifest/registry knowledge lives here.
- **`instance-bootstrap` (hands / reconciler):** pulls the desired list, then per
  plugin downloads → verifies sha256 → extracts → symlinks → renders Grafana YAML
  (idempotent via `.artifact-sha256`). It sees only `{download_url, sha256, size}`
  and knows nothing about manifests or registries.

Consequence: **every change in this design lands in control-plane.** The reconciler
is untouched — a third-party plugin's `download_url` simply names a different host,
which its generic bearer dance (§6) already handles.

## 1. Two file types

### 1a. Per-plugin manifest (owned by each plugin's repo)

Self-describing. Declares the plugin's registry coordinates and its own version
list. This is the unit a user adds by URL, and the unit `plugins.json` points to.

```json
{
  "schemaVersion": 1,
  "pluginId": "acme-charting",
  "publisher": "Acme Corp",
  "registry": {
    "host": "ghcr.io",
    "namespace": "acme/oc-plugins",
    "stagingNamespace": "acme/oc-plugins-staging",
    "publicURL": "https://ghcr.io"
  },
  "versions": ["1.4.0", "1.3.0"],
  "preview": ["1.5.0-rc1"]
}
```

**Field rules**

- `pluginId` (required) — the plugin's id; the install/display id.
- `publisher` (required) — display only (untrusted; see §2).
- `registry` (required):
  - `host` (required) — OCI registry host.
  - `namespace` (required) — repository prefix: `<host>/<namespace>/<pluginId>:<tag>`.
  - `stagingNamespace` (optional) — powers `preview`; omit if no preview channel.
  - `publicURL` (optional) — host-reachable base stamped into the reconciler's
    direct blob URL. Defaults to `https://<host>`.
- `versions` (required, may be empty) — validated versions, bare semver,
  highest-is-productive. Owned + updated by the plugin's own repo/CI on release.
- `preview` (optional) — preview versions; resolved from `stagingNamespace`.

**Validation:** reject a manifest missing `pluginId`, `registry.host`, or
`registry.namespace`; or with a non-empty `preview` but no `stagingNamespace`.

### 1b. Marketplace list (`plugins.json`, OpenCapital-curated)

Just pointers — no versions, no coordinates. The curated default set.

```json
{
  "schemaVersion": 1,
  "plugins": [
    "https://raw.githubusercontent.com/opencapital-dev/opencapital/main/plugins/core-app.json",
    "https://raw.githubusercontent.com/opencapital-dev/opencapital/main/plugins/core-datasource.json",
    "https://raw.githubusercontent.com/opencapital-dev/opencapital/main/plugins/yfinance-app.json"
  ]
}
```

**Validation:** reject a non-array `plugins` or a non-URL entry. A per-plugin
manifest that fails to fetch/parse is logged + skipped, not fatal to the list.

(For v0 the official per-plugin manifests are hosted in this repo under `plugins/`;
they can later move into each plugin's own repo — the list just points wherever.)

## 2. Source identity = the per-plugin manifest URL

A plugin's internal key is **the (canonicalized) manifest URL** — the official
list entry or the URL a user pasted — never the self-declared `pluginId`/`publisher`.
Those are display-only and untrusted. Rationale: with no central gatekeeper, a
self-declared id cannot be trusted for uniqueness or anti-impersonation, but the
URL is unique and either curated by OpenCapital or controlled by the user at
add-time. Two manifests both declaring `core-app` are distinct entries keyed by
their URLs.

A manifest *may* point its `registry` at any host — including the official
namespace or any blob host the reconciler will execute. Trust-on-add + the badge
(§5) cover this in v0; digest-pinning / signatures are the later harden.

## 3. Source store (control-plane owns user additions)

`control_db` table `plugin_sources` holds **only user-added** manifest URLs:

| column | notes |
|---|---|
| `manifest_url` | PK; the canonical per-plugin manifest URL |
| `publisher` | last-parsed display value (refreshed on fetch) |
| `enabled` | soft toggle |
| `added_at` | |

The **official** set is NOT stored — it is the live `plugins.json` fetch (cached
with a TTL). There is no "seed official" step. `verified = the manifest URL appears
in the current `plugins.json``. Per-machine locally; per-org in the cloud
deployment — same table.

## 4. Catalog aggregation & keying

- Official URLs come from fetching `plugins.json` (the marketplace list). User URLs
  come from `plugin_sources`. The catalog spans their union (dedup by URL; official
  wins → `verified=true`).
- For each URL: fetch the per-plugin manifest (cached per URL with a TTL) → its
  `registry` coords + version lists → resolve footprint + platforms from OCI at the
  highest validated version (or, if `versions` is empty, the highest `preview`
  version, with `Version=""`). Stamp `source { url, publisher, verified }`.
- Entries are keyed by manifest URL; `pluginId` is read from the manifest. No
  `_catalog` enumeration anywhere.
- `VersionsWithStatus` and `ResolveArtifact` locate the plugin by id across the
  union, then resolve via that plugin's own registry coords.
- `registry.Client` is refactored from "one static registry" to "resolve across a
  `PluginProvider`" (the union above). The OCI plumbing (footprint/manifest/tags/
  blob) is parameterized by the per-plugin `registry`.

## 5. Trust & badges

- A plugin whose manifest URL is in the official `plugins.json` → `verified = true`,
  no badge. It is featured/trusted because OpenCapital vouched by listing it — even
  if hosted in a third-party namespace.
  - ⚠️ Featuring a plugin hosted in a namespace you don't control extends official
    trust to bytes you can't guarantee (the host could swap the artifact). v0: only
    list manifests/namespaces you trust. Later harden: pin versions by digest, or
    require signatures.
- Any non-listed (user-added) plugin → badge "Third-party · `<publisher>` · `<host>`",
  `verified = false`.
- The trust warning renders at **install** time (the code-execution moment), not
  only when browsing: "Installing runs third-party code from `<host>`. Only add
  sources you trust."

### Trust boundary (v0 — accepted)

The source key (manifest URL) is **namespacing, not authentication** — a public
string, nothing to steal. What actually protects users:

- **Official plugins:** cosign signatures at publish (publisher's private key) +
  **manual PR review of the curated `plugins.json` list** + HTTPS/domain control of
  the official list + manifest URLs. NOTE: the old automated cosign-verify-before-
  trust *promotion gate* (`plugin-promote` CI) is **dropped** with the per-plugin
  model — promotion no longer exists; curation review is the human gate. An
  automated control-plane-side signature verify at catalog-resolve time is the
  follow-on hardening (§10).
- **Third-party plugins:** trust-on-add. Protections are HTTPS (real origin), the
  `sha256` byte-integrity check in the reconciler, and the visible origin badge +
  user judgment. **No** cryptographic publisher identity. Residual risks accepted for
  v0: typosquatted manifest URLs, and a malicious-but-correct manifest. Upgrading
  third-party identity = the deferred signature hook (§10).

## 6. Artifact fetch (direct)

control-plane's artifact endpoint returns a direct `download_url` =
`<registry.publicURL>/v2/<namespace>/<pluginId>/blobs/<digest>` for the plugin's own
registry; the reconciler fetches it directly from that host.

**Arbitrary-host auth — already handled (no change needed).** `instance-bootstrap`'s
`download()` does a generic Docker-v2 anonymous bearer dance: GET the blob → on 401
parse `WWW-Authenticate: Bearer realm/service/scope` → fetch a token from that realm
with no credentials → retry (`lib/instance-bootstrap/reconcile.go:147-162`). This is
host-agnostic, so a per-plugin host (ghcr.io, zot, Docker Hub, any public token-auth
registry) works with zero new config. The reconciler then verifies the downloaded
bytes against the `sha256` control-plane resolved → byte-integrity regardless of host.

Anonymous GHCR rate limit is 60 req/hr — keep per-plugin pulls lazy and lean on the
manifest caches; a proxy-through-control-plane option is deferred (see Future).

## 7. API (control-plane)

- `GET /v1/sources` → list user-added plugin manifests (URL + publisher + enabled).
  The marketplace catalog endpoint already returns the full union with verified flags;
  this is the management view of user additions.
- `POST /v1/sources { manifest_url }` → fetch, validate the per-plugin manifest
  schema (§1a), persist; reject unreachable/malformed.
- `DELETE /v1/sources { manifest_url }` → remove a user-added manifest. (Official
  entries aren't in the table, so can't be removed here.)
- Existing catalog/install/versions/artifact endpoints carry the resolved
  `source { url, publisher, verified }` per entry.

## 8. Desktop UX

- New **Sources** screen: list user-added plugin manifests; add-by-URL (paste →
  preview parsed publisher/pluginId → confirm); remove.
- Marketplace cards labelled by source; verified vs. third-party badge.
- Install dialog shows the §5 trust warning for third-party plugins.

The `registry` package splits into two clients: a **catalog** client (federated
read, manifest-driven, zero registry coords) and a **staging** client (the janitor's
publish/prune path). They no longer share construction.

- **Catalog client:** built from the `PluginProvider` only. Reads NO `REGISTRY_*`
  coords — every coordinate comes from a per-plugin manifest.
- **Drop** `REGISTRY_PUBLIC_URL` from config entirely (it was catalog-only; the
  public base now comes from each manifest's `registry.publicURL`).
- **Janitor / staging client (publish-prune subsystem, decoupled from discovery):**
  retains its own config — `REGISTRY_INTERNAL_URL` (host), `REGISTRY_NAMESPACE`
  (trusted, for the already-promoted check), `REGISTRY_STAGING_NAMESPACE`,
  `REGISTRY_OWNER`, and `REGISTRY_USERNAME`/`REGISTRY_PASSWORD`. It prunes
  OpenCapital's own staging namespace and never participates in federated discovery;
  secrets can't live in a public manifest. Behavior unchanged — only its
  construction is separated from the catalog.
- **Keep** the one discovery bootstrap: the official marketplace list URL (today
  `PLUGINS_MANIFEST_URL`, now pointing at the list form of `plugins.json`).
- **Desktop** `spawn_control_plane` env: drop all `REGISTRY_*` (the local
  control-plane's janitor no-ops without creds, and the catalog is manifest-driven);
  keep `PLUGINS_MANIFEST_URL`. The cloud deployment keeps the janitor `REGISTRY_*`
  in its own env.
- **Update** the repo `plugins.json` to the list form, and add the per-plugin
  manifests under `plugins/` for the official plugins.
- **Drop the central promotion CI** — `plugin-promote-check.yml`,
  `plugin-promote-reconcile.yml`, `.github/actions/plugin-promote/`, and
  `.github/plugins/signers.yaml`. They reconciled the trusted namespace to the old
  `plugins.json`; the new list has no trusted-set/version semantics, so promotion is
  obsolete (see §10 for the trust implication).

## 10. Risks / future hardening

- **Dropped promotion gate** → the old `plugin-promote` CI verified cosign
  signatures against `signers.yaml` before trusting an artifact. With the per-plugin
  model that gate is removed (promotion no longer exists); manual `plugins.json`
  review is the interim gate. Restore automated assurance by verifying signatures
  **control-plane-side at catalog-resolve time** against a signer allowlist.
- **Curated-but-not-controlled bytes** (§5 caveat) → digest pinning / signatures.
- **Anonymous registry rate limits** → manifest cache; optional CP blob proxy.
- **Private third-party sources** (anon 401) → per-source auth (out of scope v0).

## 11. Out of scope (YAGNI for v0)

- Central index / curated discovery (governance choice is user-added URLs).
- Per-source authentication / private registries.
- Signature verification for third-party plugins.
- Blob proxying through control-plane.
- Live registry tag-listing for versions (decided: explicit lists in the per-plugin
  manifest).
- Mirroring third-party plugins into the official namespace (the list intentionally
  references, not mirrors).

## 12. Phasing (for the implementation plan)

1. **Manifest formats + per-plugin abstraction.** Parse both file types; introduce
   the per-plugin `Plugin`/`Registry`/`PluginProvider` types; drop REGISTRY_* read
   coords; decouple the janitor onto its own staging config. Update `plugins.json` +
   add official per-plugin manifests. Drop the obsolete `plugin-promote` CI.
2. **Multi-source aggregation + source-qualified keying.** `List` /
   `VersionsWithStatus` / `ResolveArtifact` resolve across the official-list ∪
   user-URL union; catalog API carries `source`.
3. **Source CRUD API + desktop Sources UI + badges + install warning.**

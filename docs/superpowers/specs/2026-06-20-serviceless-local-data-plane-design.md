# Serviceless local data plane — design (Spec A)

- **Date:** 2026-06-20
- **Status:** Approved design (pre-implementation)
- **Topic:** Remove all standalone backend services (write-gateway, read-gateway, control-plane) and Kafka from the local-first desktop app. Plugins and the compute sidecar talk the two databases directly over pgwire. Tauri absorbs the platform/marketplace brain.
- **Follow-on:** Spec B — per-plugin schema decentralization across both stores (separate doc).

## Problem

OpenCapital began as a cloud, multi-tenant, Kafka-backed platform. Three Go
services formed a trust + transport tier between plugins and the data:

- **write-gateway** — verified session JWTs, looked up `portfolio_id → org_id`,
  stamped `org_id` onto an Avro envelope, produced to Redpanda (or, in the local
  build, wrote typed pgwire DML straight into RisingWave under `SINK_MODE=rw`).
- **read-gateway** — verified JWTs, resolved portfolio ownership, compiled a DSL
  selector (`nav@latest{portfolio=…}`) to org-scoped SQL against RisingWave views,
  executed it, returned `{columns, rows}`.
- **control-plane** — JWT/JWKS issuance, instance-token minting, Kinde identity,
  org/onboarding, plugin catalog + federation + install/uninstall + artifact
  resolution, portfolios CRUD, and `control_db` migrations incl. the CDC
  publication RisingWave consumes.

The product is now a **single-user local desktop app**. There is no Kafka in the
local build, no second tenant, and no remote trust boundary — every component runs
on loopback inside one machine the user already controls. The entire trust tier is
ceremony. Its costs are real: three services to build, stage as Tauri sidecars,
spawn, supervise, and health-check; an Avro/Schema-Registry path that is dead
locally; a DSL compiler whose only remaining job (tenancy injection) is moot; and
an HTTP hop on every read and write.

Goal: collapse the backend to its essence — **two databases and the plugins that
use them** — with Tauri as the platform shell. No standalone backend services. No
Kafka. No JWT/org on the data path. Each datastore is reached directly over pgwire.

### What this spec does NOT do (deferred to Spec B)

This spec removes the service tier and repoints traffic to direct pgwire. It does
**not** decentralize schema ownership. After Spec A, the RisingWave schema still
lives in this host repo under a single default schema, and clients connect with a
single trusted role. Spec B moves schema SQL into plugin repos, introduces
schema-per-plugin + scoped roles in both stores, the host boot reconciler that
provisions them, and the SDK migration framework. The two specs are sequential:
Spec A demolishes the service tier; Spec B decentralizes the schema.

## Decisions (locked)

| Decision | Choice | Why |
|---|---|---|
| Service tier | **Remove all three** (write-gw, read-gw, control-plane) | Loopback single-user app; the trust/transport tier is pure ceremony. |
| Transport | **Direct pgwire** to each store | No HTTP hop; plugins and compute already get DB coordinates via instance-bootstrap. |
| Kafka / Redpanda / Schema Registry | **Delete entirely** | No Kafka in the local build; the `SINK_MODE=rw` connector-less path is already canonical. |
| Auth / org_id / JWT | **Delete on the data path and as identity** | One implicit user; loopback trust. `portfolio_id` is the sole scope key. |
| Read contract | **Raw SQL, no DSL** | DSL's only jobs were tenancy + allow-list, both deleted. Plugins already author SQL. |
| Datastores | **Two, purpose-built** — Postgres (OLTP) + RisingWave (streaming) | RW is not OLTP; UI CRUD (portfolios) needs point reads / read-your-writes / `PATCH`. |
| Portfolios | **core plugin → Postgres direct** | Portfolios are domain reference data the portfolio-admin plugin owns; OLTP fits the UI write path. |
| CDC bridge | **Keep** Postgres `portfolios` → RW | The fold/metrics pipeline joins `portfolios.base_currency`; reference data must reach RW. |
| Catalog / marketplace / install | **Fold into Tauri (Rust)** | Plugin lifecycle is a platform concern; the shell exists before any plugin (no bootstrap circularity). |
| control_db migrations + CDC publication | **Host boot reconciler** | Owns DB bootstrap already (`dataplane.rs::bootstrap_control_db`); no service needed. |
| Plugin/SDK code changes | **Companion specs in their own repos** | The SDK + three plugin repos are separate; this spec defines the contract they target. |

## Target architecture

```
┌──────────────────────────── Tauri shell (opencapital-app) ────────────────────────────┐
│  spawns + supervises:  Postgres (:5432) · RisingWave (:4566) · compute sidecar          │
│  IS the platform brain: plugin catalog + federation + install/uninstall + artifact      │
│                          resolution + Grafana provisioning  (ported from control-plane) │
└─────────────────────────────────────────────────────────────────────────────────────────┘
        │ spawns                         │ spawns                          │ spawns
        ▼                                ▼                                 ▼
  Postgres (OLTP)                  RisingWave (streaming)            compute sidecar (Python)
   control_db:                       default schema:                  pgwire SQL → polars →
     portfolios  ◀── core plugin       data_log, portfolio_events_log    Python metric exec
     (CRUD via pgwire,                  fold → metrics → surface         (pg8000; replaces the
      keyed by portfolio_id)            (portfolio_id-scoped)            read-gateway HTTP hop)
        │                                ▲        ▲
        │ logical replication            │ INSERT │ SELECT
        └────────── CDC (pg_cdc) ────────┘ (write)│ (read)
                portfolios → RW                   │
                                          plugins ─┘  (direct pgwire: ingest + read + extend)

DELETED: services/gateway, services/read-gateway, services/control-plane,
         Kafka/Redpanda/Schema-Registry, JWT+org_id on the data path, the DSL compiler.
```

End state: **zero standalone backend services.** Only plugins + Tauri + the two
database runtimes + the compute sidecar. Postgres is the OLTP layer (UI CRUD),
RisingWave is the streaming analytics layer, CDC bridges reference data from the
former into the latter.

## Component changes (this repo)

Plugin-side and SDK-side code changes are companion work in their own repos; this
section is the in-repo (opencapital) surface. Where a change defines a contract a
plugin must target, it is called out as **[contract]**.

### 1. RisingWave schema — drop org_id, drop Kafka sources

- Strip `org_id` from all 31 schema files: the landing tables
  (`portfolio_events_log`, `data_log`), `fold_per_event`, the `*_per_tick` /
  `*_per_event` metrics, the surface views, `fx_rates`, `instruments`. `portfolio_id`
  becomes the sole scope key; `fold_per_event` partitions by `portfolio_id` alone.
- Delete the cloud Kafka source variant (`schemas/01-sources/`); promote the
  connector-less `schemas/01-sources-local/` tables to canonical (drop the
  `-local` suffix and the `PACKAGING` switch in `apply.sh`). **[contract]** these
  landing-table column shapes (minus `org_id`) are what ingestor plugins write.
- `portfolios` CDC table (`02-control-plane/01-portfolios-cdc.sql`): drop `org_id`,
  PK becomes `(portfolio_id)`. The `pg_cdc` source stays.

### 2. Postgres — keep as OLTP, shed identity

- Keep `control_db` and the `portfolios` table (drop `org_id`; key on
  `portfolio_id`). Keep the `rw_v6_pub` publication + `rw_replicator` role + CDC
  HBA. **[contract]** the `portfolios` column shape is what the core plugin writes
  and what CDC carries to RW.
- Delete the identity tables and their migrations: `organisations`, `user_org`,
  `user_external_ids`, `plugin_sources`, `jwt_signing_keys`, `audit_log`, and
  `plugin_installs`. Tauri owns install state locally (see §6).
- The control_db migration set moves out of the deleted control-plane: the host
  boot reconciler (`dataplane.rs`) runs the surviving DDL (portfolios table +
  publication) at boot, extending the existing `bootstrap_control_db`.

### 3. Compute sidecar (`services/compute`)

- Replace `compute/gateway.py` (HTTP `POST /v1/rows`) with a pgwire client using
  **pg8000** (pure-Python — PyInstaller-freeze-friendly, no libpq). It connects to
  RisingWave directly using the DSN passed by the shell.
- Replace the `bind(selector)` authoring contract with a `sql(query, *args)`
  primitive in the metric exec namespace: it runs the query over pgwire and returns
  a polars frame (reusing `_frame_from`'s column/null handling). **[contract]** the
  metric-author surface changes from selectors to SQL.
- Delete the JWT plumbing (`jwt` request field, Bearer header, `GatewayError`
  status passthrough). Drop the DSL dependency.
- Drop `READ_GATEWAY_URL`; add `RISINGWAVE_DSN` (host passes loopback DSN, as it
  already does for the gateways).

### 4. instance-bootstrap → folded into Tauri (delete the Go sidecar)

**Amendment (2026-06-20):** `lib/instance-bootstrap` is **folded into the Tauri
Rust shell**, not kept as a thin Go renderer. After A5 Tauri already owns catalog
+ artifact resolution; the reconciler's remaining work (download → verify sha256 →
extract → symlink → render Grafana provisioning YAML) becomes a Rust module in
`src-tauri`. This deletes the **last Go sidecar** — no `instance-bootstrap` binary,
no `go-sidecars` build, no Rust-resolves-then-Go-renders handoff file. Justified:
nothing but `grafana.rs` invokes it (no compose/CI/cloud consumer), and it already
runs as a native process (same context as Tauri — no new WSL boundary).

- Port `reconcile.go` (download/verify/extract/symlink, idempotent via
  `.artifact-sha256`), `provision_dashboards.go`, `library_panels.go`, and
  `metric_deps.go` into a `src-tauri` reconciler module (`reqwest` for download,
  `sha2` for verify, `tar`/`flate2` for extract, `serde_yaml` for provisioning).
  **Audit port-vs-delete first:** `metric_deps.go` analyzes the old
  read-gateway/DSL surface; in the `sql()` world it likely shrinks or is deleted
  rather than ported (~4k LOC total, less after the audit).
- `grafana.rs` calls the in-process reconciler before spawning `grafana-server`
  (it already orchestrates Grafana startup), instead of shelling out to the
  sidecar.
- Drop `gatewayUrl` / `readGatewayUrl` from rendered plugin jsonData. Keep
  `risingwaveHost` / `risingwavePort`. Add Postgres coordinates (`postgresHost` /
  `postgresPort` / `controlDb`) so the core plugin reaches OLTP. **[contract]**
- Delete `lib/instance-bootstrap` and its Makefile sidecar target entirely.
  these jsonData keys are the plugin's DB coordinates.

### 5. Tauri shell (`opencapital-app/src-tauri`) + Makefile

- `dataplane.rs`: remove spawn/supervise/health for gateway, read-gateway, and
  control-plane; remove `GW_PORT`, `RG_PORT`, `CP_PORT`, `LOCAL_TOKEN`, the JWKS /
  instance-token env wiring. Keep Postgres + RisingWave + compute. Extend the boot
  reconciler to run the surviving control_db DDL + publication.
- `lib.rs`: remove the `"gateway"`, `"read-gateway"`, `"control-plane"` sidecar
  references.
- Makefile: remove the `go-sidecars` entries + build/stage targets for the three
  services; remove Kafka/Redpanda/Schema-Registry artifacts and the topics-seed
  path. Keep compute-freeze + RW/PG artifact staging.
- Delete `services/gateway`, `services/read-gateway`, `services/control-plane`
  source trees and their Dockerfiles. Delete `lib/jwks` and `lib/datakey` if no
  surviving consumer remains (verify; `datakey` may still describe `rw_key`).

### 6. Catalog / marketplace / install → Tauri (Rust port) — the long pole

Port control-plane's platform brain into the shell:

- **Manifest + federation** (`internal/manifest`, `internal/registry/catalog`,
  `internal/sources`): fetch + cache the official `plugins.json` and per-plugin
  manifests; merge official + user-added sources; resolve highest validated /
  preview version; stamp the verified badge. Re-implemented in Rust in Tauri.
- **Artifact resolution** (`registry.ResolveArtifact`): fetch the OCI manifest,
  match the platform layer, produce the direct blob URL — the anonymous GHCR
  token dance. Rust port (or reuse an existing Rust OCI client).
- **Install/uninstall + state**: Tauri persists the org's (now the single user's)
  install set locally — a small SQLite or JSON store under `base_dir`. Generates
  any per-plugin token the plugin still needs (likely none, post-auth-removal).
- **Provisioning (folded in, §4)**: the reconciler — download → verify → extract →
  symlink → render Grafana provisioning YAML — is ported from `lib/instance-bootstrap`
  into a Tauri Rust module and called in-process by `grafana.rs`. No handoff file,
  no Go sidecar.

This is the largest single block and is internally phaseable in the implementation
plan (manifest/version logic first, then artifact resolution, then the reconciler
fold).

## Data flow after Spec A

**Write (ingest):** a plugin opens pgwire to RisingWave and `INSERT`s into a
landing table (`data_log` with a `source_namespace` discriminator for observations;
`portfolio_events_log` for portfolio events) using the `rw_key` PK upsert /
tombstone contract. No gateway, no Avro, no JWT.

**Read (metrics):** the compute sidecar opens pgwire to RisingWave, runs the
metric's `sql(...)` against the surface views + landing data, builds a polars frame,
runs the Python metric, returns the neutral frame. The datasource plugin issues
pgwire SQL directly for panel queries. No read-gateway, no DSL.

**Portfolios (UI CRUD):** the core plugin opens pgwire to **Postgres** and
upserts/reads `control_db.portfolios` directly — point reads and `PATCH` with
read-your-writes. CDC streams the change into RW's `portfolios` table; the
fold/metrics pipeline joins `base_currency` as before.

**Plugin install:** Tauri resolves the catalog, downloads + verifies the artifact,
records install state locally, extracts it, and renders the Grafana provisioning
YAML — all in-process (the folded-in reconciler). No control-plane, no Go sidecar.

## Risks & verification

- **pg8000 under PyInstaller.** Pure-Python, should freeze cleanly, but verify the
  frozen compute binary connects to RW over pgwire on a clean machine. RW speaks the
  Postgres wire protocol; pg8000's simple-query path is the safe default.
- **RW reference reads.** Confirm the `portfolios` CDC table (now `portfolio_id`-keyed)
  still joins correctly across the pipeline after `org_id` removal.
- **org_id removal correctness.** Stripping `org_id` touches the fold/metrics graph;
  rely on the existing RW golden/regression checks to prove NAV/MtM unchanged.
- **Catalog port fidelity.** The Rust port must preserve the federated-sources
  semantics just built on `feat/federated-plugin-sources` (verified badge, official
  wins, validated-vs-preview, direct blob URL). Port against that spec's tests.
- **Companion-repo lockstep.** The SDK (drop `readgateway.go`/`publish.go`/`dsl`,
  add pgwire read/write) and the three plugin repos must land their slices in step
  with this spec's contract changes; sequence in the implementation plan.

## Out of scope (Spec B)

Per-plugin schema/role isolation in Postgres and RisingWave; moving schema SQL into
plugin repos; the SDK `rwmigrate` / `pgmigrate` framework; the host reconciler that
provisions per-plugin schemas + roles + grants; formalizing the core surface as a
granted, versioned contract. Spec A keeps a single default schema and a single
trusted connection per store.

# Serviceless migration — overnight handoff (2026-06-21)

**Goal:** finish the serviceless local-data-plane migration; test as much as possible; **do NOT commit** (1Password needed for signing, you were away).

## TL;DR

The migration is **functionally complete and tested**. opencapital is now **serviceless** — zero `go.mod`, no gateways, no control-plane. Everything in opencapital is **uncommitted** in the working tree on `feat/federated-plugin-sources` (per your 1Password note). The SDK + 3 plugin repos were finished + **merged to their own `main`s earlier** (before the no-commit window).

**One item needs your app-level verification** (couldn't reproduce faithfully headless): the RisingWave↔Postgres **portfolios CDC** link — details in "Risks" below.

## What was done

### Plugin/SDK repos — merged to `main` (committed, local only — not pushed)
- **oc-plugin-sdk** v0.2.0: direct-pgwire `Query`/`Exec` (RW) + `PGQuery`/`PGExec` (Postgres), `datakey`, `rwmigrate`, jwt-free `computeclient`; **SQLite removed** (`OpenDB`/SQLCipher gone); DSL/readgateway/publish/session/controlplane deleted.
- **oc-plugin-core-datasource**: thin compute passthrough (`Compute(code,from,to)`); dsl/sqlite/prefetch deleted.
- **oc-plugin-core-app**: event writes → RW `data_log`/`portfolio_events_log` pgwire (shared `insertEvent`/`insertData`/`deleteEvent`); portfolios CRUD → Postgres `PGExec`/`PGQuery`; instruments → RW `instruments_catalog`; SQLite/JWT dropped.
- **oc-plugin-yfinance-app**: price writes/tombstone → `data_log` pgwire; 5 DSL reads → RW `Query`; ticker-mapping SQLite → Postgres schema `yfinance`; JWT dropped.

Each plugin's `go.mod` points at `github.com/opencapital-dev/oc-plugin-sdk v0.2.0` with a **local `replace => ../oc-plugin-sdk`** (v0.2.0 isn't published/tagged — see "Your turn").

### opencapital — UNCOMMITTED on `feat/federated-plugin-sources`
- **A1** (committed earlier): compute sidecar reads RW over pgwire (pg8000) + `sql()` metric primitive.
- **A5** (committed earlier): plugin catalog/marketplace/federation ported to Tauri Rust (`src-tauri/src/catalog/`).
- **A4**: deleted `services/gateway`, `services/read-gateway`, `lib/datakey`; rewired `dataplane.rs`/`lib.rs`/`proxy.rs`/`config.rs`/`grafana.rs`/Makefile/instance-bootstrap (dropped gateway URLs, added Postgres coords).
- **A2**: dropped `org_id` from all RW schema files; promoted connector-less landing tables to canonical; killed Kafka from `apply.sh`; **squashed** the now-redundant + org_id-broken `migrations/V002–V007` + `plugins/prices/` and reduced `apply.sh` to Phase-A only (the base `schemas/` are the squashed final state).
- **A3**: portfolios DDL + `rw_v6_pub` publication → host reconciler (`dataplane/postgres/init/02-portfolios.sql`, run by `bootstrap_control_db`); also dropped the vestigial `gateway_ro` role.
- **A6**: deleted `services/control-plane`, `lib/jwks`, `lib/instance-bootstrap`; **folded the reconciler into Tauri Rust** (`src-tauri/src/reconcile/` — download/verify-sha256/extract/provision, fed by `catalog`); `grafana.rs` calls it in-process; `dataplane.rs` no longer spawns control-plane; `kinde.rs` instance-token mint is now a local no-op.
- **CI/WSL cleanup**: `ci.yml` dropped the Go matrix + added a Rust job; `images.yml` deleted; `opencapital-release.yml` comments fixed; `dataplane/wsl/supervisor.sh` rewritten serviceless + `Dockerfile.rootfs` Go-build stage removed.

**End state:** Tauri shell + Postgres (OLTP) + RisingWave (streaming) + Python compute. No standalone backend services.

## What was tested (verified green)

- **opencapital**: `cargo build` clean; `cargo test --lib` **67 passed** (incl. `reconcile::` + `catalog::`).
- **A2 RW schema — full live end-to-end** (the main risk): stood up a real RisingWave (bundled binary + embedded-Python UDF), applied all 31 org_id-stripped schema files **clean**, seeded a golden trade+price, and the **entire pipeline produced correct output**: `fold_kernel` UDAF ran, `portfolio_per_tick` → NAV 100 / equity 1100 / cash −1000 / unrealized 100 (10 AAPL bought @100, MtM @110), `instrument_per_tick` 10@110=1100, entity views `e_nav`/`e_portfolio`/`e_instrument`/`e_cash` populated. **org_id removal preserved all fold/metrics/entity join correctness.**
- **A3 portfolios DDL**: `01-schema.sql` + `02-portfolios.sql` apply clean to a real Postgres 17 (the bundled version); PK = single `portfolio_id` (verified via pg_index/pg_constraint/information_schema); `rw_v6_pub` publication correct; portfolio row seeded + read back.
- **compute (A1)**: **215 tests pass**.
- **SDK + 3 plugins**: `go build` + `go test ./...` **all green** on their mains.
- Final dangling-ref sweep: zero functional references to deleted services anywhere (only descriptive comments remain).

## Risks / needs your verification

1. **RW pg-cdc `portfolios` link — UNVERIFIED (top item).** When wiring the live PG→RW CDC, the RisingWave 2.8 postgres-cdc connector rejected the CDC table with *"Primary key mismatch: the SQL schema defines 1 primary key column, but the source table in Postgres has 2 columns."* Postgres's PK is **verifiably 1 column** (`portfolio_id`), and the RW CDC table declares exactly that — so this is a connector/environment disagreement, not a schema bug. My headless reproduction also surfaced a `permission denied for database control_db` on a minimal isolation test, indicating my hand-rolled PG role/grant setup is **less faithful than the app's own `dataplane.rs` bootstrap**. **Action:** launch the real app once and confirm the `portfolios` CDC table materializes in RW (`psql :4566 -d dev -c "select * from portfolios"`) and that NAV renders. If it genuinely fails with the same PK error, the fix is small (options: declare the RW CDC table to match the connector's PK expectation, or seed `portfolios` as a plain RW table written by core-app instead of CDC — the pipeline is proven to work with a plain `portfolios` table).
2. **Full app boot not tested** — needs the Tauri app + a display. The runtime wiring (Tauri spawns PG+RW+compute, the Rust reconciler downloads/provisions plugins) is build-verified + unit-tested but not boot-verified.
3. **`grafana.rs` reconcile** creates a `tokio::runtime::Runtime` inside `spawn_blocking` to call the async catalog — correct in theory (separate thread), but confirm no nested-runtime panic on a real launch.
4. **Auth vestiges** (intentional, single-user local): `kinde.rs` `me_orgs`/`create_org` compile but would fail at runtime; `mint_instance_token` returns a static `"local"` token. Kinde login UI untouched. Clean up later if desired.
5. **Windows/WSL path** rewritten but **not runnable on macOS** — verify on Windows separately.

## Your turn (requires you / 1Password)

1. **Review + commit** the opencapital working tree on `feat/federated-plugin-sources` (223 tracked changes + new `src-tauri/src/reconcile/`, `dataplane/postgres/init/02-portfolios.sql`, `scripts/rw_golden_nav.sh`). Suggested commit grouping: A4 (gateways), A2 (org_id+kafka+squash), A3 (postgres reconciler), A6 (control-plane+reconciler-fold), CI/WSL.
2. **Tag `oc-plugin-sdk v0.2.0`** (and decide whether the plugins keep the local `replace` or move to the tag) so the plugins resolve the SDK without the sibling-path replace.
3. **Push** the plugin/SDK `main`s + the opencapital branch when ready.
4. **Launch the app** to verify risk #1 (CDC) + #2 (full boot).

## Scratch
SDD reports under `.superpowers/sdd/` (gitignored): `a4-report.md`, `a2-report.md`, `a6a-report.md`, `a6b-report.md`, `c1-report.md`, `c2-report.md`. Golden harness: `scripts/rw_golden_nav.sh`.

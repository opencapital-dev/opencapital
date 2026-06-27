-- ============================================================================
-- V002__daily_price_grid — cut the per-tick MtM pipeline over to the daily grid.
-- Drops `prices` (+ its CASCADE subgraph: option_marks, fx_filled, snapshot_at_day,
-- instrument_per_tick, cash_per_tick, portfolio_per_tick, and the e_*/coverage views)
-- and recreates them from the updated schema files in dependency order. Source
-- tables (data_log, portfolio_events_log, fold_per_event, events, fx_rates,
-- instruments, portfolios) are retained — the MVs rebuild from existing data.
-- ============================================================================
-- NOTE: BLOCKING creates (no BACKGROUND_DDL) — the daily MVs are small, and
-- blocking guarantees each MV's catalog entry is committed before the next
-- dependent CREATE references it (BACKGROUND_DDL caused a catalog race here).

DROP MATERIALIZED VIEW IF EXISTS prices CASCADE;
DROP MATERIALIZED VIEW IF EXISTS option_marks CASCADE;

\ir ../schemas/03-unifying-views/prices.sql
\ir ../schemas/03-unifying-views/option_marks.sql
\ir ../schemas/04-fx/fx_filled.sql
\ir ../schemas/05-fold/snapshot_at_day.sql
\ir ../schemas/06-metrics/instrument_per_tick.sql
\ir ../schemas/06-metrics/cash_per_tick.sql
\ir ../schemas/06-metrics/portfolio_per_tick.sql
\ir ../schemas/10-entities/price.sql
\ir ../schemas/10-entities/instrument.sql
\ir ../schemas/10-entities/cash.sql
\ir ../schemas/10-entities/portfolio.sql
\ir ../schemas/10-entities/nav.sql
\ir ../schemas/07-snapshots/latest_portfolio_state.sql
\ir ../schemas/08-ingestor-discovery/02-ohlcv_coverage.sql

INSERT INTO _schema_migrations (plugin_id, version, name)
VALUES ('core', 'V002__daily_price_grid', 'daily_price_grid')
ON CONFLICT DO NOTHING;

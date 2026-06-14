-- ============================================================================
-- events — unified, enriched event stream. v4 single MV.
-- ----------------------------------------------------------------------------
-- v4 collapses v3's two-stage chain (`events` projection + `enriched_events`
-- enrichment) into a single MV named `events`. The kernel and downstream
-- consumers (fold_output, discovery MVs) all read this one MV.
--
-- Lives at 04b-events/ because it depends on `fx_rates` (04-fx/) which must
-- materialise first; lex sort orders directories `04-fx/` < `04b-events/`.
-- The local `typed` CTE handles the payload::jsonb cast.
--
-- Envelope fields added to `payload` in `enriched`:
--   event_type, source_id, portfolio_id, instrument_id,
--   business_ts (microseconds UTC int),
--   base_currency       (joined from portfolios),
--   event_time_fx       (the cross-currency rate at business_ts),
--   instrument_kind     (joined from instruments — drives kernel dispatch),
--   contract_multiplier (joined from instruments — 1.0 for non-options).
--
-- event_time_fx resolution (ADR-0012):
--   1. payload.fx_rate_to_base if present (producer attached) — primary.
--   2. ASOF `fx_rates` for the row's (currency, base_currency) pair
--      at-or-before business_ts — fallback.
--   3. Otherwise NULL; kernel handles that.
--
-- ADR-0005 seam intact: reads only `portfolio_events_log` + control-plane
-- CDC tables + `fx_rates` (which itself reads `portfolio_events_log`). No
-- path to `data_log`.
--
-- v6 (ADR-0037): the envelope drops `correction_of` — every correction is
-- a re-publish under the same source_id via the UPSERT key (ADR-0006), so
-- the v5 anti-join that filtered legacy correction_of-pointing rows is
-- dead and has been removed. org_id propagates through every row so
-- downstream per-tenant logical views can `WHERE org_id = '<uuid>'`.
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS events AS
WITH typed AS (
    SELECT
        org_id,
        source_id,
        portfolio_id,
        instrument_id,
        event_type,
        business_ts,
        ingest_ts,
        payload::jsonb AS payload
    FROM portfolio_events_log
)
SELECT
    t.org_id,
    t.source_id,
    t.portfolio_id,
    t.instrument_id,
    t.event_type,
    t.business_ts,
    t.ingest_ts,
    t.payload,
    p.base_currency,
    t.payload
        || jsonb_build_object(
            'org_id',              t.org_id,
            'event_type',          t.event_type,
            'source_id',           t.source_id,
            'portfolio_id',        t.portfolio_id,
            'instrument_id',       t.instrument_id,
            'business_ts',         (extract(epoch from t.business_ts) * 1000000)::BIGINT,
            'base_currency',       p.base_currency,
            'event_time_fx', COALESCE(
                (t.payload ->> 'fx_rate_to_base')::DOUBLE PRECISION,
                fx.rate
            ),
            'instrument_kind',     i.kind,
            'contract_multiplier', i.contract_multiplier
        ) AS enriched
FROM typed t
-- v6 (Phase 7, ADR-0036): every JOIN constrains org_id so cross-tenant
-- enrichment is structurally impossible even if a portfolio_id collides
-- across orgs. portfolios + instruments PKs already lead with org_id;
-- fx_rates carries org_id explicitly per fx_rates.sql.
JOIN portfolios p USING (org_id, portfolio_id)
LEFT JOIN instruments i
    ON  i.org_id        = t.org_id
    AND i.portfolio_id  = t.portfolio_id
    AND i.instrument_id = t.instrument_id
ASOF LEFT JOIN fx_rates fx
    ON  fx.org_id   = t.org_id
    AND fx.from_ccy = t.payload ->> 'currency'
    AND fx.to_ccy   = p.base_currency
    AND t.business_ts >= fx.ts;

-- Phase 2: canonical reference tables. portfolios + instruments live in
-- control_db.public with (org_id, *) primary keys. ADR-0040 explains why
-- storage moves out of the plugin; docs/v6/02-tenant-data-topology.md
-- covers the topology.
--
-- All identifiers are UUIDv4. RisingWave's pg_cdc connector serialises
-- both columns as text on the wire; the RW-side CDC tables use VARCHAR.

CREATE TABLE portfolios (
    org_id        UUID         NOT NULL,
    portfolio_id  UUID         NOT NULL,
    base_currency VARCHAR      NOT NULL,
    attributes    JSONB        NOT NULL DEFAULT '{}'::jsonb,
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_by    VARCHAR      NOT NULL,
    PRIMARY KEY (org_id, portfolio_id)
);

CREATE TABLE instruments (
    org_id              UUID         NOT NULL,
    instrument_id       UUID         NOT NULL,
    currency            VARCHAR      NOT NULL,
    kind                VARCHAR      NOT NULL DEFAULT 'equity',
    base_currency       VARCHAR,
    sector              VARCHAR,
    subindustry         VARCHAR,
    underlying_id       UUID,
    strike              NUMERIC,
    expiry_date         DATE,
    option_type         VARCHAR,
    contract_multiplier NUMERIC      NOT NULL DEFAULT 1,
    attributes          JSONB        NOT NULL DEFAULT '{}'::jsonb,
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_by          VARCHAR      NOT NULL,
    PRIMARY KEY (org_id, instrument_id)
);

CREATE INDEX portfolios_org_idx  ON portfolios  (org_id);
CREATE INDEX instruments_org_idx ON instruments (org_id);

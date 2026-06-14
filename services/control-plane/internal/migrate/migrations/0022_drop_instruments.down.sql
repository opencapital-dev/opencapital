-- Recreate the canonical instruments table + republish for CDC. Data is not
-- restored (rollback is git revert + re-seed).
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
CREATE INDEX instruments_org_idx ON instruments (org_id);
ALTER PUBLICATION rw_v6_pub ADD TABLE instruments;

-- Phase 7+ landing flow captures base currency at org creation. Stored
-- on `organisations` so it's available before any portfolio exists; the
-- portfolio.base_currency column (0006) is per-portfolio and can override.

ALTER TABLE organisations ADD COLUMN base_currency TEXT NOT NULL DEFAULT 'USD';

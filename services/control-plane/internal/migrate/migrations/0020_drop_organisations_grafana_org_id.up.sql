-- v8: every Grafana process is single-org by construction, so the
-- per-control-plane-org -> per-grafana-org mapping that organisations.
-- grafana_org_id provided is no longer load-bearing. Drop the column.
-- See docs/grafana-desktop/design.md.

ALTER TABLE organisations DROP COLUMN IF EXISTS grafana_org_id;

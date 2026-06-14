-- Re-add the v6 grafana_org_id column. v6 onboarding seeded it with the
-- Grafana org id created by the wizard. The down path leaves it NULL on
-- existing rows; callers must repopulate manually if reverting to v6.

ALTER TABLE organisations ADD COLUMN IF NOT EXISTS grafana_org_id INTEGER UNIQUE;

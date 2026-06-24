#!/usr/bin/env bash
# Seed one portfolio_events_log trade + one data_log price, print NAV.
# Usage: RW_HOST=localhost RW_PORT=4566 bash scripts/rw_golden_nav.sh
set -euo pipefail
PSQL=(psql -h "${RW_HOST:-localhost}" -p "${RW_PORT:-4566}" -U "${RW_USER:-root}" -d "${RW_DB:-dev}" -v ON_ERROR_STOP=1 --no-psqlrc -tA)
"${PSQL[@]}" <<'SQL'
INSERT INTO portfolio_events_log (source_id,event_type,portfolio_id,instrument_id,business_ts,ingest_ts,source,plugin_id,trace_id,payload,rw_key)
VALUES ('s1','TRADE','PF1','AAPL','2024-01-01T00:00:00Z',NOW(),'golden','core','t','{"quantity":10,"price":100,"currency":"USD","direction":"BUY","base_currency":"USD"}','PF1|core|s1');
INSERT INTO data_log (source_namespace,source_id,portfolio_id,observed_at,ingest_ts,source,plugin_id,trace_id,payload,rw_key)
VALUES ('prices.ohlcv','AAPL','PF1','2024-01-02T00:00:00Z',NOW(),'golden','yf','t','{"close":110,"currency":"USD"}','PF1|yf|prices.ohlcv|AAPL|2024-01-02');
SQL
sleep 2
"${PSQL[@]}" -c "SELECT scope_id, ROUND(nav_base::numeric,2) FROM portfolio_per_tick WHERE scope_id='PF1' ORDER BY event_ts DESC LIMIT 1;"

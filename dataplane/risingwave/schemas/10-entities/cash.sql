CREATE VIEW IF NOT EXISTS e_cash AS
SELECT
    scope_id                                          AS portfolio,
    (extract(epoch from event_ts) * 1000000)::bigint  AS ts,
    currency,
    base_currency,
    balance_native,
    cash_value_base,
    realized_interest_base,
    realized_dividends_base,
    realized_fx_avg_base,
    unrealized_fx_avg_base,
    fees_base,
    deposits_cumulative_native,
    withdrawals_cumulative_native
FROM cash_per_tick;

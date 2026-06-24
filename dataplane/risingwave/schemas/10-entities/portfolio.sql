CREATE VIEW IF NOT EXISTS e_portfolio AS
SELECT
    scope_id                                          AS portfolio,
    (extract(epoch from event_ts) * 1000000)::bigint  AS ts,
    base_currency,
    nav_base,
    equity_value_base,
    cash_value_base,
    instrument_count,
    total_gross_avg_base,
    total_net_avg_base,
    realized_equity_avg_base,
    realized_forex_avg_base,
    unrealized_equity_avg_base,
    unrealized_forex_avg_base,
    realized_interest_base,
    realized_dividends_base,
    fees_base
FROM portfolio_per_tick;

DROP TABLE IF EXISTS gold_instrument_per_tick;
DROP TABLE IF EXISTS gold_portfolio_per_tick;
DROP TABLE IF EXISTS gold_cash_per_tick;
CREATE TABLE gold_instrument_per_tick AS SELECT * FROM instrument_per_tick;
CREATE TABLE gold_portfolio_per_tick  AS SELECT * FROM portfolio_per_tick;
CREATE TABLE gold_cash_per_tick       AS SELECT * FROM cash_per_tick;

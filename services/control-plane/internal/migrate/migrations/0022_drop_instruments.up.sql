-- Instruments are now event-derived in RisingWave (kind + contract_multiplier
-- ride the trade payload). Drop the canonical table + its CDC publication
-- membership. portfolios stays in rw_v6_pub (tenancy primitive).
ALTER PUBLICATION rw_v6_pub DROP TABLE instruments;
DROP TABLE IF EXISTS instruments;

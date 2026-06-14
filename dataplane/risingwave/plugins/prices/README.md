# prices plugin

The built-in plugin for the `prices.quote` and `prices.ohlcv` namespaces on
`data.v1`. Serves two purposes:

1. **Documents the payload shapes** for instrument prices (bid/ask quotes
   and OHLCV bars) in [`payload_schema.json`](payload_schema.json).
2. **Provides detail VIEWs** (`prices_quote`, `prices_ohlcv`) for callers
   that need the full namespace shape — bid/ask separately, OHLCV
   components, venue and bar cadence.

## Relationship to the core `prices` view

The unifying `prices` view (mid for quotes, close for OHLCV) lives in the
core schema (`infra/risingwave/schemas/03-unifying-views/prices.sql`),
not here. That's because the dense MtM metric MVs depend on it — moving it
into a plugin would create an apply-order dependency between plugins and
core metrics.

This plugin sits *alongside* the core view, adding detail VIEWs for
callers that want the full namespace shape. Removing this plugin doesn't
break any core MV.

## What plugin authors should learn from this

- `plugin.toml` declares the namespace this plugin owns (`prices` + the
  `prices.*` sub-namespaces).
- `migrations/V001__*.sql` ships the plugin's DDL. Numbered for replay
  ordering; idempotent on its own state.
- `payload_schema.json` documents the JSON shape inside `data.payload`
  for each sub-namespace. Advisory only — no runtime validation.
- The plugin reads from `data` filtered by `source_namespace`. It does
  not read from `portfolio_events`, `portfolio_state`, or any other
  core MV — that's allowed but isn't necessary for "extract values
  from my namespace into typed columns" use cases.

## Publishing into `prices.quote` / `prices.ohlcv`

Producers (today: yfinance-ingestor) publish to `data.v1`:

- Kafka key: `prices.quote|<instrument_id>|<observed_at_micros>` (or
  `prices.ohlcv|...`) as UTF-8 bytes.
- Avro envelope: `(source_namespace, source_id, observed_at, ingest_ts,
  source, trace_id, payload)`.
- `payload`: JSON string matching the schema in `payload_schema.json`.

Tombstone (null value) under the same Kafka key deletes the row. Useful
if a price feed needs to retract an erroneous quote.

## Why is this a plugin and not just core?

It could be either. The judgment call: by surfacing it as a plugin, we
get a worked reference for future plugin authors (Polymarket, weather,
alt-data) to copy. The cost is one indirection (the detail VIEWs live in
a plugin directory rather than core schema). The benefit is that "how do
I extend the system?" has a literal answer that future authors can
read.

## See also

- [`docs/risingwave/v2/02-plugin-guide.md`](../../../docs/risingwave/v2/02-plugin-guide.md)
  — the author-facing how-to.
- [`docs/risingwave/v2/adr/0015-plugin-schema-fragments.md`](../../../docs/risingwave/v2/adr/0015-plugin-schema-fragments.md)
  — the ADR specifying this layout.

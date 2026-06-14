package httpapi

import "time"

// Avro v2 envelopes for portfolio_events.v2 and data.v2. Field tags match the
// schemas registered in Schema Registry. The Confluent avrov2 serializer
// reflects on these tags at encode time, so they MUST stay in lockstep with
// schemas/portfolio_events.v2.avsc and schemas/data.v2.avsc (which the
// schema-registry bootstrap workstream owns).
//
// Three shape considerations:
//   - v2 envelopes carry a required `org_id` (UUID string). The gateway is
//     the only writer that can populate it correctly (from the verified JWT).
//   - portfolio_events.v2 drops `correction_of` (ADR-0037 — dead in v6).
//   - timestamp-micros logical-type fields use `time.Time`, not int64. The
//     hamba/avro encoder (used inside confluent-kafka-go's avrov2 serde)
//     rejects raw int64 for that logical type with
//     "int64 is unsupported for Avro long and logicalType timestamp-micros".

// PortfolioEventV2 is the wire shape gateway produces to portfolio_events.v2.
// PluginID is the v6 Phase 8 plugin-attribution field (ADR-0050): gateway
// reads claims.PluginID from the verified session JWT and stamps it here so
// the uninstall path can scope tombstones to events the uninstalled plugin
// authored. Nullable because pre-Phase-8 records lack the value.
type PortfolioEventV2 struct {
	OrgID        string            `avro:"org_id"`
	SourceID     string            `avro:"source_id"`
	EventType    string            `avro:"event_type"`
	PortfolioID  string            `avro:"portfolio_id"`
	InstrumentID *string           `avro:"instrument_id"`
	BusinessTs   time.Time         `avro:"business_ts"`
	IngestTs     time.Time         `avro:"ingest_ts"`
	Source       string            `avro:"source"`
	PluginID     *string           `avro:"plugin_id"`
	TraceID      *string           `avro:"trace_id"`
	Headers      map[string]string `avro:"headers"`
	Payload      string            `avro:"payload"`
}

// nullableString lifts an empty string into nil for the avro `["null",
// "string"]` union shape. JWT claims that don't carry a value (legacy
// tokens) end up as NULL in the envelope.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// DataV2 is the wire shape gateway produces to data.v2.
type DataV2 struct {
	OrgID           string    `avro:"org_id"`
	SourceNamespace string    `avro:"source_namespace"`
	SourceID        string    `avro:"source_id"`
	ObservedAt      time.Time `avro:"observed_at"`
	IngestTs        time.Time `avro:"ingest_ts"`
	Source          string    `avro:"source"`
	PluginID        *string   `avro:"plugin_id"`
	PortfolioID     *string   `avro:"portfolio_id"`
	TraceID         *string   `avro:"trace_id"`
	Payload         string    `avro:"payload"`
}

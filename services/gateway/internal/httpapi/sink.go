package httpapi

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgconn"
)

// Sink is the gateway's egress seam. The handlers build a typed envelope and
// hand it to the sink; each implementation owns its own encoding and transport.
//
//   - kafkaSink (cloud): Avro-serialize via Schema Registry, produce to Kafka.
//   - rwSink (local): typed DML straight into RisingWave over pgwire.
//
// Tombstones carry only a key (the row to delete); kafkaSink emits a null-value
// record under the upsert key, rwSink issues a DELETE by rw_key.
type Sink interface {
	PublishPortfolioEvent(ctx context.Context, key []byte, env *PortfolioEventV2) (sinkResult, error)
	PublishData(ctx context.Context, key []byte, env *DataV2) (sinkResult, error)
	Tombstone(ctx context.Context, st stream, key []byte) (sinkResult, error)
	Connected() bool
}

// stream selects which logical landing the sink op targets. It abstracts over
// "topic" (Kafka) and "table" (RisingWave), the only per-stream difference the
// handlers need to express.
type stream int

const (
	streamPortfolioEvents stream = iota
	streamData
)

// sinkResult is the transport-agnostic outcome of a write. Topic carries the
// Kafka topic for kafkaSink and the table name for rwSink; Partition/Offset are
// Kafka-only and stay zero for rwSink.
type sinkResult struct {
	Topic     string
	Partition int32
	Offset    int64
}

// --- kafkaSink (cloud) -----------------------------------------------------

type kafkaSink struct {
	ser       serializer
	prod      producer
	peTopic   string
	dataTopic string
}

// NewKafkaSink builds the cloud sink: Avro-serialize via ser, produce via prod.
func NewKafkaSink(ser serializer, prod producer, peTopic, dataTopic string) Sink {
	return &kafkaSink{ser: ser, prod: prod, peTopic: peTopic, dataTopic: dataTopic}
}

func (k *kafkaSink) topicFor(st stream) string {
	if st == streamPortfolioEvents {
		return k.peTopic
	}
	return k.dataTopic
}

func (k *kafkaSink) PublishPortfolioEvent(ctx context.Context, key []byte, env *PortfolioEventV2) (sinkResult, error) {
	value, err := k.ser.PortfolioEvents(k.peTopic, env)
	if err != nil {
		return sinkResult{}, fmt.Errorf("serialize portfolio_events: %w", err)
	}
	return k.produce(ctx, k.peTopic, key, value)
}

func (k *kafkaSink) PublishData(ctx context.Context, key []byte, env *DataV2) (sinkResult, error) {
	value, err := k.ser.Data(k.dataTopic, env)
	if err != nil {
		return sinkResult{}, fmt.Errorf("serialize data: %w", err)
	}
	return k.produce(ctx, k.dataTopic, key, value)
}

func (k *kafkaSink) Tombstone(ctx context.Context, st stream, key []byte) (sinkResult, error) {
	return k.produce(ctx, k.topicFor(st), key, nil)
}

func (k *kafkaSink) produce(ctx context.Context, topic string, key, value []byte) (sinkResult, error) {
	r, err := k.prod.Produce(ctx, topic, key, value)
	if err != nil {
		return sinkResult{}, err
	}
	return sinkResult{Topic: r.Topic, Partition: r.Partition, Offset: r.Offset}, nil
}

func (k *kafkaSink) Connected() bool { return k.prod.Connected() }

// compile-time assertion that kafkaSink satisfies Sink.
var _ Sink = (*kafkaSink)(nil)

// --- rwSink (local) --------------------------------------------------------

// pgExecer is the narrow pgx surface rwSink needs. *pgxpool.Pool satisfies it;
// tests inject a capturing fake.
type pgExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// rwSink writes events straight into RisingWave's landing tables over pgwire.
// An INSERT with an existing rw_key overwrites (RW's native upsert-on-PK); a
// DELETE by rw_key retracts through every downstream MV — reproducing the cloud
// FORMAT UPSERT + null-value tombstone contract with no normalization layer.
type rwSink struct {
	db        pgExecer
	peTable   string
	dataTable string
	connected atomic.Bool
}

// NewRWSink builds the local sink: typed INSERT/DELETE into RisingWave over the
// given pgx execer (a *pgxpool.Pool in production).
func NewRWSink(db pgExecer, peTable, dataTable string) Sink {
	r := &rwSink{db: db, peTable: peTable, dataTable: dataTable}
	// Optimistic: pgxpool manages reconnection; a failed write flips this
	// false and a later success flips it back, mirroring kafkaSink's signal.
	r.connected.Store(true)
	return r
}

func (r *rwSink) tableFor(st stream) string {
	if st == streamPortfolioEvents {
		return r.peTable
	}
	return r.dataTable
}

func (r *rwSink) PublishPortfolioEvent(ctx context.Context, key []byte, env *PortfolioEventV2) (sinkResult, error) {
	q := fmt.Sprintf(`INSERT INTO %s `+
		`(org_id, source_id, event_type, portfolio_id, instrument_id, business_ts, ingest_ts, source, plugin_id, trace_id, payload, rw_key) `+
		`VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, r.peTable)
	return r.exec(ctx, r.peTable, q,
		env.OrgID, env.SourceID, env.EventType, env.PortfolioID, env.InstrumentID,
		env.BusinessTs, env.IngestTs, env.Source, env.PluginID, env.TraceID, env.Payload, string(key))
}

func (r *rwSink) PublishData(ctx context.Context, key []byte, env *DataV2) (sinkResult, error) {
	q := fmt.Sprintf(`INSERT INTO %s `+
		`(org_id, source_namespace, source_id, portfolio_id, observed_at, ingest_ts, source, plugin_id, trace_id, payload, rw_key) `+
		`VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, r.dataTable)
	return r.exec(ctx, r.dataTable, q,
		env.OrgID, env.SourceNamespace, env.SourceID, env.PortfolioID, env.ObservedAt,
		env.IngestTs, env.Source, env.PluginID, env.TraceID, env.Payload, string(key))
}

func (r *rwSink) Tombstone(ctx context.Context, st stream, key []byte) (sinkResult, error) {
	table := r.tableFor(st)
	return r.exec(ctx, table, fmt.Sprintf(`DELETE FROM %s WHERE rw_key = $1`, table), string(key))
}

func (r *rwSink) exec(ctx context.Context, table, sql string, args ...any) (sinkResult, error) {
	if _, err := r.db.Exec(ctx, sql, args...); err != nil {
		r.connected.Store(false)
		return sinkResult{}, err
	}
	r.connected.Store(true)
	return sinkResult{Topic: table}, nil
}

func (r *rwSink) Connected() bool { return r.connected.Load() }

// compile-time assertion that rwSink satisfies Sink.
var _ Sink = (*rwSink)(nil)

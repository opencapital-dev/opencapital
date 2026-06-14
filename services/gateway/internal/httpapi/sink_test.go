package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ---- kafkaSink ------------------------------------------------------------

func TestKafkaSink_PublishPortfolioEvent_SerializesAndProduces(t *testing.T) {
	fp := &fakeProducer{}
	k := NewKafkaSink(fakeSerializer{}, fp, "portfolio_events.v2", "data.v2")

	res, err := k.PublishPortfolioEvent(context.Background(), []byte("k1"), &PortfolioEventV2{OrgID: "org", EventType: "TRADE"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Topic != "portfolio_events.v2" {
		t.Fatalf("result topic = %q, want portfolio_events.v2", res.Topic)
	}
	got := fp.produced()
	if len(got) != 1 {
		t.Fatalf("produces = %d, want 1", len(got))
	}
	if got[0].Topic != "portfolio_events.v2" {
		t.Fatalf("produced topic = %q", got[0].Topic)
	}
	if string(got[0].Key) != "k1" {
		t.Fatalf("produced key = %q, want k1", got[0].Key)
	}
	if got[0].Value == nil {
		t.Fatalf("portfolio-event publish must carry a serialized (non-nil) value")
	}
}

func TestKafkaSink_PublishData_UsesDataTopic(t *testing.T) {
	fp := &fakeProducer{}
	k := NewKafkaSink(fakeSerializer{}, fp, "portfolio_events.v2", "data.v2")

	res, err := k.PublishData(context.Background(), []byte("dk"), &DataV2{OrgID: "org"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Topic != "data.v2" {
		t.Fatalf("result topic = %q, want data.v2", res.Topic)
	}
	got := fp.produced()
	if len(got) != 1 || got[0].Topic != "data.v2" {
		t.Fatalf("bad produce %+v", got)
	}
	if string(got[0].Key) != "dk" {
		t.Fatalf("produced key = %q, want dk", got[0].Key)
	}
}

func TestKafkaSink_Tombstone_NilValueOnStreamTopic(t *testing.T) {
	fp := &fakeProducer{}
	k := NewKafkaSink(fakeSerializer{}, fp, "portfolio_events.v2", "data.v2")

	if _, err := k.Tombstone(context.Background(), streamPortfolioEvents, []byte("tk")); err != nil {
		t.Fatalf("err: %v", err)
	}
	got := fp.produced()
	if len(got) != 1 {
		t.Fatalf("produces = %d, want 1", len(got))
	}
	if got[0].Topic != "portfolio_events.v2" {
		t.Fatalf("tombstone topic = %q, want portfolio_events.v2", got[0].Topic)
	}
	if got[0].Value != nil {
		t.Fatalf("tombstone value must be nil, got %q", got[0].Value)
	}
}

// ---- rwSink ---------------------------------------------------------------

type execCall struct {
	sql  string
	args []any
}

type fakeExecer struct {
	calls []execCall
	err   error
}

func (f *fakeExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.calls = append(f.calls, execCall{sql: sql, args: args})
	return pgconn.CommandTag{}, f.err
}

func TestRWSink_PublishPortfolioEvent_InsertsTypedColumns(t *testing.T) {
	fe := &fakeExecer{}
	r := NewRWSink(fe, "portfolio_events_log", "data_log")
	ts := time.UnixMicro(1)

	res, err := r.PublishPortfolioEvent(context.Background(), []byte("org|plugin|s"), &PortfolioEventV2{
		OrgID: "org", SourceID: "s", EventType: "TRADE", PortfolioID: "pf",
		BusinessTs: ts, IngestTs: ts, Source: "gateway@test", Payload: "{}",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Topic != "portfolio_events_log" {
		t.Fatalf("result topic = %q, want portfolio_events_log", res.Topic)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(fe.calls))
	}
	c := fe.calls[0]
	if !strings.Contains(c.sql, "INSERT INTO portfolio_events_log") {
		t.Fatalf("sql = %q", c.sql)
	}
	if len(c.args) != 12 {
		t.Fatalf("args = %d, want 12", len(c.args))
	}
	// Column order: org_id, source_id, event_type, portfolio_id, instrument_id,
	// business_ts, ingest_ts, source, plugin_id, trace_id, payload, rw_key.
	if c.args[0] != "org" || c.args[1] != "s" || c.args[2] != "TRADE" || c.args[3] != "pf" {
		t.Fatalf("envelope args = %v", c.args[:4])
	}
	if c.args[10] != "{}" {
		t.Fatalf("payload arg = %v, want {}", c.args[10])
	}
	if c.args[11] != "org|plugin|s" {
		t.Fatalf("rw_key arg = %v, want org|plugin|s", c.args[11])
	}
}

func TestRWSink_PublishData_InsertsIntoDataTable(t *testing.T) {
	fe := &fakeExecer{}
	r := NewRWSink(fe, "portfolio_events_log", "data_log")

	res, err := r.PublishData(context.Background(), []byte("dk"), &DataV2{
		OrgID: "org", SourceNamespace: "prices.ohlcv", SourceID: "AAPL", Payload: "{}",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Topic != "data_log" {
		t.Fatalf("result topic = %q, want data_log", res.Topic)
	}
	c := fe.calls[0]
	if !strings.Contains(c.sql, "INSERT INTO data_log") {
		t.Fatalf("sql = %q", c.sql)
	}
	if len(c.args) != 11 {
		t.Fatalf("args = %d, want 11", len(c.args))
	}
	if c.args[0] != "org" || c.args[1] != "prices.ohlcv" || c.args[2] != "AAPL" {
		t.Fatalf("envelope args = %v", c.args[:3])
	}
	if c.args[10] != "dk" {
		t.Fatalf("rw_key arg = %v, want dk", c.args[10])
	}
}

func TestRWSink_Tombstone_DeletesByRWKeyOnStreamTable(t *testing.T) {
	feData := &fakeExecer{}
	rData := NewRWSink(feData, "portfolio_events_log", "data_log")
	if _, err := rData.Tombstone(context.Background(), streamData, []byte("dk")); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(feData.calls[0].sql, "DELETE FROM data_log WHERE rw_key") {
		t.Fatalf("sql = %q", feData.calls[0].sql)
	}
	if feData.calls[0].args[0] != "dk" {
		t.Fatalf("delete arg = %v, want dk", feData.calls[0].args[0])
	}

	fePE := &fakeExecer{}
	rPE := NewRWSink(fePE, "portfolio_events_log", "data_log")
	if _, err := rPE.Tombstone(context.Background(), streamPortfolioEvents, []byte("ek")); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(fePE.calls[0].sql, "DELETE FROM portfolio_events_log WHERE rw_key") {
		t.Fatalf("sql = %q", fePE.calls[0].sql)
	}
}

func TestRWSink_Connected_TracksWriteOutcome(t *testing.T) {
	fe := &fakeExecer{}
	r := NewRWSink(fe, "portfolio_events_log", "data_log")
	if !r.Connected() {
		t.Fatalf("rwSink should start connected (optimistic)")
	}
	fe.err = context.DeadlineExceeded
	if _, err := r.PublishData(context.Background(), []byte("dk"), &DataV2{}); err == nil {
		t.Fatalf("expected error from failing exec")
	}
	if r.Connected() {
		t.Fatalf("rwSink must report disconnected after a failed write")
	}
}

// Package kafka wraps the Confluent producer with the SASL_PLAINTEXT +
// SCRAM-SHA-256 settings the v6 broker enforces, plus the idempotent-
// produce flags so a retry on transient broker error doesn't double-
// publish.
//
// The gateway is the sole principal with produce ACLs on portfolio_events.v2
// and data.v2 (ADR-0038). The plugin process never reaches this package — it
// HTTP-POSTs to the gateway instead.
package kafka

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
)

// Config holds the broker bootstrap + SASL credentials. SASL_PLAINTEXT
// is fine on the internal compose `platform` network: bridge has no
// wire, containers don't get CAP_NET_RAW, so the SCRAM exchange is not
// observable from another container.
type Config struct {
	Brokers       string
	SASLMechanism string // "SCRAM-SHA-256"
	SASLUsername  string
	SASLPassword  string
	ClientID      string
}

// Producer wraps a Confluent producer + a delivery channel pool. Synchronous
// per-Produce: callers wait for the broker ack before HTTP returns 201.
type Producer struct {
	p         *kafka.Producer
	connected atomic.Bool
}

// New constructs the Producer and starts a goroutine draining its event
// channel so error events surface to logs even when no Produce is in flight.
func New(cfg Config) (*Producer, error) {
	cm := &kafka.ConfigMap{
		"bootstrap.servers":       cfg.Brokers,
		"client.id":               cfg.ClientID,
		"enable.idempotence":      true,
		"acks":                    "all",
		"compression.type":        "lz4",
		"linger.ms":               5,
		"socket.keepalive.enable": true,
		"security.protocol":       "SASL_PLAINTEXT",
		"sasl.mechanism":          cfg.SASLMechanism,
		"sasl.username":           cfg.SASLUsername,
		"sasl.password":           cfg.SASLPassword,
	}
	p, err := kafka.NewProducer(cm)
	if err != nil {
		return nil, fmt.Errorf("kafka producer: %w", err)
	}
	prod := &Producer{p: p}
	go prod.drain()
	return prod, nil
}

// drain consumes the producer's background event channel. The interesting
// events at this layer are broker connection state and error events; per-
// message delivery acks come back on the per-Produce delivery channel.
func (p *Producer) drain() {
	for ev := range p.p.Events() {
		switch e := ev.(type) {
		case kafka.Error:
			// Broker-level transport errors mark the producer not-connected.
			// We rely on connected returning true again once a Produce
			// succeeds — there's no positive "connected" event we can pin
			// to in librdkafka.
			if e.IsFatal() {
				p.connected.Store(false)
			}
		}
	}
}

// Connected returns true if a Produce has succeeded recently. The gateway's
// /readyz uses this as one of its three preconditions.
func (p *Producer) Connected() bool { return p.connected.Load() }

// Result is the slice returned to HTTP callers in the 201 body.
type Result struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Offset    int64  `json:"offset"`
}

// Produce sends one message synchronously: it allocates a delivery channel,
// hands the message to librdkafka, and blocks until the broker acks or ctx
// expires. Returns the topic/partition/offset triple for the response body.
func (p *Producer) Produce(ctx context.Context, topic string, key, value []byte) (Result, error) {
	deliveryChan := make(chan kafka.Event, 1)
	defer close(deliveryChan)

	msg := &kafka.Message{
		TopicPartition: kafka.TopicPartition{Topic: &topic, Partition: kafka.PartitionAny},
		Key:            key,
		Value:          value,
		Timestamp:      time.Now(),
	}
	if err := p.p.Produce(msg, deliveryChan); err != nil {
		return Result{}, fmt.Errorf("produce: %w", err)
	}

	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case ev := <-deliveryChan:
		m, ok := ev.(*kafka.Message)
		if !ok {
			return Result{}, fmt.Errorf("unexpected delivery event %T", ev)
		}
		if m.TopicPartition.Error != nil {
			return Result{}, fmt.Errorf("delivery: %w", m.TopicPartition.Error)
		}
		p.connected.Store(true)
		return Result{
			Topic:     *m.TopicPartition.Topic,
			Partition: m.TopicPartition.Partition,
			Offset:    int64(m.TopicPartition.Offset),
		}, nil
	}
}

// Close flushes outstanding messages and shuts the producer down. Called from
// the main shutdown path.
func (p *Producer) Close() {
	if p == nil || p.p == nil {
		return
	}
	p.p.Flush(5000)
	p.p.Close()
}

// ErrNotConnected is returned by /readyz when the producer has not yet
// reported a successful Produce. Keeps the call site short.
var ErrNotConnected = errors.New("kafka producer not connected")

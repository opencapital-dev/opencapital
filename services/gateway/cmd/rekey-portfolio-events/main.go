// Command rekey-portfolio-events is a one-shot migration that rewrites the
// portfolio_events.v2 topic onto the new canonical Kafka key.
//
// Background: the canonical key for portfolio_events.v2 changed from a bare
// source_id (P1.2/P1.3) to datakey.EventKey(org_id, plugin_id, source_id).
// Events already on the topic carry the OLD key, so the new gateway can no
// longer address them — a correction or tombstone built under the new key
// scheme would land under a different key and never compact the old row.
// Events are user-entered (not re-fetchable from any upstream), so we RE-KEY
// in place rather than wipe.
//
// Mechanism (Kafka-native re-key): consume the topic from the beginning to the
// high-water mark captured at start. For each live (non-tombstone, non-seed)
// record:
//
//	1. Decode the Avro VALUE just enough to read org_id, plugin_id, source_id.
//	2. Produce (newKey, originalValueBytes) — the value bytes pass through
//	   UNCHANGED so the exact Avro payload (magic byte + schema id + body) is
//	   preserved; we never re-serialize.
//	3. Produce (oldKey, nil) — a tombstone under the original key so log
//	   compaction drops the stale-keyed row.
//
// Stop condition: each partition is read only up to the high-water mark we
// captured at start, so the records THIS run produces are never re-consumed
// (no infinite loop). Existing tombstones (nil value) and the __seed__ warmup
// record are skipped — neither needs a new key.
//
// Consumer auth: the `gateway` SASL principal has read+describe+write on
// portfolio_events.v2 but NO consumer-group ACL (that is reserved for
// rw_kafka). So we consume with MANUAL partition assignment (Assign, not
// Subscribe) and auto-commit disabled — librdkafka then never issues
// JoinGroup/OffsetCommit, so no Group ACL is needed. The same principal both
// reads and produces, matching the topics-seed pattern.
//
// Config is env-driven (with --dry-run / --topic flags), reusing the gateway's
// KAFKA_* and SCHEMA_REGISTRY_* / SR_* variables so the same compose secrets
// work unchanged.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/confluentinc/confluent-kafka-go/v2/schemaregistry"
	"github.com/confluentinc/confluent-kafka-go/v2/schemaregistry/serde"
	"github.com/confluentinc/confluent-kafka-go/v2/schemaregistry/serde/avrov2"

	"github.com/portfolio-management/datakey"
)

// seedSentinel marks the per-partition warmup record produced by topics-seed.
// The seed envelope carries org_id == "__seed__" (and source_id "__seed__<p>");
// downstream MVs already filter it out, and it must not be re-keyed.
const seedSentinel = "__seed__"

// portfolioEventValue is the decode-only projection of the portfolio_events.v2
// Avro envelope. Only the three key components are needed; the rest of the
// schema's fields are skipped by the Avro reader because they are absent from
// this struct. The avro tags must match schemas/portfolio_events.v2.avsc.
type portfolioEventValue struct {
	OrgID    string  `avro:"org_id"`
	SourceID string  `avro:"source_id"`
	PluginID *string `avro:"plugin_id"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("rekey failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dryRun := flag.Bool("dry-run", false, "decode and compute new keys, log a sample + counts, but produce nothing")
	topic := flag.String("topic", envOr("PORTFOLIO_EVENTS_TOPIC", "portfolio_events.v2"), "topic to re-key")
	pollMs := flag.Int("poll-ms", 5000, "per-record poll timeout in milliseconds (also the idle cutoff)")
	flag.Parse()

	brokers := os.Getenv("KAFKA_BROKERS")
	if brokers == "" {
		return fmt.Errorf("KAFKA_BROKERS is required")
	}
	saslUser := envOr("KAFKA_SASL_USERNAME", "gateway")
	saslMech := envOr("KAFKA_SASL_MECHANISM", "SCRAM-SHA-256")
	saslPass, err := envOrFile("KAFKA_SASL_PASSWORD")
	if err != nil {
		return fmt.Errorf("KAFKA_SASL_PASSWORD: %w", err)
	}
	if saslPass == "" {
		return fmt.Errorf("KAFKA_SASL_PASSWORD (or _FILE) is required")
	}
	srURL := os.Getenv("SCHEMA_REGISTRY_URL")
	if srURL == "" {
		return fmt.Errorf("SCHEMA_REGISTRY_URL is required")
	}
	srUser := envOr("SR_USERNAME", "sr-gateway")
	srPass, err := envOrFile("SR_PASSWORD")
	if err != nil {
		return fmt.Errorf("SR_PASSWORD: %w", err)
	}

	logger.Info("rekey starting",
		"topic", *topic, "brokers", brokers, "sasl_user", saslUser,
		"sr_url", srURL, "dry_run", *dryRun)

	deser, err := newDeserializer(srURL, srUser, srPass)
	if err != nil {
		return fmt.Errorf("schema-registry deserializer: %w", err)
	}

	// Manual-assignment consumer. group.id is required by librdkafka to
	// construct the handle, but with Assign (not Subscribe) and auto-commit
	// disabled we never join a group, so no Group ACL is consumed.
	consumer, err := kafka.NewConsumer(&kafka.ConfigMap{
		"bootstrap.servers":    brokers,
		"security.protocol":    "SASL_PLAINTEXT",
		"sasl.mechanism":       saslMech,
		"sasl.username":        saslUser,
		"sasl.password":        saslPass,
		"group.id":             "rekey-portfolio-events-oneshot",
		"enable.auto.commit":   false,
		"auto.offset.reset":    "earliest",
		"client.id":            "rekey-portfolio-events",
		"enable.partition.eof": true,
	})
	if err != nil {
		return fmt.Errorf("kafka consumer: %w", err)
	}
	defer consumer.Close()

	// Discover partitions, then capture each partition's high-water mark at
	// start. The walk stops at these marks so records produced by this run are
	// never re-consumed.
	parts, err := partitionsFor(consumer, *topic)
	if err != nil {
		return fmt.Errorf("partition metadata: %w", err)
	}

	type bound struct {
		low, high int64
	}
	bounds := make(map[int32]bound, len(parts))
	assign := make([]kafka.TopicPartition, 0, len(parts))
	var totalAtStart int64
	for _, p := range parts {
		low, high, werr := consumer.QueryWatermarkOffsets(*topic, p, 10000)
		if werr != nil {
			return fmt.Errorf("watermarks p%d: %w", p, werr)
		}
		bounds[p] = bound{low: low, high: high}
		totalAtStart += high - low
		t := *topic
		assign = append(assign, kafka.TopicPartition{
			Topic:     &t,
			Partition: p,
			Offset:    kafka.Offset(low),
		})
		logger.Info("partition bound", "partition", p, "low", low, "high", high)
	}
	logger.Info("captured high-water marks", "partitions", len(parts), "records_to_scan", totalAtStart)

	if err := consumer.Assign(assign); err != nil {
		return fmt.Errorf("assign: %w", err)
	}

	var producer *kafka.Producer
	if !*dryRun {
		producer, err = kafka.NewProducer(&kafka.ConfigMap{
			"bootstrap.servers":  brokers,
			"security.protocol":  "SASL_PLAINTEXT",
			"sasl.mechanism":     saslMech,
			"sasl.username":      saslUser,
			"sasl.password":      saslPass,
			"enable.idempotence": true,
			"acks":               "all",
			"compression.type":   "lz4",
			"linger.ms":          5,
			"client.id":          "rekey-portfolio-events",
		})
		if err != nil {
			return fmt.Errorf("kafka producer: %w", err)
		}
		defer producer.Close()
	}

	// done tracks which partitions have reached their captured high-water mark.
	done := make(map[int32]bool, len(parts))
	remaining := func() bool {
		for _, p := range parts {
			b := bounds[p]
			if b.high > b.low && !done[p] {
				return true
			}
		}
		return false
	}

	var (
		scanned    int64
		rekeyed    int64
		tombstones int64 // existing nil-value records skipped
		seeds      int64
		samples    int
	)

	for remaining() {
		ev := consumer.Poll(*pollMs)
		if ev == nil {
			logger.Warn("poll idle timeout with partitions still open; stopping",
				"poll_ms", *pollMs)
			break
		}
		switch e := ev.(type) {
		case *kafka.Message:
			p := e.TopicPartition.Partition
			off := int64(e.TopicPartition.Offset)
			b := bounds[p]
			if off >= b.high {
				done[p] = true
				continue
			}
			scanned++

			// Existing tombstone: leave it; do not re-emit a new key for it.
			if e.Value == nil {
				tombstones++
			} else {
				var val portfolioEventValue
				if derr := deser.DeserializeInto(*topic, e.Value, &val); derr != nil {
					return fmt.Errorf("decode p%d off%d: %w", p, off, derr)
				}
				if val.OrgID == seedSentinel || strings.HasPrefix(val.SourceID, seedSentinel) {
					seeds++
				} else {
					pluginID := ""
					if val.PluginID != nil {
						pluginID = *val.PluginID
					}
					newKey := datakey.EventKey(val.OrgID, pluginID, val.SourceID)
					if samples < 10 {
						logger.Info("rekey sample",
							"partition", p, "offset", off,
							"old_key", string(e.Key), "new_key", string(newKey))
						samples++
					}
					if !*dryRun {
						if perr := produce(producer, *topic, p, newKey, e.Value); perr != nil {
							return fmt.Errorf("produce new-key p%d off%d: %w", p, off, perr)
						}
						if perr := produce(producer, *topic, p, e.Key, nil); perr != nil {
							return fmt.Errorf("produce old-tombstone p%d off%d: %w", p, off, perr)
						}
					}
					rekeyed++
				}
			}

			if off+1 >= b.high {
				done[p] = true
			}

		case kafka.PartitionEOF:
			// Reached the live tail of a partition before its captured
			// high-water mark (possible only if the topic was empty there).
			done[e.Partition] = true

		case kafka.Error:
			if e.IsFatal() {
				return fmt.Errorf("kafka fatal: %w", e)
			}
			logger.Warn("kafka event", "err", e)
		}
	}

	if !*dryRun {
		if n := producer.Flush(15000); n > 0 {
			return fmt.Errorf("producer flush left %d messages undelivered", n)
		}
	}

	logger.Info("rekey complete",
		"dry_run", *dryRun,
		"scanned", scanned,
		"rekeyed", rekeyed,
		"existing_tombstones_skipped", tombstones,
		"seed_records_skipped", seeds,
		"new_records_produced", produceCount(*dryRun, rekeyed))
	if *dryRun {
		logger.Info("dry-run: nothing was produced; re-run without --dry-run to apply")
	}
	return nil
}

// produceCount reports how many records a real run wrote: two per re-keyed
// event (new-key value + old-key tombstone). Zero in dry-run.
func produceCount(dryRun bool, rekeyed int64) int64 {
	if dryRun {
		return 0
	}
	return rekeyed * 2
}

// produce sends one record synchronously to the same partition the source
// record came from, so the new-key event and its tombstone land deterministically
// and key-based compaction behaves predictably. Blocks on the delivery report.
func produce(p *kafka.Producer, topic string, partition int32, key, value []byte) error {
	delivery := make(chan kafka.Event, 1)
	t := topic
	msg := &kafka.Message{
		TopicPartition: kafka.TopicPartition{Topic: &t, Partition: partition},
		Key:            key,
		Value:          value,
	}
	if err := p.Produce(msg, delivery); err != nil {
		return err
	}
	ev := <-delivery
	m, ok := ev.(*kafka.Message)
	if !ok {
		return fmt.Errorf("unexpected delivery event %T", ev)
	}
	return m.TopicPartition.Error
}

// partitionsFor returns the partition ids of topic from broker metadata.
func partitionsFor(c *kafka.Consumer, topic string) ([]int32, error) {
	md, err := c.GetMetadata(&topic, false, 10000)
	if err != nil {
		return nil, err
	}
	t, ok := md.Topics[topic]
	if !ok {
		return nil, fmt.Errorf("topic %q not found in metadata", topic)
	}
	if t.Error.Code() != kafka.ErrNoError {
		return nil, fmt.Errorf("topic %q metadata error: %w", topic, t.Error)
	}
	ids := make([]int32, 0, len(t.Partitions))
	for _, pt := range t.Partitions {
		ids = append(ids, pt.ID)
	}
	return ids, nil
}

// newDeserializer builds an SR Avro deserializer with the same TopicNameStrategy
// the gateway's serializer uses (subject = <topic>-value), so the decode resolves
// the portfolio_events.v2-value schema by topic name.
func newDeserializer(url, user, pass string) (*avrov2.Deserializer, error) {
	srCfg := schemaregistry.NewConfigWithBasicAuthentication(url, user, pass)
	client, err := schemaregistry.NewClient(srCfg)
	if err != nil {
		return nil, fmt.Errorf("schema-registry client: %w", err)
	}
	dCfg := avrov2.NewDeserializerConfig()
	dCfg.SubjectNameStrategyType = serde.TopicNameStrategyType
	return avrov2.NewDeserializer(client, serde.ValueSerde, dCfg)
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// envOrFile reads ${KEY}, else the file at ${KEY}_FILE (the compose-secrets
// pattern). Mirrors the gateway's config loader so the same env wiring works.
func envOrFile(key string) (string, error) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v, nil
	}
	path := os.Getenv(key + "_FILE")
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

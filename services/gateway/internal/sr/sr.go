// Package sr wraps the Confluent Schema Registry Avro v2 serializer with the
// flag set the v6 gateway needs: UseLatestVersion=true (gateway never invents
// schemas at runtime), AutoRegisterSchemas=false (Schema Registry only
// accepts writes from the redpanda-bootstrap one-shot, never from the gateway
// principal), and TopicNameStrategy (matches the v1 plugin producer
// convention so subjects stay <topic>-value).
//
// Auth: SR runs `authentication_method = none` on the internal platform
// network. No credentials passed in.
package sr

import (
	"fmt"

	"github.com/confluentinc/confluent-kafka-go/v2/schemaregistry"
	"github.com/confluentinc/confluent-kafka-go/v2/schemaregistry/serde"
	"github.com/confluentinc/confluent-kafka-go/v2/schemaregistry/serde/avrov2"
)

// Serializer holds two value-serdes — one per produce topic. Both share the
// same underlying Schema Registry client, so a single HTTP round-trip per
// (topic, schema-version) is amortised across all envelope encodes for that
// topic.
type Serializer struct {
	client            schemaregistry.Client
	portfolioEventsSr *avrov2.Serializer
	dataSr            *avrov2.Serializer
}

// Config describes the SR endpoint and the produce topic names. Topic names
// drive subject lookup via TopicNameStrategy: subject = <topic>-value.
type Config struct {
	URL                  string
	BasicAuthUser        string
	BasicAuthPass        string
	PortfolioEventsTopic string
	DataTopic            string
}

// New returns a Serializer wired against the supplied SR endpoint.
func New(cfg Config) (*Serializer, error) {
	srCfg := schemaregistry.NewConfigWithBasicAuthentication(cfg.URL, cfg.BasicAuthUser, cfg.BasicAuthPass)
	client, err := schemaregistry.NewClient(srCfg)
	if err != nil {
		return nil, fmt.Errorf("schema-registry client: %w", err)
	}

	serCfg := avrov2.NewSerializerConfig()
	serCfg.AutoRegisterSchemas = false
	serCfg.UseLatestVersion = true
	serCfg.NormalizeSchemas = true
	// Force TopicNameStrategy. The default coerces to AssociatedNameStrategy
	// which hits Schema Registry's associations endpoint — Redpanda doesn't
	// implement it. Topic-name strategy reduces subject lookup to a string
	// concat (matches the plugin producer behaviour pre-v6).
	serCfg.SubjectNameStrategyType = serde.TopicNameStrategyType

	peSer, err := avrov2.NewSerializer(client, serde.ValueSerde, serCfg)
	if err != nil {
		return nil, fmt.Errorf("avro serializer (portfolio_events): %w", err)
	}
	dataSer, err := avrov2.NewSerializer(client, serde.ValueSerde, serCfg)
	if err != nil {
		return nil, fmt.Errorf("avro serializer (data): %w", err)
	}

	return &Serializer{client: client, portfolioEventsSr: peSer, dataSr: dataSer}, nil
}

// PortfolioEventsTopicSerializer encodes a portfolio_events.v2 envelope under
// the configured topic name. The envelope struct must carry `avro:"..."` tags
// matching schemas/portfolio_events.v2.avsc.
func (s *Serializer) PortfolioEvents(topic string, env any) ([]byte, error) {
	return s.portfolioEventsSr.Serialize(topic, env)
}

// Data encodes a data.v2 envelope. Same convention as PortfolioEvents.
func (s *Serializer) Data(topic string, env any) ([]byte, error) {
	return s.dataSr.Serialize(topic, env)
}

// Package config loads environment-driven configuration for the v6 gateway.
//
// The gateway is the sole Kafka producer in v6 (ADR-0038): every plugin write
// passes through it, the JWT-verified org_id is injected into the Avro v2
// envelope, and produce happens under the dedicated `gateway` SASL principal.
//
// All knobs are env-var driven so the same binary works under docker-compose,
// fly.io, or any other host. No flags, no config files.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is the parsed runtime configuration.
type Config struct {
	ListenAddr string

	// Control-plane integration.
	ControlPlaneURL string // base URL; JWKS fetched from "$URL/jwt/jwks"
	JWTIssuer       string
	JWTAudience     string
	JWKSRefresh     time.Duration

	// Postgres (control_db, gateway_ro role with SELECT on portfolios
	// only). v6 Phase 6 points the gateway exclusively at the streaming
	// standby (postgres-replica). The primary is reserved for
	// control-plane DML, the rw_replicator CDC slot, and DDL during
	// plugin install (ADR-0034).
	ControlDBReplicaDSN string

	// LRUPrimeToken is the shared secret the control plane stamps into
	// the X-Lru-Prime-Token header on /internal/lru-prime; the gateway
	// constant-time-compares incoming calls against it. Loaded from
	// LRU_PRIME_TOKEN or LRU_PRIME_TOKEN_FILE. An empty value hard-
	// disables the endpoint (every call returns 401 without comparing
	// tokens).
	LRUPrimeToken string

	// TombstoneJWTAudience is the `aud` claim the gateway requires on
	// the capability JWT presented at /internal/tombstone (ADR-0050).
	// Differentiates from session JWTs (aud=gateway) so a stolen
	// session token can't be replayed against the destructive endpoint.
	// Verified via the existing control-plane JWKS the gateway already
	// polls. Empty value hard-disables the endpoint.
	TombstoneJWTAudience string

	// Kafka / Redpanda. SASL_PLAINTEXT + SCRAM-SHA-256 on the internal
	// listener. Defense-in-depth on the internal compose `platform`
	// network is marginal (no wire, no CAP_NET_RAW); auth alone is
	// the gate.
	KafkaBrokers       string
	KafkaSASLUsername  string
	KafkaSASLPassword  string
	KafkaSASLMechanism string // "SCRAM-SHA-256"; surfaced for ops parity.

	// Schema Registry. HTTP Basic Auth as sr-gateway (read-only on
	// schemas). Same `platform` network threat model.
	SchemaRegistryURL string
	SRBasicAuthUser   string
	SRBasicAuthPass   string

	// Topic names. v6 topics ship the v2 envelope; v1 topics are never produced.
	PortfolioEventsTopic string
	DataTopic            string

	// Source identifier injected into the Avro envelope `source` field.
	SourceID string

	// SinkMode selects the egress: "kafka" (cloud — Avro -> Redpanda) or "rw"
	// (local — typed DML straight into RisingWave over pgwire). Defaults to
	// "kafka". The Kafka/SR knobs above are required only in kafka mode; the
	// RW knobs below only in rw mode.
	SinkMode string

	// RisingWave egress (SinkMode=="rw"). RWDSN is the pgwire DSN; the two
	// table names are the connector-less landing tables the gateway writes.
	RWDSN                  string
	RWPortfolioEventsTable string
	RWDataTable            string

	LogLevel string
}

// Load reads env vars and validates them. Returns the populated config or an
// error if any required value is missing or malformed.
func Load() (Config, error) {
	lruPrimeTok, err := envOrFile("LRU_PRIME_TOKEN")
	if err != nil {
		return Config{}, fmt.Errorf("LRU_PRIME_TOKEN: %w", err)
	}
	srPass, err := envOrFile("SR_PASSWORD")
	if err != nil {
		return Config{}, fmt.Errorf("SR_PASSWORD: %w", err)
	}
	kafkaPass, err := envOrFile("KAFKA_SASL_PASSWORD")
	if err != nil {
		return Config{}, fmt.Errorf("KAFKA_SASL_PASSWORD: %w", err)
	}

	cfg := Config{
		ListenAddr:             envOr("LISTEN_ADDR", ":8090"),
		ControlPlaneURL:        os.Getenv("CONTROL_PLANE_URL"),
		JWTIssuer:              envOr("JWT_ISSUER", "control-plane"),
		JWTAudience:            envOr("JWT_AUDIENCE", "gateway"),
		ControlDBReplicaDSN:    os.Getenv("CONTROL_DB_REPLICA_DSN"),
		LRUPrimeToken:          lruPrimeTok,
		TombstoneJWTAudience:   envOr("TOMBSTONE_JWT_AUDIENCE", "gateway-tombstone"),
		KafkaBrokers:           os.Getenv("KAFKA_BROKERS"),
		KafkaSASLUsername:      envOr("KAFKA_SASL_USERNAME", "gateway"),
		KafkaSASLPassword:      kafkaPass,
		KafkaSASLMechanism:     envOr("KAFKA_SASL_MECHANISM", "SCRAM-SHA-256"),
		SchemaRegistryURL:      os.Getenv("SCHEMA_REGISTRY_URL"),
		SRBasicAuthUser:        envOr("SR_USERNAME", "sr-gateway"),
		SRBasicAuthPass:        srPass,
		PortfolioEventsTopic:   envOr("PORTFOLIO_EVENTS_TOPIC", "portfolio_events.v2"),
		DataTopic:              envOr("DATA_TOPIC", "data.v2"),
		SourceID:               envOr("GATEWAY_SOURCE", "gateway@v6"),
		SinkMode:               envOr("SINK_MODE", "kafka"),
		RWDSN:                  os.Getenv("RW_DSN"),
		RWPortfolioEventsTable: envOr("RW_PORTFOLIO_EVENTS_TABLE", "portfolio_events_log"),
		RWDataTable:            envOr("RW_DATA_TABLE", "data_log"),
		LogLevel:               envOr("LOG_LEVEL", "info"),
	}

	refresh := envOr("JWKS_REFRESH", "5m")
	d, errR := time.ParseDuration(refresh)
	if errR != nil {
		return cfg, fmt.Errorf("JWKS_REFRESH %q: %w", refresh, errR)
	}
	cfg.JWKSRefresh = d

	if cfg.ControlPlaneURL == "" {
		return cfg, errors.New("CONTROL_PLANE_URL is required")
	}
	if cfg.ControlDBReplicaDSN == "" {
		return cfg, errors.New("CONTROL_DB_REPLICA_DSN is required")
	}
	if cfg.LRUPrimeToken == "" {
		return cfg, errors.New("LRU_PRIME_TOKEN (or _FILE) is required so /internal/lru-prime is reachable")
	}

	// Egress requirements branch on the sink. Kafka mode needs the broker +
	// schema-registry creds; rw (local) mode needs only the RisingWave DSN.
	switch cfg.SinkMode {
	case "kafka":
		if cfg.KafkaBrokers == "" {
			return cfg, errors.New("KAFKA_BROKERS is required (SINK_MODE=kafka)")
		}
		if cfg.KafkaSASLPassword == "" {
			return cfg, errors.New("KAFKA_SASL_PASSWORD (or _FILE) is required (SINK_MODE=kafka)")
		}
		if cfg.SchemaRegistryURL == "" {
			return cfg, errors.New("SCHEMA_REGISTRY_URL is required (SINK_MODE=kafka)")
		}
		if cfg.SRBasicAuthPass == "" {
			return cfg, errors.New("SR_PASSWORD (or SR_PASSWORD_FILE) is required for SR Basic Auth (SINK_MODE=kafka)")
		}
	case "rw":
		if cfg.RWDSN == "" {
			return cfg, errors.New("RW_DSN is required (SINK_MODE=rw)")
		}
	default:
		return cfg, fmt.Errorf("SINK_MODE %q is not one of: kafka, rw", cfg.SinkMode)
	}
	return cfg, nil
}

// JWKSURL returns the control-plane JWKS endpoint derived from CONTROL_PLANE_URL.
func (c Config) JWKSURL() string {
	return trimSlash(c.ControlPlaneURL) + "/jwt/jwks"
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// envOrFile reads `${KEY}` if set, otherwise the file at `${KEY}_FILE`. The
// `_FILE` variant is the compose-secrets pattern: mount /run/secrets/<name>
// and point the env var at the file path. Whitespace (trailing newlines) is
// trimmed. Returns "" if neither is set; the caller decides whether that is
// fatal.
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

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

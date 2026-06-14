package config

import "testing"

// kafkaEnv sets the always-required vars plus a full kafka egress set.
func kafkaEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CONTROL_PLANE_URL", "http://cp")
	t.Setenv("CONTROL_DB_REPLICA_DSN", "postgres://ro@db/control")
	t.Setenv("LRU_PRIME_TOKEN", "tok")
	t.Setenv("KAFKA_BROKERS", "localhost:9092")
	t.Setenv("KAFKA_SASL_PASSWORD", "pw")
	t.Setenv("SCHEMA_REGISTRY_URL", "http://sr")
	t.Setenv("SR_PASSWORD", "pw")
}

func TestLoad_DefaultsToKafkaMode(t *testing.T) {
	t.Setenv("SINK_MODE", "") // force default
	kafkaEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SinkMode != "kafka" {
		t.Fatalf("SinkMode = %q, want kafka", cfg.SinkMode)
	}
}

func TestLoad_RWMode_NoKafkaRequired(t *testing.T) {
	t.Setenv("SINK_MODE", "rw")
	t.Setenv("RW_DSN", "postgres://root@localhost:4566/dev")
	t.Setenv("CONTROL_PLANE_URL", "http://cp")
	t.Setenv("CONTROL_DB_REPLICA_DSN", "postgres://ro@db/control")
	t.Setenv("LRU_PRIME_TOKEN", "tok")
	// Deliberately leave kafka / SR creds unset: rw mode must not require them.
	t.Setenv("KAFKA_BROKERS", "")
	t.Setenv("SCHEMA_REGISTRY_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load (rw mode): %v", err)
	}
	if cfg.SinkMode != "rw" {
		t.Fatalf("SinkMode = %q, want rw", cfg.SinkMode)
	}
	if cfg.RWDSN == "" {
		t.Fatalf("RWDSN empty")
	}
	if cfg.RWPortfolioEventsTable != "portfolio_events_log" {
		t.Fatalf("RWPortfolioEventsTable = %q, want portfolio_events_log", cfg.RWPortfolioEventsTable)
	}
	if cfg.RWDataTable != "data_log" {
		t.Fatalf("RWDataTable = %q, want data_log", cfg.RWDataTable)
	}
}

func TestLoad_RWMode_RequiresDSN(t *testing.T) {
	t.Setenv("SINK_MODE", "rw")
	t.Setenv("RW_DSN", "") // missing
	t.Setenv("CONTROL_PLANE_URL", "http://cp")
	t.Setenv("CONTROL_DB_REPLICA_DSN", "postgres://ro@db/control")
	t.Setenv("LRU_PRIME_TOKEN", "tok")

	if _, err := Load(); err == nil {
		t.Fatalf("expected error when RW_DSN missing in rw mode")
	}
}

func TestLoad_UnknownSinkMode_Errors(t *testing.T) {
	t.Setenv("SINK_MODE", "carrier-pigeon")
	t.Setenv("CONTROL_PLANE_URL", "http://cp")
	t.Setenv("CONTROL_DB_REPLICA_DSN", "postgres://ro@db/control")
	t.Setenv("LRU_PRIME_TOKEN", "tok")

	if _, err := Load(); err == nil {
		t.Fatalf("expected error for unknown SINK_MODE")
	}
}

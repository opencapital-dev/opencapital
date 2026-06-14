// Command gateway is the v6 sole Kafka producer.
//
// Per ADR-0033 / ADR-0038, plugins POST JSON + a session JWT to this binary;
// the gateway verifies the JWT against control-plane JWKS, looks up
// portfolio_id -> org_id in control_db, injects org_id into the Avro v2
// envelope, serializes via Schema Registry, and produces to Redpanda under
// the dedicated `gateway` SASL principal. No other principal in the cluster
// has produce ACLs on portfolio_events.v2 / data.v2.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/portfolio-management/gateway/internal/config"
	"github.com/portfolio-management/gateway/internal/httpapi"
	"github.com/portfolio-management/gateway/internal/jwks"
	gwkafka "github.com/portfolio-management/gateway/internal/kafka"
	"github.com/portfolio-management/gateway/internal/lru"
	"github.com/portfolio-management/gateway/internal/metrics"
	"github.com/portfolio-management/gateway/internal/sr"
	"github.com/portfolio-management/gateway/internal/store"
)

// portfolioOrgLookupStmt is the prepared-statement name the gateway
// reuses for the miss-path lookup. Declared at file scope so both the
// BeforeAcquire hook and the store layer name it consistently.
const portfolioOrgLookupStmt = "portfolio_org_lookup"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// JWKS — warm once at boot, then run a background refresh. Boot does not
	// fail on a JWKS hiccup; KeyFunc retries on first verify.
	jwksClient := jwks.New(cfg.JWKSURL(), jwks.WithRefreshInterval(cfg.JWKSRefresh))
	warmCtx, warmCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := jwksClient.Refresh(warmCtx); err != nil {
		logger.Warn("JWKS warm-up failed", "err", err, "url", cfg.JWKSURL())
	} else {
		logger.Info("JWKS warm", "url", cfg.JWKSURL())
	}
	warmCancel()
	go jwksClient.Run(ctx, func(err error) { logger.Warn("JWKS refresh", "err", err) })

	// Postgres — gateway_ro role pointed at the streaming standby
	// (postgres-replica). v6 Phase 6: the gateway never touches the
	// primary; the primary is reserved for control-plane DML, the
	// rw_replicator CDC slot, and DDL during plugin install (ADR-0034).
	// BeforeAcquire prepares the miss-path query on every checkout so
	// the hot path on a miss is a single round-trip to the replica
	// (no plan + bind cost).
	poolCfg, err := pgxpool.ParseConfig(cfg.ControlDBReplicaDSN)
	if err != nil {
		logger.Error("pgxpool parse", "err", err)
		os.Exit(1)
	}
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Prepare(ctx, portfolioOrgLookupStmt,
			`SELECT org_id FROM portfolios WHERE portfolio_id = $1`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		logger.Error("pgxpool", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		logger.Warn("postgres-replica ping failed at boot", "err", err)
	}
	st := store.New(pool)

	// LRU pre-warm. Streams every (portfolio_id, org_id) into the
	// in-process cache before /readyz can flip to 200. Cold start +
	// the LB drains us until this completes; ADR-0034 calls the
	// staleness window between writes durable on the primary and
	// notification arrival "benign" because the only requester for a
	// brand-new portfolio_id is the just-creating plugin path itself
	// (which retries on 404). Pre-warm row-by-row, no SELECT-into-slice.
	cache := lru.New()
	prewarmCtx, prewarmCancel := context.WithTimeout(ctx, 60*time.Second)
	if err := prewarmLRU(prewarmCtx, pool, cache, logger); err != nil {
		prewarmCancel()
		logger.Error("LRU prewarm failed", "err", err)
		os.Exit(1)
	}
	prewarmCancel()
	metrics.LRUSize.Set(float64(cache.Size()))
	metrics.LRUPrewarmRows.Set(float64(cache.Size()))

	// Background sampler for replica WAL replay lag. Cheap query, runs
	// every 5s. Surfaced as gateway_replica_lag_seconds for ops to
	// alert on. Implementation in main.go (no separate service yet).
	go sampleReplicaLag(ctx, pool, logger)

	// Egress sink. Cloud (SINK_MODE=kafka) Avro-serializes via Schema Registry
	// and produces to Redpanda under the `gateway` SASL principal. Local
	// (SINK_MODE=rw) writes typed DML straight into RisingWave over pgwire,
	// with no broker or schema registry. The control_db pool + LRU above are
	// needed in both modes (the ownership check is sink-independent).
	var sink httpapi.Sink
	switch cfg.SinkMode {
	case "kafka":
		ser, err := sr.New(sr.Config{
			URL:                  cfg.SchemaRegistryURL,
			BasicAuthUser:        cfg.SRBasicAuthUser,
			BasicAuthPass:        cfg.SRBasicAuthPass,
			PortfolioEventsTopic: cfg.PortfolioEventsTopic,
			DataTopic:            cfg.DataTopic,
		})
		if err != nil {
			logger.Error("schema registry", "err", err)
			os.Exit(1)
		}
		prod, err := gwkafka.New(gwkafka.Config{
			Brokers:       cfg.KafkaBrokers,
			SASLMechanism: cfg.KafkaSASLMechanism,
			SASLUsername:  cfg.KafkaSASLUsername,
			SASLPassword:  cfg.KafkaSASLPassword,
			ClientID:      "gateway",
		})
		if err != nil {
			logger.Error("kafka producer", "err", err)
			os.Exit(1)
		}
		defer prod.Close()
		sink = httpapi.NewKafkaSink(ser, prod, cfg.PortfolioEventsTopic, cfg.DataTopic)
	case "rw":
		rwPool, err := pgxpool.New(ctx, cfg.RWDSN)
		if err != nil {
			logger.Error("risingwave pool", "err", err)
			os.Exit(1)
		}
		defer rwPool.Close()
		if err := rwPool.Ping(ctx); err != nil {
			logger.Warn("risingwave ping failed at boot", "err", err)
		}
		sink = httpapi.NewRWSink(rwPool, cfg.RWPortfolioEventsTable, cfg.RWDataTable)
	default:
		logger.Error("unknown SINK_MODE", "mode", cfg.SinkMode)
		os.Exit(1)
	}

	server := httpapi.New(cfg, jwksClient, sink, st, cache, pool, logger)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("gateway listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listen", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown", "err", err)
	}
}

// prewarmLRU streams every row of control_db.portfolios into the cache
// before serving traffic. Streaming (Query + rows.Next) keeps peak
// memory bounded: pgx never materialises the result set into a Go
// slice, and the LRU absorbs rows as they arrive. ADR-0034: pre-warm
// is the design — the LRU is a fully populated mirror of the table.
func prewarmLRU(ctx context.Context, pool *pgxpool.Pool, cache *lru.Cache, logger *slog.Logger) error {
	start := time.Now()
	rows, err := pool.Query(ctx, `SELECT portfolio_id, org_id FROM portfolios`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var n int64
	for rows.Next() {
		var pid, oid uuid.UUID
		if err := rows.Scan(&pid, &oid); err != nil {
			return err
		}
		cache.Put(pid.String(), oid)
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	logger.Info("LRU prewarm complete", "rows", n, "elapsed", time.Since(start).String())
	return nil
}

// sampleReplicaLag emits gateway_replica_lag_seconds every 5s while
// the gateway is up. EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp()))
// is the standard way to report streaming-replica lag in seconds;
// pg_last_xact_replay_timestamp() returns NULL when the replica has
// applied everything, which we treat as 0.
func sampleReplicaLag(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sampleCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			var lagSeconds *float64
			err := pool.QueryRow(sampleCtx,
				`SELECT EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp()))`,
			).Scan(&lagSeconds)
			cancel()
			if err != nil {
				logger.Debug("replica lag sample failed", "err", err)
				continue
			}
			v := 0.0
			if lagSeconds != nil {
				v = *lagSeconds
			}
			metrics.ReplicaLagSeconds.Set(v)
		}
	}
}

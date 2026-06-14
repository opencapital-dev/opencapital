// Package metrics centralises the gateway's Prometheus instruments.
//
// Every counter / gauge is process-global so call sites do not have to
// thread a registry through their constructors. The default
// prometheus.DefaultRegisterer collects them, which is what
// /metrics (mounted in httpapi) exposes.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// LRU instruments. Hits and misses are the headline ratio (operators
// alert when miss-rate climbs); size + prewarm_rows tell ops the
// cache is populated; replica_lag exposes the streaming standby's
// freshness.
var (
	LRUHits = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_lru_hits_total",
		Help: "Portfolio_id -> org_id ownership lookups served from the in-process cache.",
	})
	LRUMisses = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_lru_misses_total",
		Help: "Portfolio_id -> org_id ownership lookups that fell through to the read replica.",
	})
	LRUSize = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_lru_size",
		Help: "Unique portfolio_ids currently held in the gateway's in-process cache.",
	})
	LRUPrewarmRows = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_lru_prewarm_rows",
		Help: "Rows streamed into the LRU at boot pre-warm.",
	})
	ReplicaLagSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_replica_lag_seconds",
		Help: "Estimated WAL replay lag of postgres-replica vs the primary, in seconds.",
	})
	LRUPrimeCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_lru_prime_calls_total",
		Help: "Calls to /internal/lru-prime split by outcome.",
	}, []string{"result"})
)

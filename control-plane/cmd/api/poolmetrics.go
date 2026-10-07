package main

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/metrics"
)

// poolCollector reports the Postgres connection pool at scrape time: how many
// connections are in use against the limit, and how long callers waited for one.
func poolCollector(pool *pgxpool.Pool) metrics.Collector {
	return func() []metrics.Sample {
		st := pool.Stat()
		conn := func(state string, n int32) metrics.Sample {
			return metrics.Sample{Name: "asp_db_pool_connections", Type: "gauge", Help: "Connections of the Postgres pool, by state.",
				Labels: []metrics.Label{{Name: "state", Value: state}}, Value: float64(n)}
		}
		counter := func(name, help string, v float64) metrics.Sample {
			return metrics.Sample{Name: name, Type: "counter", Help: help, Value: v}
		}
		return []metrics.Sample{
			conn("acquired", st.AcquiredConns()),
			conn("idle", st.IdleConns()),
			conn("total", st.TotalConns()),
			conn("max", st.MaxConns()),
			counter("asp_db_pool_acquires_total", "Connections acquired from the pool.", float64(st.AcquireCount())),
			counter("asp_db_pool_empty_acquires_total", "Acquires that found no idle connection and had to wait or open one.", float64(st.EmptyAcquireCount())),
			counter("asp_db_pool_canceled_acquires_total", "Acquires abandoned because the caller's context ended first.", float64(st.CanceledAcquireCount())),
			counter("asp_db_pool_acquire_wait_seconds_total", "Time callers spent waiting for a connection.", st.AcquireDuration().Seconds()),
		}
	}
}

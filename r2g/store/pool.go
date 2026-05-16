// Package store wraps the pgxpool the r2g service uses to talk to the
// shared deepresearch/platform postgres. Same database, same tables,
// same schema as R2R — r2g and the legacy Python backend are
// simultaneously valid readers and writers during the strangler-fig
// migration window. Schema ownership transfers to r2g once R2R is
// retired in Phase 7.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DialPostgres opens a pgxpool against the given DSN with the bounded
// settings r2g uses everywhere: max 16 conns (single backend on a
// 4-core Pi can't usefully fan out further), 5-minute idle reap, 30s
// health-check interval. The caller owns the returned *pgxpool.Pool
// and must call Close on shutdown.
//
// Tuning rationale:
//   - MaxConns=16 — leaves headroom for the Python backend, evo-api,
//     and the four Temporal workers all sharing the same postgres
//     instance (Postgres default max_connections=100).
//   - MinConns=2 — keep the first two warm so the dashboard's first
//     query after idle doesn't pay TCP+TLS setup latency.
//   - HealthCheckPeriod=30s — half the postgres tcp_keepalive default;
//     catches a wedged backend before the next request does.
func DialPostgres(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	cfg.MaxConns = 16
	cfg.MinConns = 2
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	cfg.ConnConfig.RuntimeParams["application_name"] = "r2g"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("dial postgres: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

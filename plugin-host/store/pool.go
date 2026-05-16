// Package store provides the pgxpool used by plugin-host to talk to the
// shared deepresearch postgres. Same database, same schema as r2g; this
// service writes platform.plugin_runs / platform.plugin_items /
// platform.plugin_config.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool dials the shared postgres with conservative bounds: plugin-host
// is bursty (each plugin sync claims a few conns) but never the largest
// consumer on the cluster, so 12 max keeps headroom for r2g + the python
// backend + temporal workers.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	cfg.MaxConns = 12
	cfg.MinConns = 2
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	cfg.ConnConfig.RuntimeParams["application_name"] = "plugin-host"

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

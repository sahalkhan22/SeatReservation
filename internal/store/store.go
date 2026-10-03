// Package store owns every SQL statement in the service. Nothing above this
// layer knows that Postgres exists, and nothing in this layer knows about HTTP.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool         *pgxpool.Pool
	queryTimeout time.Duration
}

// Open builds the pool and blocks until Postgres answers, retrying for up to
// ~15s. compose already gates on pg_isready, but a managed database can still
// be slow to accept connections and we would rather wait than crash-loop.
func Open(ctx context.Context, dsn string, poolMax int32, queryTimeout time.Duration) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	// The pool bound is the real throughput knob. Under contention on one hot
	// seat a smaller pool is often faster: lock waits queue cheaply here
	// instead of each holding a Postgres backend process open.
	cfg.MaxConns = poolMax
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	s := &Store{pool: pool, queryTimeout: queryTimeout}

	var lastErr error
	for attempt := 1; attempt <= 15; attempt++ {
		if lastErr = s.Ping(ctx); lastErr == nil {
			return s, nil
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}

	pool.Close()
	return nil, fmt.Errorf("database unreachable after 15 attempts: %w", lastErr)
}

// Ping backs the readiness probe. The short deadline matters: a probe that can
// hang is worse than one that fails, because the load balancer learns nothing.
func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	return s.pool.Ping(ctx)
}

func (s *Store) Close() { s.pool.Close() }

// InUse reports currently-checked-out connections, exported as a gauge so pool
// saturation is visible before it turns into latency.
func (s *Store) InUse() int32 { return s.pool.Stat().AcquiredConns() }

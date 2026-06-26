// Package store is the thin pgx v5 data-access layer (decision D2: pgx directly, no ORM, no
// sqlc). It owns a pgxpool and hand-written query funcs for the content tables and (later, in
// slice 4.2) the character state tables. Every table is ref/pack + columns + a JSONB tail, so
// the queries are small and explicit; pgx's native JSONB and TEXT[] support is used directly.
//
// The content read path here implements content.Source, so the loader is source-agnostic: a
// live shard loads from Postgres, while unit tests load the same data from the embedded YAML
// pack (content.EmbeddedSource). Boot-time loads run on the construction goroutine, never on
// a zone goroutine, so the synchronous pool calls are fine.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps a pgxpool with the app's query funcs. It is safe for concurrent use.
type Pool struct {
	pool *pgxpool.Pool
}

// Open dials dsn and verifies connectivity with a short ping. A failure is returned (not
// fatal): the caller (buildShard) degrades to an empty boot when the database is unreachable,
// preserving the bare-engine invariant.
func Open(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Ping(pingCtx); err != nil {
		p.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Pool{pool: p}, nil
}

// Close releases the pool's connections.
func (p *Pool) Close() {
	if p != nil && p.pool != nil {
		p.pool.Close()
	}
}

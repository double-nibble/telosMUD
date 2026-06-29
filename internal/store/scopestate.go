package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// scopestate.go is the durable home of DIRECTOR scope state (docs/WORLD-EVENTS.md §7, Phase 10.1): the
// world_state and region_state tables. Region/world state has a SINGLE owning writer (the director), so
// these reads/writes are only ever issued by the owning director — but they carry the SAME
// optimistic-concurrency `version` CAS as characters (PERSISTENCE.md §7) so a stale writer (a
// just-demoted leader racing the freshly-promoted standby during failover) is REJECTED, never clobbers.
//
// Each entry is one (scope, key) -> value JSONB. value is the director's data-only state bag (numbers/
// strings/bools/nested tables), marshalled by the caller. version is 0 for an absent key; the first
// write (expectedVersion 0) creates it at version 1; each subsequent write bumps it.

// LoadWorldState returns the value bytes + current version for a world-scope key. found=false (version 0)
// when the key has never been written.
func (p *Pool) LoadWorldState(ctx context.Context, key string) (value []byte, version uint64, found bool, err error) {
	var v int64
	err = p.pool.QueryRow(ctx, `SELECT value, version FROM world_state WHERE key = $1`, key).Scan(&value, &v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("store: load world_state %q: %w", key, err)
	}
	return value, nonNegU64(v), true, nil
}

// SaveWorldState writes value for a world-scope key under an optimistic CAS on expectedVersion: it
// inserts a brand-new key (expectedVersion 0) at version 1, or updates an existing key ONLY when its
// stored version equals expectedVersion, bumping it. ok=false (nil error) means the CAS lost — a
// concurrent writer moved the version — and the caller should reload + reconcile rather than force it.
func (p *Pool) SaveWorldState(ctx context.Context, key string, value []byte, expectedVersion uint64) (newVersion uint64, ok bool, err error) {
	var v int64
	err = p.pool.QueryRow(ctx,
		`INSERT INTO world_state (key, value, version) VALUES ($1, $2, 1)
		 ON CONFLICT (key) DO UPDATE SET value = $2, version = world_state.version + 1
		   WHERE world_state.version = $3
		 RETURNING version`,
		key, value, stateVersionParam(expectedVersion)).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil // CAS lost: the row exists at a different version than expected
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: save world_state %q: %w", key, err)
	}
	return nonNegU64(v), true, nil
}

// LoadRegionState / SaveRegionState are the per-region equivalents, keyed by (region_id, key).
func (p *Pool) LoadRegionState(ctx context.Context, regionID, key string) (value []byte, version uint64, found bool, err error) {
	var v int64
	err = p.pool.QueryRow(ctx,
		`SELECT value, version FROM region_state WHERE region_id = $1 AND key = $2`, regionID, key).Scan(&value, &v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("store: load region_state %s/%q: %w", regionID, key, err)
	}
	return value, nonNegU64(v), true, nil
}

// SaveRegionState writes a region-scope (region_id, key) value under the same optimistic CAS on
// expectedVersion as SaveWorldState.
func (p *Pool) SaveRegionState(ctx context.Context, regionID, key string, value []byte, expectedVersion uint64) (newVersion uint64, ok bool, err error) {
	var v int64
	err = p.pool.QueryRow(ctx,
		`INSERT INTO region_state (region_id, key, value, version) VALUES ($1, $2, $3, 1)
		 ON CONFLICT (region_id, key) DO UPDATE SET value = $3, version = region_state.version + 1
		   WHERE region_state.version = $4
		 RETURNING version`,
		regionID, key, value, stateVersionParam(expectedVersion)).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: save region_state %s/%q: %w", regionID, key, err)
	}
	return nonNegU64(v), true, nil
}

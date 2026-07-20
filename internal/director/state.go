package director

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrCASLost is returned by Set when the optimistic-concurrency CAS lost — a concurrent writer (a stale
// leader racing the promoted standby during failover) moved the key's version. The director has reloaded
// the winning value into its cache; the caller should re-read and retry rather than force the write.
var ErrCASLost = errors.New("director: scope-state CAS lost (concurrent writer)")

// errCASLost is the internal alias the actor returns; exported as ErrCASLost.
var errCASLost = ErrCASLost

// ErrNotLeader is returned by set when this director does not hold its scope's lease. Scope state has a
// single owning writer, and a director that lost the lease is not it — so the write path refuses rather
// than relying on every caller to have gated first (#354).
var ErrNotLeader = errors.New("director: not the scope leader (scope state has a single writer)")

// state.go is the director's authoritative scope-state read/write, run on the actor goroutine (single
// writer). Get reads the in-memory cache, lazily loading a key from the store on first miss. Set writes
// through the optimistic-concurrency CAS to the store, then updates the cache + version — so the
// in-memory copy and the durable row never drift. A CAS LOSS (a stale leader racing the promoted
// standby during failover) reloads the key from the store rather than clobbering, and the write fails
// back to the caller to retry against the fresh value.

// getResult is a director read: the value bytes + whether the key is set.
type getResult struct {
	value json.RawMessage
	found bool
	err   error
}

type getMsg struct {
	key   string
	reply chan getResult
}

type setMsg struct {
	key   string
	value json.RawMessage
	reply chan error
}

func (getMsg) directorMsg() {}
func (setMsg) directorMsg() {}

// Get returns the current value for key in this director's scope (nil + found=false when unset). It is a
// synchronous round-trip onto the actor goroutine, so it never races a concurrent Set.
func (d *Director) Get(ctx context.Context, key string) (json.RawMessage, bool, error) {
	reply := make(chan getResult, 1)
	d.post(getMsg{key: key, reply: reply})
	select {
	case r := <-reply:
		return r.value, r.found, r.err
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

// Set writes value for key in this director's scope (authoritative, persisted). Synchronous round-trip
// onto the actor goroutine.
func (d *Director) Set(ctx context.Context, key string, value json.RawMessage) error {
	reply := make(chan error, 1)
	d.post(setMsg{key: key, value: value, reply: reply})
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// get (actor goroutine) returns the cached value, loading from the store on a cache miss.
func (d *Director) get(ctx context.Context, key string) getResult {
	if v, ok := d.state[key]; ok {
		return getResult{value: v, found: true}
	}
	val, ver, found, err := d.load(ctx, key)
	if err != nil {
		return getResult{err: err}
	}
	if found {
		d.state[key] = val
		d.versions[key] = ver
		return getResult{value: val, found: true}
	}
	return getResult{found: false}
}

// set (actor goroutine) CASes value to the store on the key's known version, then updates the cache.
// On a CAS loss it reloads the key (so the cache reflects the winning writer) and returns an error so
// the caller can retry against the fresh value.
//
// EVERY failure path here records d.writeFailed, not just the CAS branch (#354). The issue framed the
// lost-write defect around ErrCASLost, but a plain store error — a Postgres blip, a reset connection —
// reaches the same end state by a far more common route: the write does not land, handleSignal acks
// anyway, and the consequence is consumed off the SHARED durable consumer and lost fleet-wide. An ack
// predicate that only knew about CAS losses would have fixed the rare half of its own defect.
// It returns the version the store assigned to THIS write, so a caller that broadcasts the change stamps
// the version of the write it just made rather than re-reading d.versions and trusting a comment to keep
// the two in step (#355). The map read happened to be correct, but correctness that depends on nobody
// introducing an intervening write is the kind that quietly stops holding.
func (d *Director) set(ctx context.Context, key string, value json.RawMessage) (version uint64, err error) {
	// A nil/empty value means DELETE, and the value column is NOT NULL — Postgres rejects a literal nil
	// with a 23502 constraint violation, while the in-memory test double happily stored it. That
	// divergence hid the bug in exactly the direction a fake should never hide one. Normalise to the JSON
	// null literal, which is what the Lua path already produces and what every reader treats as a delete.
	if len(value) == 0 {
		value = json.RawMessage("null")
	}
	// Any unsuccessful return records the failure for handleSignal's ack decision. A deferred check keeps
	// that guarantee STRUCTURAL: a future early-return added to this function inherits it automatically,
	// rather than depending on whoever adds it remembering to set the flag.
	defer func() {
		if err != nil {
			d.writeFailed = true
		}
	}()
	// A director that does not hold its scope's lease must not write scope state, full stop (#354). The
	// leader gate in handleSignal and the one in onTick both already check this, but they check it BEFORE
	// dispatch — leadership can lapse DURING a handler, and the write path itself was happy to persist
	// afterwards. Enforcing it here makes the single-writer invariant structural at the point of the write
	// instead of assumed by every caller.
	//
	// This does NOT close the window completely, and the comment should not pretend otherwise: d.leader is
	// only refreshed on a campaign (every leaseTTL/3), so a director whose lease expired seconds ago still
	// reads true. Closing it fully needs an ownership fence carried INTO the CAS predicate — a lease epoch
	// on the row, the way characters.owner_epoch fences character ownership — because a version CAS is a
	// lost-update DETECTOR, not an ownership fence. That is a schema change and its own piece of work.
	if !d.leader.Load() {
		return 0, ErrNotLeader
	}
	// Seed the version on a CACHE MISS before CASing (#354). d.versions[key] is 0 for any key this process
	// has never read, and the store's CAS predicate (`WHERE version = $expected`) REJECTS 0 against an
	// existing row — so a director that RESTARTED and blind-writes a pre-existing key would lose the CAS
	// with no concurrent writer anywhere. That is not hypothetical: it is exactly the derived-write pattern
	// the world_script idempotency contract recommends (directorlua.go: `director.set("last_boss", ...)`
	// with no prior get), so every restart silently dropped the first write to each key.
	//
	// This is load-bearing for the NAK-on-CAS-loss decision in handleSignal: an ack predicate is only as
	// good as the signal under it, and until this seed existed ErrCASLost meant "cold cache" far more often
	// than it meant "concurrent writer".
	if _, cached := d.versions[key]; !cached {
		val, ver, found, lerr := d.load(ctx, key)
		if lerr != nil {
			return 0, lerr
		}
		if found {
			d.state[key] = val
			d.versions[key] = ver
		}
	}
	newVer, ok, err := d.save(ctx, key, value, d.versions[key])
	if err != nil {
		return 0, err
	}
	if !ok {
		// Lost the CAS — another writer moved the version. Reload so the cache is correct, then surface
		// the conflict; the single-writer invariant means this only happens during a failover race.
		val, ver, found, lerr := d.load(ctx, key)
		if lerr != nil {
			return 0, lerr
		}
		if found {
			d.state[key] = val
			d.versions[key] = ver
		}
		return 0, errCASLost
	}
	d.state[key] = value
	d.versions[key] = newVer
	return newVer, nil
}

// load / save dispatch to the world- or region-scoped store methods by this director's scope.
func (d *Director) load(ctx context.Context, key string) ([]byte, uint64, bool, error) {
	if d.regionID == "" {
		return d.store.LoadWorldState(ctx, key)
	}
	return d.store.LoadRegionState(ctx, d.regionID, key)
}

func (d *Director) save(ctx context.Context, key string, value []byte, expectedVersion uint64) (uint64, bool, error) {
	if d.regionID == "" {
		return d.store.SaveWorldState(ctx, key, value, expectedVersion)
	}
	return d.store.SaveRegionState(ctx, d.regionID, key, value, expectedVersion)
}

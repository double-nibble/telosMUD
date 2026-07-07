package contentpull

import (
	"context"
	"fmt"
	"sort"
)

// guard.go is the live-hosted-pack PRUNE GUARD (#212 slice 4 PR E2). A director-coordinated pull can DROP a
// pack (present in the current registry, absent from the incoming version) — and dropping a pack strips its
// zones' rooms out of Postgres. Doing that to a pack players are currently standing in would yank content
// from under a live zone, so the DIRECTOR (which alone knows the fleet) vetoes it: a pull that would prune a
// pack with a live-hosted zone is REFUSED before any DB change, and the operator drains/rolls a reboot
// first. The uncoordinated CLI importer (cmd/telos-pull) has no fleet view and sets no guard, so this is
// purely the in-game/coordinated path's extra safety.
//
// The guard is ADVISORY, not a transactional invariant. Zone hosting is a fleet/directory fact, not a
// Postgres row, so ImportVersion's tx cannot lock it — there are two windows the pre-flight check cannot
// close: (1) a zone can become hosted (a player walks in) AFTER the check but before the prune commits;
// (2) the prune set is previewed from a NON-transactional registry read (CurrentContentVersion) while
// ImportVersion recomputes the authoritative set under SELECT ... FOR UPDATE — so a concurrent manual
// telos-pull committing in between can shift the actual prune set to a superset the guard never checked.
// The director path single-flights + is leader-only, so director-vs-director is impossible; the residual
// is director-vs-manual-CLI, which relies on operator discipline. This is acceptable because stripping a
// pack's DEFINITION rows does not yank a running zone — shard memory is authoritative (the durability
// ladder), so the harm is deferred to the next reconcile/reload/restart, i.e. the rolling-reboot the guard
// is telling the operator to do anyway. The guard catches the steady-state "this pack is obviously live
// now" case; it does not pretend to be a lock.

// ZoneLister reads the zones a pack owns. *store.Pool satisfies it (PackZones); the guard takes the
// interface so it is unit-testable without a live Postgres.
type ZoneLister interface {
	PackZones(ctx context.Context, pack string) ([]string, error)
}

// ZoneLocator reports whether a zone is currently hosted by a live shard (the fleet directory). The
// director supplies an adapter over its directory handle; a narrow interface keeps contentpull free of a
// directory/Redis dependency and makes the guard testable with a fake fleet.
type ZoneLocator interface {
	ZoneHosted(ctx context.Context, zone string) (bool, error)
}

// PruneGuard is consulted before an import with the packs a pull would PRUNE. It returns the subset that
// must NOT be stripped now (a non-empty result aborts the pull before any DB change). It receives the live
// ZoneLister (the already-open pool) so it does not reconnect. nil (the default / telos-pull) skips it.
type PruneGuard func(ctx context.Context, lister ZoneLister, prunedPacks []string) (blocked []string, err error)

// FleetPruneGuard builds the director's guard: it blocks pruning any pack that owns a zone the fleet
// currently hosts. A lister or locator error aborts the pull (fail closed on incomplete fleet info — the
// operator re-runs once the lookup is healthy; a re-run is idempotent by content SHA). Only ever called
// when there IS something to prune, so a healthy no-prune pull never touches the directory.
func FleetPruneGuard(locator ZoneLocator) PruneGuard {
	return func(ctx context.Context, lister ZoneLister, prunedPacks []string) ([]string, error) {
		var blocked []string
		for _, pack := range prunedPacks {
			zones, err := lister.PackZones(ctx, pack)
			if err != nil {
				return nil, fmt.Errorf("prune guard: list zones for pack %q: %w", pack, err)
			}
			hosted, err := anyHosted(ctx, locator, zones)
			if err != nil {
				return nil, err
			}
			if hosted {
				blocked = append(blocked, pack)
			}
		}
		sort.Strings(blocked)
		return blocked, nil
	}
}

// anyHosted reports whether any of zones is currently hosted by a live shard, short-circuiting on the first.
func anyHosted(ctx context.Context, locator ZoneLocator, zones []string) (bool, error) {
	for _, z := range zones {
		hosted, err := locator.ZoneHosted(ctx, z)
		if err != nil {
			return false, fmt.Errorf("prune guard: hosting lookup for zone %q: %w", z, err)
		}
		if hosted {
			return true, nil
		}
	}
	return false, nil
}

// prunePreview computes the packs a new version would PRUNE: those in the current registry (`current`) but
// absent from the incoming pack set (`incoming`), sorted. It mirrors ImportVersion's registry-diff prune,
// computed BEFORE the import so the guard can veto stripping a live-hosted pack.
func prunePreview(current, incoming []string) []string {
	next := make(map[string]bool, len(incoming))
	for _, p := range incoming {
		next[p] = true
	}
	var pruned []string
	for _, p := range current {
		if !next[p] {
			pruned = append(pruned, p)
		}
	}
	sort.Strings(pruned)
	return pruned
}

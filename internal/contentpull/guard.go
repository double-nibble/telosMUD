package contentpull

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// guard.go is the live-hosted-pack PRUNE GUARD (#212 slice 4 PR E2). A director-coordinated pull can DROP a
// pack (present in the current registry, absent from the incoming version) — and dropping a pack strips its
// zones' rooms out of Postgres. Doing that to a pack players are currently standing in would yank content
// from under a live zone, so the DIRECTOR (which alone knows the fleet) vetoes it: a pull that would prune a
// pack with a live-hosted zone is REFUSED before any DB change, and the operator drains/rolls a reboot
// first. The uncoordinated CLI importer (cmd/telos-pull) has no fleet view and sets no guard, so this is
// purely the in-game/coordinated path's extra safety.
//
// # The deferred-harm argument applies to LEASED zones only (#416)
//
// The paragraph below argues a prune is survivable because shard memory is authoritative, so the harm is
// deferred to the next reconcile — i.e. to the rolling reboot the guard is telling the operator to do
// anyway. That reasoning holds for a leased zone. It does NOT hold for an INSTANCE TEMPLATE: instances are
// minted continuously, and the very next MintInstance after a prune fails validateMintTemplate with "no
// such zone", which is a runtime failure with no operator action in between.
//
// Instances also take no lease, and a dungeon template is typically never in cfg.Zones at all, so the
// hosting question had to be answered a second way. The locator now also consults a TTL'd template-in-use
// claim each shard heartbeats. That adds a THIRD window to the two enumerated below: a template whose first
// copy is being minted right now is advertised at mint rather than on a tick, so the window is narrow, but a
// dropped kick leaves it unadvertised until the next heartbeat.
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

// pruneDecision turns the guard's blocked list into either a REFUSAL or a forced-prune record (#427).
//
// It is a separate pure function because it is the whole of the override's decision logic, and Pull itself
// opens a real Postgres pool — so without this seam the branch could only be exercised against a live
// database. A nil error with a non-empty result means the pull proceeds and these packs are being stripped
// against the guard's advice.
//
// Note force does NOT skip the guard: the caller still runs it, and the blocked list still reaches here.
// Force downgrades the veto to a report. An operator who overrides needs to know exactly what they
// overrode, and so does whoever reads the log afterwards.
func pruneDecision(version string, blocked []string, force bool) (forced []string, err error) {
	if len(blocked) == 0 {
		return nil, nil
	}
	if !force {
		return nil, fmt.Errorf(
			"refusing content version %q: it would strip live-hosted pack(s) [%s] — players are in those zones; "+
				"drain them or roll a reboot before removing the pack(s), or re-run with force to override",
			version, strings.Join(blocked, ", "))
	}
	// Decision only. The Warn that RECORDS the force-prune is emitted by the caller AFTER the import
	// commits (LogForcedPrune) — logging it here would assert a strip that an import failure then rolled
	// back, and the one durable-ish record of a break-glass action should not describe something that
	// did not happen.
	slog.Info("content pull: prune guard blocked pack(s) and force was requested; proceeding to import",
		"version", version, "packs", blocked)
	return blocked, nil
}

// LogForcedPrune records a completed force-prune, AFTER the import has committed. Split from pruneDecision
// so the log describes what actually happened rather than what was about to be attempted (an import that
// then failed would otherwise leave a Warn asserting a strip that rolled back).
//
// It says what actually happens to people, because it is easy to assume "force" evicts somebody and it does
// not: nothing on this path touches a running zone. Shard memory is authoritative, so players inside a
// stripped pack's zones keep playing and can walk out, and a party inside an instance copy finishes its run.
// What breaks is everything AFTER — no new instance copy of a stripped template can be minted, saved
// character locations stop resolving, and crucially a DRAIN OR RESTART cannot rebuild those zones, so the
// handover fails and the occupants are dropped and reclaimed to their home start room. That makes
// "redirect them FIRST, then reboot" the instruction, not "reboot".
func LogForcedPrune(version string, forced []string) {
	if len(forced) == 0 {
		return
	}
	slog.Warn("content pull: FORCE-PRUNED live-hosted pack(s) — the prune guard blocked these and was "+
		"deliberately overridden. Players currently inside keep playing from shard memory and can walk out, "+
		"but no new instance copies can be minted and a DRAIN OR RESTART cannot rebuild these zones: their "+
		"occupants are disconnected and reclaimed to their home start room. REDIRECT those characters FIRST, "+
		"then roll a reboot. Also confirm no world shard pins these packs via TELOS_CONTENT_PACKS, or it "+
		"will refuse to boot",
		"version", version, "packs", forced)
}

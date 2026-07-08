package world

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// Bounded-retry delivery for a dropped zone-shape reconcile (#191 PR 3/3, the #194 reliability piece for
// this path). The reconcile fan-out stays NON-BLOCKING (postOrDrop) — a wedged zone inbox must never
// head-of-line-stall every later invalidation shard-wide — but a dropped reconcileZoneMsg is worse than a
// dropped Lua reload: a dropped REMOVE leaves an interactive GHOST ROOM that accumulates players and a
// singleton never self-heals. So on a drop we hand the message to a short-lived retry goroutine that
// re-posts it with bounded backoff. The retry re-posts the SAME immutable message with its ORIGINAL
// version (the reconcileZoneMsg invariant): the version guard advances the cursor only on APPLICATION, so
// a dropped-then-retried reconcile re-applies cleanly, and a retry can never resurrect stale state over a
// newer reload (a superseding reload advanced the cursor past this version, so the stale retry is dropped
// by the guard — safe). These are package vars so a test can shrink the timing.
var (
	// reconcileRetryAttempts is how many times a dropped reconcile is re-posted before giving up (the
	// operator remedy is then to re-run `reload`). The inbox is 256-deep and drains fast, so a few paced
	// attempts cover a transient backlog.
	reconcileRetryAttempts = 5
	// reconcileRetryBackoff is the base inter-attempt delay; attempt N waits N×this (linear backoff).
	reconcileRetryBackoff = 50 * time.Millisecond
	// maxReconcileRetryGoroutines caps concurrent retry goroutines so a reload storm against a saturated
	// shard cannot spawn unbounded goroutines; past the cap a drop is logged and abandoned (re-run remedy).
	maxReconcileRetryGoroutines int64 = 128
)

// reload.go is the shard-side hot-reload applier (docs/PHASE4-PLAN.md §5). It subscribes to the
// content bus; on an invalidation it RE-READS just the named (kind, ref) definition from the
// content source, REBUILDS the one *Prototype via the same DTO->component mapper the boot loader
// uses (content_map.go), and SWAPS it into the per-shard prototype cache race-safely
// (protoCache.reload — the atomic table swap, prototype.go). The NEXT spawn of that ref uses the
// new data; live instances spawned earlier keep the old prototype (it stays alive via GC), which
// is the documented MUD semantics: an existing mob keeps its stats, the next repop uses the edit.
//
// # Optional, never fatal
//
// Hot reload is OPTIONAL: a shard with no bus OR no source has a nil reloader and behaves exactly
// as a pre-4.3 shard (boot-load still works, every storeless/busless test stays green). A bus
// subscribe failure is logged, never fatal. Mirrors WithPersistence's disabled fallback.
//
// # Single-writer of the cache swap
//
// The bus delivers invalidations SERIALLY per subscription, so the applier runs reloads one at a
// time on the subscription's own goroutine — it is the sole RUNTIME writer of the cache table.
// spawn never writes the table (only get -> atomic Load), so the swap races neither a reader nor
// another reload. This is the per-shard concurrency contract the distributed-systems-architect
// scrutinizes: the cache is a shared-read structure mutated only by atomic whole-table swap.

// reloadIOTimeout bounds the single-ref re-read so a slow/hung content source cannot wedge the
// subscriber goroutine (which would silently stop every later reload). On timeout the reload is
// abandoned; the last-known prototype stays in the cache and the next invalidation retries.
const reloadIOTimeout = 5 * time.Second

// reloader holds the hot-reload wiring for one shard: the content source to re-read a single
// definition from, the shared prototype cache to swap into, and the live bus subscription. It is
// nil on a shard with no bus/source (hot reload disabled).
type reloader struct {
	src   content.DefinitionSource // single-ref re-read (Postgres in prod, embedded/mem in tests)
	cache *protoCache              // the per-shard cache whose entries this swaps (shared read)
	bus   contentbus.Bus           // the invalidation bus
	sub   contentbus.Subscription  // live subscription; Unsubscribe on stop
	packs map[string]bool          // packs this shard loads; an edit to another pack is ignored
	// enabled is the same pack set as packs, kept as the ORDERED boot list so the `reload` staff command
	// (reloadcmd.go) can iterate + display them deterministically. Set once at construction, read-only.
	enabled []string
	shard   *Shard // the hosting shard — to post a reloadLuaMsg to each zone (7.7); nil on a bare test
	log     *slog.Logger

	// Bounded-retry delivery of a dropped zone-shape reconcile (#191 PR 3/3). retryInFlight caps concurrent
	// retry goroutines; retryDone is closed once at stop() to cancel any in-flight retry promptly.
	retryInFlight atomic.Int64
	retryDone     chan struct{}
	stopOnce      sync.Once

	// Reconcile-on-join (#212 slice 4 PR D): appliedContentVersion is the monotonic content version this
	// shard's in-memory prototypes reflect (seeded at boot from the version the content was loaded at). On
	// a bus reconnect, reconcileOnJoin compares it against the CURRENT Postgres content version and, if the
	// shard fell behind during the gap (a pull it missed), re-applies locally to catch up. reconcileInFlight
	// single-flights concurrent reconnects.
	appliedContentVersion atomic.Uint64
	reconcileInFlight     atomic.Bool
}

// contentVersioner is the subset of the content source that reports the current Postgres content
// version — satisfied by *store.Pool in production, absent on the embedded/mem sources (which then
// skip reconcile-on-join). Kept structural so the world package needs no store import (store imports
// world, so the reverse would cycle).
type contentVersioner interface {
	ContentVersion(ctx context.Context) (uint64, error)
}

// contentVersionBumper is the subset of the content source that ATOMICALLY bumps + returns the monotonic
// Postgres content version — satisfied by *store.Pool, absent on the embedded/mem sources. When present, a
// shard-local reload mints its bus version by BUMPING the single PG authority (#232) instead of stamping
// the wall clock, so the version is monotonic fleet-wide (no per-shard clock skew) and the bump is visible
// to reconcile-on-join. Structural, to keep the world package free of a store import (store imports world).
//
// SEAM WARNING: a PRODUCTION source must be a *store.Pool (or a wrapper that FORWARDS BumpContentVersion).
// A decorator that dropped it would fail this assertion, silently take the wall-clock fallback path in
// mintReloadVersion, and reintroduce the #222/#232 clock-skew residual — with a green test suite. Same
// accepted risk as the sibling contentVersioner assertion; keep the live source concrete.
type contentVersionBumper interface {
	BumpContentVersion(ctx context.Context) (uint64, error)
}

// localApplyBus is a contentbus.Bus whose Publish applies each invalidation to THIS shard directly
// (r.onInvalidation) instead of broadcasting. reconcileOnJoin feeds a re-read pack through PublishPack
// over it, reusing the exact per-ref swap + zone-shape reconcile the wire path uses — but LOCAL-only,
// so a rejoining shard catches itself up without re-broadcasting to (and re-swapping) the whole fleet.
type localApplyBus struct{ r *reloader }

func (b localApplyBus) Publish(_ context.Context, inv contentbus.Invalidation) error {
	b.r.onInvalidation(inv)
	return nil
}

func (localApplyBus) Subscribe(func(contentbus.Invalidation)) (contentbus.Subscription, error) {
	return nil, nil // unused: a local-apply bus is never subscribed to
}
func (localApplyBus) OnReconnect(func()) {}
func (localApplyBus) Close() error       { return nil }

// newReloader wires a reloader over src/cache/bus for the given enabled packs and SUBSCRIBES. A
// nil bus or nil src yields a nil reloader (hot reload disabled). A subscribe failure logs and
// returns nil — never fatal, so an unreachable/closed bus simply disables hot reload.
func newReloader(src content.DefinitionSource, cache *protoCache, bus contentbus.Bus, enabledPacks []string, bootVersion uint64, shard *Shard) *reloader {
	if bus == nil || src == nil || cache == nil {
		return nil
	}
	r := &reloader{
		src:       src,
		cache:     cache,
		bus:       bus,
		packs:     map[string]bool{},
		enabled:   append([]string(nil), enabledPacks...),
		shard:     shard,
		log:       slog.With("component", "contentreload"),
		retryDone: make(chan struct{}),
	}
	// Seed the applied version with the version the boot content was loaded at (read BEFORE the packs
	// in loadContent, so it is never AHEAD of the loaded content — a pull racing boot fails safe to a
	// redundant re-apply on the first reconnect, never a missed catch-up).
	r.appliedContentVersion.Store(bootVersion)
	for _, p := range enabledPacks {
		r.packs[p] = true
	}
	sub, err := bus.Subscribe(r.onInvalidation)
	if err != nil {
		r.log.Warn("content invalidation subscribe failed; hot reload disabled", "err", err)
		return nil
	}
	r.sub = sub
	// Reconcile-on-join (#212 slice 4 PR D): after a bus reconnect, catch up on any pull missed during
	// the gap. Off the bus goroutine (reconcileOnJoin does PG I/O), single-flighted.
	bus.OnReconnect(func() { go r.reconcileOnJoin() })
	r.log.Debug("hot reload enabled", "packs", enabledPacks, "boot_content_version", bootVersion)
	return r
}

// reconcileOnJoin catches this shard up to the current Postgres content version after a bus gap
// (#212 slice 4 PR D). Core NATS buffers nothing while disconnected, so a pull that broadcast during
// the gap was missed — the DB rows are current (Model 1) but the in-memory prototypes are stale. It
// reads the current content version; if the shard is behind, it re-reads its enabled packs from
// Postgres and applies them LOCALLY (via localApplyBus — no fleet re-broadcast) stamped with that
// version, exactly as the missed broadcast would have. It NEVER re-pulls (the source is Postgres, not
// git), NEVER refuses logins, and on a read failure just logs and keeps serving the current content.
// Single-flighted against concurrent reconnects; runs off the bus goroutine.
func (r *reloader) reconcileOnJoin() {
	if !r.reconcileInFlight.CompareAndSwap(false, true) {
		return // a catch-up is already running (rapid reconnects) — its result covers this one
	}
	defer r.reconcileInFlight.Store(false)

	vs, ok := r.src.(contentVersioner)
	if !ok {
		return // source can't report a version (embedded/mem dev source): nothing to reconcile
	}
	ctx, cancel := context.WithTimeout(context.Background(), reloadRepublishTimeout)
	defer cancel()
	cur, err := vs.ContentVersion(ctx)
	if err != nil {
		r.log.Warn("reconcile-on-join: could not read the content version; serving current content", "err", err)
		return
	}
	applied := r.appliedContentVersion.Load()
	if cur == 0 || cur <= applied {
		return // not behind (0 = never imported): the reconnect changed nothing to catch up on
	}
	src, ok := r.src.(content.Source)
	if !ok {
		return
	}
	packs, err := src.LoadPacks(ctx, r.enabled)
	if err != nil {
		r.log.Warn("reconcile-on-join: re-read of packs failed; serving current content", "err", err)
		return
	}
	local := localApplyBus{r: r}
	total := 0
	for _, pk := range packs {
		n, _ := contentbus.PublishPack(ctx, local, pk, cur) // applies locally via r.onInvalidation
		total += n
	}
	// advanceApplied (not a raw Store): a wire delivery of a NEWER pull could land on the subscription
	// goroutine during this reconcile and advance the cursor past `cur` — a hard Store would regress it.
	r.advanceApplied(cur)
	r.log.Info("reconcile-on-join: caught up after a content-bus gap",
		"from_version", applied, "to_version", cur, "definitions_applied", total)
}

// advanceApplied monotonically raises appliedContentVersion to v (a no-op if v is 0 or not higher),
// via a CAS loop so a concurrent wire delivery and a reconcile-on-join agree on the maximum.
func (r *reloader) advanceApplied(v uint64) {
	for v > 0 {
		prev := r.appliedContentVersion.Load()
		if v <= prev {
			return
		}
		if r.appliedContentVersion.CompareAndSwap(prev, v) {
			return
		}
	}
}

// onInvalidation is the bus handler: it runs OFF every zone goroutine (the bus's subscription
// goroutine), serially per subscription. It filters by pack, re-reads the single definition,
// rebuilds the prototype, and swaps it into the cache. Every failure is non-fatal (logged):
// hot reload is best-effort and never disturbs the running world beyond the one ref it targets.
func (r *reloader) onInvalidation(inv contentbus.Invalidation) {
	// Ignore an edit to a pack this shard does not load (an empty pack matches nothing). A shard
	// only caches prototypes from its enabled packs, so a foreign-pack invalidation is a no-op.
	if inv.Pack != "" && !r.packs[inv.Pack] {
		r.log.Debug("invalidation ignored: pack not loaded here", "kind", inv.Kind, "ref", inv.Ref, "pack", inv.Pack)
		return
	}
	r.log.Debug("invalidation received", "kind", inv.Kind, "ref", inv.Ref, "pack", inv.Pack)

	// The version-complete SENTINEL (#212 slice 4 PR D): a versioned pull's LAST invalidation, carrying
	// only the version. Advancing appliedContentVersion ONLY here — not on every per-ref message — is what
	// makes a PARTIALLY-delivered pull safe: a prefix-then-drop never reaches the sentinel, so applied
	// stays behind and reconcile-on-join re-applies the rest on reconnect. It has no ref to swap. (A
	// shard-local `reload` / dev seed does NOT emit it, so a hot-edit's nanos version never poisons the
	// cursor — only a real versioned pull advances it.)
	if inv.Kind == content.KindVersionComplete {
		r.advanceApplied(inv.Version)
		return
	}

	// Phase 8.3: a `channel` invalidation reloads a pack-GLOBAL channel_def into the per-shard channel
	// REGISTRY (the atomic-swap defRegistry), not the prototype cache. It is a different swap target
	// (a channelDef, not a *Prototype) and a different table, so it forks off here before the
	// prototype path. Channel verbs are derived from the registry on each dispatch (channelForVerb), so
	// a verb added/removed by the edit takes effect with no second swap.
	if inv.Kind == content.KindChannel {
		r.reloadChannel(inv)
		return
	}

	// A `zone` invalidation carries no spawnable prototype — it drives the live-room-SHAPE reconcile
	// (#191): it carries the zone's DESIRED room set + start room on the wire, so the hosting zone
	// converges its live room graph (spawn ADDs, resync UPDATEs, tear down DELETIONs) off the
	// already-swapped cache with no source re-read. It forks off before the prototype path, like the
	// channel case.
	if inv.Kind == content.KindZone {
		r.reconcileZone(inv)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), reloadIOTimeout)
	defer cancel()
	def, err := r.src.LoadDefinition(ctx, inv.Kind, inv.Ref, inv.Pack)
	if err != nil {
		// Infrastructure failure on the re-read: keep the last-known prototype (do NOT drop it on
		// a transient read error — that would empty the ref on a Postgres blip). The next
		// invalidation retries.
		r.log.Warn("hot reload re-read failed; keeping last-known prototype",
			"kind", inv.Kind, "ref", inv.Ref, "err", err)
		return
	}

	ref := ProtoRef(inv.Ref)
	if !def.Found {
		// The row was deleted/renamed: remove the entry so a later spawn of this ref returns nil
		// (logged as unknown) rather than serving a now-orphaned prototype. Live instances already
		// spawned keep their aliased prototype (GC holds it) — they are unaffected, by design.
		r.cache.reload(ref, nil)
		r.log.Debug("hot reload: prototype removed (definition deleted)", "kind", inv.Kind, "ref", inv.Ref)
		r.notifyZones(inv.Kind, inv.Ref) // a deleted scripted proto → its live instances go scriptless
		return
	}

	p := buildPrototype(def)
	if p == nil {
		r.log.Warn("hot reload: unbuildable definition, skipped", "kind", inv.Kind, "ref", inv.Ref)
		return
	}
	// The atomic whole-table swap (prototype.go reload): the new prototype is published in one
	// step; concurrent spawns see the old or the new table, never a half-applied map. The old
	// prototype stays alive while any live instance aliases it.
	r.cache.reload(ref, p)
	r.log.Debug("hot reload: prototype swapped", "kind", inv.Kind, "ref", inv.Ref)
	// Notify each hosted zone to apply the LUA reload on its own goroutine (7.7): recompile the
	// chunk, re-register live instances' handlers (keeping self.state), bump the timer generation,
	// reset the breaker. The shared-cache swap above is the cross-goroutine-safe publish; the
	// per-zone Lua state is updated single-writer via the inbox post.
	r.notifyZones(inv.Kind, inv.Ref)
}

// reloadChannel applies a `channel` content hot reload (Phase 8.3): it re-reads the single edited
// channel_def, rebuilds the runtime channelDef via the SAME mapper the boot loader uses
// (buildChannelDef), and SWAPS it into the per-shard channel registry race-safely (defRegistry.reload
// — the atomic whole-table swap). A deleted channel (Found=false) is REMOVED from the registry, so its
// verb stops resolving and a speak attempt falls through to "Huh?". Every failure is non-fatal
// (logged): hot reload is best-effort and never disturbs the running world beyond the one channel.
//
// It runs OFF every zone goroutine (the content-bus subscription goroutine), serially per
// subscription, and the registry swap is the cross-goroutine-safe publish (a zone goroutine reading
// channelForVerb sees the old or the new table whole). Channel verbs are DERIVED from the registry on
// each dispatch, so a verb the edit added/removed/renamed takes effect with no further notification —
// nothing per-zone to re-register (unlike a scripted prototype's Lua handlers).
func (r *reloader) reloadChannel(inv contentbus.Invalidation) {
	if r.shard == nil || r.shard.defs == nil {
		// A bare reloader (a test without a shard) has no channel registry to swap; nothing to do.
		return
	}
	reg := r.shard.defs.channel
	if reg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reloadIOTimeout)
	defer cancel()
	def, err := r.src.LoadDefinition(ctx, inv.Kind, inv.Ref, inv.Pack)
	if err != nil {
		// Infrastructure failure on the re-read: keep the last-known channel_def (do NOT drop it on a
		// transient read error). The next invalidation retries.
		r.log.Warn("hot reload re-read failed; keeping last-known channel", "ref", inv.Ref, "err", err)
		return
	}
	if !def.Found {
		// The channel was deleted/renamed: remove it so its verb stops resolving.
		reg.reload(inv.Ref, nil, true)
		r.log.Debug("hot reload: channel removed (definition deleted)", "ref", inv.Ref)
		r.republishCommsToZones(inv.Ref) // a removed channel must drop live subscribers' subscriptions (#75)
		return
	}
	reg.reload(inv.Ref, buildChannelDef(def.Channel), false)
	r.log.Debug("hot reload: channel swapped", "ref", inv.Ref)
	// #75: the registry now holds the new access/hear_access, but live sessions' hear-sets are PUSHED — a
	// retightened channel leaves an already-subscribed player over-permissive until their next
	// toggle/handoff/relog. Fan a republish out to every hosted zone so the gate re-filters now.
	r.republishCommsToZones(inv.Ref)
}

// republishCommsToZones fans a republishCommsMsg out to every hosted zone after a channel_def reload, so each
// zone re-publishes its players' comms config on its own goroutine (#75). Runs OFF every zone goroutine (the
// content-bus subscriber). The fan-out is NON-BLOCKING (postOrDrop) so one saturated zone can't head-of-line-
// stall the shard-wide reload — but unlike the Lua reload (notifyZones), a DROPPED comms republish leaves the
// hear-set PERMANENTLY stale (it is pushed, not pulled) and RE-OPENS the security gap, so a drop is handed to
// bounded-retry (retryRepublishComms), the same posture the zone-shape reconcile uses (#191). Idempotent +
// level-triggered, so the retry (or a re-run) always converges to the current state.
func (r *reloader) republishCommsToZones(ref string) {
	if r == nil || r.shard == nil {
		return
	}
	for _, z := range r.shard.zonesList() { // mu-guarded: safe against a runtime HostZone (16.4a)
		if !z.postOrDrop(republishCommsMsg{ref: ref}) {
			r.retryRepublishComms(z, ref)
		}
	}
}

// reconcileZone drives a `zone` content hot reload (#191): the KindZone invalidation carries the zone's
// DESIRED room-ref set + start room + a monotonic version, so this simply posts that desired state to the
// hosting zone as a reconcileZoneMsg — the diff-and-converge (spawn ADDs, resync UPDATEs, tear down
// DELETIONs, apply the start_room change) runs single-writer ON THE ZONE GOROUTINE. It runs OFF every zone
// goroutine (the content-bus subscription goroutine) and does NO source re-read: the payload travels on
// the wire, and because the contentbus is a single ordered subject, this shard already applied the pack's
// per-ref prototype cache swaps (delivered before this trailing KindZone invalidation) — so the zone
// reconciles ADDs/UPDATEs off the already-swapped cache.
//
// Only a shard HOSTING this zone reconciles it (the reconcile is a no-op elsewhere). Delivery is the same
// non-blocking postOrDrop the rest of hot reload uses — a wedged zone inbox must not head-of-line-stall
// every later invalidation shard-wide — but a dropped reconcile is worse than a dropped Lua reload (a lost
// REMOVE leaves a ghost room), so a drop is handed to bounded-retry delivery (retryReconcile) rather than
// merely warned. The reconcile is level-triggered + idempotent, so a retry (or a re-run) converges.
func (r *reloader) reconcileZone(inv contentbus.Invalidation) {
	if r == nil || r.shard == nil {
		return
	}
	// A local, unleased bootstrap zone (#212 core pack) is embedded-only and never reloaded — never
	// let a KindZone invalidation naming it drive a shape-reconcile (which tears down rooms not in the
	// desired set) against the live lobby on every shard. Defense-in-depth on top of the reserved-
	// namespace lint: a core-namespace ref should never reach the broadcast, but fail safe if one does.
	if r.shard.isLocalZone(inv.Ref) {
		return
	}
	msg := reconcileZoneMsg{
		zoneRef:   inv.Ref,
		version:   inv.Version,
		rooms:     inv.Rooms,
		startRoom: ProtoRef(inv.StartRoom),
	}
	for _, z := range r.shard.zonesList() { // mu-guarded against a runtime HostZone (16.4a)
		if z.id != inv.Ref {
			continue
		}
		if !z.postOrDrop(msg) {
			r.retryReconcile(z, msg) // bounded-retry the dropped reconcile off this goroutine (#191 PR 3/3)
		}
		return // a zone is hosted by at most one shard-local Zone; done once matched
	}
}

// retryReconcile re-posts a DROPPED reconcileZoneMsg to z with bounded linear backoff, on a short-lived
// goroutine so the subscriber goroutine is never blocked (#191 PR 3/3 / #194). It re-posts the SAME
// immutable msg (original version) — the version guard makes that safe: a dropped reconcile never advanced
// z.lastReconciledPackVer, so the retry re-applies, and a retry superseded by a newer reload is harmlessly
// dropped by the guard. Concurrent retries are capped (maxReconcileRetryGoroutines) so a reload storm on a
// saturated shard can't spawn unbounded goroutines; past the cap the drop is abandoned (re-run remedy). The
// goroutine self-terminates on success, on attempt exhaustion, or when the reloader stops (retryDone).
func (r *reloader) retryReconcile(z *Zone, msg reconcileZoneMsg) {
	if r.retryInFlight.Add(1) > maxReconcileRetryGoroutines {
		r.retryInFlight.Add(-1)
		r.log.Warn("hot-reload zone-shape reconcile dropped (retry budget exhausted); a room add/remove is lost until re-run",
			"zone", msg.zoneRef)
		return
	}
	go func() {
		defer r.retryInFlight.Add(-1)
		for attempt := 1; attempt <= reconcileRetryAttempts; attempt++ {
			select {
			case <-r.retryDone:
				return // reloader stopped — abandon the retry (posting to a torn-down shard is pointless)
			case <-time.After(reconcileRetryBackoff * time.Duration(attempt)):
			}
			if z.postOrDrop(msg) {
				r.log.Debug("zone-shape reconcile retry delivered", "zone", msg.zoneRef, "attempt", attempt)
				return
			}
		}
		r.log.Warn("hot-reload zone-shape reconcile retries exhausted; a room add/remove is lost until re-run",
			"zone", msg.zoneRef, "attempts", reconcileRetryAttempts)
	}()
}

// retryRepublishComms re-posts a DROPPED republishCommsMsg to z with bounded linear backoff on a short-lived
// goroutine, so the content-bus subscriber goroutine is never blocked (#75). It mirrors retryReconcile's
// posture because the failure mode is the same class — a dropped push that re-opens a gap (here: a stale,
// too-permissive hear-set) — but the message is level-triggered + carries no version, so a re-post always
// republishes the CURRENT config and a retry superseded by a newer channel reload is harmlessly redundant
// (no version guard needed, unlike reconcile).
//
// Retry EXHAUSTION degrades to exactly the pre-fix behavior (the hear-set is re-pushed on the player's next
// toggle/handoff/relog) — a strict Pareto improvement, never worse than before. Two caveats the distsys
// review recorded: (a) the operator "re-run the reload" remedy is weaker than reconcile's — re-applying an
// IDENTICAL edit may emit no KindChannel invalidation, so it can be a no-op; a genuinely stuck hear-set clears
// only on the player's next toggle/handoff/relog. (b) Unlike reconcile (which retries at most ONE zone per
// invalidation), a channel reload fans to EVERY hosted zone, so a KindChannel drop-storm consumes the SHARED
// maxReconcileRetryGoroutines budget 1:Z and can starve concurrent reconcile retries (and vice versa) — the
// coalescing follow-up #269 bounds this. Past the cap the drop is abandoned with a loud warn. Self-terminates on
// success, exhaustion, or reloader stop (retryDone).
func (r *reloader) retryRepublishComms(z *Zone, ref string) {
	if r.retryInFlight.Add(1) > maxReconcileRetryGoroutines {
		r.retryInFlight.Add(-1)
		r.log.Warn("hot-reload comms republish dropped (retry budget exhausted); a channel's hear-set stays stale until re-run",
			"zone", z.id, "channel", ref)
		return
	}
	go func() {
		defer r.retryInFlight.Add(-1)
		for attempt := 1; attempt <= reconcileRetryAttempts; attempt++ {
			select {
			case <-r.retryDone:
				return // reloader stopped — abandon the retry
			case <-time.After(reconcileRetryBackoff * time.Duration(attempt)):
			}
			if z.postOrDrop(republishCommsMsg{ref: ref}) {
				r.log.Debug("comms republish retry delivered", "zone", z.id, "channel", ref, "attempt", attempt)
				return
			}
		}
		r.log.Warn("hot-reload comms republish retries exhausted; a channel's hear-set stays stale until re-run",
			"zone", z.id, "channel", ref, "attempts", reconcileRetryAttempts)
	}()
}

// reloadLua applies a content Lua hot reload for (kind, ref) ON THE ZONE GOROUTINE (slice 7.7,
// P7-D7 / §1.1). The shard reloader already swapped the new prototype/def into the shared cache;
// this re-runs on each hosted zone so the per-zone Lua state (the chunk cache, the LState, the
// per-instance entityScripts) — all zone-owned — is updated single-writer. It:
//
//  1. BUMPS the chunk generation so pending old-gen mud.after timers DROP at fire (don't run old
//     code against new state; a durable=true finalizer still completes).
//  2. INVALIDATES the per-(kind,ref) shared chunk cache entry (an ability on_resolve / affect hook /
//     formula / pvp policy) so the NEXT invocation recompiles from the new source. The source-aware
//     chunkFor would recompile anyway on a changed source, but a SHARED def's source lives on the
//     swapped shared registry, not passed here — so we drop the cache entry and let the next entry-
//     point invocation re-read + recompile (the security-critical pvp_allowed case takes effect on
//     its next consult, never the stale permissive policy).
//  3. RE-REGISTERS the handlers of every LIVE instance of a scripted prototype (a mob/room with a
//     `lua` block) from the NEW source, PRESERVING each instance's self.state (the DATA survives).
//  4. RESETS the circuit breaker for that def so a script a bug had quarantined is re-enabled by the
//     fix reload.
func (z *Zone) reloadLua(kind, ref string) {
	if z.lua == nil {
		return
	}
	rt := z.lua
	rt.chunkGen++ // (1) old-gen mud.after timers drop at fire (P7-D7)

	// (2) Invalidate the shared chunk cache entries that key off this ref, so the next invocation
	// recompiles from the swapped registry source. Keys are "<kind>:<ref>:<hook>" / "<kind>:<ref>" /
	// "formula:<name>" / "pvp_allowed". We drop every cache entry that BELONGS to this ref (matched as a
	// whole colon-delimited segment, not a raw substring — #57, so reloading "orc" never drops "sorcerer")
	// plus the global policy/formula keys, and reset their breakers.
	for key := range rt.chunks {
		if keyMatchesRef(key, ref) || key == "pvp_allowed" || strings.HasPrefix(key, "formula:") {
			delete(rt.chunks, key)
			rt.breakerReset(breakerKeyShared(key)) // (4) re-enable a quarantined shared def
		}
	}

	// (3) Re-register live instances of the reloaded scripted prototype from the new source, keeping
	// their self.state. A non-prototype (ability/affect/formula) reload has no instances to walk.
	rt.reloadEntityScriptsForProto(ProtoRef(ref))

	z.log.Debug("lua hot reload applied", "kind", kind, "ref", ref, "gen", rt.chunkGen)
}

// keyMatchesRef reports whether a chunk-cache key belongs to content ref — i.e. ref appears in the key as a
// whole colon-delimited segment run, not merely as a raw substring (#57). Keys are "<kind>:<ref>[:<hook>]"
// and a ref may ITSELF contain colons (e.g. "midgaard:mob:orc"), so ref is matched bounded by ':' (or a key
// boundary) on both sides. This stops reloading ref "orc" from over-invalidating "sorcerer"/"orchard" chunk
// entries (whose keys merely contain the substring "orc"). Keys always carry a "<kind>:" prefix, so in
// practice ref matches as the interior ":<ref>:" or trailing ":<ref>" run; the bare-equal / leading forms
// are kept for completeness.
func keyMatchesRef(key, ref string) bool {
	if ref == "" {
		return false
	}
	return key == ref ||
		strings.HasPrefix(key, ref+":") ||
		strings.HasSuffix(key, ":"+ref) ||
		strings.Contains(key, ":"+ref+":")
}

// notifyZones posts a reloadLuaMsg to every hosted zone's inbox so each applies the Lua reload on
// its own goroutine (the per-zone Lua state is zone-owned). Called by the subscriber goroutine
// AFTER the shared prototype cache swap; the post is the cross-goroutine-safe hand-off (the only
// sanctioned way to reach zone state). A reloader with no shard (a bare test) skips it.
func (r *reloader) notifyZones(kind, ref string) {
	if r == nil || r.shard == nil {
		return
	}
	for _, z := range r.shard.zonesList() { // mu-guarded: safe against a runtime HostZone (16.4a)
		// NON-BLOCKING fan-out: a blocking post here would let ONE saturated (or wedged) zone inbox
		// head-of-line-stall every LATER zone's invalidation shard-wide, and a wedged zone would halt hot
		// reload entirely (distsys review) — so a full inbox DROPS the message. This carries only the LUA
		// recompile now (chunk re-read + per-instance handler re-register): a shared def (ability/formula)
		// self-heals on next invocation (chunkFor re-reads the swapped source); a scripted-prototype's live
		// instances keep their old handler until a reload of that ref lands, so a dropped one loses that
		// Lua EDIT until re-run — hence the loud warn. Room SHAPE (add/update/remove + start_room) does NOT
		// ride this message at all — it rides the reconcileZoneMsg (reconcileZone), whose own dropped-delivery
		// gap is tracked as #194 (PR 3/3, bounded-retry).
		if !z.postOrDrop(reloadLuaMsg{kind: kind, ref: ref}) {
			slog.Warn("hot-reload Lua invalidation dropped (zone inbox full); a shared def self-heals on next "+
				"access, but a scripted prototype's live instances keep the old handler until re-run",
				"zone", z.id, "kind", kind, "ref", ref)
		}
	}
}

// stop unsubscribes the reloader from the bus and cancels any in-flight reconcile-retry goroutines.
// Idempotent (the retryDone close is once-guarded); safe on a nil reloader.
func (r *reloader) stop() {
	if r == nil || r.sub == nil {
		return
	}
	_ = r.sub.Unsubscribe()
	r.stopOnce.Do(func() {
		if r.retryDone != nil {
			close(r.retryDone)
		}
	})
}

// buildPrototype turns a single re-read Definition into a fresh *Prototype using the SAME
// DTO->component mapper the boot loader uses (content_map.go), so a hot-reloaded prototype is
// byte-identical to one built at boot. It does NOT touch the cache — the caller swaps it in. A
// nil result means the kind carries no spawnable prototype (e.g. a zone definition). Note: a room
// prototype's display name lives in RoomDTO.Name (short) and its long in RoomDTO.Long, matching
// defineContent's room define call.
func buildPrototype(def content.Definition) *Prototype {
	switch def.Kind {
	case content.KindRoom:
		r := def.Room
		return newPrototype(ProtoRef(r.Ref), nil, r.Name, r.Long, roomComponents(r))
	case content.KindItem, content.KindMob:
		d := def.Proto
		return newPrototype(ProtoRef(d.Ref), d.Keywords, d.Short, d.Long, protoComponents(d))
	default:
		return nil
	}
}

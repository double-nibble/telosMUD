package world

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// scopeSignalQueue bounds the shard's outbound signal-up queue. A signal is a low-rate event (a boss
// died, a gate opened), so a small buffer absorbs a burst; a full queue drops with a log rather than
// blocking the zone goroutine (signal-up is fire-and-forget from the script's view).
const scopeSignalQueue = 256

// scope.go is the ZONE-SIDE half of the cross-zone orchestration (docs/WORLD-EVENTS.md §2, Phase 10.3b):
// each zone keeps a read-only REPLICA of the region/world scope state it cares about, and the shard
// subscribes to the scoped event bus so a director's broadcast DOWN updates that replica. The reads are
// local & cached (Lua world.flag / world.get / region:get, luascope.go); the writes go UP to the director
// (signal_region / signal_world, 10.3c) — never a cross-scope mutation. The golden rule is structural:
// the bus only delivers a MESSAGE, and the zone applies it on its own goroutine (applyScopeDelta is
// posted as a scopeDeltaMsg, exactly like the hot-reload reloadLuaMsg), so the single-writer invariant
// holds one scope level up.

// scopeReplica is a zone's read-only cache of region + world scope state (Phase 10.3b). Owned by the zone
// goroutine: written ONLY by applyScopeDelta (from a posted scopeDeltaMsg) and read ONLY by the Lua
// world.*/region:* surface — both on the zone goroutine, so it needs no lock. regionID is the zone's
// region ("" = none; the zone then has only the world scope).
type scopeReplica struct {
	world    map[string]json.RawMessage
	region   map[string]json.RawMessage
	regionID string
}

// newScopeReplica builds an empty replica (no region). A shard adopting the zone into a region sets
// regionID via WithScopeBus; until a director broadcasts, both maps are empty (every read returns nil).
func newScopeReplica() *scopeReplica {
	return &scopeReplica{
		world:  map[string]json.RawMessage{},
		region: map[string]json.RawMessage{},
	}
}

// scopeDeltaMsg is a director's region/world state broadcast, posted to the zone inbox by the shard's
// scoped-bus subscription so it is applied on the zone goroutine (the golden rule: a message, applied
// locally — never a cross-goroutine write into zone state). kind is "world" or "region".
type scopeDeltaMsg struct {
	kind  string
	key   string
	value json.RawMessage // nil => delete the key
}

func (scopeDeltaMsg) zoneMsg() {}

// scopeSeedMsg carries a FULL scope-state snapshot read from the store, posted to a zone BEFORE the shard's
// scoped-bus subscription activates (#44 snapshot-on-join). It seeds the zone's read-replica so a zone that
// was DOWN when a transient state delta broadcast missed it starts with the authoritative current state,
// rather than empty until the next broadcast of each key. Applied on the zone goroutine (the golden rule).
type scopeSeedMsg struct {
	kind  string // "world" or "region"
	state map[string]json.RawMessage
}

func (scopeSeedMsg) zoneMsg() {}

// applyScopeSeed replaces this zone's replica for the seed's scope with the authoritative store snapshot.
// Runs on the zone goroutine, BEFORE any live delta (the seed is posted before the subscription activates),
// so it is the base state subsequent deltas build on. A region seed for a region-less zone is ignored.
func (z *Zone) applyScopeSeed(m scopeSeedMsg) {
	ns := make(map[string]json.RawMessage, len(m.state))
	for k, v := range m.state {
		if len(v) > 0 && string(v) != "null" {
			ns[k] = v
		}
	}
	switch m.kind {
	case "world":
		z.scopes.world = ns
	case "region":
		if z.scopes.regionID == "" {
			return
		}
		z.scopes.region = ns
	}
}

// scopeEventMsg is a director's REMOTE-EFFECT broadcast (a custom event, not a state set), posted to the
// zone inbox so it fires the zone's on_world/on_region Lua handlers on the zone goroutine. kind is
// "world" or "region".
type scopeEventMsg struct {
	kind    string
	event   string
	payload json.RawMessage
}

func (scopeEventMsg) zoneMsg() {}

// fireScopeEvent fires the zone's on_world/on_region handlers for a director's remote-effect broadcast.
// Runs on the zone goroutine (posted as scopeEventMsg). It delegates to the Lua runtime, which fires every
// scripted entity that registered an on_world("<event>")/on_region("<event>") handler.
func (z *Zone) fireScopeEvent(m scopeEventMsg) {
	if z.lua == nil {
		return
	}
	z.lua.fireScopeEvent(m.kind, m.event, m.payload)
}

// applyScopeDelta updates this zone's replica from a director broadcast. Runs on the zone goroutine. A
// delta for a scope this zone does not track (a region delta on a region-less zone) is ignored — the
// shard subscription only routes a region's deltas to its member zones, but this is a defensive backstop.
func (z *Zone) applyScopeDelta(m scopeDeltaMsg) {
	var tgt map[string]json.RawMessage
	switch m.kind {
	case "world":
		tgt = z.scopes.world
	case "region":
		if z.scopes.regionID == "" {
			return
		}
		tgt = z.scopes.region
	default:
		return
	}
	if len(m.value) == 0 || string(m.value) == "null" {
		delete(tgt, m.key)
		return
	}
	tgt[m.key] = m.value
}

// --- Shard-side subscription plumbing ----------------------------------------------------------

// scopeReplication is the shard's scoped-event-bus wiring (Phase 10.3b): the bus handle, the zone→region
// membership (from region_defs content), and the live subscriptions. It subscribes to the world scope and
// to each region the shard hosts a member of; on a state-set broadcast it posts a scopeDeltaMsg to every
// affected hosted zone. Nil on a shard built without a scoped bus (the single-shard tests + a bare run) —
// such a shard does zero scope work and is byte-identical to a pre-10.3 shard.
type scopeReplication struct {
	bus   *scopebus.Bus
	shard *Shard
	// mu guards zoneRegion/regions/subs, which were construction-immutable until registerZone (16.4a runtime
	// zone-add) can add a hosted zone at runtime. The delivery goroutine reads under RLock.
	mu         sync.RWMutex
	zoneRegion map[string]string // hosted zone id -> its region id (only zones that are in a region)
	regions    map[string]bool   // the distinct regions this shard hosts a member of
	subs       []commbus.Subscription
	signals    chan scopeSignalJob // outbound signal-up queue, drained by signalLoop off the zone goroutine
	log        *slog.Logger
	// snapshot is the authoritative state source read on join to SEED each zone's replica (#44). nil disables
	// seeding (the single-shard tests / a shard with no store) — the pre-#44 behavior (start empty).
	snapshot ScopeSnapshotSource
}

// ScopeSnapshotSource reads the full current world/region scope state, so a joining zone can seed its
// read-replica rather than start empty and miss a delta broadcast while it was down (#44). Satisfied by
// *store.Pool; nil disables snapshot-on-join. It lives here as an interface so the world package stays free
// of the store dependency, mirroring MailReaper / ContentPuller in the director.
type ScopeSnapshotSource interface {
	SnapshotWorldState(ctx context.Context) (map[string][]byte, error)
	SnapshotRegionState(ctx context.Context, regionID string) (map[string][]byte, error)
}

// scopeSignalJob is one queued signal-UP (a zone commanding its region/world director). The event name +
// payload are the script's (signal_region("boss_slain", {...})); the scope is the zone's region or the
// world. Drained by signalLoop, which publishes it on the DURABLE tier so a momentary broker blip never
// loses a state-changing report.
type scopeSignalJob struct {
	scope   scopebus.Scope
	event   string
	payload json.RawMessage
}

// WithScopeBus wires the shard's scoped event bus + region membership (docs/WORLD-EVENTS.md §2, Phase
// 10.3b). bus is the scopebus over the comms transport (cmd/telos-world builds it; a test passes a
// MemBus-backed one). regions is the loaded region_defs content — the shard derives which of its hosted
// zones belong to which region and stamps each member zone's replica.regionID. The subscriptions start in
// Run (startScopeReplication). A nil bus leaves scope replication disabled. Returns the shard for chaining.
func (s *Shard) WithScopeBus(bus *scopebus.Bus, regions []content.RegionDTO) *Shard {
	if bus == nil {
		return s
	}
	zoneRegion := map[string]string{}
	regionSet := map[string]bool{}
	for _, rg := range regions {
		for _, zoneID := range rg.Zones {
			if _, hosted := s.zones[zoneID]; !hosted {
				continue // a region member this shard does not host — another shard replicates it
			}
			zoneRegion[zoneID] = rg.Ref
			regionSet[rg.Ref] = true
		}
	}
	// Stamp each hosted member zone's replica with its region so region:get resolves and a region delta
	// is accepted. Done here (construction, before Run) so it is set before any zone goroutine reads it.
	for zoneID, regionID := range zoneRegion {
		s.zones[zoneID].scopes.regionID = regionID
	}
	s.scopes = &scopeReplication{
		bus:        bus,
		shard:      s,
		zoneRegion: zoneRegion,
		regions:    regionSet,
		signals:    make(chan scopeSignalJob, scopeSignalQueue),
		log:        slog.With("component", "scope-replication"),
	}
	return s
}

// WithScopeSnapshot wires the store the shard reads to SEED each zone's scope replica on join (#44). Without
// it (a shard with no store / the tests) a zone starts with an empty replica and only catches up via live
// broadcasts — the pre-#44 gap. Must be called after WithScopeBus (a no-op otherwise) and before Run.
func (s *Shard) WithScopeSnapshot(src ScopeSnapshotSource) *Shard {
	if s.scopes != nil {
		s.scopes.snapshot = src
	}
	return s
}

// scopeSnapshotTimeout bounds the boot-time snapshot read so a slow/unreachable store degrades to "start
// empty (catch up on the next broadcast)" rather than hanging shard boot.
const scopeSnapshotTimeout = 5 * time.Second

// seedFromSnapshot reads the authoritative world/region state and posts a scopeSeedMsg to each affected
// hosted zone, BEFORE start() subscribes to the live bus (#44). Because the seed is posted to the zone inbox
// ahead of any subscription delivery, it is applied first and later deltas build on it. A snapshot read
// error degrades to "no seed" (the zone catches up on the next broadcast). Runs on the shard-start goroutine.
//
// ORDERING: Run calls this AFTER the zone actor loops are launched but BEFORE start()'s subscribe. The
// post here is the BLOCKING z.post (a seed must never be dropped), so it is only safe once a drainer exists
// — seeding before the actors launch could wedge boot if an inbox filled (e.g. mass inbound handoff Prepare
// during a failover) past its buffer before any zone loop drained it. Seeds still precede every live delta
// because the subscriptions that produce deltas do not exist until start() runs, after this returns.
//
// RESIDUAL: a state delta broadcast in the narrow window between this snapshot read and the subscription
// becoming active is missed (the transient tier has no backlog) — permanently, until that key is broadcast
// again. This closes the DOMINANT gap (a delta missed across the whole time a zone was down) and shrinks the
// residual to that boot-time window; a version-stamped snapshot+delta merge would close it fully (follow-up).
func (sr *scopeReplication) seedFromSnapshot() {
	if sr == nil || sr.snapshot == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), scopeSnapshotTimeout)
	defer cancel()

	// World scope → every hosted zone.
	if raw, err := sr.snapshot.SnapshotWorldState(ctx); err != nil {
		sr.log.Warn("world state snapshot failed; zones start without seeded world state", "err", err)
	} else {
		seed := toRawMap(raw)
		for _, z := range sr.shard.zonesList() {
			z.post(scopeSeedMsg{kind: "world", state: seed})
		}
	}

	// Region scopes → the member zones of each hosted region.
	sr.mu.RLock()
	regions := make([]string, 0, len(sr.regions))
	for r := range sr.regions {
		regions = append(regions, r)
	}
	zoneRegion := make(map[string]string, len(sr.zoneRegion))
	for z, r := range sr.zoneRegion {
		zoneRegion[z] = r
	}
	sr.mu.RUnlock()

	for _, regionID := range regions {
		raw, err := sr.snapshot.SnapshotRegionState(ctx, regionID)
		if err != nil {
			sr.log.Warn("region state snapshot failed; zones start without seeded region state", "region", regionID, "err", err)
			continue
		}
		seed := toRawMap(raw)
		for zoneID, rID := range zoneRegion {
			if rID != regionID {
				continue
			}
			if z := sr.shard.zoneByID(zoneID); z != nil {
				z.post(scopeSeedMsg{kind: "region", state: seed})
			}
		}
	}
}

// toRawMap converts a store snapshot ([]byte values) to the replica's json.RawMessage form.
func toRawMap(in map[string][]byte) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		out[k] = json.RawMessage(v)
	}
	return out
}

// startScopeReplication subscribes to the world scope and each hosted region, routing a state-set
// broadcast to the affected zones. Called once from Run (after the zones are adopted). A subscribe error
// is logged and skipped — scope replication degrades to "no updates" rather than failing the shard boot.
//
// CONCURRENCY: since the #44 seed-before-subscribe reorder, start() runs AFTER the shard is Run-ning, so a
// runtime HostZone→registerZone can mutate sr.regions / sr.subs concurrently. So read the boot region set
// under the lock and guard every sr.subs append with it, matching registerZone's discipline. (A boot region
// is populated at construction and never overlaps a genuinely-new runtime region, so no double-subscribe.)
func (sr *scopeReplication) start() {
	if sr == nil {
		return
	}
	// World scope: every hosted zone replicates world state, so a world delta fans out to all of them.
	if sub, err := sr.bus.Subscribe(scopebus.World(), func(event string, payload json.RawMessage, _ string) {
		sr.onScopeEvent("world", "", event, payload)
	}); err != nil {
		sr.log.Warn("world scope subscribe failed; world state will not replicate", "err", err)
	} else {
		sr.mu.Lock()
		sr.subs = append(sr.subs, sub)
		sr.mu.Unlock()
	}
	// Snapshot the boot region set under the lock, then subscribe outside it (registerZone may be adding a
	// runtime region concurrently).
	sr.mu.RLock()
	regions := make([]string, 0, len(sr.regions))
	for regionID := range sr.regions {
		regions = append(regions, regionID)
	}
	sr.mu.RUnlock()
	// Each region this shard hosts a member of: a region delta routes only to that region's member zones.
	for _, rid := range regions {
		rid := rid
		if sub, err := sr.bus.Subscribe(scopebus.Region(rid), func(event string, payload json.RawMessage, _ string) {
			sr.onScopeEvent("region", rid, event, payload)
		}); err != nil {
			sr.log.Warn("region scope subscribe failed; region state will not replicate", "region", rid, "err", err)
		} else {
			sr.mu.Lock()
			sr.subs = append(sr.subs, sub)
			sr.mu.Unlock()
		}
	}
}

// adoptSeedTimeout bounds the snapshot read on the ADOPTION path. It is deliberately tighter than the boot
// budget: AdoptZone sits on the drain critical path, the draining source blocks on it, and BeginDrain hands
// zones over serially — so every second here is a second of drain the deadline does not get back. A slow
// snapshot store is also correlated with the failover that triggered the drain. Degrading to "no seed"
// quickly beats stalling the drain into reclaim-from-durable, which drops players to a reconnect.
const adoptSeedTimeout = 2 * time.Second

// seedZone seeds ONE runtime-hosted zone's scope replica from the authoritative store (#280), closing the
// #44 residual for drain adoption — which is precisely the failover moment scope replication exists to
// survive. Without it a drain-adopted zone starts with an EMPTY replica and learns each world/region key
// only when it is next broadcast, so a sticky world flag ("war active") reads false there, possibly forever.
//
// IT MUST BE CALLED BEFORE THE ZONE IS EXPOSED TO ANY LIVE DELTA — before Shard.adoptLocked puts it in
// s.zones (world deltas fan out over zonesList), and therefore before registerZone adds it to zoneRegion
// (region deltas route through that map). applyScopeSeed is a full-map REPLACE, so a seed landing after a
// delta would clobber newer state. Posting first makes the inbox do the ordering: the seed is consumed
// first and every later delta builds on it. This is why the fix is not a seedFromSnapshot call inside
// registerZone.
//
// ctx is the ADOPTING RPC's context, so a source shard that gives up on the handoff also cancels this read
// rather than leaving it to run out its own clock.
//
// The WORLD snapshot is read LAST, immediately before returning, because the caller exposes the zone as its
// next act. Anything read earlier is stale for however long the rest of this function takes — and the world
// scope is the one every zone subscribes to. The region read therefore goes first and absorbs that staleness,
// exactly as it does at boot, where seedFromSnapshot reads every region before subscribing.
//
// The blocking posts are safe: the zone's actor has not started, so nothing drains its inbox, but nothing
// has FILLED it either — buildZone posts no messages to the zone's own inbox — so the 256-slot buffer is
// empty and two posts cannot block. (seedFromSnapshot's doc warns that a blocking post needs a live drainer;
// here the guarantee comes from the other side, an empty buffer. If buildZone ever starts posting, revisit.)
//
// A snapshot read error degrades to "no seed" (catch up on the next broadcast), exactly as at boot. A nil
// replication or nil snapshot source (single-shard tests, a shard with no store) is a clean no-op.
func (sr *scopeReplication) seedZone(ctx context.Context, z *Zone) {
	if sr == nil || sr.snapshot == nil || z == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, adoptSeedTimeout)
	defer cancel()

	// Region first: its staleness window is the one we can afford to widen.
	regionID := sr.resolveRegionOnce(z)
	var regionSeed map[string]json.RawMessage
	if regionID != "" {
		if raw, err := sr.snapshot.SnapshotRegionState(ctx, regionID); err != nil {
			sr.log.Warn("region state snapshot failed; adopted zone starts without seeded region state",
				"zone", z.id, "region", regionID, "err", err)
		} else {
			regionSeed = toRawMap(raw)
		}
	}

	// World last: the caller exposes the zone immediately after we return, so this read is as fresh as we
	// can make it, and the window in which a world delta can slip past the seed is as small as we can make it.
	if raw, err := sr.snapshot.SnapshotWorldState(ctx); err != nil {
		sr.log.Warn("world state snapshot failed; adopted zone starts without seeded world state",
			"zone", z.id, "err", err)
	} else {
		z.post(scopeSeedMsg{kind: "world", state: toRawMap(raw)})
	}
	if regionSeed != nil {
		z.post(scopeSeedMsg{kind: "region", state: regionSeed})
	}
}

// registerZone brings a RUNTIME-hosted zone (HostZone / a drain adoption, 16.4a) into scope replication: it
// stamps the zone's region-id replica, adds it to the region delivery map, and SUBSCRIBES to its region if
// this shard wasn't already a member — so region deltas reach a zone hosted after boot (world deltas already
// fan out to every hosted zone via zonesList). MUST be called before the zone's actor starts, so the
// regionID stamp isn't a data race with a region:get on the zone goroutine. A zone in no region, or a nil
// replication (no scoped bus), is a no-op.
//
// The zone's replica was already SEEDED by seedZone, which HostZone calls before adoptLocked exposes the
// zone to any delta (#280). Do NOT seed here: by this point the zone is in s.zones, so a world delta may
// already be sitting in its inbox, and applyScopeSeed's full-map replace would clobber it with older state.
func (sr *scopeReplication) registerZone(z *Zone) {
	if sr == nil {
		return
	}
	regionID := sr.resolveRegionOnce(z)
	if regionID == "" {
		return // not a region member; world-scope deltas still reach it via zonesList
	}
	z.scopes.regionID = regionID // safe: the caller invokes this BEFORE z.Run starts
	sr.mu.Lock()
	sr.zoneRegion[z.id] = regionID
	newRegion := !sr.regions[regionID]
	sr.regions[regionID] = true
	sr.mu.Unlock()
	if newRegion {
		rid := regionID
		if sub, err := sr.bus.Subscribe(scopebus.Region(rid), func(event string, payload json.RawMessage, _ string) {
			sr.onScopeEvent("region", rid, event, payload)
		}); err != nil {
			sr.log.Warn("runtime region scope subscribe failed", "region", rid, "zone", z.id, "err", err)
		} else {
			sr.mu.Lock()
			sr.subs = append(sr.subs, sub)
			sr.mu.Unlock()
		}
	}
	sr.log.Debug("registered runtime-hosted zone for scope replication", "zone", z.id, "region", regionID)
}

// regionFor returns the region a live zone belongs to, resolving by id and falling back to the zone's
// TEMPLATE (#411).
//
// region_defs list AUTHORED zone refs, so an instance's synthetic id can never appear in one and a raw
// regionForZone(z.id) misses. The consequence is not "no region" but SILENT INERTNESS: region:get reads
// empty inside every copy of a dungeon, so content authored against the template — a war flag, a world-event
// gate — works perfectly in playtest and does nothing in every instance, with no error anywhere to find it by.
//
// This is the READ direction only, and only the read direction may widen to the template. Writes UP (a zone
// commanding its director) stay refused: the signal envelope carries no source, so a director cannot tell one
// private party's progress from the shared world's, and letting an instance report up would let a party
// drive shared world state by farming a copy. See scopeSignalRegion.
func (sr *scopeReplication) regionFor(z *Zone) string {
	if z == nil {
		return ""
	}
	if r := sr.regionForZone(z.id); r != "" {
		return r
	}
	return sr.regionForZone(z.template)
}

// resolveRegionOnce answers "which region is this zone in" ONCE per zone build, caching the answer on the
// zone, and it is the only thing seedZone and registerZone may ask.
//
// Both run on the same goroutine before the zone's actor starts — seedZone first, registerZone after — but
// a full buildZone plus a store round trip sits between them. Since #418 the content snapshot they resolve
// against can be SWAPPED in that gap, so two independent reads can return two different answers, and the
// two answers are used for different things: seedZone picks which region's state to seed the replica from,
// registerZone stamps which region's deltas the zone will receive. Disagreement is not a stale value, it is
// a zone permanently holding one region's state while subscribed to another's — or, when the second read
// returns "", holding a region's values while subscribed to nothing, which is exactly the sticky-stale
// region gate (#44) seedZone exists to prevent, with no error anywhere to show for it.
//
// Caching on z.scopes.regionID is safe for the same reason registerZone's stamp is: strictly before z.Run.
func (sr *scopeReplication) resolveRegionOnce(z *Zone) string {
	if z == nil {
		return ""
	}
	if z.scopes.regionID != "" {
		return z.scopes.regionID
	}
	regionID := sr.regionFor(z)
	z.scopes.regionID = regionID
	return regionID
}

// regionForZone returns the region id a zone belongs to per the shard's loaded region_defs, or "".
func (sr *scopeReplication) regionForZone(zoneID string) string {
	if sr.shard == nil {
		return ""
	}
	// The LIVE snapshot (#418), not a boot-time capture: region membership is read straight off the content
	// here rather than registered into one of the atomic-swap def registries at construction, so a refreshed
	// snapshot is simply the current answer. A zone's stamped regionID is still taken once at registerZone,
	// so — as everywhere else the refresh reaches — the change lands on zones registered from here on.
	lc := sr.shard.liveContent()
	if lc == nil {
		return ""
	}
	for _, rg := range lc.Regions {
		for _, z := range rg.Zones {
			if z == zoneID {
				return rg.Ref
			}
		}
	}
	return ""
}

// onScopeEvent routes a director broadcast to the affected zones. Runs OFF the zone goroutines (a bus-
// owned goroutine), so it only ever POSTS — it never touches zone state. The reserved EventStateSet is a
// STATE delta (updates the read-replica); any OTHER event is a REMOTE EFFECT (10.4b) that fires the
// zones' on_world/on_region Lua handlers.
func (sr *scopeReplication) onScopeEvent(kind, regionID, event string, payload json.RawMessage) {
	var m msg
	switch event {
	case scopebus.EventStateSet:
		var p scopebus.StatePayload
		if err := json.Unmarshal(payload, &p); err != nil || p.Key == "" {
			sr.log.Debug("dropping malformed scope state delta", "kind", kind, "event", event)
			return
		}
		m = scopeDeltaMsg{kind: kind, key: p.Key, value: p.Value}
		// A STATE delta reaches EVERY zone including instances: an instance reads world/region state exactly
		// like its template (regionFor resolves its region by template for the same reason). Pass "" so the
		// instance filter below never withholds one — state is the read direction, and reads are allowed.
		sr.postToScopeZones(kind, regionID, "", m)
		return
	case contentbus.PullResultEvent:
		// Operator feedback for a coordinated pull (#230): NOT a Lua on_world effect — the shard consumes
		// it to tell the requesting builder how their `pull` settled, then stops (no on_world fan-out).
		sr.deliverPullResult(payload)
		return
	default:
		m = scopeEventMsg{kind: kind, event: event, payload: payload}
	}
	sr.postToScopeZones(kind, regionID, event, m)
}

// deliverPullResult fans a director's pull-outcome broadcast (#230) to hosted zones as a pullResultMsg;
// the zone that still hosts the requesting builder shows them the pass/fail line, every other zone no-ops.
// A malformed payload or a blank actor is dropped (nothing to deliver). Runs off the zone goroutine — it
// only POSTS, matching the golden rule.
func (sr *scopeReplication) deliverPullResult(payload json.RawMessage) {
	var r contentbus.PullResult
	if err := json.Unmarshal(payload, &r); err != nil || r.Actor == "" {
		sr.log.Debug("dropping malformed pull result", "event", contentbus.PullResultEvent)
		return
	}
	var summary string
	switch {
	case r.OK && r.Detail != "":
		// A FORCED pull (#427): Detail carries the packs the live-hosted-pack guard blocked and the operator
		// overrode. Naming them here is the whole point of running the guard under force — an override the
		// operator cannot see the consequences of is not an audited action.
		summary = fmt.Sprintf("pull: content version %q installed and hot-reloaded across the fleet.\r\n"+
			"  FORCE-PRUNED past the live-hosted-pack guard: %s\r\n"+
			"  Players inside those zones keep playing from shard memory for now, but a drain or restart "+
			"CANNOT rebuild the zones — its occupants are dropped and reclaimed to their home start room. "+
			"REDIRECT them first, THEN roll a reboot. Also check no world shard pins these packs via "+
			"TELOS_CONTENT_PACKS, or it will refuse to boot.", r.Version, r.Detail)
	case r.OK:
		summary = fmt.Sprintf("pull: content version %q installed and hot-reloaded across the fleet.", r.Version)
	case r.Version == "":
		summary = fmt.Sprintf("pull: request rejected — %s", r.Detail)
	default:
		summary = fmt.Sprintf("pull: content version %q was not installed — %s", r.Version, r.Detail)
	}
	// A pull result is always a WORLD-scope broadcast (the world director owns pulls), so it fans to every
	// hosted zone; the one hosting the builder delivers. postOrDrop (not the blocking z.post the state/effect
	// fan-out uses) keeps this best-effort operator notice from ever stalling the bus goroutine on a full
	// zone inbox — a dropped notice is acceptable (the reload readout it mirrors is best-effort too).
	m := pullResultMsg{player: r.Actor, summary: summary}
	for _, z := range sr.shard.zonesList() {
		z.postOrDrop(m)
	}
}

// reservedScheduleEvents are the ENGINE-RESERVED scoped events a runtime-minted instance never receives
// (#411). Today that is the scheduled-boss spawn the director's schedule loop broadcasts
// (director.SpawnBossEvent — duplicated as a literal because world must not import director; the direction
// of that dependency is director -> world).
//
// WHY. A schedule is a WORLD-scope object with world-scope scarcity: one boss, one loot table, on one timer.
// The broadcast fans out to every hosted zone, so with instances live it would spawn that boss in the
// template AND in every private copy of it simultaneously. Each kill then signals boss.died back up, which
// reschedules the ONE shared world timer, last-writer-wins. The scarce thing stops being scarce and the
// shared timer becomes player-drivable.
//
// It is a deliberately NARROW list — reserved engine events only. A content-authored world event still
// reaches instances, because the engine cannot know whether the author means it to; mud.zone() (luamud.go) is
// how content expresses that choice for its own events.
var reservedScheduleEvents = map[string]bool{
	"spawn.boss": true,
}

// deliverableToInstance reports whether a scoped event may be delivered to an instance zone.
func deliverableToInstance(event string) bool { return !reservedScheduleEvents[event] }

// ReservedScheduleEvent reports whether event is engine-reserved and therefore withheld from instance zones.
//
// Exported for ONE reason: reservedScheduleEvents duplicates director.SpawnBossEvent as a string literal
// (world must not import director — the dependency runs director -> world), and a duplicated literal with no
// link between the two ends is a silent-break waiting to happen. Rename the director's constant and the
// exclusion here simply stops matching: the boss fan-out this list exists to stop is re-enabled, in every
// live instance, with no error and no failing test anywhere. internal/director's parity test binds the two
// through this function so that rename fails the build instead.
func ReservedScheduleEvent(event string) bool { return reservedScheduleEvents[event] }

// postToScopeZones posts m to every zone the scope addresses: a world scope to all hosted zones, a region
// scope only to that region's hosted member zones. `event` is "" for a STATE delta, which every zone
// receives — an instance reads region/world state exactly like its template (that is what regionFor is for);
// only reserved remote EFFECTS are withheld from it.
func (sr *scopeReplication) postToScopeZones(kind, regionID, event string, m msg) {
	if kind == "world" {
		for _, z := range sr.shard.zonesList() {
			if z.isInstance() && !deliverableToInstance(event) {
				sr.log.Debug("withholding a reserved scoped event from a zone instance",
					"zone", z.id, "template", z.template, "event", event)
				continue
			}
			z.post(m)
		}
		return
	}
	sr.mu.RLock()
	targets := make([]string, 0, len(sr.zoneRegion))
	for zoneID, rgID := range sr.zoneRegion {
		if rgID == regionID {
			targets = append(targets, zoneID)
		}
	}
	sr.mu.RUnlock()
	for _, zoneID := range targets {
		z := sr.shard.zoneByID(zoneID)
		if z == nil {
			continue
		}
		if z.isInstance() && !deliverableToInstance(event) {
			sr.log.Debug("withholding a reserved scoped event from a zone instance",
				"zone", z.id, "template", z.template, "event", event)
			continue
		}
		z.post(m)
	}
}

// unregisterZone drops a torn-down zone (#288 UnhostZone) from the region delivery map. The shard's region
// SUBSCRIPTION stays: other hosted zones may share the region, and a zone this shard re-adopts later would
// have to re-subscribe anyway. A stale zoneRegion entry would be harmless today — postToScopeZones resolves
// each target through zoneByID, which returns nil for an unhosted zone — but leaving it would make the map
// grow without bound across rebalances, which is the very leak UnhostZone exists to close. nil-safe.
func (sr *scopeReplication) unregisterZone(zoneID string) {
	if sr == nil {
		return
	}
	sr.mu.Lock()
	delete(sr.zoneRegion, zoneID)
	sr.mu.Unlock()
}

// stop unsubscribes every scope subscription (called at Run teardown). Idempotent.
func (sr *scopeReplication) stop() {
	if sr == nil {
		return
	}
	sr.mu.Lock()
	subs := sr.subs
	sr.subs = nil
	sr.mu.Unlock()
	for _, sub := range subs {
		_ = sub.Unsubscribe()
	}
}

// --- Signal UP (a zone commands its director) --------------------------------------------------

// enqueueSignal hands a signal-up job to the shard's drain loop WITHOUT blocking the zone goroutine
// (the publish does network I/O — it must never run on a zone actor). A full queue drops with a log
// rather than stalling the simulation. Concurrency-safe; called from a zone goroutine via the Lua
// signal_region/signal_world builtins. A nil replication (no scoped bus) silently no-ops.
func (sr *scopeReplication) enqueueSignal(j scopeSignalJob) {
	if sr == nil {
		return
	}
	select {
	case sr.signals <- j:
	default:
		sr.log.Warn("signal-up queue full; dropping", "event", j.event, "scope", j.scope.Label())
	}
}

// signalLoop is the shard's SINGLE signal-up publisher (started by Shard.Run). It drains the queue and
// publishes each signal on the DURABLE tier so a state-changing report (a boss slain) survives a broker
// blip and a director restart — the at-least-once half of the golden rule. A bus with no durable tier
// (no JetStream wired) makes every publish a logged no-op, never a crash (the never-fatal posture,
// mirroring the durable-tell publishLoop). Runs off every zone goroutine.
func (sr *scopeReplication) signalLoop(ctx context.Context) {
	if sr == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-sr.signals:
			if err := sr.bus.SignalDurable(ctx, j.scope, j.event, j.payload); err != nil {
				sr.log.Warn("signal-up publish failed; dropped", "event", j.event, "scope", j.scope.Label(), "err", err)
			}
		}
	}
}

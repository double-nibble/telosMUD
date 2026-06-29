package world

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
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
	bus        *scopebus.Bus
	shard      *Shard
	zoneRegion map[string]string // hosted zone id -> its region id (only zones that are in a region)
	regions    map[string]bool   // the distinct regions this shard hosts a member of
	subs       []commbus.Subscription
	signals    chan scopeSignalJob // outbound signal-up queue, drained by signalLoop off the zone goroutine
	log        *slog.Logger
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

// startScopeReplication subscribes to the world scope and each hosted region, routing a state-set
// broadcast to the affected zones. Called once from Run (after the zones are adopted). A subscribe error
// is logged and skipped — scope replication degrades to "no updates" rather than failing the shard boot.
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
		sr.subs = append(sr.subs, sub)
	}
	// Each region this shard hosts a member of: a region delta routes only to that region's member zones.
	for regionID := range sr.regions {
		rid := regionID
		if sub, err := sr.bus.Subscribe(scopebus.Region(rid), func(event string, payload json.RawMessage, _ string) {
			sr.onScopeEvent("region", rid, event, payload)
		}); err != nil {
			sr.log.Warn("region scope subscribe failed; region state will not replicate", "region", rid, "err", err)
		} else {
			sr.subs = append(sr.subs, sub)
		}
	}
}

// onScopeEvent routes a director broadcast to the affected zones. Runs OFF the zone goroutines (a bus-
// owned goroutine), so it only ever POSTS — it never touches zone state. The reserved EventStateSet is a
// STATE delta (updates the read-replica); any OTHER event is a REMOTE EFFECT (10.4b) that fires the
// zones' on_world/on_region Lua handlers.
func (sr *scopeReplication) onScopeEvent(kind, regionID, event string, payload json.RawMessage) {
	var m msg
	if event == scopebus.EventStateSet {
		var p scopebus.StatePayload
		if err := json.Unmarshal(payload, &p); err != nil || p.Key == "" {
			sr.log.Debug("dropping malformed scope state delta", "kind", kind, "event", event)
			return
		}
		m = scopeDeltaMsg{kind: kind, key: p.Key, value: p.Value}
	} else {
		m = scopeEventMsg{kind: kind, event: event, payload: payload}
	}
	sr.postToScopeZones(kind, regionID, m)
}

// postToScopeZones posts m to every zone the scope addresses: a world scope to all hosted zones, a region
// scope only to that region's hosted member zones.
func (sr *scopeReplication) postToScopeZones(kind, regionID string, m msg) {
	if kind == "world" {
		for _, z := range sr.shard.zones {
			z.post(m)
		}
		return
	}
	for zoneID, rgID := range sr.zoneRegion {
		if rgID == regionID {
			sr.shard.zones[zoneID].post(m)
		}
	}
}

// stop unsubscribes every scope subscription (called at Run teardown). Idempotent.
func (sr *scopeReplication) stop() {
	if sr == nil {
		return
	}
	for _, sub := range sr.subs {
		_ = sub.Unsubscribe()
	}
	sr.subs = nil
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

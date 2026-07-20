package world

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/double-nibble/telosmud/internal/content"
)

// reloadvalidate.go — pre-publish content PACK-HEALTH gate for `reload` (#192, Option B: shard-local
// validate-before-broadcast). Before a shard broadcasts a content reload fleet-wide, it dry-run-checks the
// re-read pack(s) and REJECTS the publish (naming the problems) when the content is DEFINITIVELY BROKEN —
// so a bad edit can't be propagated. Because every shard re-reads the same source, gating the publish at the
// triggering shard gates the whole fleet (shared-source convergence), which is why this needs no central
// director tier (see the #192 design note).
//
// WHAT "definitively broken" MEANS HERE — an honest scope note (mudlib review). Boot's builders are
// deliberately FAIL-SAFE: a bad attribute formula is logged and registered with a zero base, an attribute
// cycle is logged, a room/item/mob/channel always builds SOMETHING (buildChannelDef/buildPrototype
// log-and-continue). So "boot would build it" is nearly always true — boot never aborts. This gate is
// therefore STRICTER than boot on purpose: it rejects content boot would tolerate-but-degrade, on the
// theory that a builder should FIX a broken pack rather than propagate a silently-degraded one. It reuses
// boot's two ERROR-RETURNING functions (parseAttributeBase + lintAttributeCycles) as the faithful signal.
//
// COVERAGE: the attribute checks cover a BOOT concern (attributes do NOT hot-swap on this path — they need
// a rolling reboot), so they are a pack-HEALTH proxy ("is this pack sound to push?"). validateChannels
// (#197 slice 1) adds real PAYLOAD validation for the FIRST propagated kind: channels DO hot-swap here
// (reloadChannel/buildChannelDef), so a structurally-dead channel is rejected against the same build path.
// validateRoomExits (#197 slice 2) adds the FIRST room-graph payload check: a dangling exit — one pointing
// to a room that provably does not exist. Rooms also hot-swap here (the per-ref applier + resyncRoom, #191),
// so an edit that adds a room with a broken exit is caught before it propagates. validateResets (#197 slice
// 2b) checks zone reset references (unknown op / undefined target room / undefined prototype) — a pack-HEALTH
// check (resets don't hot-swap; they run at boot/repop), catching a broken edit before broadcast.
// validateProtoRefs (#197 slice 2c) rejects a prototype ref collision — two rooms/items/mobs sharing a ref
// silently collapse to one in the shared cache. FULL-GRAPH validation (#205): every check now resolves refs
// against the WHOLE enabled pack set (so a cross-pack attribute cycle and a cross-zone dangling exit/reset —
// previously invisible to a scoped `reload <onepack>` — are caught), while REJECTING only findings a reloaded
// (in-scope) pack contributes to the merged graph (provenance-by-last-writer, see reloadScope). The framework
// — collect problems -> gate the publish -> report — is what each slots into.
//
// Validation is PURE over the parsed DTOs (it reuses parseAttributeBase + lintAttributeCycles and builds a
// THROWAWAY attributeDef map — never the shard's live registries), so it is safe on the off-zone-goroutine
// republish path and never touches live state. Only definitively-broken content blocks; a transient re-read
// blip stays best-effort (handled in republish), preserving reload's optional posture.

// reloadOutcome is the result of a republish attempt, shaped so the builder readout can distinguish the
// three cases: a clean propagation, a validation REJECTION (nothing published — the content is broken), and
// a best-effort INFRA failure (a re-read/publish blip; the applier's per-ref fail-safe is the backstop).
type reloadOutcome struct {
	published  int      // invalidations put on the wire (0 when rejected or check-only)
	rejected   []string // validation problems; NON-EMPTY => nothing was published (a hard content gate)
	failed     bool     // an infrastructure failure (re-read/publish error) — logged, best-effort
	checkOnly  bool     // true => a `reload --check` dry run that validated OK and deliberately published nothing
	sharedDefs []string // #56: shared-def kinds present in the reloaded packs that are NOT hot-applied (rolling-reboot reminder)
	advisories []string // #309: ADVISORY (non-blocking) heads-ups — a NOT-reloaded pack references a room/proto THIS reload removes
}

// sharedDefKinds returns the sorted, de-duplicated labels of the pack-GLOBAL "shared def" kinds present in
// the reloaded packs that have NO hot-reload loop — the contentbus only emits room/item/mob/zone/channel
// invalidations (contentbus/publish.go), so editing one of these takes effect only after a ROLLING REBOOT of
// the world shards. This is the settled design (#56/#53: a live z.defs swap is optional, not required). The
// reload readout uses this to REMIND the operator, so a shared-def edit is never silently un-applied.
//
// SOURCE OF TRUTH: this list must mirror the pack-global registrations in defineGlobals (build.go) MINUS the
// ones with a hot-reload loop (channels, via KindChannel) and MINUS the ones the world shard doesn't hold
// (regions / spawn schedules / chargens are consumed by directors / telos-account — a WORLD reboot wouldn't
// apply to them). When a new pack-global def kind is added to defineGlobals, add it here too.
func sharedDefKinds(loaded []content.Pack) []string {
	seen := map[string]bool{}
	for i := range loaded {
		pk := &loaded[i]
		mark := func(label string, present bool) {
			if present {
				seen[label] = true
			}
		}
		mark("attributes", len(pk.Attributes) > 0)
		mark("resources", len(pk.Resources) > 0)
		mark("damage types", len(pk.DamageTypes) > 0)
		mark("affects", len(pk.Affects) > 0)
		mark("abilities", len(pk.Abilities) > 0)
		mark("combat profiles", len(pk.CombatProfiles) > 0 || pk.DefaultCombat != "")
		mark("progression tracks", len(pk.Tracks) > 0)
		mark("bundles", len(pk.Bundles) > 0)
		mark("rarity tiers", len(pk.RarityTiers) > 0)
		mark("affixes", len(pk.Affixes) > 0)
		mark("loot tables", len(pk.LootTables) > 0)
		mark("recipes", len(pk.Recipes) > 0)
		mark("wear slots", len(pk.WearSlots) > 0)
		mark("trust tiers", len(pk.TrustTiers) > 0)
		mark("custom commands", len(pk.Commands) > 0)
		mark("player toggles", len(pk.ToggleDefs) > 0)
		mark("display templates", len(pk.DisplayDefs) > 0)
		mark("ruleset formulas", len(pk.Formulas) > 0)
		mark("pvp policy", pk.PvpLua != "")
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reloadScope carries the FULL enabled content graph as validation CONTEXT plus the PROVENANCE that attributes
// a finding back to a reloaded pack (#205). validatePacks resolves refs against `full` — so a CROSS-PACK
// attribute cycle or a CROSS-ZONE dangling exit/reset now resolves (previously invisible to a scoped reload,
// which only saw the in-scope packs) — but REJECTS only a finding whose rooted unit's LAST-WRITER pack is in
// `scoped`. That gives both invariants at once: no false positives (an unrelated broken pack B never blocks a
// valid `reload A`), and full coverage (a defect A actually contributes to the merged graph is caught).
//
// Provenance is last-writer-wins, matching what content.Load makes live: attributes + channels merge by ref
// (a later enabled pack's def overrides an earlier one, so the earlier is INERT and its defects are not A's
// problem), and zones merge whole by ref (a room/exit/reset's owner is its owning zone's last-writer).
type reloadScope struct {
	full      []content.Pack
	scoped    map[string]bool   // pack names being reloaded (a finding is rejectable only if rooted in one of these)
	attrOwner map[string]string // attribute ref -> last-writer pack
	zoneOwner map[string]string // zone ref -> last-writer pack (a room/exit/reset inherits its zone's owner)
	chanOwner map[string]string // channel ref -> last-writer pack
}

func newReloadScope(full []content.Pack, scoped map[string]bool) *reloadScope {
	s := &reloadScope{
		full: full, scoped: scoped,
		attrOwner: map[string]string{}, zoneOwner: map[string]string{}, chanOwner: map[string]string{},
	}
	for i := range full {
		pk := &full[i]
		for _, a := range pk.Attributes {
			s.attrOwner[a.Ref] = pk.Pack
		}
		for _, z := range pk.Zones {
			s.zoneOwner[z.Ref] = pk.Pack
		}
		for _, ch := range pk.Channels {
			if strings.TrimSpace(ch.Ref) != "" {
				s.chanOwner[ch.Ref] = pk.Pack
			}
		}
	}
	return s
}

func (s *reloadScope) inScope(pack string) bool { return s.scoped[pack] }

// zoneInScope reports whether a zone's LIVE definition (its last-writer) is among the reloaded packs — the
// provenance gate for a room/exit/reset finding, which is rooted in its owning zone.
func (s *reloadScope) zoneInScope(zoneRef string) bool { return s.inScope(s.zoneOwner[zoneRef]) }

// liveZones returns the merged zone set content.Load builds: whole-zone last-write-wins by ref, first-seen
// order preserved. Judging over the LIVE zones (not every pack's copy) is what makes the cross-zone checks
// resolve against the real world graph and attribute a finding to the zone's actual owner.
func (s *reloadScope) liveZones() []content.ZoneDTO {
	byRef := map[string]content.ZoneDTO{}
	var order []string
	for i := range s.full {
		for _, z := range s.full[i].Zones {
			if _, seen := byRef[z.Ref]; !seen {
				order = append(order, z.Ref)
			}
			byRef[z.Ref] = z
		}
	}
	out := make([]content.ZoneDTO, 0, len(order))
	for _, ref := range order {
		out = append(out, byRef[ref])
	}
	return out
}

// validatePacks returns one human-readable problem per definitively-broken thing that a reloaded (in-scope)
// pack CONTRIBUTES to the FULL merged graph, or nil when the reloaded content is sound. A non-empty result
// MUST block the publish. `full` is the whole enabled pack set (resolution context, in enabled/boot order for
// last-writer provenance); `scoped` is the set of pack names being reloaded. On a bare `reload` (full == all,
// scoped == all) every finding is rejectable, so behaviour is identical to the pre-#205 scoped-only pass.
func validatePacks(full []content.Pack, scoped map[string]bool) []string {
	s := newReloadScope(full, scoped)
	return append(zoneGraphProblems(s), sharedDefProblems(s)...)
}

// validateSnapshotPacks is the #423 CONTENT-SNAPSHOT gate: the same checks, split by whether a finding can
// make the published snapshot unsafe to BUILD A ZONE FROM. Rejectable findings block the publish; the rest
// are returned as warnings and merely logged.
//
// # The rule that generated the split, so it is derivable rather than a preference
//
// A check is REJECTABLE here iff a finding in it can make the published snapshot unsafe to build a zone
// from. That rule follows from what the snapshot is actually consumed as. It has exactly three readers —
// HostZone, MintInstance and regionForZone — and between them they read zone existence, the room-ref set,
// StartRoom, Resets/ResetSecs, Instanceable, and Regions. It is a ZONE GRAPH, and nothing else.
//
// # Why the demoted checks are not merely lower-priority — they protect NOTHING on this path
//
// This is the load-bearing half. Attributes, resources, abilities, formulas and the trust ladder are
// registered by defineGlobals, which runs at BOOT ONLY; a snapshot refresh re-registers nothing (setContent
// is a bare atomic store). Channels hot-swap through their own KindChannel path off the staff `reload`,
// which has its own gate and its own narrow #205 scoping, and never consult the snapshot at all.
//
// So a trust-ladder finding here cannot prevent a single bad ladder from reaching a running world: the only
// path a ladder takes is a RESTART, and boot deliberately does not gate (ReportBootContentProblems — see
// bootvalidate.go for why refusing at boot would turn a content defect into an outage). Rejecting on it
// would be pure veto surface: no protective value, and — because the snapshot scope is every enabled pack —
// the power to freeze every pack's zone graph fleet-wide over a finding in one pack's rows.
//
// Detection is NOT reduced by any of this. The demoted findings are still computed and still logged, and
// boot still runs the FULL validatePacks at Error. What changes is only which findings hold a veto.
//
// validatePacks itself is deliberately untouched: the staff `reload` gate and the boot report keep full
// strictness. A `reload` is a human deliberately propagating an edit and can afford to be refused; the
// refresh is an automatic background reaction to somebody else's content event and cannot.
func validateSnapshotPacks(full []content.Pack, scoped map[string]bool) (reject, warn []string) {
	s := newReloadScope(full, scoped)
	return zoneGraphProblems(s), sharedDefProblems(s)
}

// zoneGraphProblems returns findings that make the merged content unsafe to BUILD A ZONE FROM — the subset
// the snapshot gate rejects on. See validateSnapshotPacks for the rule.
//
//   - room exits: a proxy, honestly. buildZone reads only r.Ref out of the snapshot, so a dangling exit
//     rides the prototype from the cache rather than the snapshot. It is kept rejectable anyway because a
//     zone's room set and its exits are one authored unit, and a dangling exit is THE canonical signal of a
//     broken zone-graph deploy.
//   - resets: directly consumed — buildZone calls runResets/startRepop straight off the snapshot DTO.
//   - proto-ref collisions: collapse a zone's room set in the shared cache, so the zone builds wrong.
//   - reserved core namespace: a core-namespace zone ref in the merged snapshot IS a lc.Zone() clobber of
//     the embedded bootstrap lobby.
//   - ref charset: room/zone refs become ProtoRefs and GMCP keys via spawnRoom.
func zoneGraphProblems(s *reloadScope) []string {
	problems := validateRoomExits(s)
	problems = append(problems, validateResets(s)...)
	problems = append(problems, validateProtoRefs(s)...)
	// Reserved core: namespace (#212): a real pack shipping a core-namespace world-ref would, on
	// broadcast, drive a KindZone reconcile against the embedded bootstrap lobby (which reshapes/tears
	// down rooms) on every shard. Hard REJECT — but only when the OFFENDING pack is in scope (a not-reloaded
	// pack's pre-existing violation must not block this reload).
	for _, v := range content.LintReservedCoreRefs(s.full) {
		if s.inScope(v.Pack) {
			problems = append(problems, fmt.Sprintf("pack %q ships %s %q under the reserved core: namespace (would clobber the embedded bootstrap pack)", v.Pack, v.Kind, v.Ref))
		}
	}
	// Ref charset (#66, extended #234): an identity token with a character outside its safe charset can break
	// a GMCP key, a comms subject, or the tokenizer. Hard REJECT for an in-scope pack only.
	for _, v := range content.LintRefCharset(s.full) {
		if s.inScope(v.Pack) {
			problems = append(problems, fmt.Sprintf("pack %q %s %q has characters outside its safe charset %s (would break GMCP keys / comms subjects / the tokenizer)", v.Pack, v.Field, v.Value, v.Charset))
		}
	}
	return problems
}

// sharedDefProblems returns findings in the pack-GLOBAL definition kinds — the ones registered only by
// defineGlobals at boot, or hot-swapped by a path that never reads the snapshot. The `reload` gate and the
// boot report reject/report on these; the snapshot gate only warns. See validateSnapshotPacks.
func sharedDefProblems(s *reloadScope) []string {
	full := s.full
	var problems []string
	// --- attributes: build the FULL merged def graph (last-write-wins by ref) exactly as boot does, then
	// reject a parse error / cycle only when an IN-SCOPE attribute is the live cause. Building over the full
	// graph fixes the cross-pack blind spot (a cycle spanning a reloaded pack + a not-reloaded pack); the
	// provenance gate keeps an unrelated pack's cycle from blocking this reload.
	attrDefs := map[string]*attributeDef{}
	for i := range full {
		pk := &full[i]
		for _, a := range pk.Attributes {
			if s.attrOwner[a.Ref] != pk.Pack {
				continue // a later pack overrides this attr — the earlier copy is inert (never the live def)
			}
			base, err := parseAttributeBase(a)
			if err != nil {
				if s.inScope(pk.Pack) {
					problems = append(problems, fmt.Sprintf("attribute %q: bad base formula: %v", a.Ref, err))
				}
				continue
			}
			attrDefs[a.Ref] = &attributeDef{ref: a.Ref, base: base}
		}
	}
	for _, cycle := range attributeCycles(attrDefs) {
		if anyAttrInScope(s, cycle) {
			problems = append(problems, fmt.Sprintf("attribute cycle: %v", cycle))
		}
	}
	// Channels hot-swap through their OWN KindChannel path, which never reads the content snapshot.
	problems = append(problems, validateChannels(s)...)
	// Trust ladder (#111): a baseline tier granting a capability elevates every un-elevated account on next
	// login. Reject-severity findings are a hard REJECT for an in-scope pack; warn-severity is authoring noise.
	for _, v := range content.LintTrustLadder(full) {
		if v.Severity != content.TrustLadderReject || !s.inScope(v.Pack) {
			continue
		}
		where := "pack " + strconv.Quote(v.Pack)
		if v.Tier != "" {
			where += " tier " + strconv.Quote(v.Tier)
		}
		problems = append(problems, fmt.Sprintf("%s trust ladder: %s", where, v.Detail))
	}
	return problems
}

// anyAttrInScope reports whether any attribute on a cycle is owned (last-writer) by an in-scope pack — the
// provenance gate for a cross-pack cycle. Every node on a cycle owns the outgoing edge that closes it, so
// node-provenance is edge-provenance; a cycle A->B->A is attributed to A when A owns a participating attr.
func anyAttrInScope(s *reloadScope, cycle []string) bool {
	for _, ref := range cycle {
		if s.inScope(s.attrOwner[ref]) {
			return true
		}
	}
	return false
}

// validateProtoRefs reports a problem per prototype (room/item/mob) that would be SILENTLY DROPPED from the
// shared per-shard proto cache: an empty ref, or a ref that COLLIDES with another prototype's. defineContent
// (build.go) registers every loaded zone's rooms, items and mobs into ONE cache via protoCache.define, which
// does `next[ref] = p` — a last-write-wins overwrite. So two prototypes sharing a ref (a room and an item, a
// mob in another zone, a copy-paste that forgot to rename) silently collapse to one at boot with NO error;
// the dropped content just never spawns. This gate rejects that before it is broadcast.
//
// Faithful with no false positives. It first mirrors content.Load's cross-pack ZONE dedup (whole-zone
// last-write-wins by ref — loader.go): the validator sees the raw []content.Pack, so on a bare `reload`
// two packs that legitimately OVERRIDE the same zone both appear, and their (identical) rooms would look
// like collisions; deduping zones by ref first collapses the override to the single zone the cache actually
// builds, exactly as boot does. After dedup, each zone ref appears once (rooms/items/mobs already unioned
// within a pack by packtree), so a surviving duplicate ref is a genuine cross-zone / cross-kind collision.
func validateProtoRefs(s *reloadScope) []string {
	var problems []string
	// owner records the first zone that claimed a ref, so a collision names both sides. A finding is rejected
	// only when a reloaded (in-scope) zone is one of the two sides — a collision entirely between two
	// not-reloaded zones is a pre-existing defect this reload didn't cause (#205 provenance gate).
	owner := map[string]string{}
	check := func(zoneRef, kind, ref string) {
		if strings.TrimSpace(ref) == "" {
			if s.zoneInScope(zoneRef) {
				problems = append(problems, fmt.Sprintf("zone %q: %s prototype with empty ref", zoneRef, kind))
			}
			return
		}
		if prev, ok := owner[ref]; ok {
			if s.zoneInScope(zoneRef) || s.zoneInScope(prev) {
				problems = append(problems, fmt.Sprintf("prototype ref %q is defined more than once (zones %q and %q) — the later one silently overwrites the earlier in the shared cache", ref, prev, zoneRef))
			}
			return
		}
		owner[ref] = zoneRef
	}
	for _, z := range s.liveZones() {
		for _, r := range z.Rooms {
			check(z.Ref, "room", r.Ref)
		}
		for _, p := range z.Items {
			check(z.Ref, "item", p.Ref)
		}
		for _, p := range z.Mobs {
			check(z.Ref, "mob", p.Ref)
		}
	}
	return problems
}

// validateResets reports one problem per zone reset that would spawn NOTHING — a reset applyReset (reset.go)
// silently logs-and-skips: an unknown op, a target room that is not a room of the reset's own zone, or a
// prototype that is not defined. Resets do NOT hot-swap on the reload path (PublishPack propagates room/
// item/mob/channel invalidations, never resets — they take effect at boot and on the repop timer), so unlike
// the channel/room checks this is a pack-HEALTH check (catch a broken edit before broadcasting it), in the
// same spirit as the attribute checks. It is faithful to applyReset's resolution rules and holds the no-
// false-positive invariant:
//
// Every finding below is gated by provenance — only a reset in a reloaded (in-scope) zone is rejected:
//   - OP: applyReset understands only {spawn_item, spawn_mob, ""}; any other op is a dead reset (it warns
//     "op not understood" and spawns nothing). Provably dead — the op vocabulary is engine-fixed.
//   - ROOM: applyReset resolves the target via z.rooms[ref] — the reset's OWN zone's rooms ONLY (a reset
//     never places into another zone). So a Room absent from the owning zone's authored room set is provably
//     dangling (the owning zone is always loaded whole here), regardless of ref prefix — a cross-zone-looking
//     Room can never resolve at runtime. An empty Room is dead.
//   - PROTO: applyReset resolves via z.spawn, which reads the SHARED per-shard proto cache. That cache holds
//     every loaded zone's ROOM, item AND mob prototypes (defineContent registers all three — build.go), so a
//     reset naming a room ref as its proto is degenerate but still spawns SOMETHING; the resolvable set here
//     therefore includes rooms too, to avoid a false positive. #205: the proto set is now built over the FULL
//     merged graph (every enabled zone), so a CROSS-zone prototype that lives in a not-reloaded zone resolves;
//     a proto absent from the WHOLE world is provably undefined. Findings are gated by provenance — a reset in
//     a not-reloaded zone (its owning zone's last-writer is out of scope) never blocks this reload. Empty Proto
//     is dead.
//
// The `into` container target is a RUNTIME instance lookup (intoTargetInRoom finds a live container/mob in
// the room, dependent on earlier resets' spawn order), so it cannot be judged statically and is deliberately
// not checked — no false positive.
func validateResets(s *reloadScope) []string {
	zones := s.liveZones()
	// The shared proto cache z.spawn reads: EVERY live zone's ROOM, item AND mob prototype refs (defineContent
	// registers all three into one cache — build.go). Built over the FULL merged graph (#205), so a CROSS-ZONE
	// reset prototype that lives in a not-reloaded zone now resolves — the intra-zone-only hedge is dropped.
	// Rooms are included so a reset naming a room ref as its proto (degenerate but resolvable) is no false hit.
	protoRefs := map[string]bool{}
	for _, z := range zones {
		for _, r := range z.Rooms {
			protoRefs[r.Ref] = true
		}
		for _, p := range z.Items {
			protoRefs[p.Ref] = true
		}
		for _, p := range z.Mobs {
			protoRefs[p.Ref] = true
		}
	}
	var problems []string
	for _, z := range zones {
		if !s.zoneInScope(z.Ref) {
			continue // a reset in a not-reloaded zone is not this reload's problem (#205 provenance gate)
		}
		// The reset's target room resolves against THIS zone's rooms only (z.rooms).
		zoneRooms := make(map[string]bool, len(z.Rooms))
		for _, r := range z.Rooms {
			zoneRooms[r.Ref] = true
		}
		for ri := range z.Resets {
			r := &z.Resets[ri]
			switch r.Op {
			case "spawn_item", "spawn_mob", "":
				// understood — check its references below.
			default:
				problems = append(problems, fmt.Sprintf("zone %q reset: unknown op %q (understood: spawn_item, spawn_mob)", z.Ref, r.Op))
				continue // a dead reset; its refs are moot
			}
			if strings.TrimSpace(r.Room) == "" {
				problems = append(problems, fmt.Sprintf("zone %q reset (proto %q): empty target room", z.Ref, r.Proto))
			} else if !zoneRooms[r.Room] {
				problems = append(problems, fmt.Sprintf("zone %q reset: target room %q is not a room of this zone", z.Ref, r.Room))
			}
			if strings.TrimSpace(r.Proto) == "" {
				problems = append(problems, fmt.Sprintf("zone %q reset (room %q): empty prototype", z.Ref, r.Room))
			} else if !protoRefs[r.Proto] {
				// Resolves nowhere in the FULL merged proto set — provably undefined (no cross-zone-out-of-scope
				// escape hatch now that the whole graph is the context).
				problems = append(problems, fmt.Sprintf("zone %q reset: prototype %q is not defined", z.Ref, r.Proto))
			}
		}
	}
	return problems
}

// validateRoomExits reports one problem per room exit that PROVABLY points nowhere in the re-read content —
// a dangling exit (RoomDTO.Exits maps a direction to a destination room ref; roomComponents wires it into
// the live Room graph verbatim, no trimming, so the runtime routes on the raw target). Rooms hot-swap on
// this path (the per-ref applier + resyncRoom, #191), so a reload adding a room with a broken exit is caught
// before it propagates. Boot merely tolerates a dangling exit (the move just fails), but this gate is
// deliberately stricter — a dangling exit is an authoring error, not an intended degradation.
//
// #205 FULL-GRAPH + PROVENANCE: the resolution target set is every room in the WHOLE merged graph (all enabled
// zones, whole-zone last-write-wins), so a CROSS-ZONE exit now resolves against the real world — the old
// intra-zone-only hedge (which deferred cross-zone targets to avoid a false rejection on a scoped subset) is
// gone, closing the gap where a cross-zone exit into an out-of-scope zone was never checked. To keep the
// no-false-positive invariant on a scoped `reload`, a finding is REJECTED only when its owning zone's live
// definition is a reloaded (in-scope) pack; an exit in a not-reloaded zone is that zone's owner's concern, not
// this reload's. So: a target present anywhere in the world resolves; a target absent from the WHOLE world,
// owned by an in-scope zone, is provably dangling and rejected. An empty/whitespace target is always dead.
func validateRoomExits(s *reloadScope) []string {
	zones := s.liveZones()
	// Every room ref in the FULL merged graph — the resolution target set (#205). Building it over the whole
	// enabled set (not just the in-scope packs) is what lets a CROSS-ZONE exit resolve, so the intra-zone-only
	// hedge is dropped: a target absent from the WHOLE world is provably dangling.
	roomRefs := map[string]bool{}
	for _, z := range zones {
		for _, r := range z.Rooms {
			roomRefs[r.Ref] = true
		}
	}
	var problems []string
	for _, z := range zones {
		if !s.zoneInScope(z.Ref) {
			continue // an exit in a not-reloaded zone is not this reload's problem (#205 provenance gate)
		}
		for _, r := range z.Rooms {
			// Deterministic readout: map iteration order is random, so sort the directions.
			dirs := make([]string, 0, len(r.Exits))
			for dir := range r.Exits {
				dirs = append(dirs, dir)
			}
			sort.Strings(dirs)
			for _, dir := range dirs {
				target := r.Exits[dir]
				if strings.TrimSpace(target) == "" {
					problems = append(problems, fmt.Sprintf("room %q: exit %q has an empty target", r.Ref, dir))
					continue
				}
				if !roomRefs[target] {
					problems = append(problems, fmt.Sprintf("room %q: exit %q points to unknown room %q", r.Ref, dir, target))
				}
			}
			problems = append(problems, validateEntrances(r)...)
		}
	}
	return problems
}

// validateEntrances reports the STRUCTURAL problems with a room's instance entrances (#435) — the ones that
// are wrong under any combination of packs, so they are the builder's mistake rather than a missing dependency.
//
// Deliberately NOT checked here: whether the target zone exists, is `instanceable`, or has a usable start
// room. Those are real problems, but they are cross-pack: a pack declaring a door into a dungeon shipped by
// another pack would be unloadable without it, which is the dangling-ref tolerance the loader already extends
// to exits. MintInstance refuses all three at entry time and the player is told the way fails to open.
func validateEntrances(r content.RoomDTO) []string {
	if len(r.InstanceEntrances) == 0 {
		return nil
	}
	dirs := make([]string, 0, len(r.InstanceEntrances))
	for dir := range r.InstanceEntrances {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs) // map order is random; the readout must be deterministic
	var problems []string
	for _, dir := range dirs {
		target := strings.TrimSpace(r.InstanceEntrances[dir])
		switch {
		case target == "":
			problems = append(problems, fmt.Sprintf("room %q: instance entrance %q has an empty target", r.Ref, dir))
		case strings.Contains(target, ":"):
			// A ROOM ref where a ZONE ref belongs. This must be rejected rather than tolerated: parseRef would
			// silently keep only the leading segment, so `crypt:room:altar` would mint the `crypt` zone and
			// appear to work — landing the player at the zone's own start room rather than the room named.
			problems = append(problems, fmt.Sprintf("room %q: instance entrance %q targets %q, which is a ROOM "+
				"ref; an entrance names a ZONE, and the target zone's own start_room decides where you land",
				r.Ref, dir, target))
		case strings.Contains(target, instanceSep):
			// The instance-id separator. Mirrors validateMintTemplate's check, but at load time and in front
			// of the builder rather than as a runtime refusal a player sees.
			problems = append(problems, fmt.Sprintf("room %q: instance entrance %q targets %q, which contains "+
				"%q — that is the engine's instance-id separator and never appears in an authored ref",
				r.Ref, dir, target, instanceSep))
		}
		if _, clash := r.Exits[dir]; clash {
			// The two maps share the direction namespace, so an overlap is ambiguous. The move path resolves
			// exits first, which makes this fail SAFE (toward the ordinary exit) rather than toward minting —
			// but ambiguous content must not load in the first place.
			problems = append(problems, fmt.Sprintf("room %q: %q is both an exit and an instance entrance; "+
				"one direction cannot be both", r.Ref, dir))
		}
	}
	return problems
}

// validateChannels reports one problem per definitively-DEAD channel_def in the re-read packs. Unlike the
// attribute checks (a boot-only concern), channels are propagated + hot-swapped by this very reload path
// (contentbus.PublishPack -> reloadChannel -> buildChannelDef), so this is real payload validation of the
// content the builder edited. Channels merge across packs last-write-wins by ref (mirroring content.Load,
// loader.go), so the check runs over the merged set. Faithful to the boot build path — it rejects only a
// channel boot would build STRUCTURALLY DEAD, never one that is merely degraded:
//
//   - an EMPTY ref: the channel registry is keyed by ref and the comms subject is built from it
//     (commbus.ChanSubject) — a ref-less channel can neither be swapped in (reloadChannel keys by ref) nor
//     addressed on the wire;
//   - a SUBJECT-UNSAFE ref: ChanSubject concatenates the ref raw into telos.comms.chan.<ref>, so a ref
//     with whitespace/control bytes, a NATS wildcard (* or >), or an empty dot-delimited token builds a
//     subject NATS rejects on publish (or can't publish to) — the channel can never carry a line. This is
//     the char-safety half of the P8-A8 subject-injection contract ChanSubject's doc defers to here;
//   - NO usable verb word: after the same lower/trim buildChannelDef applies no word survives, so
//     channelForVerb can never resolve the channel — it is unreachable (a channel nobody can speak on);
//   - a format that DROPS the player's message: rendered through the SAME renderChannelFormat boot uses
//     (an empty format defaults to defaultChannelFormat, which carries $t), a non-empty template with no
//     surviving $t silently swallows every line — the channel carries no content.
//
// A merely-degraded channel (an unknown $token that surfaces literally, an over-restrictive access
// predicate) is NOT rejected: only definitively-dead content blocks the publish, preserving the
// no-false-positive invariant.
func validateChannels(s *reloadScope) []string {
	var problems []string
	// Merge across packs EXACTLY as content.Load / mergeByKey do: last-write-wins keyed on the RAW ref
	// (loader.go, packtree.go — and buildChannelDef/reloadChannel/ChanSubject all key raw too), order
	// preserved. Keying raw is what keeps this faithful: it can't collapse two whitespace-variant refs that
	// stay distinct live channels and thereby miss (or falsely flag) a dead one. A whitespace-only ref
	// (trims to empty) is separately caught as "missing ref" below. A finding is rejected only when its
	// channel's LAST-WRITER pack is in scope (#205 provenance gate).
	merged := map[string]content.ChannelDTO{}
	var order []string
	for i := range s.full {
		pk := &s.full[i]
		for _, ch := range pk.Channels {
			if strings.TrimSpace(ch.Ref) == "" {
				// Empty or whitespace-only: no usable merge key, and an unaddressable subject. Flag each — but
				// only for an in-scope pack (a ref-less channel has no merge key, so provenance is its pack).
				if s.inScope(pk.Pack) {
					problems = append(problems, fmt.Sprintf("channel %q: missing ref", ch.Name))
				}
				continue
			}
			ref := ch.Ref
			if _, seen := merged[ref]; !seen {
				order = append(order, ref)
			}
			merged[ref] = ch
		}
	}
	// A sentinel that content can't contain (NUL bytes) — its survival through the format proves $t renders.
	const probe = "\x00telos-msg-probe\x00"
	for _, ref := range order {
		if !s.inScope(s.chanOwner[ref]) {
			continue // a channel whose live def is a not-reloaded pack's is not this reload's problem
		}
		ch := merged[ref]
		if reason := channelRefSubjectProblem(ref); reason != "" {
			problems = append(problems, fmt.Sprintf("channel %q: ref builds an invalid comms subject (%s)", ref, reason))
		}
		hasWord := false
		for _, w := range ch.Words {
			if strings.TrimSpace(w) != "" {
				hasWord = true
				break
			}
		}
		if !hasWord {
			problems = append(problems, fmt.Sprintf("channel %q: no usable verb word", ref))
		}
		format := ch.Format
		if format == "" {
			format = defaultChannelFormat
		}
		if !strings.Contains(renderChannelFormat(format, ch.Name, "probe", probe), probe) {
			problems = append(problems, fmt.Sprintf("channel %q: format template drops the message (no $t token)", ref))
		}
	}
	return problems
}

// channelRefSubjectProblem reports why a channel ref would build a malformed or unpublishable NATS comms
// subject (commbus.ChanSubject concatenates it RAW into telos.comms.chan.<ref>), or "" when it is
// subject-safe. This is the char-safety half of the P8-A8 subject-injection contract ChanSubject's doc
// explicitly defers to channel_defs: a ref carrying whitespace/control bytes, a NATS wildcard token
// (* or >), or an empty dot-delimited token yields a subject NATS rejects on publish (or a wildcard the
// world can't publish to). Either way the channel can never carry a line — it is definitively dead — so
// the gate rejects it. Legit refs ([a-z0-9_-], optionally dotted) pass untouched, so there is no
// false-positive risk against good content. NOTE (#66): the ref-charset lint now runs in this same gate and
// hard-rejects '.', so a DOTTED channel ref is caught upstream by that stricter, uniform check — this
// function's dot-tolerance is now effectively moot but kept as the subject-specific belt-and-suspenders
// (whitespace/control/wildcard/empty-token remain its own to catch).
func channelRefSubjectProblem(ref string) string {
	for _, r := range ref {
		switch {
		case r <= ' ' || r == 0x7f:
			return "whitespace or control character"
		case r == '*' || r == '>':
			return "a NATS wildcard character (* or >)"
		}
	}
	for _, tok := range strings.Split(ref, ".") {
		if tok == "" {
			return "an empty dot-delimited subject token"
		}
	}
	return ""
}

// advisoryReloadRemovals surfaces (NON-BLOCKING, #309) the inverse of the #205 provenance gate: a room or
// prototype that THIS reload removes, still referenced by a NOT-reloaded pack. Example: reloading pack A drops
// room az:room:5, but pack B (not reloaded) has an exit bz:room:9 -> az:room:5. #205 correctly does NOT
// hard-block A for B's dependency — B's dangling exit is rooted out of scope — but silently letting the
// operator remove something another pack still points at is a footgun. This is the heads-up: not a rejection,
// a line in the reload readout.
//
// PRECISION needs a pre-vs-post diff, not a snapshot: a dangling out-of-scope reference is only THIS reload's
// doing if its target USED TO resolve. A target that was never live is a pre-existing defect in the depending
// pack, not ours, and must not be warned (that is the no-false-positive invariant #205 also holds). `wasLive`
// is the "before" snapshot — the shard's live proto cache at VALIDATE TIME, which still holds the pre-reload
// prototypes: this reload's removal lands only when the publish drives the zone-shape reconcile
// (KindZone -> reconcileZone -> removeRoom -> protoCache.reload(ref,nil)), which runs AFTER the advisory is
// computed. So target-absent-from-the-re-read-graph AND wasLive == "this reload removes a currently-live
// room/proto", and since only in-scope packs are re-read/changed, the removal is attributable to this reload.
//
// It scans only OUT-OF-SCOPE zones: an in-scope pack's own dangling reference to something it removed is
// already hard-rejected by validateRoomExits/validateResets, so it never reaches here.
//
// One honest caveat: the DEPENDER side (the out-of-scope zone's exits/resets) is read from the re-read `full`
// graph, which is the current SOURCE, not necessarily the depender's LIVE state — the same basis #205 uses. If
// an out-of-scope pack's source was edited since boot without reloading it, the reference set can diverge. So
// the wording is "still references", not a promise about the running world.
func advisoryReloadRemovals(s *reloadScope, wasLive func(ref string) bool) []string {
	if wasLive == nil {
		return nil
	}
	zones := s.liveZones()
	// The post-reload resolution sets: every room ref (exit targets) and every spawnable proto ref (reset
	// targets: rooms are spawnable too — a degenerate reset naming a room — so rooms are in both sets).
	roomRefs := map[string]bool{}
	protoRefs := map[string]bool{}
	for _, z := range zones {
		for _, r := range z.Rooms {
			roomRefs[r.Ref] = true
			protoRefs[r.Ref] = true
		}
		for _, p := range z.Items {
			protoRefs[p.Ref] = true
		}
		for _, p := range z.Mobs {
			protoRefs[p.Ref] = true
		}
	}
	var advisories []string
	for _, z := range zones {
		if s.zoneInScope(z.Ref) {
			continue // an in-scope pack's own dangler is already hard-rejected; #309 is about the OUT-OF-SCOPE depender
		}
		for _, r := range z.Rooms {
			dirs := make([]string, 0, len(r.Exits))
			for dir := range r.Exits {
				dirs = append(dirs, dir)
			}
			sort.Strings(dirs)
			for _, dir := range dirs {
				target := strings.TrimSpace(r.Exits[dir])
				if target == "" || roomRefs[target] || !wasLive(target) {
					continue
				}
				advisories = append(advisories, fmt.Sprintf(
					"room %q exit %q still references %q, which this reload removes (the referencing room is not "+
						"reloaded, so it is not blocked — but the exit now leads nowhere)", r.Ref, dir, target))
			}
		}
		// A not-reloaded zone whose START ROOM this reload removes: logins/recalls into that zone dead-end.
		// Same room-ref mechanism as an exit, and arguably more severe.
		if start := strings.TrimSpace(z.StartRoom); start != "" && !roomRefs[start] && wasLive(start) {
			advisories = append(advisories, fmt.Sprintf(
				"zone %q start room %q is removed by this reload (the zone is not reloaded, so it is not "+
					"blocked — but entering the zone will fail)", z.Ref, start))
		}
		for ri := range z.Resets {
			proto := strings.TrimSpace(z.Resets[ri].Proto)
			if proto == "" || protoRefs[proto] || !wasLive(proto) {
				continue
			}
			advisories = append(advisories, fmt.Sprintf(
				"zone %q has a reset that spawns %q, which this reload removes (the zone is not reloaded, so it "+
					"is not blocked — but the reset will spawn nothing)", z.Ref, proto))
		}
	}
	sort.Strings(advisories)
	return dedupeSorted(advisories)
}

// pinnedInstanceAdvisories surfaces (NON-BLOCKING, #411) every zone in this reload's scope that has live
// runtime-minted INSTANCES. Those instances are pinned to the content they were minted from: reload.go
// withholds both the zone-shape reconcile and the Lua fan-out from them, so a party mid-run finishes the
// version they entered on.
//
// That freeze is correct — a converge that tears down rooms would delete the room a party is standing in —
// but it is INVISIBLE without this. A builder edits a dungeon, the readout reports success, and the runs in
// flight keep the old behavior with nothing anywhere explaining it. The line names the count so the operator
// can decide whether to wait for the reaper (instances are short-lived) or stop caring.
//
// Scoped by the zone's last-writer pack, exactly like the #309 advisories, so a reload of an unrelated pack
// does not narrate every dungeon on the shard. A nil shard (a bare validate-only reloader) yields nothing.
func pinnedInstanceAdvisories(s *Shard, scope *reloadScope) []string {
	if s == nil || scope == nil {
		return nil
	}
	var advisories []string
	for template, n := range s.instanceTemplateCounts() {
		if n == 0 || !scope.zoneInScope(template) {
			continue
		}
		advisories = append(advisories, fmt.Sprintf(
			"zone %q has %d live instance(s), which are PINNED to the content they were minted from: this "+
				"reload does NOT reach them (no room reconcile, no Lua recompile). They pick it up when they "+
				"are reaped and the next one is minted", template, n))
	}
	sort.Strings(advisories)
	return advisories
}

// dedupeSorted collapses adjacent duplicates in a sorted slice (two resets in one zone spawning the same
// removed proto produce identical advisory lines).
func dedupeSorted(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

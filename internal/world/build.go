package world

import (
	"log/slog"
	"sort"
	"strings"

	"github.com/double-nibble/telosmud/internal/content"
)

// build.go is the content loader's world-side half (docs/PHASE4-PLAN.md §3): it takes the
// neutral *content.LoadedContent produced by the content package and turns it into live
// prototypes + zones, REPLACING the hand-authored newDemoZone. It is the new caller of
// protoCache.define — define stays the sole prototype-construction entry point — and of
// Zone.spawn for the reset placements. spawn and the COW model are untouched.
//
// Two stages, both run at shard construction (before any zone goroutine starts), so the
// synchronous work here never races a running zone:
//
//   1. defineContent fills the shared per-shard protoCache from every loaded zone's room,
//      item, and mob prototypes (via the DTO->component mapper, content_map.go).
//   2. buildZone wires one zone: it spawns its room singletons (sharing the immutable
//      prototypes) and runs its reset script to place ephemeral instances on the floor.

// defineContent registers every prototype in lc into the cache. Build-time only: called from
// newShard while the cache is still private to the construction goroutine. Rooms, items, and
// mobs from every loaded zone are defined into the one shared cache, so a cross-zone exit
// target resolves regardless of which zone hosts the destination room.
func defineContent(c *protoCache, lc *content.LoadedContent) {
	if lc == nil {
		return
	}
	for i := range lc.Zones {
		z := &lc.Zones[i]
		for _, r := range z.Rooms {
			c.define(ProtoRef(r.Ref), nil, r.Name, r.Long, roomComponents(r))
		}
		for _, p := range z.Items {
			c.define(ProtoRef(p.Ref), p.Keywords, p.Short, p.Long, protoComponents(p))
		}
		for _, p := range z.Mobs {
			c.define(ProtoRef(p.Ref), p.Keywords, p.Short, p.Long, protoComponents(p))
		}
	}
}

// defineGlobals registers every PACK-GLOBAL definition (attributes/resources/damage-types) from lc
// into the shard's registry bundle (defs.go). Build-time only: called from the shard constructors
// while the bundle is still private to the construction goroutine, so the copy-then-swap registers
// are uncontended and leave the bundle PUBLISHED for the lock-free runtime read path. It parses each
// attribute's base formula (the prefix-AST, formula.go) once here, then runs the load-time cycle
// lint over the assembled attribute graph. A nil lc (bare-engine boot) registers nothing — zero
// attrs/resources/damage-types, the engine still boots and a stat read returns 0.
func defineGlobals(d *defRegistries, lc *content.LoadedContent) {
	if lc == nil {
		return
	}
	for _, a := range lc.Attributes {
		base, err := parseAttributeBase(a)
		if err != nil {
			// A malformed formula is a content error: log loudly and register the def WITHOUT a base
			// (resolves to 0) rather than aborting the whole boot. Content-lint is the real gate.
			slog.Error("content: attribute base parse failed; registering with zero base",
				"attr", a.Ref, "err", err)
			base = nil
		}
		d.attr.register(a.Ref, &attributeDef{
			ref: a.Ref, displayName: a.DisplayName, valueKind: a.ValueKind,
			base: base, min: a.Min, max: a.Max,
		})
	}
	for _, r := range lc.Resources {
		rd := &resourceDef{
			ref: r.Ref, displayName: r.DisplayName, maxAttr: r.MaxAttr,
			vital: r.Vital, regen: r.Regen, depletedThreshold: r.DepletedThreshold,
			onEvent: parseEventMap(r.OnEvent, "resource "+r.Ref),
		}
		lintEventMap("resource "+r.Ref, rd.onEvent)
		d.res.register(r.Ref, rd)
	}
	for _, dt := range lc.DamageTypes {
		d.dmg.register(dt.Ref, &damageTypeDef{
			ref: dt.Ref, displayName: dt.DisplayName, color: dt.Color, resist: dt.Resist,
		})
	}
	for _, af := range lc.Affects {
		ad := buildAffectDef(af)
		lintEventMap("affect "+af.Ref, ad.onEvent)
		d.affect.register(af.Ref, ad)
	}
	// Abilities (Phase 5.3): parse each ability's on_resolve op-list into the runtime def and register
	// it. A command-invocation ability also binds its verb words into the per-shard ability command
	// table (abilityCmds), which dispatch consults after the built-in baseTable. A malformed op-list is
	// logged loudly and the def registers with whatever ops parsed (content-lint is the real gate).
	for _, ab := range lc.Abilities {
		def, err := buildAbilityDef(ab)
		if err != nil {
			slog.Error("content: ability on_resolve parse failed; registering with parsed ops only",
				"ability", ab.Ref, "err", err)
		}
		lintAbilityOps(ab.Ref, def.ops)
		lintEventMap("ability "+ab.Ref, def.onEvent)
		d.ability.register(ab.Ref, def)
		if def.invocation == "command" {
			for _, w := range def.words {
				lw := strings.ToLower(strings.TrimSpace(w))
				if lw == "" {
					continue
				}
				if d.abilityCmds == nil {
					d.abilityCmds = map[string]*abilityDef{}
				}
				d.abilityCmds[lw] = def
			}
		}
	}
	// Combat profiles (Phase 6.3a): parse each into its to-hit/avoidance/damage runtime form and
	// register it. A mob/player references one by ref (Living.combatRef); the swing pipeline resolves it.
	for _, cp := range lc.CombatProfiles {
		d.combat.register(cp.Ref, buildCombatProfile(cp))
	}
	d.defaultCombat = lc.DefaultCombat
	// Load-time content-lint: reject a derived-attribute graph with a self/mutual reference.
	for _, err := range lintAttributeCycles(d.attr.table()) {
		slog.Error("content: attribute derivation cycle (def will resolve to 0)", "err", err)
	}
	// Load-time content-lint: combat + death assume ONE vital resource per pack (vitalResource picks
	// deterministically by sorted ref, but >1 vital is a content modelling error — only the lowest-ref
	// one ever takes damage / drives death). Warn loudly so an author sees it at load, not in a fight.
	lintVitalResources(d.res.table())
	slog.Debug("global defs registered", "attributes", d.attr.len(),
		"resources", d.res.len(), "damage_types", d.dmg.len(), "affects", d.affect.len(),
		"abilities", d.ability.len(), "ability_commands", len(d.abilityCmds))
}

// lintVitalResources warns if a pack registers more than one VITAL resource. The combat/death machinery
// (vitalResource, the swing's apply stage, the 6.3b on_depleted path) models ONE vital pool (hp); a
// second is a content error — vitalResource deterministically picks the lowest ref, so only it ever
// takes damage and the others silently never deplete. Build-time only; logs at WARN with the offending
// refs (sorted) so an author sees it at load. Does not abort boot (content-lint discipline).
func lintVitalResources(table map[string]*resourceDef) {
	var vitals []string
	for ref, def := range table {
		if def != nil && def.vital {
			vitals = append(vitals, ref)
		}
	}
	if len(vitals) > 1 {
		sort.Strings(vitals)
		slog.Warn("content: more than one VITAL resource defined; only the lowest ref takes damage / drives death",
			"vitals", vitals, "used", vitals[0])
	}
}

// parseAttributeBase turns an AttributeDTO's default_base (a {lit} OR {expr} spec) into a parsed
// formula tree (formula.go). A literal becomes a litNode; an expr is parsed from the prefix-AST. A
// zero spec (neither set) yields nil (base 0).
func parseAttributeBase(a content.AttributeDTO) (formulaNode, error) {
	if a.DefaultBase.Lit != nil {
		return litNode{v: *a.DefaultBase.Lit}, nil
	}
	if a.DefaultBase.Expr != nil {
		return parseFormula(a.DefaultBase.Expr)
	}
	return nil, nil
}

// buildZone constructs zone z (id == zoneRef) from its loaded definition: it spawns each room
// as a singleton entity, records the start room, and runs the reset script. The prototype
// cache must already be filled (defineContent). If the loaded content has no such zone, the
// zone is left EMPTY (no rooms, no start room) — the bare-engine boot: a login to an empty
// zone is rejected cleanly (Zone.join / resolveRoom guards), never a panic.
func (z *Zone) buildZone(lc *content.LoadedContent) {
	zd := lc.Zone(z.id)
	if zd == nil {
		z.log.Debug("zone has no loaded content; booting empty", "zone", z.id)
		return
	}
	for _, r := range zd.Rooms {
		z.spawnRoom(ProtoRef(r.Ref))
	}
	z.startRoom = ProtoRef(zd.StartRoom)
	// Boot reset: run the script once to place the starting content. The SAME interpreter
	// (reset.go) the repop timer uses — boot fills an empty zone, repop tops it back up — so
	// there is one spawn/Move/counting code path. Then register the timed repop cadence
	// (reset.go) keyed off reset_secs (a no-op when reset_secs==0: no timed reset).
	z.runResets(zd.Resets)
	z.startRepop(zd.Resets, zd.ResetSecs)
	z.log.Debug("zone built from content", "zone", z.id,
		"rooms", len(zd.Rooms), "start_room", z.startRoom, "resets", len(zd.Resets),
		"reset_secs", zd.ResetSecs)
}

// newDemoZone builds the named demo zone from the EMBEDDED demo pack (content.DemoPack),
// sharing the given per-shard prototype cache. It is the test/bare-run helper that the
// Phase 1-3 tests still call (handoff_fixes_test, container_test indirectly): the hand-
// authored body is gone, so this is now a thin loader call that produces byte-identical
// prototypes. The first call into the shared cache defines the demo content; a second call
// for a sibling zone re-defines the same immutable prototypes (idempotent — same data).
//
// Production does NOT call this: buildShard loads from the configured source (Postgres or the
// embedded pack) and builds every hosted zone via buildZone. This wrapper exists so the unit
// tests construct the demo world without a live database.
func newDemoZone(id string, protos *protoCache) *Zone {
	z := newZone(id)
	z.protos = protos
	lc, err := content.LoadDemoPack()
	if err != nil {
		// The demo pack is embedded and checked in; a parse failure is a build-time bug, not a
		// runtime condition. Panic loudly so a malformed pack can never silently ship.
		panic("world: load embedded demo pack: " + err.Error())
	}
	defineContent(protos, lc)
	// Register the demo pack's pack-global defs (attributes/resources/damage-types) into THIS
	// zone's private bundle, so a test zone built via newDemoZone resolves the demo's
	// strength/max_hp/hp standalone. The shard path (newShard) shares one bundle across zones;
	// here each demo zone gets its own equivalent registration (same immutable defs).
	defineGlobals(z.defs, lc)
	z.buildZone(lc)
	return z
}

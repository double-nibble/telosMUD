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
	// Content-lint (#194): warn on any room whose ref prefix != its owning zone. Build-time, non-fatal.
	for _, m := range lintRoomZonePrefixes(lc) {
		slog.Warn("content: room ref prefix does not match its owning zone; cross-zone exits will misroute "+
			"and a hot-reload new-room add (#191) will skip it — author the room as <zone>:...",
			"room", m.room, "zone", m.zone, "ref_prefix", m.refZone)
	}
}

// roomZonePrefixMiss is one content-lint finding: a room whose ref prefix (parseRef's zone) differs from
// the zone it is authored in.
type roomZonePrefixMiss struct{ room, zone, refZone string }

// lintRoomZonePrefixes returns a finding for every room whose ref does NOT carry its owning zone's ref as
// the prefix. Cross-zone exit ROUTING and the hot-reload new-room ownership guard (#191, resyncRoom) both
// key a room to a zone by that prefix (parseRef), but boot assigns a room to a zone by its content `rooms:`
// LIST membership — so a divergent ref boots fine yet misroutes cross-zone exits and is skipped by a
// hot-reload ADD. Build-time only; the caller logs (does not abort boot), like the other content-lints.
// The demo pack keeps prefix==zone, so it is silent there.
func lintRoomZonePrefixes(lc *content.LoadedContent) []roomZonePrefixMiss {
	if lc == nil {
		return nil
	}
	var misses []roomZonePrefixMiss
	for i := range lc.Zones {
		z := &lc.Zones[i]
		for _, r := range z.Rooms {
			if zoneOf, _ := parseRef(ProtoRef(r.Ref)); zoneOf != z.Ref {
				misses = append(misses, roomZonePrefixMiss{room: r.Ref, zone: z.Ref, refZone: zoneOf})
			}
		}
	}
	return misses
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
			base: base, min: a.Min, max: a.Max, stat: a.Stat,
		})
	}
	for _, r := range lc.Resources {
		rd := &resourceDef{
			ref: r.Ref, displayName: r.DisplayName, maxAttr: r.MaxAttr,
			vital: r.Vital, regen: r.Regen, regenInCombat: r.RegenInCombat,
			depletedThreshold: r.DepletedThreshold,
			perRound:          r.PerRound,
			gauge:             r.Gauge,
			onEvent:           parseEventMap(r.OnEvent, "resource "+r.Ref),
			onEventLua:        parseLuaEventMap(r.OnEventLua, "resource "+r.Ref),
			onReactionLua:     parseLuaEventMap(r.OnReactionLua, "resource "+r.Ref+" (reaction)"),
		}
		// on_depleted ([G-D]): the death-hook op-list, parsed like any op-list. A malformed list logs
		// loudly and registers with whatever parsed (content-lint discipline); nil/absent => engine default
		// death only.
		if len(r.OnDepleted) > 0 {
			ops, err := parseOpList(r.OnDepleted)
			if err != nil {
				slog.Error("content: resource on_depleted parse failed; death hook runs parsed ops only",
					"resource", r.Ref, "err", err)
			}
			rd.onDepleted = ops
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
	// Channels (Phase 8.3): register each content channel_def into the per-shard channel registry. Its
	// verb(s) become channel commands (dispatch consults channelForVerb after baseTable/abilities/custom
	// — a channel verb never shadows a core verb). An empty pack registers ZERO channels => no channel
	// verbs (the empty-boot invariant). Channels are CONTENT; the engine names none.
	for _, ch := range lc.Channels {
		d.channel.register(ch.Ref, buildChannelDef(ch))
	}
	// Tracks (Phase 11.2): parse each track's per-step grant op-lists into the runtime def and register it.
	// A malformed step op-list is logged and the track registers with whatever parsed (content-lint gate).
	for _, tr := range lc.Tracks {
		def, err := buildTrackDef(tr)
		if err != nil {
			slog.Error("content: track step op-list parse failed; registering with parsed steps only",
				"track", tr.Ref, "err", err)
		}
		d.track.register(tr.Ref, def)
	}
	// Bundles (Phase 11.4b): parse each bundle's grant op-list into the runtime def and register it.
	for _, bn := range lc.Bundles {
		def, err := buildBundleDef(bn)
		if err != nil {
			slog.Error("content: bundle grant op-list parse failed; registering with parsed grants only",
				"bundle", bn.Ref, "err", err)
		}
		d.bundle.register(bn.Ref, def)
	}
	// Loot (Phase 12.1): rarity tiers + named affixes + loot tables into the per-shard registries. Affixes
	// (#37) MUST register BEFORE loot tables: buildLootTableDef resolves each pool's `ref` affix against the
	// affix registry, so the referenced defs have to be present first.
	for _, rt := range lc.RarityTiers {
		d.rarity.register(rt.Ref, buildRarityTierDef(rt))
	}
	for _, af := range lc.Affixes {
		d.affix.register(af.Ref, buildAffixDef(af))
	}
	for _, lt := range lc.LootTables {
		lintAffixRefs(lt, d.affix) // #37: boot-time warn if a quality pool names an unknown affix_def
		d.loot.register(lt.Ref, buildLootTableDef(lt, d.affix))
	}
	// Recipes (Phase 13.5): crafting recipes into the per-shard registry.
	for _, rc := range lc.Recipes {
		d.recipe.register(rc.Ref, buildRecipeDef(rc))
	}
	// Help topics (#64): builder-defined help into the per-shard registry. The `help` command reads it and
	// layers the auto-included built-in command set on top, so an empty list still yields a command index.
	for _, hd := range lc.HelpDefs {
		d.help.register(hd.Ref, buildHelpDef(hd))
	}
	d.defaultCombat = lc.DefaultCombat
	// Trust ladder (#27/#29, Round 9 Slice 0): build the content-defined tier→rank+flags ladder. An empty
	// list leaves d.trust nil so z.trustLadder() falls back to the engine default (round-8 mapping).
	d.trust = buildTrustLadder(lc.TrustTiers)
	// Wear-slot vocabulary (#35): build the content-defined equipment slot set. An empty list leaves
	// d.wearSlots nil so z.wearSlots() falls back to the engine default (the classic Diku slots).
	d.wearSlots = buildWearVocab(lc.WearSlots)
	// Custom Lua commands (7.4e): register each verb + its aliases into the per-shard custom-command
	// table by EXACT word. Skips a word that collides with a BUILT-IN verb (a custom command may never
	// shadow a core/movement verb); logs the skip loudly. dispatch consults this table last + exact.
	for _, cmd := range lc.Commands {
		registerCustomCommand(d, cmd)
	}
	// Display templates: register each surface's Lua render body into the per-shard display table (last-write
	// -wins by surface). Warn LOUDLY when a template names a surface the engine does not yet consume (only
	// `score` is wired today) — otherwise the author's body is a silently-dead seam, the same discipline as
	// the formula-not-consulted warning below.
	for _, dd := range lc.DisplayDefs {
		if dd.Surface == "" {
			slog.Warn("content: display_def with an empty surface; skipped")
			continue
		}
		if d.displayDefs == nil {
			d.displayDefs = map[string]string{}
		}
		d.displayDefs[dd.Surface] = dd.Render
		if !consultedDisplaySurfaces[dd.Surface] {
			slog.Warn("content: display template defined for a surface not yet consumed by the engine; it will not render",
				"surface", dd.Surface)
		}
	}
	// PvP policy + ruleset formulas (7.4f): carried for the gate/formula seams to consult.
	d.pvpLua = lc.PvpLua
	for name, body := range lc.Formulas {
		if d.formulas == nil {
			d.formulas = map[string]string{}
		}
		d.formulas[name] = body
		// Warn LOUDLY when a content author defines a ruleset formula the engine does not yet
		// consult (7.4f wired only `regen`; to_hit/soak/xp_for live in checkSpec/attr/OnKill and
		// are NOT yet read from formulas[]). Without this the author's Lua body is a silently-dead
		// seam — they'd never know it never ran. Same discipline as parseLuaEventMap's unknown-kind.
		if !consultedLuaFormulas[name] {
			slog.Warn("content: ruleset formula defined but NOT YET consulted (only 'regen' is live in 7.4f); the body will not run",
				"formula", name)
		}
	}
	// Load-time content-lint: reject a derived-attribute graph with a self/mutual reference.
	for _, err := range lintAttributeCycles(d.attr.table()) {
		slog.Error("content: attribute derivation cycle (def will resolve to 0)", "err", err)
	}
	// Load-time content-lint: combat + death assume ONE vital resource per pack (vitalResource picks
	// deterministically by sorted ref, but >1 vital is a content modelling error — only the lowest-ref
	// one ever takes damage / drives death). Warn loudly so an author sees it at load, not in a fight.
	lintVitalResources(d.res.table())
	// Load-time content-lint (docs/REMAINING.md §4): every learn_profession op's `profession` must name a
	// registered kind:"profession" bundle. professionIsCapped/uncapped resolve the D2 cap by looking up
	// that bundle (ref == the membership ref by convention); a miss means the trade grants nothing AND its
	// capped/uncapped resolution silently defaults to CAPPED. Runs after all bundles are registered so the
	// kind is resolvable. Logs (does not fail the build), like the other content-lints.
	for _, m := range lintLearnProfessionRefs(d) {
		slog.Error("content: learn_profession.profession does not name a kind:profession bundle",
			"owner", m.owner, "profession", m.profession)
	}
	slog.Debug("global defs registered", "attributes", d.attr.len(),
		"resources", d.res.len(), "damage_types", d.dmg.len(), "affects", d.affect.len(),
		"abilities", d.ability.len(), "ability_commands", len(d.abilityCmds),
		"channels", d.channel.len())
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

// learnProfessionMiss is one content-lint finding: a learn_profession op whose `profession` does not name
// a kind:"profession" bundle. owner locates the offending op-list.
type learnProfessionMiss struct {
	owner      string
	profession string
}

// lintLearnProfessionRefs walks every registered op-list (ability on_resolve + on_event, bundle grants,
// track step grants, affect on_event + tick, resource on_event + on_depleted) and returns a finding for each learn_profession op whose
// `profession` does NOT name a registered kind:"profession" bundle. This machine-checks the
// ref==profession-bundle-ref convention that professionIsCapped/uncapped keys the D2 cap off
// (docs/REMAINING.md §4): a miss means the learned trade grants nothing and its cap resolution silently
// defaults to CAPPED. Build-time only; the caller logs (does not abort boot — the runtime already defaults
// conservatively), like the other content-lints.
func lintLearnProfessionRefs(d *defRegistries) []learnProfessionMiss {
	isProfBundle := func(ref string) bool {
		b := d.bundle.get(ref)
		return b != nil && b.kind == "profession"
	}
	var misses []learnProfessionMiss
	var walk func(owner string, ops []effectOp)
	walk = func(owner string, ops []effectOp) {
		for i := range ops {
			op := &ops[i]
			if op.kind == "learn_profession" && !isProfBundle(op.profession) {
				misses = append(misses, learnProfessionMiss{owner: owner, profession: op.profession})
			}
			walk(owner, op.then)
			walk(owner, op.els)
			if op.check != nil {
				for j := range op.check.bands {
					walk(owner, op.check.bands[j].ops)
				}
			}
		}
	}
	for ref, def := range d.ability.table() {
		walk("ability "+ref, def.ops)
		for kind, ops := range def.onEvent {
			walk("ability "+ref+" on_event["+string(kind)+"]", ops)
		}
	}
	for ref, def := range d.bundle.table() {
		walk("bundle "+ref, def.grants)
	}
	for ref, def := range d.track.table() {
		for _, ops := range def.steps {
			walk("track "+ref+" step", ops)
		}
	}
	for ref, def := range d.affect.table() {
		for kind, ops := range def.onEvent {
			walk("affect "+ref+" on_event["+string(kind)+"]", ops)
		}
		walk("affect "+ref+" tick", def.tickOps)
	}
	for ref, def := range d.res.table() {
		for kind, ops := range def.onEvent {
			walk("resource "+ref+" on_event["+string(kind)+"]", ops)
		}
		walk("resource "+ref+" on_depleted", def.onDepleted)
	}
	return misses
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

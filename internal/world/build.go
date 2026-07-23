package world

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/logcap"
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
			vital: r.Vital, primary: r.Primary, regen: r.Regen, regenInCombat: r.RegenInCombat,
			depletedThreshold: r.DepletedThreshold,
			perRound:          r.PerRound,
			gauge:             r.Gauge,
			onEvent:           parseEventMap(r.OnEvent, "resource "+r.Ref),
			onEventLua:        parseLuaEventMap(r.OnEventLua, "resource "+r.Ref),
			onReactionLua:     parseLuaEventMap(r.OnReactionLua, "resource "+r.Ref+" (reaction)"),
		}
		// on_depleted ([G-D]): the DEPLETION-hook op-list, parsed like any op-list — for EVERY resource, not
		// just a vital one (#406): a non-vital pool's hook is its non-lethal consequence. A malformed list logs
		// loudly and registers with whatever parsed (content-lint discipline); nil/absent => no hook (a vital
		// pool still dies the engine-default way).
		if len(r.OnDepleted) > 0 {
			ops, err := parseOpList(r.OnDepleted)
			if err != nil {
				slog.Error("content: resource on_depleted parse failed; the depletion hook runs parsed ops only",
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
			targetResource: dt.TargetResource,
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
	// Player toggles (#358): register each content toggle_def. Its verb(s) become toggle commands
	// (dispatch consults toggleForVerb after baseTable/abilities/custom/channels — a toggle verb never
	// shadows a core verb). A verb word colliding with a BUILT-IN verb is dropped from the def + logged
	// LOUDLY (never silently shadowing), mirroring the custom-command collision guard. An empty pack
	// registers ZERO toggles => no toggle verbs (the empty-boot invariant). Toggles are CONTENT.
	for _, tg := range lc.ToggleDefs {
		def := buildToggleDef(tg)
		kept := def.words[:0]
		for _, w := range def.words {
			if _, builtin := baseTable.byExact[w]; builtin {
				slog.Warn("content: toggle verb rejected — it collides with a built-in command; rename it",
					"verb", w, "toggle", tg.Ref)
				continue
			}
			kept = append(kept, w)
		}
		def.words = kept
		d.toggle.register(tg.Ref, def)
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
	// Load-time content-lint: multiple vital resources are supported (#71 multi-vital, each independently
	// lethal), but with >1 vital and none flagged `primary` the default-damage pool falls back to an
	// arbitrary sorted-ref pick — nudge the author to designate one.
	lintVitalResources(d.res.table())
	// Load-time content-lint (#71): a deal_damage op's `resource` (the routed pool) must name a registered
	// resource — a typo silently discards the blow at runtime (max<=0 immunity). Warn so an author sees it.
	for _, m := range lintDealDamageResources(d) {
		slog.Warn("content: deal_damage.resource does not name a registered resource (blow will be discarded)",
			"owner", m.owner, "resource", m.resource)
	}
	// Load-time content-lint (#405): a damage type's `target_resource` must name a registered resource. The
	// blast radius here is far wider than a typo'd deal_damage.resource, which breaks ONE op: this one
	// silently routes EVERY blow of that kind — every weapon, spell, DoT and third-party source — into the
	// max<=0 immunity discard, so the whole damage kind quietly does nothing, everywhere, forever. ERROR,
	// not WARN, for that reason.
	for _, m := range lintDamageTypeResources(d) {
		slog.Error("content: damage_type.target_resource does not name a usable resource (unregistered, or no max_attr so its cap is always 0) — EVERY blow of this damage kind will be discarded",
			"damage_type", logcap.Value(m.dmgType), "resource", logcap.Value(m.resource))
	}
	// Load-time content-lint (#405 companion): a deal_damage op's `type` must name a registered damage type.
	// Nothing validated it before, because the only consumer (mitigate) no-ops silently on an unknown type.
	// Once a type can carry a ROUTE, a typo'd `type` loses the route as well and the blow falls back to the
	// primary vital — a psychic strike that quietly hits hp instead of sanity. That is a new silent-failure
	// mode the routing feature creates, so it gets its own lint.
	for _, m := range lintDealDamageTypes(d) {
		slog.Warn("content: deal_damage.type does not name a registered damage_type (no resist matrix, and no #405 pool routing)",
			"owner", m.owner, "type", logcap.Value(m.dmgType))
	}
	// Load-time content-lint (#406): a NON-VITAL resource's on_depleted is FARMABLE if it rewards. Unlike the
	// vital/death hook — latched to an actual kill by posDead, and for a mob followed by extraction — a
	// non-vital hook is level-triggered per BLOW, so a pool held at 0 re-runs it for one point of damage,
	// indefinitely, with no kill required. A granting op in one is therefore an economy exploit, not a
	// design. Warn LOUDLY at build rather than leaving it to prose in the DTO comment.
	for _, m := range lintDepletionHookGrants(d) {
		slog.Warn("content: a NON-VITAL resource's on_depleted contains a REWARDING op — it can be farmed by repeatedly damaging a pool already at 0 (no kill required); move the reward to a vital pool's hook or an event handler",
			"resource", m.resource, "op", m.op)
	}
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

// lintVitalResources nudges an author who defines MORE THAN ONE vital resource (#71 multi-vital) to
// designate a `primary` — the pool unrouted damage (a melee swing, a deal_damage with no `resource`)
// hits. Multiple vitals are fully supported (each is independently lethal; deal_damage routes to a
// chosen pool), but with none flagged primary the default-damage pool falls back to the lowest ref by
// sort order — an arbitrary, footgun-prone pick ("blood" sorts before "hp"). Build-time only; logs at
// WARN with the sorted vitals + the fallback pool. Does not abort boot (content-lint discipline).
func lintVitalResources(table map[string]*resourceDef) {
	var vitals []string
	hasPrimary := false
	for ref, def := range table {
		if def != nil && def.vital {
			vitals = append(vitals, ref)
			if def.primary {
				hasPrimary = true
			}
		}
	}
	if len(vitals) > 1 && !hasPrimary {
		sort.Strings(vitals)
		slog.Warn("content: multiple VITAL resources defined but none flagged `primary`; unrouted damage defaults to the lowest ref — designate a primary",
			"vitals", vitals, "fallback", vitals[0])
	}
}

// walkContentOps visits EVERY parsed op in the pack — every registered op-list (ability on_resolve +
// on_event, bundle grants, track step grants, affect on_event + tick, resource on_event + on_depleted),
// recursing into flow-op branches (then/els) and check bands. `owner` locates the op-list for a lint
// message. The op-payload content-lints (learn_profession refs, deal_damage pools) share this one
// traversal so a newly-reachable op-list is covered by every lint at once.
func walkContentOps(d *defRegistries, visit func(owner string, op *effectOp)) {
	var walk func(owner string, ops []effectOp)
	walk = func(owner string, ops []effectOp) {
		for i := range ops {
			op := &ops[i]
			visit(owner, op)
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
}

// dealDamageResourceMiss is one content-lint finding: a deal_damage op whose `resource` (the routed pool,
// #71 multi-vital) does NOT name a registered resource. owner locates the offending op-list.
type dealDamageResourceMiss struct {
	owner    string
	resource string
}

// lintDealDamageResources flags each deal_damage whose `resource` names a pool that is NOT a registered
// resource — an author typo that, at runtime, discards the blow as "no capacity" immunity (dealDamage's
// max<=0 guard) and so silently does nothing. Empty `resource` (route to the primary vital) is the common
// case and never flagged. Build-time only; the caller logs at WARN (does not abort boot). NOTE: a
// runtime-supplied pool string (Lua h:damage{resource=...}) can't be seen here — the runtime vital-gate is
// the real defense; this only catches static content typos.
func lintDealDamageResources(d *defRegistries) []dealDamageResourceMiss {
	var misses []dealDamageResourceMiss
	walkContentOps(d, func(owner string, op *effectOp) {
		if op.kind == "deal_damage" && op.resource != "" && d.res.get(op.resource) == nil {
			misses = append(misses, dealDamageResourceMiss{owner: owner, resource: op.resource})
		}
	})
	return misses
}

// damageTypeResourceMiss is one content-lint finding (#405): a damage type whose `target_resource` does
// not name a registered resource.
type damageTypeResourceMiss struct {
	dmgType  string
	resource string
}

// lintDamageTypeResources flags each damage type whose `target_resource` names a pool that is NOT a
// registered resource (#405). At runtime that route resolves to a pool with no def, so resourceMax reads 0
// and dealDamage's immunity discard drops the blow — meaning the ENTIRE damage kind silently does nothing,
// against every target, including damage authored by other packs.
//
// This is a flat registry walk, NOT walkContentOps: a damage-type def is not an op-list. Deterministic
// order (sorted) so the boot log is stable. Build-time only; the caller logs at ERROR.
func lintDamageTypeResources(d *defRegistries) []damageTypeResourceMiss {
	var misses []damageTypeResourceMiss
	for ref, def := range d.dmg.table() {
		if def == nil || def.targetResource == "" {
			continue
		}
		// BOTH ways to get the blast radius, not just one. An UNREGISTERED resource is the obvious typo; a
		// registered resource with NO max_attr is the silent twin — resourceMax is then always 0, so every
		// blow of this kind is discarded against every target, everywhere, exactly as if the ref were wrong.
		if rd := d.res.get(def.targetResource); rd == nil || rd.maxAttr == "" {
			misses = append(misses, damageTypeResourceMiss{dmgType: ref, resource: def.targetResource})
		}
	}
	sort.Slice(misses, func(i, j int) bool { return misses[i].dmgType < misses[j].dmgType })
	return misses
}

// dealDamageTypeMiss is one content-lint finding (#405): a deal_damage op whose `type` does not name a
// registered damage type. owner locates the offending op-list.
type dealDamageTypeMiss struct {
	owner   string
	dmgType string
}

// lintDealDamageTypes flags each deal_damage whose `type` names an unregistered damage type. Before #405
// an unknown type was merely inert (mitigate finds no resist matrix and applies none), which is why nothing
// linted it. Now a type can carry a POOL ROUTE, so a typo additionally loses the routing and the blow falls
// back to the primary vital — a `psychick` strike that quietly damages hp instead of sanity, with no error
// anywhere. Empty `type` (untyped damage) is the common case and never flagged. Build-time only; WARN.
//
// As with lintDealDamageResources, a runtime-supplied type (Lua h:damage{type=...}) is invisible here.
func lintDealDamageTypes(d *defRegistries) []dealDamageTypeMiss {
	var misses []dealDamageTypeMiss
	walkContentOps(d, func(owner string, op *effectOp) {
		if op.kind == "deal_damage" && op.dmgType != "" && d.dmg.get(op.dmgType) == nil {
			misses = append(misses, dealDamageTypeMiss{owner: owner, dmgType: op.dmgType})
		}
	})
	return misses
}

// depletionHookGrant is one content-lint finding (#406): a NON-VITAL resource's on_depleted op-list
// contains a REWARDING op. resource names the pool; op names the offending op kind.
type depletionHookGrant struct {
	resource string
	op       string
}

// rewardingOps is the set of op kinds that hand out durable value — items, progression, abilities,
// professions. They are the ops that must not sit in a farmable hook. Deliberately a DENY-list of things
// that GRANT, not an allow-list of things that are safe: a narration/affect/damage op in a depletion hook
// is the normal case and must stay lint-clean. Keep in sync with the op registry (effect_op.go).
var rewardingOps = map[string]bool{
	"produce_item": true, "augment_item": true,
	"grant_track": true, "advance_track": true,
	"grant_ability": true, "apply_bundle": true,
	"learn_profession": true,
}

// lintDepletionHookGrants flags a rewarding op inside a NON-VITAL resource's on_depleted (#406). The
// vital/death hook is exempt: it is latched to a real kill (posDead, and a mob is then extracted), so its
// reward is once-per-death. A non-vital hook has no such latch and is LEVEL-triggered — every blow that
// leaves the pool at 0 re-runs it, so a pool parked at 0 pays out for one point of damage per swing,
// forever, with nothing dying. It walks the op TREE (flow branches + check bands) via the shared walker, so
// a reward buried in an `if`/`chance`/`check` band is caught too. Build-time only; the caller logs at WARN
// (does not abort boot), like the other content-lints.
func lintDepletionHookGrants(d *defRegistries) []depletionHookGrant {
	var found []depletionHookGrant
	for ref, def := range d.res.table() {
		if def == nil || def.vital || len(def.onDepleted) == 0 {
			continue
		}
		var walk func(ops []effectOp)
		walk = func(ops []effectOp) {
			for i := range ops {
				op := &ops[i]
				if rewardingOps[op.kind] {
					found = append(found, depletionHookGrant{resource: ref, op: op.kind})
				}
				walk(op.then)
				walk(op.els)
				if op.check != nil {
					for j := range op.check.bands {
						walk(op.check.bands[j].ops)
					}
				}
			}
		}
		walk(def.onDepleted)
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].resource != found[j].resource {
			return found[i].resource < found[j].resource
		}
		return found[i].op < found[j].op
	})
	return found
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
	walkContentOps(d, func(owner string, op *effectOp) {
		if op.kind == "learn_profession" && !isProfBundle(op.profession) {
			misses = append(misses, learnProfessionMiss{owner: owner, profession: op.profession})
		}
	})
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

// errNoZoneContent is returned when the snapshot has no definition for the zone's template, and
// errIncompleteZoneBuild when it declares rooms whose prototypes are not in the cache. Both are ADVISORY to
// a boot caller and FATAL to a runtime one — see buildZone's contract.
var (
	errNoZoneContent       = errors.New("no such zone in the content snapshot")
	errIncompleteZoneBuild = errors.New("zone declares rooms with no prototype in the cache")
)

// buildZone constructs zone z (id == zoneRef) from its loaded definition: it spawns each room
// as a singleton entity, records the start room, and runs the reset script. The prototype
// cache must already be filled (defineContent).
//
// # It reports whether the build was COMPLETE, and the caller decides what that is worth
//
// It returns an error when the snapshot has no such zone, or when it declares rooms whose prototypes are
// missing from the cache. In both cases the zone is left partially built (possibly with no rooms at all).
// The zone object is still usable — every read path guards nil — so the error is a REPORT, not a panic.
//
// The two classes of caller want opposite things from that report, which is why it is returned rather than
// handled here:
//
//   - BOOT (NewShardFromContent) must tolerate it. "The engine boots with zero content" is a documented
//     invariant: a bare deployment with no packs comes up, serves a clean rejection on login, and lets a
//     builder connect and pull. Boot logs and continues.
//   - RUNTIME (HostZone, MintInstance) must REFUSE it, and this is the half #418 added. A runtime build
//     happens when a zone is already someone's destination — a drain handing players over, or a player
//     walking through a dungeon door. Publishing a roomless zone there does not degrade, it DROPS them:
//     transferIn's "destination has no rooms" branch disconnects every arriving session, and for HostZone
//     the shard then holds a renewed lease on a black hole that nothing self-heals, because from the
//     coordinator's view the zone is claimed and healthy. Refusing instead leaves the zone unowned, which
//     is visible, alertable and retryable.
//
// Before #418 the runtime case could not arise: the snapshot was frozen at boot, so it always described a
// zone whose prototypes defineContent had already loaded. Now the snapshot tracks reloads while the cache is
// fed ref-by-ref off the bus, so a build can land in the window where the two disagree.
func (z *Zone) buildZone(lc *content.LoadedContent) error {
	// By TEMPLATE, not id: this is the one genuinely content-shaped question in the zone lifecycle, so it is
	// the one place the two must differ for an instance (#72). They are equal for every zone that exists
	// today, so this is inert until instances land.
	//
	// Normalize an unset template to the id first. newZone always sets it, but a hand-built Zone struct
	// literal (a few tests) does not, and an empty template would look up `lc.Zone("")`, find nothing, and
	// boot the zone EMPTY behind a Debug line — a zone silently having no rooms is the worst failure shape in
	// this package, so fail safe rather than fail quiet.
	if z.template == "" {
		z.template = z.id
	}
	zd := lc.Zone(z.template)
	if zd == nil {
		z.log.Debug("zone has no loaded content; booting empty", "zone", z.id, "template", z.template)
		return errNoZoneContent
	}
	missing := 0
	for _, r := range zd.Rooms {
		if z.spawnRoom(ProtoRef(r.Ref)) == nil {
			missing++
		}
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
	if missing > 0 {
		return fmt.Errorf("%w: %q is missing %d of %d room prototypes", errIncompleteZoneBuild, z.template, missing, len(zd.Rooms))
	}
	return nil
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
	// The demo pack is embedded and defineContent ran over the same content two lines up, so an incomplete
	// build here is a corrupt checked-in pack, not a runtime condition — panic, like the parse failure above.
	if err := z.buildZone(lc); err != nil {
		panic("world: build demo zone " + id + ": " + err.Error())
	}
	return z
}

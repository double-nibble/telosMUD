package world

import (
	"sync"
	"sync/atomic"
)

// defs.go holds the per-shard registries for the pack-GLOBAL definition kinds (attributes,
// resources, damage types — Phase 5.1; affects/abilities arrive in 5.2/5.3). These are NOT the
// prototype cache: a prototype is an instancing template the spawner clones, whereas these are
// flat content DEFINITIONS the runtime reads by ref (attr() looks up an attributeDef, the resource
// model looks up a resourceDef). They are content (docs/ABILITIES.md §1) — the engine knows the
// KIND, content supplies the instances.
//
// # The atomic-swap shape (mirrors protoCache, prototype.go)
//
// Each registry is a defRegistry[T]: a single per-shard atomic.Pointer to an immutable ref->def
// table, swapped wholesale. Reads (the hot attr()/resource paths, on ANY zone goroutine) are a
// lock-free atomic.Load; writes (build-time register, the 4.3-style hot reload) copy-then-Store a
// fresh table under writeMu. This is the EXACT pattern protoCache uses, so a later slice can
// hot-reload a damage_type/attribute without restart by dropping a reload() in — the shape is
// already here even though invalidation isn't published this slice.
//
// Like protoCache, the registries are built once at shard construction (before any zone goroutine
// runs) and then only read, so the cross-goroutine sharing needs no lock beyond the publish.

// defRegistry is the generic atomic-swap registry for one definition kind. T is a pointer to the
// def struct (so a Load returns nil for an absent ref, never a zero-struct false positive).
type defRegistry[T any] struct {
	// live is the published table, swapped atomically. Every read (get) Loads it; every write
	// (register/reload) Stores a fresh copy with the one entry changed. Holding a pointer to the
	// map makes the swap a single atomic op. NEVER index a Loaded table for WRITE.
	live atomic.Pointer[map[string]T]
	// writeMu serializes the WRITE path (register/reload's read-copy-store) so two writers can't
	// both copy the same base and clobber each other. Readers never take it.
	writeMu sync.Mutex
}

// newDefRegistry builds an empty registry with an empty published table.
func newDefRegistry[T any]() *defRegistry[T] {
	r := &defRegistry[T]{}
	empty := map[string]T{}
	r.live.Store(&empty)
	return r
}

// table returns the currently published table (always non-nil after newDefRegistry).
func (r *defRegistry[T]) table() map[string]T { return *r.live.Load() }

// get returns the def for ref, or the zero value of T (nil for a pointer T) when absent. Read-only
// and safe from any zone goroutine: a pure atomic.Load, never racing a concurrent reload swap.
func (r *defRegistry[T]) get(ref string) T {
	var zero T
	if d, ok := r.table()[ref]; ok {
		return d
	}
	return zero
}

// has reports whether ref is registered.
func (r *defRegistry[T]) has(ref string) bool {
	_, ok := r.table()[ref]
	return ok
}

// len reports how many defs are registered (used by the bare-engine assertion: 0 with no pack).
func (r *defRegistry[T]) len() int { return len(r.table()) }

// register publishes def under ref, copy-then-swap under writeMu (build-time path: uncontended,
// the cache is still private to the construction goroutine). It leaves the registry PUBLISHED so
// the runtime read path is identical whether a ref was registered at boot or hot-reloaded.
func (r *defRegistry[T]) register(ref string, def T) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	cur := r.table()
	next := make(map[string]T, len(cur)+1)
	for k, v := range cur {
		next[k] = v
	}
	next[ref] = def
	r.live.Store(&next)
}

// reload swaps the def for ref race-safely (the 4.3-style hot-reload path, mirrored from
// protoCache.reload): a non-nil def replaces/inserts the entry; a nil def REMOVES it (a deleted
// channel_def). It copy-then-Stores a fresh table under writeMu, so a concurrent lock-free reader
// (get on any zone goroutine) sees the old or the new table whole, never a half-applied map. Unlike
// register (build-time, uncontended) this runs at RUNTIME off the content-bus subscription goroutine,
// so the copy-then-swap discipline is load-bearing.
func (r *defRegistry[T]) reload(ref string, def T, remove bool) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	cur := r.table()
	next := make(map[string]T, len(cur)+1)
	for k, v := range cur {
		if remove && k == ref {
			continue
		}
		next[k] = v
	}
	if !remove {
		next[ref] = def
	}
	r.live.Store(&next)
}

// defRegistries bundles the per-shard global-definition registries so a zone holds one pointer
// (mirroring how it holds one *protoCache). Built once at shard construction, shared read-only.
type defRegistries struct {
	attr    *defRegistry[*attributeDef]
	res     *defRegistry[*resourceDef]
	dmg     *defRegistry[*damageTypeDef]
	affect  *defRegistry[*affectDef]
	ability *defRegistry[*abilityDef]
	combat  *defRegistry[*combatProfile]
	channel *defRegistry[*channelDef]
	track   *defRegistry[*trackDef]      // Phase 11.2 advancement tracks
	bundle  *defRegistry[*bundleDef]     // Phase 11.4b class/race/feat bundles
	rarity  *defRegistry[*rarityTierDef] // Phase 12.1 rarity tiers
	loot    *defRegistry[*lootTableDef]  // Phase 12.1 loot tables
	recipe  *defRegistry[*recipeDef]     // Phase 13.5 crafting recipes

	// defaultCombat is the pack's player-default combat profile ref (Phase 6.3a): the profile a player
	// entity fights with when its own (none — players aren't prototyped) declares none. newPlayerEntity
	// stamps it onto the player's Living.combatRef so a player `kill` runs the same content pipeline as a
	// mob. Empty => players have no profile (auto-hit). Set once at construction, then read-only.
	defaultCombat string

	// abilityCmds is the per-shard ability COMMAND table (defs.go is per-shard data): the verb
	// words a command-invocation ability registers (defineGlobals), each mapping to the abilityDef
	// the lifecycle enters. dispatch consults it AFTER the built-in baseTable so a content ability
	// never shadows a core verb. Built once at construction (single goroutine), then read-only — same
	// publish-once-then-read discipline as the registries, so no lock is needed for the read path.
	abilityCmds map[string]*abilityDef

	// customCmds is the per-shard CUSTOM-COMMAND table (Phase 7.4e): a verb/alias word -> the Lua
	// body implementing it. dispatch consults it AFTER baseTable AND abilityCmds, by EXACT match
	// only, so a custom verb can never shadow or abbreviate a core/movement/ability verb. Built once
	// at construction, then read-only.
	customCmds map[string]string

	// pvpLua is the pack PvP-policy Lua hook (7.4f): consulted by the harm gate; empty => the
	// built-in policy. formulas maps a ruleset-formula name (to_hit/soak/regen/xp_for) to its Lua
	// body (7.4f). Both built once, read-only.
	pvpLua   string
	formulas map[string]string
}

// newDefRegistries builds an empty bundle (all three registries empty/published). A bare zone gets
// its own so attr()/resource reads work standalone and report 0/absent — the bare-engine invariant.
func newDefRegistries() *defRegistries {
	return &defRegistries{
		attr:        newDefRegistry[*attributeDef](),
		res:         newDefRegistry[*resourceDef](),
		dmg:         newDefRegistry[*damageTypeDef](),
		affect:      newDefRegistry[*affectDef](),
		ability:     newDefRegistry[*abilityDef](),
		combat:      newDefRegistry[*combatProfile](),
		channel:     newDefRegistry[*channelDef](),
		track:       newDefRegistry[*trackDef](),
		bundle:      newDefRegistry[*bundleDef](),
		rarity:      newDefRegistry[*rarityTierDef](),
		loot:        newDefRegistry[*lootTableDef](),
		recipe:      newDefRegistry[*recipeDef](),
		abilityCmds: map[string]*abilityDef{},
		customCmds:  map[string]string{},
		formulas:    map[string]string{},
	}
}

// attrDefs / resourceDefs / damageTypeDefs are the zone-goroutine read accessors for the global
// registries. Each is a lock-free atomic.Load under the hood. A bare zone (no shard) falls back to
// its own empty bundle so the reads never nil-deref and report "no content defined".
func (z *Zone) attrDefs() *defRegistry[*attributeDef] {
	return z.defBundle().attr
}

func (z *Zone) resourceDefs() *defRegistry[*resourceDef] {
	return z.defBundle().res
}

func (z *Zone) damageTypeDefs() *defRegistry[*damageTypeDef] {
	return z.defBundle().dmg
}

func (z *Zone) affectDefs() *defRegistry[*affectDef] {
	return z.defBundle().affect
}

func (z *Zone) abilityDefs() *defRegistry[*abilityDef] {
	return z.defBundle().ability
}

// combatProfiles is the zone-goroutine read accessor for the global combat-profile registry (combat.go,
// Phase 6.3a). Lock-free atomic.Load; a bare zone falls back to its own empty bundle (no profiles).
func (z *Zone) combatProfiles() *defRegistry[*combatProfile] {
	return z.defBundle().combat
}

// channelDefs is the zone-goroutine read accessor for the global channel registry (Phase 8.3). Lock-
// free atomic.Load; a bare zone falls back to its own empty bundle (no channels => no channel verbs).
func (z *Zone) channelDefs() *defRegistry[*channelDef] {
	return z.defBundle().channel
}

// trackDefs is the zone-goroutine read accessor for the global advancement-track registry (Phase 11.2).
// Lock-free atomic.Load; a bare zone falls back to its own empty bundle (no tracks).
func (z *Zone) trackDefs() *defRegistry[*trackDef] {
	return z.defBundle().track
}

// bundleDefs is the zone-goroutine read accessor for the global class/race/feat bundle registry (Phase
// 11.4b). Lock-free atomic.Load; a bare zone falls back to its own empty bundle (no bundles).
func (z *Zone) bundleDefs() *defRegistry[*bundleDef] {
	return z.defBundle().bundle
}

// rarityTierDefs / lootTableDefs are the zone-goroutine read accessors for the loot registries (Phase
// 12.1). Lock-free atomic.Load; a bare zone falls back to its own empty bundle (no loot).
func (z *Zone) rarityTierDefs() *defRegistry[*rarityTierDef] { return z.defBundle().rarity }
func (z *Zone) lootTableDefs() *defRegistry[*lootTableDef]   { return z.defBundle().loot }
func (z *Zone) recipeDefs() *defRegistry[*recipeDef]         { return z.defBundle().recipe }

// channelForVerb returns the channel a verb emits on (lower-cased), or nil. The verb→channel mapping
// is DERIVED from the channel registry on each lookup (the table is small) rather than cached in a
// separate map, so it stays consistent with a hot-reloaded registry FOR FREE: a channel whose verb
// changed (or that was removed) is reflected on the next dispatch without a second swap to keep in
// sync. dispatch consults this AFTER the built-in baseTable + abilityCmds + customCmds (a channel verb
// never shadows a core/ability/custom verb). Read-only, safe from any zone goroutine.
func (z *Zone) channelForVerb(v string) *channelDef {
	for _, def := range z.channelDefs().table() {
		for _, w := range def.words {
			if w == v {
				return def
			}
		}
	}
	return nil
}

// abilityForVerb returns the command-invocation ability bound to verb `v` (lower-cased), or nil.
// dispatch consults it after the built-in command table. Read-only (the table is published once at
// construction), safe from any zone goroutine.
func (z *Zone) abilityForVerb(v string) *abilityDef {
	b := z.defBundle()
	if b.abilityCmds == nil {
		return nil
	}
	return b.abilityCmds[v]
}

// defBundle returns the zone's registry bundle, lazily creating an empty private one if a bare zone
// was constructed without it (defensive — newZone wires one). Single-writer (zone goroutine).
func (z *Zone) defBundle() *defRegistries {
	if z.defs == nil {
		z.defs = newDefRegistries()
	}
	return z.defs
}

// --- The def structs (runtime forms of the content DTOs) -------------------------------------

// attributeDef is the runtime form of an AttributeDTO: a content-defined attribute with its base
// (literal or a parsed formula AST) and an optional clamp range. It is immutable after build —
// shared read-only across zone goroutines via the registry, exactly like a *Prototype.
type attributeDef struct {
	ref         string
	displayName string
	valueKind   string // "int" | "float" | "derived"

	// base is the default base of the attribute, evaluated against an entity's attributes when no
	// per-entity override is present. nil means base 0. A literal is a litNode; a derived attribute
	// is an arbitrary formula tree (formula.go). attr() resolves it recursively (derived-of-derived).
	base formulaNode

	// min/max clamp the resolved value (after mods). nil means unbounded on that side.
	min *float64
	max *float64

	// stat marks a PLAYER-FACING character stat surfaced in the GMCP Char.Stats HUD (Phase 9.2). The
	// engine never decides which attributes are "stats" — content flags them (AttributeDTO.Stat), so a
	// derived/internal attribute stays out of the panel. Default false.
	stat bool
}

// resourceDef is the runtime form of a ResourceDTO: a named pool whose MAX is a derived attribute
// (maxAttr) — so gear/affects that raise that attribute flow through to the cap (§1.2). The engine
// holds `current` per entity (Living); this def supplies max/vital/regen. Immutable after build.
type resourceDef struct {
	ref               string
	displayName       string
	maxAttr           string // derived-attr ref capping the pool; "" => no cap (unbounded)
	vital             bool   // depletion drives death (on_depleted) — wired in 5.2/combat
	regen             int    // per-tick flat regen (reserved; regen ticks ride 5.2)
	regenInCombat     bool   // keep regenerating while the owner is fighting (default false: pause in combat)
	depletedThreshold int    // reserved (vital depletion threshold)
	// perRound marks a per-round REACTION budget ([G9], combat.go topUpReactions): a resource topped up to
	// its derived max at the start of EVERY combat round, so a reactor gets a bounded number of reactions
	// (opportunity attacks) per round and a spent reaction does not refill until the next round. Content
	// declares `per_round: true`; the engine names no "reactions" pool — the flag is the convention.
	perRound bool
	// onEvent subscribes content op-lists to in-zone engine events ([G3], event.go). An entity that
	// HAS this resource (a positive max or a stored current) reacts to the keyed event — e.g. a `rage`
	// pool with onEvent[OnHit] = modify_resource rage +N is the canonical builder. nil => no handlers.
	onEvent map[eventKind][]effectOp
	// onEventLua is the Lua-BODY alternative to onEvent (Phase 7.4g): a Lua handler for the keyed
	// event, run under the SAME depth/width budget as an op-list handler. nil => no Lua handlers.
	onEventLua map[eventKind]string
	// onReactionLua is the RESULT-ALTERING reaction surface (Phase 7.9): a Lua handler keyed by a
	// reaction checkpoint kind that receives a typed `rx` (e.g. a `reactions` pool carrying a
	// onReactionLua[BeforeCastCommit] counterspell). Distinct from onEventLua so a checkpoint fired by
	// both the bus and the reaction pass never double-fires one handler. nil => none.
	onReactionLua map[eventKind]string
	// onDepleted is the parsed op-list the engine runs on the dying entity when this VITAL resource
	// hits 0 ([G-D] death hook, death.go). Runs BEFORE die() drops combat / builds the corpse, with the
	// victim as $actor, so content can narrate / fire a last effect. nil/empty => engine default death
	// only. Only consulted for the vital resource (vitalResource). Immutable after build.
	onDepleted []effectOp
}

// damageTypeDef is the runtime form of a DamageTypeDTO: a named damage type with a resist/vuln/
// immune matrix (other-type ref -> multiplier). The shared mitigation pipeline (5.3) reads it.
type damageTypeDef struct {
	ref         string
	displayName string
	color       string
	resist      map[string]float64
}

// affectStacking is the stacking mode of an affect_def (P5-D3, docs/PHASE5-PLAN.md §1.4). It governs
// what happens when an affect is applied to a target that already has an instance keyed by the same
// (ref[, source]). The default (zero / unknown) is refresh.
type affectStacking int

const (
	stackRefresh affectStacking = iota // reset duration to full (default); buffs like haste
	stackCount                         // count up to maxStacks, magnitude scales; DoTs like poison
	stackExtend                        // sum remaining + new duration
	stackIgnore                        // first wins; the new application is a no-op
)

// parseStacking maps the content stacking string onto the enum. Unknown/"" => refresh (the §5 default).
func parseStacking(s string) affectStacking {
	switch s {
	case "stack":
		return stackCount
	case "extend":
		return stackExtend
	case "ignore":
		return stackIgnore
	default:
		return stackRefresh
	}
}

// affectModifier is one parsed entry of an affect's modifier list: it adds (add==true) `value` to
// attribute `attr` or multiplies by it (add==false) while the affect is active. The Affected runtime
// sums/multiplies these across active affects into the entity's single mod source (attributes.go §1.1).
type affectModifier struct {
	attr  string
	add   bool // true => additive (flatMod); false => multiplicative (mulMod)
	value float64
}

// affectDef is the runtime form of an AffectDTO (docs/ABILITIES.md §5): a content-defined status
// effect. Immutable after build — shared read-only across zone goroutines via the registry, exactly
// like a *Prototype/*attributeDef. The Affected runtime reads it on attach/tick/expire.
type affectDef struct {
	ref         string
	name        string
	category    string
	stacking    affectStacking
	maxStacks   int  // ceiling for stackCount; >=1
	scopeTarget bool // stack_scope=="target": one instance per ref (ignore source); else per (ref,source)
	dispellable bool
	// roomScoped marks a ROOM-scoped affect ([G13], docs/PHASE6-PLAN.md §1.3): instead of attaching to a
	// living entity, it attaches to the ROOM entity, ticks over the room's living OCCUPANTS each interval
	// (applying its on_tick / its modifier-or-CC to them), and lands on ENTRANTS on arrival. A web,
	// darkness, silence-field, or consecrate is a room affect. Default false (the Phase-5 per-entity
	// shape). The Affected runtime hosts both; roomScoped routes the tick over occupants (affect_room.go).
	roomScoped bool

	duration int // base duration in PULSES (heartbeat-denominated; conserved across save/load)

	modifiers []affectModifier // additive/multiplicative attribute mods while active
	prevents  []string         // tags this affect blocks (§6 tag CC); the runtime unions these

	tickInterval int  // fire on_tick every N pulses; 0 => no tick
	hasTick      bool // whether a tick spec was authored (interval may legitimately be 0-guarded)
	// onTick is the raw on_tick op-list (carried opaque from the DTO). tickOps is the PARSED form the
	// gated effect-op interpreter runs each tick (a DoT's deal_damage). Phase 5.3 completes this: the
	// tick MECHANISM was live in 5.2; the op execution lands here. A nil/empty tickOps => a timer-only
	// tick (the interval still counts, but fires no effect).
	onTick  any
	tickOps []effectOp
	// onApply/onExpire are the RESERVED apply/expire hooks (Phase 7 Lua op-list shape). Read-not-run.
	onApply  any
	onExpire any
	// onApplyLua/onExpireLua/onDispelLua are the Lua affect hooks (Phase 7.4d): run when the affect
	// attaches/expires/is dispelled, `self` = the affected entity, actor = the affect's source.
	onApplyLua  string
	onExpireLua string
	onDispelLua string
	// onEvent subscribes content op-lists to in-zone engine events ([G3], event.go) while the affect is
	// active on an entity — a proc affect (e.g. a "bloodlust" buff whose onEvent[OnKill] heals). The
	// runtime gathers these from the entity's ACTIVE affects at fire time. nil => no handlers.
	onEvent map[eventKind][]effectOp
	// onEventLua is the Lua-BODY alternative to onEvent (Phase 7.4g), run under the SAME budget.
	onEventLua map[eventKind]string
	// onReactionLua is the RESULT-ALTERING reaction surface (Phase 7.9, P7-D8/T12): a Lua handler
	// keyed by a reaction CHECKPOINT kind (BeforeCastCommit / ToHit / OnDamageTaken) that receives a
	// typed `rx` and may cancel/modify/replace_target/consume_resource. It is DELIBERATELY SEPARATE
	// from onEventLua (the declarative bus): a declarative bus handler runs via fireEvent and CANNOT
	// alter a result; a reaction handler runs via the reaction pass (luareact.go) WITH an `rx`. Keeping
	// them distinct means a checkpoint fired by BOTH the bus and the reaction pass (OnDamageTaken) never
	// double-fires one handler. A concentration affect carries onReactionLua[OnDamageTaken] (rx:cancel()
	// drops the affect on a failed save). nil => no reaction handlers. Run under the SAME budget.
	onReactionLua map[eventKind]string
}

// detrimentalCategories is the set of affect categories the engine treats as harmful BY CATEGORY,
// regardless of the affect's modifiers. A content category-name is just data, but these well-known
// names denote affliction kinds (a debuff/affliction/curse/poison/disease) so the derived-harm gate
// (affectIsDetrimental) errs toward gating even when an author labeled the apply_affect helpful. It is
// an OR input, not the whole story — the stat/prevents derivation below catches the unlabeled cases.
var detrimentalCategories = map[string]bool{
	"debuff":     true,
	"affliction": true,
	"curse":      true,
	"poison":     true,
	"disease":    true,
}

// affectIsDetrimental DERIVES whether an affect_def is harmful to its target FROM THE DEF ITSELF —
// never from a content-supplied "harmful"/disposition label (which a mislabeled or malicious pack can
// lie about, §7/D2). The derived-harm gate (opApplyAffect) ORs this with the explicit label so an
// author can still force-gate, but can never UN-gate a genuinely-detrimental affect by labeling it
// helpful/neutral/unlabeled. An affect counts as detrimental when ANY of:
//
//   - a modifier REDUCES a stat: an additive modifier with value<0 (e.g. -2 strength), or a
//     multiplicative modifier with value<1 (e.g. ×0.5 speed);
//   - the def declares ANY prevents tags (a CC affect — root/silence/etc. is harm by construction);
//   - the def's category is in detrimentalCategories (a well-known affliction kind).
//
// A genuinely-beneficial affect (only stat-raising modifiers, no prevents, a non-affliction category)
// returns false and stays UNGATED — a buff on an ally still lands. Pure read of the immutable def.
func affectIsDetrimental(def *affectDef) bool {
	if def == nil {
		return false
	}
	for _, m := range def.modifiers {
		if m.add && m.value < 0 {
			return true // a flat stat reduction
		}
		if !m.add && m.value < 1 {
			return true // a multiplicative stat reduction (×<1)
		}
	}
	if len(def.prevents) > 0 {
		return true // any CC tag is harm by construction
	}
	if detrimentalCategories[def.category] {
		return true
	}
	return false
}

// abilityDisposition is the harmful/helpful/neutral intent of an ability or op (docs/ABILITIES.md
// §7). It is what drives the PvP gate: ONLY dispHarmful routes through pvp_allowed. The op-level
// guard (guardHarmful) also keys off disposition so a debuff apply_affect is gated while a buff is not.
type abilityDisposition int

const (
	dispNeutral abilityDisposition = iota
	dispHelpful
	dispHarmful
)

// parseDisposition maps the content string onto the enum. Unknown/"" => neutral (ungated). A neutral
// or helpful ability never routes through the PvP gate; only "harmful" does.
func parseDisposition(s string) abilityDisposition {
	switch s {
	case "harmful":
		return dispHarmful
	case "helpful":
		return dispHelpful
	default:
		return dispNeutral
	}
}

// targetMode is an ability's targeting mode (docs/ABILITIES.md §2): which entity(ies) the resolver
// selects. self/none need no argument; enemy/ally select a living in the room by the typed keyword.
type targetMode int

const (
	tmNone targetMode = iota
	tmSelf
	tmEnemy
	tmAlly
)

func parseTargetMode(s string) targetMode {
	switch s {
	case "self":
		return tmSelf
	case "enemy":
		return tmEnemy
	case "ally":
		return tmAlly
	default:
		return tmNone
	}
}

// resourceCost is one resource an ability spends (reserved on cast, paid on commit, refunded on
// interrupt). The runtime form of a content.ResourceCostDTO.
type resourceCost struct {
	resource string
	amount   int
}

// abilityDef is the runtime form of an AbilityDTO (docs/ABILITIES.md §2): a content-defined ability
// the engine's fixed lifecycle (ability.go) runs. Immutable after build — shared read-only across
// zone goroutines via the registry, exactly like a *Prototype/*affectDef. The lifecycle reads its
// targeting/requires/costs/timing and its on_resolve op-list; the engine knows the KIND, this is the
// whole skill.
type abilityDef struct {
	ref        string
	name       string
	invocation string // "command" | "proc" | "passive" — this phase wires "command"; proc/passive reserve hooks
	words      []string

	mode        targetMode
	disposition abilityDisposition
	// area is the AoE shape ([G12]): "" (single-target), "room", or "room_and_adjacent". It is also
	// stamped onto each top-level on_resolve op (ability_build.go) so the interpreter loops per target;
	// kept on the def for inspection/lint and so the lifecycle can relax single-target resolution for an
	// area ability (an area ability needs no explicit keyword target).
	area string

	tags               []string           // §6 CC tags this ability carries (an affect's prevents[] blocks them)
	notPrevented       []string           // requires.not_prevented: extra tags the actor must not be prevented from
	reqAttr            map[string]float64 // requires.attr: per-attribute minimum thresholds
	skill              string             // Phase 11.3: marks this ability as a SKILL — using it fires OnSkillUse (the use-based hook)
	requiresGrant      bool               // Phase 11.4a: when true, the actor must have been GRANTED this ability (a class/trained ability)
	requiresProfession string             // Phase 13.3: when set, the actor must have LEARNED this profession (a crafting verb)

	costs []resourceCost

	castTime int // pulses of interruptible cast lockout; 0 => straight to commit
	lag      int // WAIT_STATE pulses imposed on commit (reserved-coarse: logged this phase)
	cooldown int // per-ability cooldown in pulses, armed on commit (transient this slice)

	// ops is the PARSED on_resolve op-list (effect_op.go). Each op is a registered handler; the
	// interpreter walks them in step 8. nil => the ability resolves with no effect (just messages).
	ops []effectOp

	// onResolveLua is RESERVED (Phase 7): read at load, NEVER executed this phase.
	onResolveLua string

	// onEvent subscribes content op-lists to in-zone engine events ([G3], event.go) for a known/granted
	// ability. Per-entity ability subscriptions await the Skilled component (a later slice); the field +
	// parse exist now so an ability pack can author them. nil => no handlers.
	onEvent map[eventKind][]effectOp

	msgActor string // step-9 "You ..." emit template
	msgRoom  string // step-9 "$n ..." bystander emit template
}

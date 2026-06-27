package world

import (
	"log/slog"
	"reflect"

	"github.com/double-nibble/telosmud/internal/content"
)

// The DTO -> component mapper (docs/PHASE4-PLAN.md D5). The content package owns the on-disk
// transfer structs (content.*DTO); THIS file owns the explicit translation onto the runtime
// *Room/*Physical/*Wearable/*Weapon/*Container component structs. Keeping the mapper here
// (and the json tags only on the DTOs) means the world component layout is never frozen to a
// persistence format, and the world package is the sole place a *Prototype is constructed —
// the loader calls protoCache.define (build.go), and define's component set comes from here.
//
// These builders mirror the old defineTorch/defineHelmet/defineSword/defineChest exactly:
// each adds only the components the DTO carries (a nil DTO pointer => component absent), so a
// prototype's component set is byte-identical to the hand-authored one. The parity test
// (content_parity_test.go) is the guard.

// roomComponents builds the component template for a room prototype: a *Room whose exits map
// is populated from the DTO at authoring time (immutable thereafter; an instance that
// re-routes an exit COWs via mutableRoom). Mirrors the old defineRoom.
func roomComponents(r content.RoomDTO) componentSet {
	exits := make(map[string]ProtoRef, len(r.Exits))
	for dir, to := range r.Exits {
		exits[dir] = ProtoRef(to)
	}
	room := &Room{exits: exits, sector: r.Sector}
	if len(r.Flags) > 0 {
		room.namedFlags = make(map[string]bool, len(r.Flags))
		for _, f := range r.Flags {
			room.namedFlags[f] = true
		}
	}
	return componentSet{reflect.TypeFor[*Room](): room}
}

// protoComponents builds the component template for an item/mob prototype from the present
// DTO sub-structs. Only non-nil components are added, exactly as the old define* helpers did.
func protoComponents(p content.ProtoDTO) componentSet {
	comps := componentSet{}
	if d := p.Physical; d != nil {
		comps[reflect.TypeFor[*Physical]()] = &Physical{
			weight: d.Weight, size: d.Size, material: d.Material,
		}
	}
	if d := p.Wearable; d != nil {
		comps[reflect.TypeFor[*Wearable]()] = wearableFromNames(d.Locations)
	}
	if d := p.Weapon; d != nil {
		comps[reflect.TypeFor[*Weapon]()] = &Weapon{
			diceNum: d.DiceNum, diceSize: d.DiceSize, damageType: d.DamageType,
			class: d.Class, attackVerb: d.AttackVerb,
		}
	}
	if d := p.Container; d != nil {
		comps[reflect.TypeFor[*Container]()] = &Container{
			capacity: d.Capacity, weightLimit: d.WeightLimit,
			closed: d.Closed, locked: d.Locked, keyRef: ProtoRef(d.KeyRef),
		}
	}
	// A prototype with a Living block is a MOB (Phase 6.3a): give it a *Living template carrying its
	// stat sheet + combat-profile ref. Spawn promotes this to the instance's hot e.living pointer; a
	// first stat/resource write COWs it (mutableComponent), so two goblins never alias each other's hp.
	if l := protoLiving(p.Living); l != nil {
		comps[reflect.TypeFor[*Living]()] = l
	}
	return comps
}

// buildAffectDef maps an AffectDTO onto the runtime affectDef (defs.go). It parses the stacking enum,
// the modifier list (add|mul), the prevents tags, and the tick spec; the on_tick/on_apply/on_expire
// op-lists are carried OPAQUE (RESERVED for 5.3's gated effect-op interpreter — this slice builds the
// tick mechanism, not the op execution). Duration is in pulses. Build-time only (defineGlobals).
func buildAffectDef(a content.AffectDTO) *affectDef {
	maxStacks := a.MaxStacks
	if maxStacks < 1 {
		maxStacks = 1
	}
	mods := make([]affectModifier, 0, len(a.Body.Modifiers))
	for _, m := range a.Body.Modifiers {
		// op defaults to "add"; only "mul" is multiplicative. An unknown op is treated as add (the
		// safe additive identity-friendly default) — content-lint is the real gate.
		mods = append(mods, affectModifier{attr: m.Attr, add: m.Op != "mul", value: m.Value})
	}
	var prevents []string
	if len(a.Body.Prevents) > 0 {
		prevents = append(prevents, a.Body.Prevents...)
	}
	def := &affectDef{
		ref:         a.Ref,
		name:        a.Name,
		category:    a.Category,
		stacking:    parseStacking(a.Stacking),
		maxStacks:   maxStacks,
		scopeTarget: a.StackScope == "target",
		dispellable: a.Dispellable,
		duration:    a.Body.Duration,
		modifiers:   mods,
		prevents:    prevents,
		onApply:     a.Body.OnApply,
		onExpire:    a.Body.OnExpire,
		onEvent:     parseEventMap(a.Body.OnEvent, "affect "+a.Ref),
	}
	if t := a.Body.Tick; t != nil {
		def.hasTick = true
		def.tickInterval = t.Interval
		def.onTick = t.OnTick
		// Phase 5.3: parse the on_tick op-list into the typed effectOp tree the gated interpreter runs
		// each tick (a DoT's deal_damage). A malformed list logs + carries whatever parsed (content-lint
		// is the real gate); a nil list (a timer-only tick) parses to nil.
		ops, err := parseOpList(t.OnTick)
		if err != nil {
			slog.Error("content: affect on_tick parse failed; tick will fire no effect",
				"affect", a.Ref, "err", err)
		}
		def.tickOps = ops
	}
	return def
}

// protoLiving builds the *Living component template for a mob prototype from its LivingDTO (Phase
// 6.3a): the per-entity attribute BASE overrides (the mob's str/con/accuracy/...) and the combat
// profile REF the swing pipeline resolves. A nil DTO returns nil (an inert item — no Living). The
// attrBase map is the mob's stat sheet; combatRef names the pack-global profile (resolved at fight
// time, not here, so build order is irrelevant). position defaults to posStanding (the zero value).
func protoLiving(d *content.LivingDTO) *Living {
	if d == nil {
		return nil
	}
	l := &Living{combatRef: d.CombatProfile}
	if len(d.Attributes) > 0 {
		l.attrBase = make(map[string]float64, len(d.Attributes))
		for k, v := range d.Attributes {
			l.attrBase[k] = v
		}
	}
	return l
}

// buildCombatProfile parses a CombatProfileDTO into the runtime combatProfile (combat.go): the to-hit
// check spec, the ordered avoidance ladder (each a check spec), and the [G-A] damage bonus formula.
// Build-time only (defineGlobals). A malformed sub-spec logs loudly and is dropped from the profile
// (content-lint is the real gate) rather than aborting boot. Returns the profile (never nil — an
// all-malformed profile is an empty one that auto-hits).
func buildCombatProfile(d content.CombatProfileDTO) *combatProfile {
	p := &combatProfile{}
	if d.ToHit != nil {
		spec, err := parseCheckSpec(d.ToHit)
		if err != nil {
			slog.Error("content: combat profile to_hit parse failed; profile auto-hits",
				"profile", d.Ref, "err", err)
		} else {
			p.toHit = spec
		}
	}
	for i, av := range d.Avoidance {
		spec, err := parseCheckSpec(av)
		if err != nil {
			slog.Error("content: combat profile avoidance check parse failed; dropped",
				"profile", d.Ref, "index", i, "err", err)
			continue
		}
		p.avoidance = append(p.avoidance, spec)
	}
	if d.DamageBonus != nil {
		node, err := parseFormula(d.DamageBonus)
		if err != nil {
			slog.Error("content: combat profile damage_bonus parse failed; ignored",
				"profile", d.Ref, "err", err)
		} else {
			p.damageBonus = node
		}
	}
	return p
}

// wearLocByName resolves a content wear-location NAME to the internal WearLoc slot. The names
// are the human labels (the inverse of wearLocName), so content authors never see the enum.
var wearLocByName = map[string]WearLoc{
	"head":    WearLocHead,
	"body":    WearLocBody,
	"hands":   WearLocHands,
	"feet":    WearLocFeet,
	"wield":   WearLocWield,
	"wielded": WearLocWield, // accept the display label too
	"hold":    WearLocHold,
	"held":    WearLocHold,
}

// wearableFromNames builds a *Wearable advertising exactly the named slots. An unknown name
// is ignored (content-lint would flag it); the demo uses only "head" and "wield".
func wearableFromNames(names []string) *Wearable {
	var locs []WearLoc
	for _, n := range names {
		if loc, ok := wearLocByName[n]; ok {
			locs = append(locs, loc)
		}
	}
	return wearableFor(locs...)
}

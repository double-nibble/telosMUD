// Package content is the content pipeline: it reads definition rows (from an embedded
// YAML pack or from Postgres) and produces a neutral LoadedContent the world package
// turns into prototypes. It is the new caller-of-record for prototype construction,
// replacing the hand-authored newDemoZone (docs/PHASE4-PLAN.md §3).
//
// # The DTO boundary (decision D5)
//
// The structs here are hand-written TRANSFER structs with encoding/json + yaml tags. They
// are the stable on-disk / on-wire shape of content. They are deliberately NOT the
// internal/world component structs: those have unexported, runtime-tuned fields (hot
// pointers, COW) and must not have their layout frozen to a persistence format. The world
// package owns the mapper that turns these DTOs into *Room/*Physical/*Wearable/*Weapon/
// *Container components (world/content_map.go); this package never imports world, so the
// dependency is one-directional (world -> content) and there is no cycle.
package content

// Pack is the top-level shape of a content pack file (one pack = one YAML document, or the
// rows of one `pack` column value). A pack ships one or more whole zones AND the pack-GLOBAL,
// zone-independent definition kinds (attributes/resources/damage-types — and, in 5.2/5.3,
// affects/abilities). The globals are NOT under any ZoneDTO: a `strength` attribute or a `fire`
// damage type is owned by the pack, not by Midgaard (docs/PHASE5-PLAN.md §2.2).
type Pack struct {
	Pack        string          `json:"pack" yaml:"pack"`
	Zones       []ZoneDTO       `json:"zones" yaml:"zones"`
	Attributes  []AttributeDTO  `json:"attributes" yaml:"attributes"`
	Resources   []ResourceDTO   `json:"resources" yaml:"resources"`
	DamageTypes []DamageTypeDTO `json:"damage_types" yaml:"damage_types"`
	Affects     []AffectDTO     `json:"affects" yaml:"affects"`
	Abilities   []AbilityDTO    `json:"abilities" yaml:"abilities"`
}

// AttributeDTO is one content-defined attribute (docs/ABILITIES.md §1, docs/PHASE5-PLAN.md §1.1).
// value_kind is 'int'|'float'|'derived'; a 'derived' attribute's base is a formula AST. default_base
// is the base: a literal {"lit": n} OR a formula {"expr": <ast>} where the AST is a nested prefix
// array — ["+", ["*", ["attr","con"], 10], ["*", ["attr","level"], 5]] = con*10 + level*5. Allowed
// heads: + - * / min max clamp, ["attr",name], ["lit",n]. min/max clamp the resolved value.
type AttributeDTO struct {
	Ref         string      `json:"ref" yaml:"ref"`
	DisplayName string      `json:"display_name" yaml:"display_name"`
	ValueKind   string      `json:"value_kind" yaml:"value_kind"`
	DefaultBase BaseSpecDTO `json:"default_base" yaml:"default_base"`
	Min         *float64    `json:"min" yaml:"min"`
	Max         *float64    `json:"max" yaml:"max"`
}

// BaseSpecDTO is an attribute's base: EXACTLY one of Lit (a literal value) or Expr (a formula AST).
// The AST is carried as the generic nested-array form (heads + - * / min max clamp / attr / lit).
// A zero BaseSpecDTO (neither set) resolves to 0 — a sane default for a contentless attribute.
type BaseSpecDTO struct {
	Lit  *float64       `json:"lit" yaml:"lit"`
	Expr FormulaNodeDTO `json:"expr" yaml:"expr"`
}

// FormulaNodeDTO is the prefix-AST expression form, decoded generically (it is a nested array or a
// scalar in YAML/JSON). The world-side mapper (content_map.go) parses it into a typed evaluable
// node; keeping it `any` here keeps the DTO free of the evaluator type. nil == no formula.
type FormulaNodeDTO = any

// ResourceDTO is one content-defined resource pool (docs/ABILITIES.md §1, §1.2). max_attr names the
// DERIVED attribute that caps the pool (so gear/affects that raise max_hp flow through derivation);
// the engine holds `current`. vital + on_depleted is how "hp at 0 = death" is content (5.2/combat).
type ResourceDTO struct {
	Ref               string `json:"ref" yaml:"ref"`
	DisplayName       string `json:"display_name" yaml:"display_name"`
	MaxAttr           string `json:"max_attr" yaml:"max_attr"`
	Vital             bool   `json:"vital" yaml:"vital"`
	Regen             int    `json:"regen" yaml:"regen"`                           // per-tick flat regen (reserved; ticks ride 5.2)
	DepletedThreshold int    `json:"depleted_threshold" yaml:"depleted_threshold"` // reserved (vital depletion, 5.2)
}

// DamageTypeDTO is one content-defined damage type with its resist/vuln/immune matrix (§1). The
// matrix maps an OTHER damage-type/category ref to a multiplier (1.0 = neutral, <1 resist, >1
// vuln, 0 = immune). The shared mitigation pipeline (5.3) reads it; slice 5.1 just loads it.
type DamageTypeDTO struct {
	Ref         string             `json:"ref" yaml:"ref"`
	DisplayName string             `json:"display_name" yaml:"display_name"`
	Color       string             `json:"color" yaml:"color"`
	Resist      map[string]float64 `json:"resist" yaml:"resist"`
}

// AffectDTO is one content-defined status effect (docs/ABILITIES.md §5, docs/PHASE5-PLAN.md §1.4).
// The Affected runtime (5.2) owns its duration/stacking/tick/expire and feeds its modifiers into
// attribute derivation (§1.1) and its prevents set into the tag-CC gate (§6).
//
//   - stacking is one of refresh|stack|extend|ignore (P5-D3). refresh (default) resets duration;
//     stack counts up to max_stacks (magnitude scales); extend sums durations; ignore = first wins.
//   - stack_scope keys an existing instance: "source" (default — one per (ref,source)) or "target"
//     (one per ref regardless of who applied it).
//   - body carries duration (pulses), the modifier list, the prevents tags, the tick spec, and the
//     RESERVED on_apply/on_expire hooks + resist (the op-list hooks land in 5.3).
type AffectDTO struct {
	Ref         string        `json:"ref" yaml:"ref"`
	Name        string        `json:"name" yaml:"name"`
	Category    string        `json:"category" yaml:"category"`
	Stacking    string        `json:"stacking" yaml:"stacking"`
	MaxStacks   int           `json:"max_stacks" yaml:"max_stacks"`
	StackScope  string        `json:"stack_scope" yaml:"stack_scope"`
	Dispellable bool          `json:"dispellable" yaml:"dispellable"`
	Body        AffectBodyDTO `json:"body" yaml:"body"`
}

// AffectBodyDTO is the JSONB-tail of an affect_defs row: everything that is not a first-class column.
// Duration is in PULSES (already heartbeat-denominated, so durations are conserved across save/load).
// Modifiers feed derivation; Prevents feeds the tag-CC set; Tick carries the interval + the RESERVED
// on_tick op-list (a DoT's deal_damage lands in 5.3). OnApply/OnExpire/Resist are reserved shape.
type AffectBodyDTO struct {
	Duration  int                 `json:"duration" yaml:"duration"`
	Modifiers []AffectModifierDTO `json:"modifiers" yaml:"modifiers"`
	Prevents  []string            `json:"prevents" yaml:"prevents"`
	Tick      *AffectTickDTO      `json:"tick" yaml:"tick"`
	OnApply   any                 `json:"on_apply" yaml:"on_apply"`   // RESERVED op-list (5.3)
	OnExpire  any                 `json:"on_expire" yaml:"on_expire"` // RESERVED op-list (5.3)
	Resist    map[string]any      `json:"resist" yaml:"resist"`       // RESERVED resist spec (5.3)
}

// AffectModifierDTO is one entry of an affect's modifier list: it adds (op:add) or multiplies
// (op:mul) attribute `attr` by `value` while the affect is active. The Affected runtime sums these
// across active affects into the entity's single mod source (§1.1).
type AffectModifierDTO struct {
	Attr  string  `json:"attr" yaml:"attr"`
	Op    string  `json:"op" yaml:"op"` // "add" | "mul"
	Value float64 `json:"value" yaml:"value"`
}

// AffectTickDTO is an affect's periodic-hook spec: every Interval pulses the runtime fires OnTick.
// OnTick is a RESERVED op-list this slice (the gated effect-op interpreter is 5.3); the tick
// MECHANISM (interval counting + the hook point) is live now so a DoT only needs its op-list later.
type AffectTickDTO struct {
	Interval int `json:"interval" yaml:"interval"`
	OnTick   any `json:"on_tick" yaml:"on_tick"` // op-list (Phase 5.3 — a DoT's deal_damage)
}

// AbilityDTO is one content-defined ability (docs/ABILITIES.md §2, docs/PHASE5-PLAN.md §1.6): a
// skill/spell/mob-special/item-proc that COMPOSES the engine's effect-op vocabulary. The engine
// provides the lifecycle; this data is the whole skill. The engine has never heard of "fireball".
//
//   - invocation is 'command' (becomes a verb once granted), 'proc' (fires on an event), or
//     'passive' (always-on). Phase 5.3 wires 'command'; proc/passive RESERVE the hooks (events 6/7).
//   - targeting drives the resolver AND the PvP gate: mode/scope/range + disposition
//     (helpful/harmful/neutral). A harmful disposition vs a non-consenting player is gated (§7).
//   - tags are the §6 CC tags ("cast","verbal","fire"); an affect's prevents[] blocks them (step 3).
//   - requires/costs gate the cast (known-skill, attr thresholds, not_prevented tag; resource costs).
//   - cast_time/lag/cooldown are timing (pulses). cast_time 0 (fireball) skips straight to commit.
//   - on_resolve is the declarative op-list (this phase). on_resolve_lua is RESERVED (read-not-run,
//     Phase 7). messages carries the actor/room emit templates (step 9).
type AbilityDTO struct {
	Ref          string             `json:"ref" yaml:"ref"`
	Name         string             `json:"name" yaml:"name"`
	Invocation   string             `json:"invocation" yaml:"invocation"` // 'command' | 'proc' | 'passive'
	Words        []string           `json:"words" yaml:"words"`           // command verbs that invoke it (invocation=command)
	Targeting    TargetingDTO       `json:"targeting" yaml:"targeting"`
	Tags         []string           `json:"tags" yaml:"tags"`
	Requires     RequiresDTO        `json:"requires" yaml:"requires"`
	Costs        []ResourceCostDTO  `json:"costs" yaml:"costs"`
	CastTime     int                `json:"cast_time" yaml:"cast_time"`
	Lag          int                `json:"lag" yaml:"lag"`
	Cooldown     int                `json:"cooldown" yaml:"cooldown"`
	OnResolve    any                `json:"on_resolve" yaml:"on_resolve"`         // declarative op-list (Phase 5.3)
	OnResolveLua string             `json:"on_resolve_lua" yaml:"on_resolve_lua"` // RESERVED, read-not-run (Phase 7)
	Messages     AbilityMessagesDTO `json:"messages" yaml:"messages"`
}

// TargetingDTO is an ability's target spec (docs/ABILITIES.md §2). mode is self/ally/enemy/area/
// room/object/direction/none; scope (room/...) is reserved-coarse this phase; disposition
// (helpful/harmful/neutral) drives the PvP gate (§7) — only 'harmful' routes through pvp_allowed.
type TargetingDTO struct {
	Mode        string `json:"mode" yaml:"mode"`
	Scope       string `json:"scope" yaml:"scope"`
	Range       int    `json:"range" yaml:"range"`
	Disposition string `json:"disposition" yaml:"disposition"` // 'helpful' | 'harmful' | 'neutral'
}

// RequiresDTO is an ability's declarative gate set (docs/ABILITIES.md §2, step 3). NotPrevented is
// the tag-CC check (does any active affect prevent this tag?) on TOP of the ability's own tags;
// Attr is a per-attribute minimum threshold. Known-skill/wielding/zone-flag gates are reserved shape.
type RequiresDTO struct {
	NotPrevented []string           `json:"not_prevented" yaml:"not_prevented"`
	Attr         map[string]float64 `json:"attr" yaml:"attr"`
}

// ResourceCostDTO is one resource an ability spends (docs/ABILITIES.md §2). Reserved on cast,
// paid on commit, refunded on interrupt. The fireball milestone is {resource: mana, amount: 30}.
type ResourceCostDTO struct {
	Resource string `json:"resource" yaml:"resource"`
	Amount   int    `json:"amount" yaml:"amount"`
}

// AbilityMessagesDTO carries the step-9 emit templates (act perspective strings). Actor is the
// "You ..." line; Room is the "$n ..." bystander line. Either may be empty (no message).
type AbilityMessagesDTO struct {
	Actor string `json:"actor" yaml:"actor"`
	Room  string `json:"room" yaml:"room"`
}

// ZoneDTO is one zone definition plus everything authored inside it: its rooms, the item
// and mob prototypes it owns, and its reset script. start_room names the room a fresh login
// spawns in (Zone.startRoom).
type ZoneDTO struct {
	Ref       string     `json:"ref" yaml:"ref"`
	Name      string     `json:"name" yaml:"name"`
	StartRoom string     `json:"start_room" yaml:"start_room"`
	ResetSecs int        `json:"reset_secs" yaml:"reset_secs"`
	Rooms     []RoomDTO  `json:"rooms" yaml:"rooms"`
	Items     []ProtoDTO `json:"item_prototypes" yaml:"item_prototypes"`
	Mobs      []ProtoDTO `json:"mob_prototypes" yaml:"mob_prototypes"`
	Resets    []ResetDTO `json:"resets" yaml:"resets"`
}

// RoomDTO is one room definition. ref is the stable PK / exit target; name is the display
// name (decoupled from ref). exits maps a canonical direction to a destination room ref
// (which may be a cross-zone ref, e.g. midgaard:room:market -> darkwood:room:grove).
type RoomDTO struct {
	Ref    string            `json:"ref" yaml:"ref"`
	Name   string            `json:"name" yaml:"name"`
	Long   string            `json:"long" yaml:"long"`
	Sector string            `json:"sector" yaml:"sector"`
	Exits  map[string]string `json:"exits" yaml:"exits"`
	// Flags are open-set named room booleans (docs/ABILITIES.md §1): "safe" (no PvP harm lands here),
	// "arena" (PvP forced on), etc. The PvP gate (world/pvp.go) reads them; the engine never invents a
	// flag name. Empty for an unflagged room. Mapped onto Room.namedFlags (world/content_map.go).
	Flags []string `json:"flags" yaml:"flags"`
}

// ProtoDTO is one item or mob prototype: targeting keywords, the inline short and the
// ground/room long line, plus the optional component templates. A nil component pointer
// means "this prototype does not carry that component" — the mapper adds only the present
// ones, matching the old defineTorch/defineHelmet/... which built exactly the components
// each item needed.
type ProtoDTO struct {
	Ref      string   `json:"ref" yaml:"ref"`
	Keywords []string `json:"keywords" yaml:"keywords"`
	Short    string   `json:"short" yaml:"short"`
	Long     string   `json:"long" yaml:"long"`

	Physical  *PhysicalDTO  `json:"physical" yaml:"physical"`
	Wearable  *WearableDTO  `json:"wearable" yaml:"wearable"`
	Weapon    *WeaponDTO    `json:"weapon" yaml:"weapon"`
	Container *ContainerDTO `json:"container" yaml:"container"`
}

// PhysicalDTO mirrors the world.Physical component template (mass/size/material).
type PhysicalDTO struct {
	Weight   int    `json:"weight" yaml:"weight"`
	Size     int    `json:"size" yaml:"size"`
	Material string `json:"material" yaml:"material"`
}

// WearableDTO mirrors world.Wearable: the set of wear-location names this item may occupy
// ("head","body","hands","feet","wield","hold"). The mapper resolves the names to WearLoc
// slots and packs the bitmask, keeping the slot enum an internal detail.
type WearableDTO struct {
	Locations []string `json:"locations" yaml:"locations"`
}

// WeaponDTO mirrors world.Weapon (damage dice + type + class + attack verb).
type WeaponDTO struct {
	DiceNum    int    `json:"dice_num" yaml:"dice_num"`
	DiceSize   int    `json:"dice_size" yaml:"dice_size"`
	DamageType string `json:"damage_type" yaml:"damage_type"`
	Class      string `json:"class" yaml:"class"`
	AttackVerb string `json:"attack_verb" yaml:"attack_verb"`
}

// ContainerDTO mirrors world.Container (capacity / weight limit / closed / locked / key).
type ContainerDTO struct {
	Capacity    int    `json:"capacity" yaml:"capacity"`
	WeightLimit int    `json:"weight_limit" yaml:"weight_limit"`
	Closed      bool   `json:"closed" yaml:"closed"`
	Locked      bool   `json:"locked" yaml:"locked"`
	KeyRef      string `json:"key_ref" yaml:"key_ref"`
}

// ResetDTO is one reset-script op (the `body` JSONB of a zone_resets row). The reset
// interpreter (world/reset.go) runs the SAME ops at zone boot and on the repop timer.
//
// The op kind, proto ref, room ref, max, and optional into-container are all DATA — adding a
// new placement is a content write, never engine code (docs/PERSISTENCE.md §5).
//
//   - op: "spawn_item" or "spawn_mob" — spawn the prototype `proto`. Both spawn the same
//     flyweight; the kind is advisory (content-lint / future mob-only ops).
//   - proto: the prototype ref to spawn.
//   - room: the destination room ref the instances live in.
//   - count: the number to ensure at BOOT when max is unset (back-compat with the demo seed;
//     <=0 means 1). When max>0 it is ignored — max is the ceiling for both boot and repop.
//   - max: the top-up ceiling. On every reset (boot and repop) the interpreter counts the live
//     instances this op owns and spawns ONLY (max - live), never exceeding max, never leaking.
//     0 means "use count" (a fixed boot seed with no repop top-up beyond the boot placement).
//   - into: optional. A container prototype ref already present in `room`; the spawned items
//     go INTO that container's contents instead of onto the room floor (a chest of loot).
//   - persistent: this op's objects are world-persistent (housing, persistent rooms). They are
//     NOT ephemerally re-spawned on repop — they load once from object_instances (the durable
//     table, docs/PERSISTENCE.md §4) so a flagged object is never duplicated on each timer tick.
//     The demo flags none; the gate path exists so a future persistent op routes correctly.
type ResetDTO struct {
	Op         string `json:"op" yaml:"op"`
	Proto      string `json:"proto" yaml:"proto"`
	Room       string `json:"room" yaml:"room"`
	Count      int    `json:"count" yaml:"count"`
	Max        int    `json:"max,omitempty" yaml:"max,omitempty"`
	Into       string `json:"into,omitempty" yaml:"into,omitempty"`
	Persistent bool   `json:"persistent,omitempty" yaml:"persistent,omitempty"`
}

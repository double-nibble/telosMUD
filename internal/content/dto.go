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

	// CombatProfiles are the pack-GLOBAL combat profiles (docs/COMBAT.md §3, Phase 6.3a): a named
	// bundle of the to-hit check, the ordered avoidance ladder, and the damage bonus formula an entity
	// uses when it fights. They are pack globals (zone-independent, like attributes) referenced by a
	// mob prototype's `combat_profile` or by the pack's DefaultCombat (the player default). The whole
	// to-hit/avoidance/damage SHAPE is content here — the engine only runs the pipeline (P6-D6).
	CombatProfiles []CombatProfileDTO `json:"combat_profiles" yaml:"combat_profiles"`
	// DefaultCombat names the combat profile a PLAYER entity uses when its own prototype declares none
	// — the pack's "this is how an unarmed/unspecced character fights" default. Empty => players have
	// no combat profile (a `kill` then auto-hits with weapon-only damage — the degenerate bare case).
	DefaultCombat string `json:"default_combat" yaml:"default_combat"`

	// Commands are pack-GLOBAL custom verbs implemented in Lua (Phase 7.4e). Each registers a new
	// verb into the command table — consulted AFTER the built-in baseTable AND content abilities, by
	// EXACT match only, so a custom verb can never shadow or abbreviate a core/movement/ability verb.
	Commands []CommandDTO `json:"commands" yaml:"commands"`

	// Channels are pack-GLOBAL comms channels (Phase 8.3, docs/PHASE8-PLAN.md P8-D3): named
	// broadcast channels (gossip/newbie/auction/OOC) with a verb, a color/format template, an access
	// predicate, and a default-on flag. Channels are CONTENT, not engine: the engine knows the KIND
	// (channel_defs) and the comms transport; `gossip` exists only because a pack defines it. An empty
	// pack ships none and there are NO channel verbs (the empty-boot invariant). Same last-write-wins
	// override-by-ref rule as the other pack globals.
	Channels []ChannelDTO `json:"channels" yaml:"channels"`

	// Regions are pack-GLOBAL region definitions (Phase 10.3, docs/WORLD-EVENTS.md §1): a content-
	// defined grouping of member zones (an "area/city" a builder thinks of as one place) that a region
	// director owns the supra-zone state of. A region is pure CONTENT — the engine knows the KIND
	// (region_defs: a ref + its member zone refs), not which regions exist; a pack names them. Same
	// last-write-wins override-by-ref rule as the other pack globals. Empty => no regions (only the
	// world scope exists), the empty-boot invariant.
	Regions []RegionDTO `json:"regions" yaml:"regions"`

	// Tracks are pack-GLOBAL advancement tracks (Phase 11.2, gap [G6a]): a content-defined progression
	// track (XP/level, a use-based skill, a guild rank) — a progress attribute, the thresholds that mark
	// each step, and the grant op-list run when a step is reached. Pure CONTENT: the engine knows the KIND
	// (track_defs + the threshold machinery), not which tracks exist; `level` is just an attribute some
	// tracks raise. Same last-write-wins override-by-ref rule as the other pack globals.
	Tracks []TrackDTO `json:"tracks" yaml:"tracks"`

	// Bundles are pack-GLOBAL content bundles (Phase 11.4b, gap [G6c]): class/race/background/feat/talent
	// templates — a kind discriminator + a grant op-list applied when the bundle is chosen (chargen) or a
	// track step grants it. Pure CONTENT: the engine knows the KIND "bundle" (apply its grants), never
	// "fighter". Same last-write-wins override-by-ref rule as the other pack globals.
	Bundles []BundleDTO `json:"bundles" yaml:"bundles"`

	// RarityTiers are pack-GLOBAL rarity tiers (Phase 12.1, gap loot): ordered named tiers
	// (common→…→legendary) the loot resolver uses for weighting + quality_floor filtering. Pure CONTENT;
	// same last-write-wins override-by-ref rule. Empty => no tiers (a loot pool then has no floor filter).
	RarityTiers []RarityTierDTO `json:"rarity_tiers" yaml:"rarity_tiers"`
	// LootTables are pack-GLOBAL loot tables (Phase 12.1): a list of independent rolls a mob drops from on
	// death. A mob prototype references one by ref (LivingDTO.LootTable). Same last-write-wins rule.
	LootTables []LootTableDTO `json:"loot_tables" yaml:"loot_tables"`
	// Affixes are pack-GLOBAL named affixes (#37): reusable attribute+range rolls a loot entry's quality pool
	// references by ref (AffixRollDTO.Ref), so a shared affix is authored once. Pure CONTENT; same
	// last-write-wins override-by-ref rule. Empty => pools inline their affixes (the pre-#37 form).
	Affixes []AffixDefDTO `json:"affix_defs" yaml:"affix_defs"`

	// SpawnSchedules are pack-GLOBAL scheduled spawns (Phase 12.4): long-timer boss spawns the DIRECTOR
	// owns (a weekly world boss), distinct from per-zone resets. Pure CONTENT; same last-write-wins rule.
	SpawnSchedules []SpawnScheduleDTO `json:"spawn_schedules" yaml:"spawn_schedules"`
	// Recipes are pack-GLOBAL crafting recipes (Phase 13.5): a profession+skill (+ optional station room
	// flag) gating the consume-inputs/produce-output a `craft` ability runs. Pure CONTENT; same rule.
	Recipes []RecipeDTO `json:"recipes" yaml:"recipes"`

	// WearSlots is the pack-GLOBAL content-defined equipment vocabulary (#35): the ordered set of wear
	// locations an item may occupy (head/body/…/a new "waist"), replacing the engine-fixed slot enum. Pure
	// CONTENT; same last-write-wins override-by-ref rule. Empty => the engine's DEFAULT slot set (the classic
	// Diku core), so the bare engine and any pack that declares none behave exactly as before.
	WearSlots []WearSlotDTO `json:"wear_slots" yaml:"wear_slots"`

	// Chargens are pack-GLOBAL character-generation flows (Phase 14.8): an ordered list of chargen STEPS the
	// website walks (pick a race/class bundle, allocate attributes by point-buy, …). Pure CONTENT — content
	// drives HOW generation works, the engine knows only the step KINDS. One flow per pack by convention;
	// same last-write-wins override-by-ref rule.
	Chargens []ChargenDTO `json:"chargens" yaml:"chargens"`

	// TrustTiers is the pack-GLOBAL content-defined trust ladder (#27/#29, Round 9 Slice 0): the ordered
	// set of account trust tiers (player/moderator/builder/architect/admin/…) with their ordinal ranks and
	// granted capability flags. BOTH telos-account (tier validation + promote authz) and the world (rank +
	// flag derivation, command gating) load it, so tiers and the permission model are a single authority.
	// Empty => the engine's DEFAULT ladder (player/builder/admin) — the round-8 behavior. Accumulated
	// last-write-wins by tier name.
	TrustTiers []TrustTierDTO `json:"trust_tiers" yaml:"trust_tiers"`

	// PvpLua is the OPTIONAL pack PvP-policy hook (Phase 7.4f): a Lua function body
	// `function(actor, target) … return true/false end` consulted by the harm gate. Empty => the
	// engine's built-in pvp_allowed policy. A missing/erroring policy FAILS CLOSED (denies harm).
	PvpLua string `json:"pvp_lua" yaml:"pvp_lua"`

	// Formulas are the OPTIONAL Lua ruleset-formula overrides (Phase 7.4f): a map of formula name
	// (to_hit/soak/regen/xp_for) to a Lua body that returns a number, an alternative to the prefix-AST
	// data formula. A ref uses the data formula OR the Lua one, never both.
	Formulas map[string]string `json:"formulas" yaml:"formulas"`

	// DisplayDefs are pack-GLOBAL display templates (docs/REMAINING.md display-templating): a Lua render body
	// per SURFACE (score/who/inventory/room/…) that returns the rendered sheet string, typically built with the
	// sandbox `ui` toolkit. Content-authored so a 5e vs WoW pack shows its own sheet layout, labels, and stat
	// order without an engine change (the mechanism/flavor pillar). Same last-write-wins override-by-surface.
	DisplayDefs []DisplayDefDTO `json:"display_defs" yaml:"display_defs"`
}

// DisplayDefDTO is one content-defined display template: a surface name and the Lua render body. The body runs
// in the sandbox with `self` bound to the VIEWING entity's handle and returns a string (typically assembled with
// `ui.sheet()`). Surfaces the engine consumes today: "score" (more — who/inventory/room — as they are wired).
type DisplayDefDTO struct {
	Surface string `json:"surface" yaml:"surface"` // e.g. "score", "who", "inventory"
	Render  string `json:"render" yaml:"render"`   // Lua body returning the sheet string
}

// RegionDTO is one content-defined region (Phase 10.3, docs/WORLD-EVENTS.md §1): a named grouping of
// member zones whose supra-zone state a region director owns. A region may span multiple zones/shards —
// a "city" a builder treats as one place is often several zones (a hot zone can be split). Region ≠
// shard. It is pure DATA: Ref is the stable region id (and the scoped-event subject token,
// telos.scope.region.<ref> — validated before a subject is built); Name is the display name; Zones are
// the member zone refs (a zone's membership in at most one region drives which region state it
// replicates). The director/zone wiring (read replica + signal-up) is 10.3b/c.
type RegionDTO struct {
	Ref   string   `json:"ref" yaml:"ref"`
	Name  string   `json:"name" yaml:"name"`
	Zones []string `json:"zones" yaml:"zones"`
}

// TrackDTO is one content-defined advancement track (Phase 11.2, gap [G6a], docs/PHASE11-PLAN.md §11.2).
// A track is the union abstraction for ALL advancement modes — XP-threshold auto-level, train-at-trainer,
// point-buy, and use-based — differing only in WHICH event feeds the progress attribute. It is pure DATA:
//
//   - Ref is the stable track id (a `grant_track`/`advance_track` op names it; an entity's per-track
//     current step is keyed by it in state).
//   - ProgressAttr is the attribute whose value advances the track (`xp`, `mining_skill`, `warrior_xp`) —
//     just an ordinary attribute; `advance_track` raises it and re-evaluates the thresholds.
//   - LevelAttr, if set, is the attribute a step is expected to raise (the track's "level"); it marks the
//     track as a LEVEL track so the machinery can fire OnLevel (vs OnTrackStep) — empty for a use-based,
//     level-less track. The engine grows NO level concept; this just names which attr (if any) is the level.
//   - Thresholds is the ascending progress values that mark each step: crossing Thresholds[i] reaches step
//     i+1 (so len(Thresholds) is the max step). Steps[i] is the grant op-list run when step i+1 is reached
//     (same op-list shape as an ability's on_resolve — modify_attribute_base / set_flag / grant_* / …).
type TrackDTO struct {
	Ref          string    `json:"ref" yaml:"ref"`
	ProgressAttr string    `json:"progress_attr" yaml:"progress_attr"`
	LevelAttr    string    `json:"level_attr,omitempty" yaml:"level_attr,omitempty"`
	Thresholds   []float64 `json:"thresholds" yaml:"thresholds"`
	Steps        []any     `json:"steps,omitempty" yaml:"steps,omitempty"` // grant op-list per step (index i => step i+1)
}

// BundleDTO is one content-defined template/bundle (Phase 11.4b, gap [G6c], docs/PHASE11-PLAN.md §11.4):
// a class/race/background/feat/talent — a set of grants applied as a unit when chosen or reached. It is
// pure DATA: Ref is the stable bundle id (an `apply_bundle` op / chargen names it); Kind is the template
// kind ("class"/"race"/"background"/"feat"/"talent") — a discriminator for queries/chargen, the engine
// just runs the grants; Grants is the grant op-list (modify_attribute_base / grant_ability / grant_track /
// set_flag / …) applied to the entity. Entry prerequisites (a prestige class's stat floor) are expressed
// by gating the apply behind a `check` in the calling content, not on the bundle itself.
type BundleDTO struct {
	Ref  string `json:"ref" yaml:"ref"`
	Kind string `json:"kind" yaml:"kind"`
	// Uncapped marks a `kind: profession` bundle whose profession does NOT count against the learned-profession
	// cap (a gathering/utility trade, unlimited — vs a capped crafting trade). Ignored for non-profession
	// bundles. The cap VALUE is the content attribute `max_professions` (defaults when unset). docs/REMAINING.md §4.
	Uncapped bool `json:"uncapped,omitempty" yaml:"uncapped,omitempty"`
	Grants   any  `json:"grants,omitempty" yaml:"grants,omitempty"` // a grant op-list (same shape as on_resolve)
}

// ChargenDTO is one content-defined character-generation flow (Phase 14.8, docs/ACCOUNT.md §14.8). It is pure
// DATA: Ref is the stable flow id (one flow per pack by convention, e.g. "demo:chargen"); Steps is the ordered
// list the website walks. Content drives HOW generation works — the website knows the step KINDS, never a
// ruleset — so the same engine renders 5e point-buy, a standard-array game, or a roll-and-assign MUD from data.
type ChargenDTO struct {
	Ref   string           `json:"ref" yaml:"ref"`
	Steps []ChargenStepDTO `json:"steps" yaml:"steps"`
}

// ChargenStepDTO is one chargen step — a tagged union over Kind. Common fields (Kind/ID/Prompt) plus the
// fields the kind reads:
//
//   - kind "bundle_choice": pick exactly Pick (default 1) bundles whose BundleKind matches (race/class/…).
//     The website lists the pack's bundles of that kind; the result is the chosen bundle ref(s).
//   - kind "point_buy": allocate Points across Attributes; each attribute starts at Base, costs Cost[target]
//     (cumulative points to reach the target value from Base) and is bounded to [Min, Max]. The result is the
//     chosen value per attribute, applied as the attribute BASE on first spawn (racial bundle mods add on top).
//
// Future kinds (array_assign, roll) add fields here + a handler; existing flows are untouched.
type ChargenStepDTO struct {
	Kind   string `json:"kind" yaml:"kind"`
	ID     string `json:"id" yaml:"id"`
	Prompt string `json:"prompt,omitempty" yaml:"prompt,omitempty"`

	// bundle_choice
	BundleKind string `json:"bundle_kind,omitempty" yaml:"bundle_kind,omitempty"`
	Pick       int    `json:"pick,omitempty" yaml:"pick,omitempty"`

	// point_buy
	Attributes []string       `json:"attributes,omitempty" yaml:"attributes,omitempty"`
	Points     int            `json:"points,omitempty" yaml:"points,omitempty"`
	Base       int            `json:"base,omitempty" yaml:"base,omitempty"`
	Min        int            `json:"min,omitempty" yaml:"min,omitempty"`
	Max        int            `json:"max,omitempty" yaml:"max,omitempty"`
	Cost       map[string]int `json:"cost,omitempty" yaml:"cost,omitempty"` // target value (as string) -> cumulative cost from Base
}

// RarityTierDTO is one content-defined rarity tier (Phase 12.1, docs/LOOT-AND-SPAWNS.md §2): an ordered,
// named tier in the rarity ladder. Order is the ordinal (common=0 … legendary=N) the resolver compares
// for a roll's quality_floor; Weight is the tier's default pool weight (a loot entry may override it);
// Color is a markup token for rendering the item name. Pure DATA.
type RarityTierDTO struct {
	Ref    string  `json:"ref" yaml:"ref"`
	Order  int     `json:"order" yaml:"order"`
	Weight float64 `json:"weight" yaml:"weight"`
	Color  string  `json:"color,omitempty" yaml:"color,omitempty"`
	// Binds (Phase 13.4, D1) marks a tier whose items BIND on creation — the top-tier no-trade sink: a
	// salvage/loot component of a binds tier is bound when produced (the legendary essence that can't be
	// bought), while low/mid tiers stay tradeable to feed the market. false (the default) for every
	// tradeable tier; existing tiers are unchanged.
	Binds bool `json:"binds,omitempty" yaml:"binds,omitempty"`
}

// LootTableDTO is one content-defined loot table (Phase 12.1): a list of INDEPENDENT rolls a mob drops
// from on death. Each roll resolves on its own (not mutually exclusive), so a boss table can carry a
// guaranteed rare+ roll AND an independent legendary chance. Referenced by a mob prototype's loot_table.
type LootTableDTO struct {
	Ref   string        `json:"ref" yaml:"ref"`
	Rolls []LootRollDTO `json:"rolls" yaml:"rolls"`
	// OnRoll is an optional Lua escape hatch (Phase 12.1, docs/REMAINING.md §4): a body run once per
	// eligible looter AFTER the declarative rolls resolve, returning a list of item prototype refs to
	// additionally drop. It expresses CONDITIONAL drops the declarative form can't (branch on looter/victim
	// state). Each returned ref is delivered through the same loot pipeline (quality/binding/merge). "" =
	// no hatch (the fully-declarative path, unchanged).
	OnRoll string `json:"on_roll,omitempty" yaml:"on_roll,omitempty"`
}

// LootRollDTO is one roll within a loot table. Kind is "guaranteed" (always yields), "chance" (an
// independent probability), "weighted_one" (pick 1 from the weighted pool), or "weighted_n" (pick N).
// Chance is the probability for kind=chance; N the count for weighted_n; QualityFloor (a rarity tier ref)
// filters the pool to a minimum tier; Pool is the weighted item pool. Pity (Phase 12.2) is the optional
// bad-luck-protection spec on a chance roll — carried now, applied in 12.2.
type LootRollDTO struct {
	Kind         string         `json:"kind" yaml:"kind"`
	Chance       float64        `json:"chance,omitempty" yaml:"chance,omitempty"`
	N            int            `json:"n,omitempty" yaml:"n,omitempty"`
	QualityFloor string         `json:"quality_floor,omitempty" yaml:"quality_floor,omitempty"`
	Pool         []LootEntryDTO `json:"pool,omitempty" yaml:"pool,omitempty"`
	Pity         *LootPityDTO   `json:"pity,omitempty" yaml:"pity,omitempty"`
}

// LootEntryDTO is one weighted entry in a roll's pool: an item prototype ref, its rarity Tier (for the
// quality_floor filter + rendering), an optional Weight (0 => use the tier's default weight), and an
// optional Quality spec (Phase 12.3) — when present, a dropped instance rolls a level + affixes into its
// per-instance delta, so two drops of the same prototype vary while the prototype stays shared.
type LootEntryDTO struct {
	Item    string          `json:"item" yaml:"item"`
	Tier    string          `json:"tier,omitempty" yaml:"tier,omitempty"`
	Weight  float64         `json:"weight,omitempty" yaml:"weight,omitempty"`
	Quality *QualitySpecDTO `json:"quality,omitempty" yaml:"quality,omitempty"`
}

// QualitySpecDTO is a loot entry's item-quality roll (Phase 12.3, docs/LOOT-AND-SPAWNS.md §3): on drop,
// roll an item Level in [LevelMin, LevelMax] and Count affixes chosen from the Affixes pool, each rolled
// to a value in its [Min, Max] range — written into the dropped item's per-instance delta. Coarse v1 (the
// affix pool is inline here; a normalized affix_defs table is a follow-up). The within-tier "always good,
// but it varies" layer; optional per entry.
type QualitySpecDTO struct {
	Affixes  []AffixRollDTO `json:"affixes" yaml:"affixes"`
	Count    int            `json:"count" yaml:"count"`
	LevelMin int            `json:"level_min,omitempty" yaml:"level_min,omitempty"`
	LevelMax int            `json:"level_max,omitempty" yaml:"level_max,omitempty"`
}

// AffixRollDTO is one affix in a quality pool. TWO shapes (#37): INLINE — Attr + the [Min, Max] range its
// rolled value falls in — OR a Ref naming a shared affix_def (AffixDefDTO), from which Attr/Min/Max are
// resolved at build time. Ref takes precedence when set, so an edit to the affix_def propagates to every pool
// that references it on the next reload (the normalization win). A legendary's richer pool is just a longer
// Affixes list with a higher Count.
type AffixRollDTO struct {
	Ref  string  `json:"ref,omitempty" yaml:"ref,omitempty"` // #37: name a shared affix_def instead of inlining
	Attr string  `json:"attr,omitempty" yaml:"attr,omitempty"`
	Min  float64 `json:"min,omitempty" yaml:"min,omitempty"`
	Max  float64 `json:"max,omitempty" yaml:"max,omitempty"`
}

// AffixDefDTO is one content-defined NAMED affix (#37, docs/LOOT-AND-SPAWNS.md §3): a reusable
// attribute + [Min, Max] roll range referenced by ref from a loot entry's quality pool (AffixRollDTO.Ref),
// so a shared affix ("of the bear" = +str) is authored once and reused across many drops. A first-class def
// table (like recipe_defs / rarity_tier_defs): edit the def, and every pool that references it changes on
// reload — instead of the value being baked into each inline pool.
type AffixDefDTO struct {
	Ref  string  `json:"ref" yaml:"ref"`
	Attr string  `json:"attr" yaml:"attr"`
	Min  float64 `json:"min,omitempty" yaml:"min,omitempty"`
	Max  float64 `json:"max,omitempty" yaml:"max,omitempty"`
}

// LootPityDTO is a chance roll's bad-luck-protection spec (Phase 12.2): each miss nudges the effective
// chance up by Step (to Cap); a hit resets the per-character counter keyed by Key.
type LootPityDTO struct {
	Key  string  `json:"key" yaml:"key"`
	Step float64 `json:"step" yaml:"step"`
	Cap  float64 `json:"cap" yaml:"cap"`
}

// SpawnScheduleDTO is one content-defined scheduled spawn (Phase 12.4, docs/LOOT-AND-SPAWNS.md §1): a
// long-timer boss the DIRECTOR spawns on a schedule (a weekly world boss), restart-safe via persisted
// scope state. Ref is the schedule id; Proto the mob prototype to spawn; Zone the hosting zone (the
// director broadcasts the spawn command to it) and Room an optional room within it; IntervalAfterDeathSec
// is how long after the boss DIES it respawns (a weekly boss = 604800); OnMissed is the downtime policy
// ("spawn_if_overdue" spawns immediately if the window passed during downtime, "skip_to_next" waits for
// the next window); Announce is the spawn announcement broadcast to the zone.
type SpawnScheduleDTO struct {
	Ref                   string `json:"ref" yaml:"ref"`
	Proto                 string `json:"proto" yaml:"proto"`
	Zone                  string `json:"zone" yaml:"zone"`
	Room                  string `json:"room,omitempty" yaml:"room,omitempty"`
	IntervalAfterDeathSec int    `json:"interval_after_death_sec" yaml:"interval_after_death_sec"`
	OnMissed              string `json:"on_missed,omitempty" yaml:"on_missed,omitempty"`
	Announce              string `json:"announce,omitempty" yaml:"announce,omitempty"`
}

// RecipeDTO is one content-defined crafting recipe (Phase 13.5, docs/CRAFTING.md): the data the craft op
// (craft_recipe) runs. Profession + Track-or-Skill/MinSkill gate it (the actor must have learned the trade and
// reached the skill level — Track resolves the level attr from the track_def, Skill is the raw-attr fallback);
// Station is an optional REQUIRED ROOM FLAG (D3 — a `forge`; "" crafts anywhere); Inputs are
// the components consumed; Output is the produced item. QualityBase is the coarse output-quality band base —
// the crafted item's level is QualityBase + the actor's skill level (the rich affix roll stays §10-deferred).
type RecipeDTO struct {
	Ref string `json:"ref" yaml:"ref"`
	// Name is the recipe's display name for discovery listings ("what can I craft?", #34). "" => the ref
	// is shown. Aliases are the builder-declared SHORT names a player types after `craft` — isname/prefix
	// resolved (like item keywords) so `craft vest` finds `craft:leather_vest`. The ref's own leaf token
	// is always an implicit alias, so a recipe is craftable-by-ref even with no explicit aliases.
	Name       string   `json:"name,omitempty" yaml:"name,omitempty"`
	Aliases    []string `json:"aliases,omitempty" yaml:"aliases,omitempty"`
	Profession string   `json:"profession,omitempty" yaml:"profession,omitempty"`
	// Track, when set, names the skill TRACK whose level_attr gates + scales this recipe — the engine resolves
	// the attribute live from the track_def, so the recipe follows the track's level_attr instead of duplicating
	// it (docs/REMAINING.md §4). Skill is the raw-attribute fallback used when Track is unset (a level-less or
	// ad-hoc gate). Track takes precedence when both are set.
	Track       string           `json:"track,omitempty" yaml:"track,omitempty"`
	Skill       string           `json:"skill,omitempty" yaml:"skill,omitempty"`         // the skill LEVEL attribute (fallback when Track unset)
	MinSkill    int              `json:"min_skill,omitempty" yaml:"min_skill,omitempty"` // minimum skill level required
	Station     string           `json:"station,omitempty" yaml:"station,omitempty"`     // required room flag (D3); "" = craft anywhere
	Inputs      []RecipeInputDTO `json:"inputs" yaml:"inputs"`                           // components consumed
	Output      RecipeOutputDTO  `json:"output" yaml:"output"`                           // the produced item
	QualityBase int              `json:"quality_base,omitempty" yaml:"quality_base,omitempty"`
}

// RecipeInputDTO is one consumed component of a recipe: a prototype ref + a quantity (default 1).
type RecipeInputDTO struct {
	Item string `json:"item" yaml:"item"`
	Qty  int    `json:"qty,omitempty" yaml:"qty,omitempty"`
}

// RecipeOutputDTO is a recipe's produced item: a prototype ref + a quantity (default 1) + an optional bind
// override ("bound" => the crafted item is soulbound on creation).
type RecipeOutputDTO struct {
	Item string `json:"item" yaml:"item"`
	Qty  int    `json:"qty,omitempty" yaml:"qty,omitempty"`
	Bind string `json:"bind,omitempty" yaml:"bind,omitempty"`
}

// ChannelDTO is one content-defined comms channel (Phase 8.3, docs/PHASE8-PLAN.md P8-D3). A channel
// is pure CONTENT: the engine names no channel (no hardcoded `gossip`) and only knows the channel_def
// shape + the comms transport. Authoring a channel registers its verb(s) and gives the source world
// the format/color/access rules it applies before publishing a line to telos.comms.chan.<ref>.
//
//   - Ref is the stable channel id and the SUBJECT token (telos.comms.chan.<ref>); it is validated
//     against the loaded channel_defs before a subject is ever built (P8-A8, subject injection).
//   - Name is the display name; Words are the command verbs that emit on the channel (an empty pack =>
//     no such verb). Words are registered EXACT-only beside the custom-command table.
//   - Color is the channel's color token (markup applied to the rendered line); Format is the
//     listener-perspective template with $-substitution ("[$channel] $name: $t") — $t is the
//     player's text as DATA (sanitized; a `$`/`%`/ANSI in it is literal — P8-A7), $name the
//     ENGINE-SET author, $channel the channel name. The default (empty Format) is "[$channel] $name: $t".
//   - Access is the speak/hear predicate (who may use the channel) — a CONTENT predicate evaluated
//     ENGINE-SIDE against the speaking *Entity, never trusting the client (P8-A8).
//   - DefaultOn is whether a fresh character is subscribed by default (drives the gate subscription;
//     per-player toggles are slice 8.6). History is the recent-lines buffer size (carried; retrieval
//     is deferred — slice 8.3 only records the field).
type ChannelDTO struct {
	Ref    string           `json:"ref" yaml:"ref"`
	Name   string           `json:"name" yaml:"name"`
	Words  []string         `json:"words" yaml:"words"`
	Color  string           `json:"color" yaml:"color"`
	Format string           `json:"format" yaml:"format"`
	Access ChannelAccessDTO `json:"access" yaml:"access"`
	// HearAccess, when present, is the LISTEN predicate, split from the speak predicate above. nil
	// (the field absent in YAML) keeps the v1 rule — hear mirrors Access, a restricted channel
	// restricts both directions. Present-but-EMPTY (`hear_access: {}`) means anyone may hear — the
	// "announce" channel shape (only privileged speakers, everyone listens). A non-empty predicate
	// restricts hearing independently of speaking.
	HearAccess *ChannelAccessDTO `json:"hear_access,omitempty" yaml:"hear_access"`
	DefaultOn  bool              `json:"default_on" yaml:"default_on"`
	History    int               `json:"history" yaml:"history"`
}

// ChannelAccessDTO is a channel's access predicate (P8-A8): a small content gate the source world
// evaluates against the speaking *Entity before publishing. All conditions present must hold (AND). A
// zero ChannelAccessDTO (no conditions) means "anyone may speak" (the open channel). It is DATA only —
// a flag the entity must carry, an attribute floor — never code; the engine reads the live entity, the
// content names the rule. Hear-side access for cross-shard receivers is the gate's per-enabled-channel
// subscription (slice 8.6 wires per-player toggles); the speak-side predicate here is authoritative.
type ChannelAccessDTO struct {
	// RequireFlag, if non-empty, is a named entity flag the speaker MUST carry (e.g. an "immortal"
	// channel requires the "immortal" flag). Empty => no flag requirement.
	RequireFlag string `json:"require_flag" yaml:"require_flag"`
	// MinAttr, if non-nil, requires the speaker's named attribute to be >= the value (e.g. a level
	// floor on a channel). Both Attr and Min must be set to take effect.
	MinAttr *MinAttrDTO `json:"min_attr" yaml:"min_attr"`
}

// MinAttrDTO is one attribute-floor condition: the speaker's Attr (a content attribute ref) must
// resolve >= Min. Used by a channel access predicate (a level-gated channel).
type MinAttrDTO struct {
	Attr string  `json:"attr" yaml:"attr"`
	Min  float64 `json:"min" yaml:"min"`
}

// TrustTierDTO is one rung of the content-defined trust ladder (#27/#29, Round 9 Slice 0): a named tier,
// its ordinal RANK (higher = more trusted — gated inspection/moderation commands compare ranks, so an
// actor may act on any target whose rank is <= its own), and the reserved capability flags it grants on
// login. Both telos-account and the world load the same ladder. Flags may name ONLY the engine's reserved
// trust flags (holylight/builder/admin) — the ladder is trusted derivation, not a way to invent
// capabilities; the world ignores any non-reserved flag here. A tier with no flags is a pure rank rung
// (e.g. a "moderator" that can inspect but carries no engine flag).
type TrustTierDTO struct {
	Name  string   `json:"name" yaml:"name"`
	Rank  int      `json:"rank" yaml:"rank"`
	Flags []string `json:"flags,omitempty" yaml:"flags,omitempty"`
}

// CommandDTO is one custom Lua verb (Phase 7.4e). Verb is the word the player types; Lua is the
// body (`self` = the actor, `arg` = the verb's argument tail). Aliases are optional exact
// spellings. The verb is registered EXACT-only (no abbreviation) and never shadows a core verb.
type CommandDTO struct {
	Verb    string   `json:"verb" yaml:"verb"`
	Aliases []string `json:"aliases" yaml:"aliases"`
	Lua     string   `json:"lua" yaml:"lua"`
}

// CombatProfileDTO is one named combat profile (Phase 6.3a). to_hit is the attacker's to-hit CHECK
// (a CheckDTO body — dice/bonus/vs/bands, same shape an ability's `check` op uses). avoidance is the
// DEFENDER's ordered avoidance ladder: zero-or-more named checks run in order, the first success
// negates the swing ([G-F] — an empty ladder is the 5e/WoW "straight to soak" case). damage_bonus is
// the [G-A] scoped damage formula (`$actor.str_bonus + $actor.damroll`) added to weapon dice. EVERY
// numeric here is content — the engine names none of it.
type CombatProfileDTO struct {
	Ref         string         `json:"ref" yaml:"ref"`
	ToHit       any            `json:"to_hit" yaml:"to_hit"`             // a check body (parseCheckSpec)
	Avoidance   []any          `json:"avoidance" yaml:"avoidance"`       // ordered avoidance check bodies
	DamageBonus FormulaNodeDTO `json:"damage_bonus" yaml:"damage_bonus"` // [G-A] scoped damage bonus formula
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
	// Stat marks this attribute as a PLAYER-FACING character stat (Phase 9.2): it appears in the GMCP
	// Char.Stats HUD payload. Defaults false so derived/internal attributes (max_hp, accuracy, soak_*)
	// stay out of the stat panel — only the content-chosen stats (strength, level, …) are surfaced.
	Stat bool `json:"stat" yaml:"stat"`
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
	// RegenInCombat lets a resource keep regenerating while its owner is FIGHTING. Default false: the
	// engine PAUSES passive regen for an entity in combat (the classic Diku "no rest mid-fight" rule), so
	// a mob's hp regen does not claw back a fresh player's swings round after round. A pack that wants
	// regen-during-combat (a troll's regeneration, a mana pool that ticks in a fight) sets this true. The
	// engine names no resource — this is the per-resource flag content authors choose; the mechanism
	// (pause-while-fighting) is the engine default.
	RegenInCombat bool `json:"regen_in_combat" yaml:"regen_in_combat"`
	// PerRound marks a per-round REACTION budget ([G9], Phase 6.4b): the resource is topped up to its max
	// at the start of every combat round, so a reactor (e.g. a mob with an OnLeaveRoom opportunity attack)
	// gets a bounded number of reactions/round and a spent reaction does not refill until the next round.
	PerRound bool `json:"per_round" yaml:"per_round"`
	// Gauge marks this pool as PLAYER-FACING for the HUD (GMCP Char.Vitals + the live-vitals prompt), #50 —
	// the resource mirror of AttributeDTO.Stat. When ANY resource in the pack sets it, only gauged pools
	// appear (an internal pool like `reactions` stays out); when NONE do, all pools show (backward-compat).
	Gauge bool `json:"gauge" yaml:"gauge"`
	// OnEvent subscribes op-lists to in-zone engine events ([G3]) for an entity that HAS this resource
	// (a rage pool that builds on OnHit). Map of event-name -> op-list. Phase 6.2.
	OnEvent map[string]any `json:"on_event" yaml:"on_event"`
	// OnEventLua is the Lua-BODY alternative to OnEvent (Phase 7.4g): event-name -> Lua handler body.
	OnEventLua map[string]string `json:"on_event_lua" yaml:"on_event_lua"`
	// OnReactionLua is the RESULT-ALTERING reaction surface (Phase 7.9): a Lua handler keyed by a
	// reaction checkpoint (BeforeCastCommit/ToHit/OnDamageTaken) that receives a typed `rx` (cancel/
	// modify/replace_target/consume_resource). Distinct from OnEventLua so a dual-fired checkpoint never
	// runs one handler twice.
	OnReactionLua map[string]string `json:"on_reaction_lua" yaml:"on_reaction_lua"`
	// OnDepleted is the op-list the engine runs when a VITAL resource hits 0 — the [G-D] death hook
	// (Phase 6.3b). It runs ON the dying entity (the victim is $actor) BEFORE the engine's die() drops
	// combat and builds the corpse, so content can narrate or fire a last-gasp effect. An empty/absent
	// list means "default death" (the engine still runs die()); the op-list is additive flavor, not a
	// replacement for the engine death machinery. Only meaningful on a vital resource.
	OnDepleted []any `json:"on_depleted" yaml:"on_depleted"`
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
	Ref         string `json:"ref" yaml:"ref"`
	Name        string `json:"name" yaml:"name"`
	Category    string `json:"category" yaml:"category"`
	Stacking    string `json:"stacking" yaml:"stacking"`
	MaxStacks   int    `json:"max_stacks" yaml:"max_stacks"`
	StackScope  string `json:"stack_scope" yaml:"stack_scope"`
	Dispellable bool   `json:"dispellable" yaml:"dispellable"`
	// Scope is the affect's attachment target ([G13], docs/PHASE6-PLAN.md §1.3): "entity" (default —
	// attaches to a living entity, the Phase-5 shape) or "room" (attaches to the ROOM entity, ticks over
	// the room's occupants, and lands on entrants). A room-scoped affect (web/darkness/silence-field/
	// consecrate) is how an area condition persists in a place rather than on a creature. Transient: a
	// room affect is re-applied by content/reset, not snapshotted (matches combat's transient model).
	Scope string        `json:"scope" yaml:"scope"`
	Body  AffectBodyDTO `json:"body" yaml:"body"`
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
	// OnApplyLua/OnExpireLua/OnDispelLua are the OPTIONAL Lua affect hooks (Phase 7.4d): a Lua body
	// run when the affect attaches / expires / is dispelled, with `self` the affected entity and
	// the affect's source as the harm actor. Empty for a pure-data affect.
	OnApplyLua  string `json:"on_apply_lua" yaml:"on_apply_lua"`
	OnExpireLua string `json:"on_expire_lua" yaml:"on_expire_lua"`
	OnDispelLua string `json:"on_dispel_lua" yaml:"on_dispel_lua"`
	// OnEvent subscribes op-lists to in-zone engine events ([G3]) while this affect is active (a proc
	// buff). Map of event-name -> op-list. Phase 6.2.
	OnEvent map[string]any `json:"on_event" yaml:"on_event"`
	// OnEventLua is the Lua-BODY alternative to OnEvent (Phase 7.4g): event-name -> Lua handler body.
	OnEventLua map[string]string `json:"on_event_lua" yaml:"on_event_lua"`
	// OnReactionLua is the RESULT-ALTERING reaction surface (Phase 7.9): a Lua reaction handler keyed
	// by a reaction checkpoint, receiving a typed `rx` (a concentration affect carries
	// on_reaction_lua[OnDamageTaken] that rx:cancel()s itself on a failed save). Distinct from
	// OnEventLua so a dual-fired checkpoint never double-fires one handler.
	OnReactionLua map[string]string `json:"on_reaction_lua" yaml:"on_reaction_lua"`
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
	Ref           string             `json:"ref" yaml:"ref"`
	Name          string             `json:"name" yaml:"name"`
	Invocation    string             `json:"invocation" yaml:"invocation"` // 'command' | 'proc' | 'passive'
	Words         []string           `json:"words" yaml:"words"`           // command verbs that invoke it (invocation=command)
	Targeting     TargetingDTO       `json:"targeting" yaml:"targeting"`
	Tags          []string           `json:"tags" yaml:"tags"`
	Skill         string             `json:"skill" yaml:"skill"`                   // Phase 11.3: the skill/track this ability trains — using it fires OnSkillUse
	RequiresGrant bool               `json:"requires_grant" yaml:"requires_grant"` // Phase 11.4a: ownership-gated — usable only after grant_ability
	Requires      RequiresDTO        `json:"requires" yaml:"requires"`
	Costs         []ResourceCostDTO  `json:"costs" yaml:"costs"`
	CastTime      int                `json:"cast_time" yaml:"cast_time"`
	Lag           int                `json:"lag" yaml:"lag"`
	Cooldown      int                `json:"cooldown" yaml:"cooldown"`
	OnResolve     any                `json:"on_resolve" yaml:"on_resolve"`         // declarative op-list (Phase 5.3)
	OnResolveLua  string             `json:"on_resolve_lua" yaml:"on_resolve_lua"` // RESERVED, read-not-run (Phase 7)
	Messages      AbilityMessagesDTO `json:"messages" yaml:"messages"`
	// OnEvent subscribes op-lists to in-zone engine events ([G3]) for a known/granted ability. Map of
	// event-name -> op-list. Phase 6.2 (per-entity ability subscriptions await the Skilled component).
	OnEvent map[string]any `json:"on_event" yaml:"on_event"`
}

// TargetingDTO is an ability's target spec (docs/ABILITIES.md §2). mode is self/ally/enemy/area/
// room/object/direction/none; scope (room/...) is reserved-coarse this phase; disposition
// (helpful/harmful/neutral) drives the PvP gate (§7) — only 'harmful' routes through pvp_allowed.
type TargetingDTO struct {
	Mode        string `json:"mode" yaml:"mode"`
	Scope       string `json:"scope" yaml:"scope"`
	Range       int    `json:"range" yaml:"range"`
	Disposition string `json:"disposition" yaml:"disposition"` // 'helpful' | 'harmful' | 'neutral'
	// Area is the AoE shape ([G12], docs/PHASE6-PLAN.md §1.3): "" / "self" / "target" = single-target
	// (the degenerate case, unchanged); "room" = every valid living target in the actor's room;
	// "room_and_adjacent" = that room plus rooms one exit away (SAME-ZONE only — a cross-zone exit is
	// excluded). It is stamped onto each top-level on_resolve op so the effect-op interpreter LOOPS the
	// op per target, gating each independently. A per-op `area` field overrides it for that one op.
	Area string `json:"area" yaml:"area"`
}

// RequiresDTO is an ability's declarative gate set (docs/ABILITIES.md §2, step 3). NotPrevented is
// the tag-CC check (does any active affect prevent this tag?) on TOP of the ability's own tags;
// Attr is a per-attribute minimum threshold. Known-skill/wielding/zone-flag gates are reserved shape.
type RequiresDTO struct {
	NotPrevented []string           `json:"not_prevented" yaml:"not_prevented"`
	Attr         map[string]float64 `json:"attr" yaml:"attr"`
	// Profession (Phase 13.3) gates a crafting ability on trade MEMBERSHIP: the actor must have LEARNED
	// this profession (state.professions, profession.go) to use the verb. "" => no profession gate (the
	// established opt-in default; every existing ability is unchanged).
	Profession string `json:"profession" yaml:"profession"`
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
	Ref    string `json:"ref" yaml:"ref"`
	Name   string `json:"name" yaml:"name"`
	Long   string `json:"long" yaml:"long"`
	Sector string `json:"sector" yaml:"sector"`
	// Coord is the room's [x,y,z] position within its zone for the GMCP Room.Info minimap (Phase 9.3b).
	// Optional — nil when unauthored (a client falls back to topological layout from exits). It rides
	// the dedicated rooms.coord JSONB column.
	Coord []int             `json:"coord" yaml:"coord"`
	Exits map[string]string `json:"exits" yaml:"exits"`
	// Flags are open-set named room booleans (docs/ABILITIES.md §1): "safe" (no PvP harm lands here),
	// "arena" (PvP forced on), etc. The PvP gate (world/pvp.go) reads them; the engine never invents a
	// flag name. Empty for an unflagged room. Mapped onto Room.namedFlags (world/content_map.go).
	Flags []string `json:"flags" yaml:"flags"`
	// Lua is the OPTIONAL room trigger block (Phase 7.4c): registers `on(event, fn)` triggers on the
	// room entity (enter/leave/speech/…) and seeds `self.state`. Empty for a pure-data room.
	Lua string `json:"lua" yaml:"lua"`
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

	// Bind is the item's binding rule (Phase 13.1, docs/CRAFTING.md §1): "bind_on_pickup" (binds when
	// looted — BoP), "bind_on_equip" (binds when worn — BoE), or "" / "unbound" (freely tradeable). A
	// bound item cannot be given/dropped-for-others/traded, but can still be equipped, destroyed, and
	// deconstructed by its owner. Binding is a TRADE restriction the engine enforces uniformly.
	Bind string `json:"bind,omitempty" yaml:"bind,omitempty"`
	// Tier is the item's rarity tier ref (a rarity_tier_def, Phase 12.1), used for the tier-dependent
	// component-binding threshold (D1) + recipe/salvage gating. "" => untiered (common).
	Tier string `json:"tier,omitempty" yaml:"tier,omitempty"`
	// Tags are open-set item tags (Phase 13.1): "material", "magical", a profession tag — content rules
	// (recipe inputs, salvage requirements) match on them. The engine names no tag.
	Tags []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	// Material, when present, makes this item a STACKABLE material (Phase 13.2): identical stacks merge on
	// pickup, bounded by MaxStack. nil => a normal (non-stacking) item.
	Material *MaterialDTO `json:"material,omitempty" yaml:"material,omitempty"`

	// Living, when present, makes this prototype a LIVING entity (a mob): it carries the per-entity
	// attribute BASE overrides (the mob's str/con/accuracy/evasion/...) and the combat profile ref the
	// swing pipeline uses (Phase 6.3a). A nil Living means an inert item (no stats, no combat) — every
	// existing demo item is unchanged.
	Living *LivingDTO `json:"living" yaml:"living"`

	// Lua is the OPTIONAL trigger block (Phase 7.4c): a Lua source string that runs ONCE per
	// spawned instance to register `on(event, fn)` triggers (enter/leave/speech/greet/death/…) and
	// to seed `self.state`. A nil/empty Lua means a pure-data prototype (no scripts) — the
	// bare-engine invariant. Carried opaque through the prototype to the spawned entity, compiled
	// per-zone on first spawn (world/luaentry.go).
	Lua string `json:"lua" yaml:"lua"`
}

// LivingDTO is the mob-statting block on a prototype (Phase 6.3a). Attributes is the per-entity
// attribute base overrides (the same shape a character's saved bases take) — a goblin's strength/
// constitution/accuracy/evasion/attacks/soak_slash/... live here, so the mob's combat numbers are
// pure content (P6-D6). CombatProfile names the pack-global CombatProfileDTO this mob fights with (its
// to-hit/avoidance/damage). A mob with no CombatProfile fights with the to-hit auto-hit default.
type LivingDTO struct {
	Attributes    map[string]float64 `json:"attributes" yaml:"attributes"`
	CombatProfile string             `json:"combat_profile" yaml:"combat_profile"`
	// LootTable names the pack-global loot_table_def this mob drops from on death (Phase 12.1). Empty =>
	// the mob drops only its carried inventory (the pre-12 behavior). The resolver runs the table per
	// eligible looter on death (loot.go).
	LootTable string `json:"loot_table" yaml:"loot_table"`
}

// MaterialDTO marks a stackable crafting material (Phase 13.2, docs/CRAFTING.md §5): MaxStack is the
// largest stack size (identical material instances merge up to it); Type is a free-form material category
// ("leather", "essence") content groups by. A material with MaxStack < 1 defaults to a large cap.
type MaterialDTO struct {
	MaxStack int    `json:"max_stack,omitempty" yaml:"max_stack,omitempty"`
	Type     string `json:"type,omitempty" yaml:"type,omitempty"`
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

// WearSlotDTO is one content-defined equipment slot (#35, docs/MUDLIB.md §3): a wear location an item may
// occupy. Ref is the stable slot id (the token an item's `wearable.locations` names and the Wearer map key);
// Label is the human word act() shows ("head", "wielded"); Order fixes the deterministic display/selection
// order (the `equipment` list + the `N.` ordinal agree on it); Kind routes the equip verb — "worn" (the
// generic `wear`), "wield" (the `wield` verb / the combat weapon slot), or "hold" (the off-hand `hold` verb).
type WearSlotDTO struct {
	Ref   string `json:"ref" yaml:"ref"`
	Label string `json:"label,omitempty" yaml:"label,omitempty"`
	Order int    `json:"order,omitempty" yaml:"order,omitempty"`
	Kind  string `json:"kind,omitempty" yaml:"kind,omitempty"` // "worn" (default) | "wield" | "hold"
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

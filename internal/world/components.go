package world

// Core component structs (docs/MUDLIB.md §3). Each is a typed struct granting one
// capability; they are added to entities via Add and read via Get/Must (component.go).
//
// Slice 1 (docs/PHASE3-PLAN.md) only populates the three components the entity/session
// split needs — Room, Living, PlayerControlled. The rest are present as documented
// stubs carrying the *shape* MUDLIB §3 settled on, so later slices fill in behaviour
// without churning call sites or the component registry. Only the fields a slice
// actually uses are wired; everything else is a stub field with the documented intent.

// Room grants "is a location": exits, environment, and room flags. A room is just an
// Entity with a *Room component and no location (its container is the zone); its
// occupants and ground items live in the entity's contents (MUDLIB §4). Slice 1 uses
// exits + the display fields (which live on the Entity: short/long); sector and flags
// are stubs until content/sectors arrive.
type Room struct {
	// exits maps a canonical direction ("north") to the destination room's ProtoRef
	// ("midgaard:room:market" or a cross-zone "darkwood:room:grove"). This replaces the
	// old Room.exits string map; routing splits the ProtoRef via parseRef.
	exits map[string]ProtoRef

	// sector classifies the terrain/environment (city, forest, water…) for movement
	// cost and look flavour. Stub: unused in slice 1.
	sector string
	// flags carries the legacy bitmask room flags (dark/indoor…). Stub: the visibility filter
	// consults it once content supplies bit flags.
	flags uint64 //nolint:unused // TODO(world-engineer): legacy bitmask room-flags stub; keep until content supplies bit flags
	// namedFlags is the open-set room flag store (flags.go): builder-authored named booleans like
	// "safe"/"arena" that the PvP gate (pvp.go) and other content rules read. Populated from the
	// room DTO at authoring (content_map.go); immutable at runtime this phase. nil => no flags set.
	namedFlags map[string]bool
}

func (*Room) componentKind() Kind { return KindRoom }

// dirOrder is the canonical display order for exits, so the "Exits:" line always reads
// N/E/S/W/U/D regardless of map iteration order (carried over from the old Room).
var dirOrder = []string{"north", "east", "south", "west", "up", "down"}

// sortedExits returns this room's exit directions in canonical display order.
func (r *Room) sortedExits() []string {
	var out []string
	for _, d := range dirOrder {
		if _, ok := r.exits[d]; ok {
			out = append(out, d)
		}
	}
	return out
}

// Living grants "is alive and can act": stats, vitals, position, and a fighting target. Held as a
// direct pointer on Entity (the hot component movement/combat touch every tick, MUDLIB §3).
//
// Phase 5.1 makes it REAL: the old stub hp/maxHP/mp/mv int fields are GONE; vitals are now
// content-defined RESOURCES (resCur holds each pool's current; max is a derived attribute) and stats
// are content-defined ATTRIBUTES (attrBase holds per-entity base overrides; the resolved value comes
// from the modifier stack, attributes.go). All reads route through accessors (attr()/resourceCurrent/
// the Living methods below) so the Phase 1-4 call sites that never touched the int fields stay green,
// and an entity with NO content defs behaves sanely (every accessor reports 0/full).
//
// The two maps and the cache are INSTANCE state, mutated only by the owning zone goroutine. For a
// player (prototype==nil) they hold its bases/currents directly; a future prototype-backed mob would
// inherit bases from its prototype and COW these maps on first write (cloneComponent handles them).
type Living struct {
	// attrBase holds this entity's per-attribute BASE OVERRIDES (race/class/level/point-buy); a
	// present override replaces the attribute_def's default base in derivation (attributes.go). nil
	// until the first setAttrBase. Persisted (base overrides only — derived values are recomputed).
	attrBase map[string]float64
	// resCur holds each resource pool's CURRENT (max is the derived attr). nil until the first
	// setResourceCurrent. Persisted (current only); resourceCurrent clamps it to the live derived max.
	resCur map[string]int
	// flags is the entity's open-set named-flag store (flags.go): per-entity booleans like the "pvp"
	// consent flag the PvP gate reads. nil until the first setFlag. Persisted in the StateJSON `flags`
	// subtree. Instance state, zone-goroutine-owned; a COW'd instance starts with no flags.
	flags map[string]bool
	// attrs is the memoized derivation cache + dirty bit (attributes.go). Recomputed lazily after any
	// base/mod change. NOT persisted (it is a pure function of bases + mods + defs) and NOT shared —
	// each instance owns its own; cloneComponent gives a COW'd instance a fresh (empty) cache.
	attrs attrCache
	// modSrcs are this entity's modifier sources feeding derivation (gear + affects). The 5.2
	// Affected runtime registers its view here (addModSource); empty until then, so derivation is
	// base-only. Instance state, recomputed at runtime — NOT persisted (affects re-attach in 5.2 and
	// re-register their source). A COW'd instance starts with NO sources (it re-registers its own).
	modSrcs []modSource

	// position is standing/resting/sleeping/fighting… gating which commands run
	// (MUDLIB §6, position.go). Stored as an int (the Position enum's underlying type) so the COW
	// shallow-copy and the persisted shape are unchanged; position()/setPosition give it names.
	position int
	// fighting is the current combat target (a live entity), nil when not fighting. Set by startFight
	// (combat.go) and cleared by stopFight; the per-zone round driver swings every posFighting entity
	// at its `fighting` target each PULSE_VIOLENCE. Transient — never persisted (combat drops on a
	// crash/handoff, P6-D8); a captured *Entity here is re-validated each round (same-room/alive gates).
	fighting *Entity
	// combatRef names the pack-global combat profile this entity fights with (combat.go): its to-hit
	// check, avoidance ladder, and damage bonus. Resolved by ref through the per-shard combat-profile
	// registry (combatProfileFor) — the ref, not a resolved pointer, is stored so prototype build order
	// (defineContent vs defineGlobals) never matters and a hot-reloaded profile is picked up. "" => no
	// combat profile (the bare-engine case: a `kill` auto-hits, weapon-only damage). A mob carries its
	// prototype's ref; a player carries the pack DefaultCombat ref (set at entity creation).
	combatRef string
	// cooldowns maps an ability ref to the pulse number its cooldown ELAPSES on ([G8], combat.go). The
	// ability lifecycle step-3 gate refuses an ability still cooling down. Transient instance state
	// (zone-goroutine-owned); serialized as REMAINING pulses into StateJSON.Cooldowns (P6-D8). nil until
	// the first armed cooldown.
	cooldowns map[string]uint64
	// threat is this entity's threat table (death.go, Phase 6.3b): attacker entity -> accumulated threat
	// (damage dealt + heals weighted). A MOB picks its round target as the highest-threat live attacker
	// in its room, so pulling aggro is a content-numbers consequence, not engine-hardcoded. TRANSIENT
	// (never persisted, like fighting) and keyed by live *Entity — die()/disengage scrub the dead/departed
	// entry so no stale pointer survives. nil until the first threat is added. Zone-goroutine-owned.
	threat map[*Entity]float64
}

func (*Living) componentKind() Kind { return KindLiving }

// PlayerControlled is the bridge between an in-world entity and its connection
// (docs/PHASE3-PLAN.md §2). It is how the zone goes entity -> output (session.send)
// and how a command finds the actor's connection. Slice 1 uses only the session link;
// account/aliases/prompt/GMCP are the documented shape for later (MUDLIB §3).
type PlayerControlled struct {
	// session is the connection/handoff state for this player (session.go). The session
	// holds the reverse pointer (session.entity), so entity <-> session is a two-way
	// link established at construction and carried together through every handoff.
	session *session

	// account, aliases, promptCfg, gmcpSupports — per MUDLIB §3. Stubs until the
	// account model and GMCP negotiation arrive (Phase 8+).
	account string //nolint:unused // TODO(world-engineer): account-model stub (MUDLIB §3); keep until Phase 8+
	aliases map[string]string
}

func (*PlayerControlled) componentKind() Kind { return KindPlayerControlled }

// Physical grants mass/size/material/condition (MUDLIB §3). Functional in slice 4: items
// carry weight so a Container can enforce a weight limit and a wearer can weigh its load
// (the latter is shape only this phase). The fields are value types, so a COW of Physical
// is the shallow struct copy in cloneComponent.
type Physical struct {
	weight   int
	size     int
	material string
}

func (*Physical) componentKind() Kind { return KindPhysical }

// Container grants "holds other entities" beyond the universal contents tree: capacity,
// weight limit, and open/closed/locked state (MUDLIB §3). Functional in slice 4: get/put
// respect closed and capacity; open/close flip `closed`, which — because Container is a
// prototype-shared component on a spawned instance — MUST go through mutableComponent so
// the write lands on the instance, never the shared prototype (the slice-3 COW path,
// Finding 6). capacity 0 means "unbounded" (no limit authored). All fields are value
// types, so cloneComponent copies a Container with the shallow struct copy.
type Container struct {
	capacity    int      // max item count this container holds; 0 == unbounded
	weightLimit int      // shape only this phase (no aggregate-weight enforcement yet)
	closed      bool     // an item cannot be taken from / put into a closed container
	locked      bool     // shape only this phase (no key/lock command yet)
	keyRef      ProtoRef // shape only: the key prototype that unlocks it
}

func (*Container) componentKind() Kind { return KindContainer }

// hasRoom reports whether the container can accept one more item, given how many it
// currently holds (n). A capacity of 0 is unbounded (no authored limit).
func (c *Container) hasRoom(n int) bool { return c.capacity == 0 || n < c.capacity }

// WearLoc is a wear-location slot (MUDLIB §3). A Wearable advertises which of these slots it
// can occupy; a slot maps a worn item to a body location on the wearer. Held/wielded are slots
// too (the hands), which is why wield/hold share the worn-slot machinery. The set is the classic
// Diku core; content extends it later. Each is a distinct slot index (NOT a bitmask position)
// used as the Wearer map key and the Wearable.locations bit shift.
type WearLoc int

// WearLoc values: the worn-equipment slots. WearLocNone is the not-wearable sentinel.
const (
	WearLocNone  WearLoc = iota // sentinel: not wearable anywhere
	WearLocHead                 // a helmet
	WearLocBody                 // armor on the torso
	WearLocHands                // gloves
	WearLocFeet                 // boots
	WearLocWield                // primary weapon hand
	WearLocHold                 // off-hand held item (light, shield-substitute, focus)
	wearLocCount                // table size; keep last
)

// wearLocName is the human label for a slot, used in the equipment list and act() lines
// ("You wear $p on your head.").
var wearLocName = map[WearLoc]string{
	WearLocHead:  "head",
	WearLocBody:  "body",
	WearLocHands: "hands",
	WearLocFeet:  "feet",
	WearLocWield: "wielded",
	WearLocHold:  "held",
}

// Wearable grants wear locations: the set of slots this item can occupy (MUDLIB §3).
// locations is a bitmask of (1 << WearLoc) bits. Functional in slice 4: wear/wield/hold
// consult it to pick a legal slot; remove returns the item to inventory.
type Wearable struct {
	locations uint64 // bitmask of valid wear slots: bit (1<<loc) set == may occupy loc
}

func (*Wearable) componentKind() Kind { return KindWearable }

// canWear reports whether this item may occupy slot loc.
func (w *Wearable) canWear(loc WearLoc) bool { return w.locations&(1<<loc) != 0 }

// slots returns the wearable's legal slots in WearLoc order, so a generic `wear` (no
// explicit slot) can pick the first free legal one.
func (w *Wearable) slots() []WearLoc {
	var out []WearLoc
	for loc := WearLocHead; loc < wearLocCount; loc++ {
		if w.canWear(loc) {
			out = append(out, loc)
		}
	}
	return out
}

// wearableFor builds a Wearable advertising exactly the given slots (authoring helper).
func wearableFor(locs ...WearLoc) *Wearable {
	var bits uint64
	for _, l := range locs {
		bits |= 1 << l
	}
	return &Wearable{locations: bits}
}

// Wearer holds the equipment state for a living entity: which item occupies each worn
// slot (MUDLIB §3 — the wearer side of Wearable). It lives on the WEARER, not the item,
// so it is plain instance state: a player entity has prototype==nil, so mutating its
// Wearer map is never a prototype-shared write and needs no COW. (A spawned mob that
// wears gear would COW its Wearer like any other component — handled by cloneComponent.)
//
// A worn item stays in the wearer's contents (inventory) — equipped is a STATE over a
// carried item, exactly as Diku models it — so it is reachable both as inventory and via
// the Wearer slot map. Targeting's ScopeEquipment reads worn; ScopeInventory reads the
// rest. remove just clears the slot, leaving the item carried.
type Wearer struct {
	worn map[WearLoc]*Entity
}

func (*Wearer) componentKind() Kind { return KindWearable } // shares the wearable kind tag

// slotOf returns the slot an item currently occupies on this wearer, or WearLocNone.
func (w *Wearer) slotOf(item *Entity) WearLoc {
	for loc, e := range w.worn {
		if e == item {
			return loc
		}
	}
	return WearLocNone
}

// Weapon grants damage dice, damage type, class, and attack verb (MUDLIB §3). Data only
// this phase: wield records the weapon in the WearLocWield slot; combat resolution
// (rolling the dice, applying damageType) is Phase 6. The pulse scheduler (pulse.go) is
// the substrate those rounds will hang off.
type Weapon struct {
	diceNum, diceSize int
	damageType        string
	class             string
	attackVerb        string
}

func (*Weapon) componentKind() Kind { return KindWeapon }

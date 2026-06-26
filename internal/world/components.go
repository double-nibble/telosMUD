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
	// flags carries room flags (safe/dark/indoor…). Stub: the visibility filter and
	// safe-room checks consult it once content supplies flags.
	flags uint64
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

// Living grants "is alive and can act": vitals, position, stats, and a fighting
// target. Held as a direct pointer on Entity (the hot component movement/combat touch
// every tick, MUDLIB §3). Slice 1 adds it to player entities to mark them living but
// reads none of its fields yet; vitals/stats derivation is Phase 5, combat Phase 6, so
// the fields below are the documented shape, all stubs.
type Living struct {
	// hp/mp/mv and their maxima — current vitals. Stub until Phase 5 stat derivation.
	hp, maxHP int
	mp, maxMP int
	mv, maxMV int

	// position is standing/resting/sleeping/fighting… gating which commands run
	// (MUDLIB §6). Stub: defaults to the zero value until the Position enum lands.
	position int
	// fighting is the current combat target (a live entity), nil when not fighting.
	// Stub: set by combat in Phase 6.
	fighting *Entity
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
	account string
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

// Wear locations (MUDLIB §3). A Wearable advertises which of these slots it can occupy;
// a slot maps a worn item to a body location on the wearer. Held/wielded are slots too
// (the hands), which is why wield/hold share the worn-slot machinery. The set is the
// classic Diku core; content extends it later. Each is a distinct slot index (NOT a
// bitmask position) used as the Wearer map key and the Wearable.locations bit shift.
type WearLoc int

const (
	WearLocNone   WearLoc = iota // sentinel: not wearable anywhere
	WearLocHead                  // a helmet
	WearLocBody                  // armor on the torso
	WearLocHands                 // gloves
	WearLocFeet                  // boots
	WearLocWield                 // primary weapon hand
	WearLocHold                  // off-hand held item (light, shield-substitute, focus)
	wearLocCount                 // table size; keep last
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

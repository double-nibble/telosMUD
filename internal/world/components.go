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

// --- Stubs below: shape only, not built or read this phase. ---

// Physical grants mass/size/material/condition (MUDLIB §3). Stub: items become real in
// slice 4.
type Physical struct {
	weight   int
	size     int
	material string
}

func (*Physical) componentKind() Kind { return KindPhysical }

// Container grants "holds other entities" beyond the universal contents tree: capacity,
// weight limit, and open/closed/locked state (MUDLIB §3). Stub: functional in slice 4.
type Container struct {
	capacity    int
	weightLimit int
	closed      bool
	locked      bool
	keyRef      ProtoRef
}

func (*Container) componentKind() Kind { return KindContainer }

// Wearable grants wear locations (worn/wield/hold) (MUDLIB §3). Stub: functional in
// slice 4 (wear/wield/remove).
type Wearable struct {
	locations uint64 // bitmask of valid wear slots
}

func (*Wearable) componentKind() Kind { return KindWearable }

// Weapon grants damage dice, damage type, class, and attack verb (MUDLIB §3). Stub:
// data only; combat resolution is Phase 6.
type Weapon struct {
	diceNum, diceSize int
	damageType        string
	class             string
	attackVerb        string
}

func (*Weapon) componentKind() Kind { return KindWeapon }

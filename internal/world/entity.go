package world

// Entity (docs/MUDLIB.md §2). Everything in the world — player, mob, item, room — is an
// Entity: an identity plus uniform containment plus a bag of optional components (§3).
// Identity + containment are core (every entity has them); capabilities are components.
//
// Field access goes through this type's own methods and the generic component
// accessors, never raw cross-package field reads. That keeps slice 3 free to make
// shared immutable fields fall through to a prototype (flyweight + copy-on-write,
// MUDLIB §5) without touching call sites; slice 1 just stores everything locally.
type Entity struct {
	rid   RuntimeID  // cheap live handle, per-zone, ephemeral (MUDLIB §1)
	proto ProtoRef   // template it was spawned from / its stable content key
	pid   *PersistID // durable id; nil unless saved (plumbed but nil in Phase 3)

	keywords []string // targeting tokens: {"long","sword","sharp"} (used in slice 2)
	short    string   // inline name: "a long sword" / a player's name
	long     string   // room line: "A long sword lies here."

	location *Entity   // what holds this entity (room/container); nil for rooms
	contents []*Entity // what this entity holds (occupants, ground items, inventory)

	comps componentSet // optional capabilities (§3)

	// room and living are the two near-universal hot components, held as direct typed
	// pointers in addition to the comps map so movement/look/combat never pay a map
	// lookup (the MUDLIB §3 escape hatch). Add keeps them in sync with the map; they are
	// nil on entities lacking the component.
	room   *Room
	living *Living

	zone *Zone // single-writer owner (MUDLIB §2); set when the entity enters a zone
}

// newEntity builds a bare entity with a freshly allocated per-zone RuntimeID and the
// given content key. Components and containment are layered on by the caller (newRoom,
// newPlayerEntity). The entity is not yet placed in the world tree (location nil).
func (z *Zone) newEntity(proto ProtoRef) *Entity {
	return &Entity{
		rid:   z.rids.alloc(),
		proto: proto,
		zone:  z,
		comps: componentSet{},
	}
}

// Name returns the entity's inline/display name (short). An accessor rather than a raw
// field read so slice 3 can fall through to a shared prototype description.
func (e *Entity) Name() string { return e.short }

// RuntimeID returns the entity's per-zone live handle (MUDLIB §1).
func (e *Entity) RuntimeID() RuntimeID { return e.rid }

// Move is the single containment primitive (MUDLIB §4): detach e from its current
// container's contents, set its location to dest, and append it to dest's contents. All
// intra-zone moves are plain slice ops on the owning zone goroutine — lock-free,
// exactly as the old occupant-map ops were. dest may be nil to remove e from the tree
// entirely (used when a player departs via a handoff).
//
// Single-writer: callers must already own e (be the entity's zone goroutine). Move
// never crosses a goroutine boundary; cross-zone/cross-shard moves hand the entity off
// through the inbox and the destination goroutine then Moves it into the dest room.
func Move(e, dest *Entity) {
	if e.location != nil {
		e.location.removeContent(e)
	}
	e.location = dest
	if dest != nil {
		dest.contents = append(dest.contents, e)
	}
}

// removeContent detaches child from e.contents (order-preserving), if present. Internal
// to Move; the slice swap is cheap at MUD room populations.
func (e *Entity) removeContent(child *Entity) {
	for i, c := range e.contents {
		if c == child {
			e.contents = append(e.contents[:i], e.contents[i+1:]...)
			return
		}
	}
}

// contentsByKeyword returns the entities in e.contents whose keywords match the typed
// word under Diku isname semantics (the word is a prefix of one keyword). Slice 1 does
// not use it yet — the real Diku targeting grammar (2.sword, all.coin) is slice 2 — but
// it is the containment-side hook the resolver will build on, and keeping it here keeps
// the contents tree the single place containment is queried.
func (e *Entity) contentsByKeyword(word string) []*Entity {
	var out []*Entity
	for _, c := range e.contents {
		for _, kw := range c.keywords {
			if len(word) <= len(kw) && kw[:len(word)] == word {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

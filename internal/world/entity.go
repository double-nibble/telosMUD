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

	// prototype is the immutable template this entity is a DELTA over (flyweight + COW,
	// MUDLIB §5, prototype.go). nil for entities authored without a prototype (players,
	// the slice-1 inline rooms). When set, the shared-immutable fields below (keywords/
	// short/long) and the component pointers in comps start aliased to the prototype; a
	// COW helper copies the one field being written onto this instance before mutating, so
	// a write here never touches the prototype or a sibling. Reads fall through the Entity
	// accessors transparently.
	prototype *Prototype

	// keywords/short/long are the instance's view of its display/targeting data. On a
	// prototype-backed entity they are initially SHARED with the prototype (the slice
	// header and the strings are the prototype's) and become instance-local on first COW
	// (mutableKeywords / setShort / setLong). On a non-prototype entity they are plain
	// instance-local fields. Either way, read them through the accessors, never raw across
	// packages, so the fall-through stays an implementation detail.
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
// field read so a prototype-backed instance can fall through to its shared template
// description (MUDLIB §5). e.short is either the instance's own (after COW / a non-proto
// entity) or the prototype's shared string; both read identically.
func (e *Entity) Name() string { return e.short }

// Long returns the entity's room/ground line (the long description). Like Name, an
// accessor so the prototype fall-through stays an implementation detail.
func (e *Entity) Long() string { return e.long }

// keywordList returns the entity's targeting keywords. Read-only by contract: callers
// must never mutate the returned slice in place — on a prototype-backed instance it is
// the prototype's SHARED slice (mutating it would corrupt every sibling). Use
// mutableKeywords to change keywords.
func (e *Entity) keywordList() []string { return e.keywords }

// --- Copy-on-write setters (MUDLIB §5) -------------------------------------------------
//
// Each mutator copies the one shared field it touches onto the instance before writing, so
// a write to any instance affects ONLY that instance — never the shared prototype, never a
// sibling. For value fields (short/long) the "copy" is just assigning the instance's own
// field (Go strings are immutable, so no aliasing risk; the COW is the assignment itself).
// For reference-typed fields (keywords slice, the exits map on a component) the copy must
// be deep enough that the new backing array/map shares no storage with the prototype.

// setShort overrides this instance's inline name (COW of the short field). Safe because
// strings are immutable values: assigning e.short detaches the instance from the
// prototype's string without aliasing.
func (e *Entity) setShort(s string) { e.short = s }

// setLong overrides this instance's room/ground line (COW of the long field).
func (e *Entity) setLong(s string) { e.long = s }

// mutableKeywords returns a keywords slice the caller may freely mutate, performing
// copy-on-write the first time: if the slice is still the prototype's shared backing
// array, it is reallocated into an instance-local copy so the write cannot alias the
// prototype or a sibling. It detects "still shared" by identity against the prototype's
// slice header. Callers assign the result back (e.keywords = e.mutableKeywords()) or mutate
// in place after calling — either way the returned slice is instance-owned.
func (e *Entity) mutableKeywords() []string {
	if e.prototype != nil && sameSlice(e.keywords, e.prototype.keywords) {
		dup := make([]string, len(e.keywords))
		copy(dup, e.keywords)
		e.keywords = dup
		e.zone.log.Debug("cow: keywords", "rid", e.rid, "proto", e.proto)
	}
	return e.keywords
}

// setKeywords replaces this instance's keywords with an instance-owned copy of kw (COW:
// the prototype's slice is never touched, and the instance never aliases the caller's
// slice either). A convenience over mutableKeywords for a wholesale replace.
func (e *Entity) setKeywords(kw []string) {
	dup := make([]string, len(kw))
	copy(dup, kw)
	e.keywords = dup
}

// sameSlice reports whether two string slices share the same backing array and length —
// i.e. a is still the prototype's shared slice, not an instance-local copy. Comparing the
// header (pointer + len) is enough for the COW guard: spawn aliases the prototype's exact
// slice, and any COW reallocates, so identity distinguishes shared from owned.
func sameSlice(a, b []string) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

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
		for _, kw := range c.keywordList() {
			if len(word) <= len(kw) && kw[:len(word)] == word {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

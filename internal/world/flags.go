package world

// flags.go is the open-set named-flag store (docs/ABILITIES.md §1 — "flags & tags are open sets of
// named booleans the engine stores and queries; content gives them meaning"). The engine never
// invents a flag name; content sets them and the engine reads them (the pillar). Two carriers:
//
//   - ENTITY flags (on Living.flags): per-entity booleans like the "pvp" consent flag. Instance
//     state, zone-goroutine-owned, persisted in the StateJSON `flags` subtree (character.go).
//   - ROOM flags (on Room.flags as named strings): builder-set room booleans like "safe"/"arena".
//     Authored on the room prototype; read-only at runtime this phase.
//
// The PvP gate (pvp.go) is the first consumer, but the store is generic — any content rule can read
// a flag. Both are simple string sets; the open-string discipline matches tags (§6).

// hasFlag reports whether entity e carries the named flag. A nil/Living-less entity, or one with no
// flag set, reports false. Zone-goroutine read.
func hasFlag(e *Entity, name string) bool {
	if e == nil || e.living == nil || e.living.flags == nil {
		return false
	}
	return e.living.flags[name]
}

// setFlag sets (on=true) or clears (on=false) a named flag on entity e. The "pvp" consent flag is set
// this way (a consent command / chargen, later). Single-writer: zone goroutine. A no-op without Living.
func setFlag(e *Entity, name string, on bool) {
	if e == nil || e.living == nil {
		return
	}
	if !on {
		if e.living.flags != nil {
			delete(e.living.flags, name)
		}
		return
	}
	if e.living.flags == nil {
		e.living.flags = map[string]bool{}
	}
	e.living.flags[name] = true
}

// roomFlag reports whether a ROOM entity carries the named room flag. The room's flags live on its
// *Room component (a named-string set, populated from the room DTO at authoring). A non-room entity
// or an unset flag reports false. Read-only at runtime.
func roomFlag(roomEntity *Entity, name string) bool {
	if roomEntity == nil || roomEntity.room == nil || roomEntity.room.namedFlags == nil {
		return false
	}
	return roomEntity.room.namedFlags[name]
}

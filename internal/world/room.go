package world

// Room is a single location. Phase 1 keeps it minimal; the full entity/component
// model (docs/MUDLIB.md) arrives in Phase 3.
type Room struct {
	id        string
	name      string
	desc      string
	exits     map[string]string // direction -> destination room id
	occupants map[string]bool   // set of player ids
}

func newRoom(id, name, desc string) *Room {
	return &Room{
		id:        id,
		name:      name,
		desc:      desc,
		exits:     map[string]string{},
		occupants: map[string]bool{},
	}
}

// dirOrder is the canonical display order for exits.
var dirOrder = []string{"north", "east", "south", "west", "up", "down"}

func (r *Room) sortedExits() []string {
	var out []string
	for _, d := range dirOrder {
		if _, ok := r.exits[d]; ok {
			out = append(out, d)
		}
	}
	return out
}

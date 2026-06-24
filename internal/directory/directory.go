// Package directory resolves which world shard owns a given zone or character.
// Phase 1 ships a single-shard static implementation; the Redis-backed locator
// (docs/ARCHITECTURE.md §4) arrives in Phase 2.
package directory

// Directory maps a character to the address of the world shard that should host
// them.
type Directory interface {
	ShardForCharacter(characterID string) (addr string, ok bool)
}

// Static always resolves to a single configured shard address.
type Static struct {
	Addr string
}

func (s Static) ShardForCharacter(string) (string, bool) {
	return s.Addr, s.Addr != ""
}

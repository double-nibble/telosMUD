// Package directory resolves which world shard owns a given zone or character.
// Phase 1 ships a single-shard static implementation; the Redis-backed locator
// (docs/ARCHITECTURE.md §4) arrives in Phase 2.
//
// The directory is the gate's seam onto the world topology: before opening a
// Play stream the gate asks the directory "which shard hosts this character?"
// and dials the address it gets back. Phase 1 always answers with the single
// configured shard, but every caller already goes through this interface so the
// later (player -> shard, Redis-backed, NATS-invalidated) implementation drops in
// without touching the gate's control flow.
package directory

import "log/slog"

// Directory maps a character to the address of the world shard that should host
// them. It is the one place the gate consults to turn a character identity into
// a concrete dial address; implementations may be static (Phase 1) or backed by
// Redis with NATS cache invalidation (Phase 2+).
type Directory interface {
	// ShardForCharacter resolves characterID to a world shard dial address.
	// ok is false when no shard can serve the character (e.g. nothing is
	// configured / the world is unavailable), in which case addr is empty.
	ShardForCharacter(characterID string) (addr string, ok bool)
}

// Static always resolves to a single configured shard address. It is the Phase 1
// directory: every character maps to the same world. An empty Addr means "no
// world available", which surfaces to the player as a polite refusal at the gate.
type Static struct {
	Addr string
}

// ShardForCharacter returns the single configured shard address. ok is false
// (and addr empty) only when no address was configured. The lookup is logged at
// Debug so DEBUG=1 traces show the directory seam resolving each login.
func (s Static) ShardForCharacter(characterID string) (string, bool) {
	ok := s.Addr != ""
	slog.Debug("directory lookup",
		"component", "directory",
		"character", characterID,
		"addr", s.Addr,
		"ok", ok,
	)
	return s.Addr, ok
}

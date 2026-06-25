package world

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
)

// Locator is the slice of the directory the world needs for cross-shard handoff:
// resolving which shard owns a zone (zone -> shard id), resolving that shard id to a
// dial endpoint, and recording a player's new placement with a monotonic epoch
// (docs/ARCHITECTURE.md §4, PROTOCOL.md §5). directory.Redis implements it.
type Locator interface {
	ShardForZone(ctx context.Context, zoneID string) (string, error)      // -> shard id
	EndpointForShard(ctx context.Context, shardID string) (string, error) // shard id -> dial endpoint
	SetPlayerShard(ctx context.Context, playerID, shardID string, epoch uint64) (bool, error)
}

// buildSnapshot serializes a player's authoritative in-memory state for transfer to
// the destination shard. Slice 1 keeps it behavior-preserving: the same minimal fields
// — identity plus the applied-input high-water mark (the linchpin that ties the source's
// freeze-point to the gate's replay-point, PROTOCOL.md §5) — now sourced from the
// session and its entity instead of the old player struct. Inventory/equipment/affects
// (proto fields 6–11) stay unset until later slices make those components carry real
// state (PHASE3-PLAN.md §4); cross-shard inventory is deferred past Phase 3. Built on
// the zone goroutine, so the read of session/entity state is race-free.
func buildSnapshot(s *session) *handoffv1.PlayerSnapshot {
	return &handoffv1.PlayerSnapshot{
		CharacterId: s.character,
		Name:        s.entity.Name(),
		AppliedSeq:  s.appliedSeq,
		Flags:       map[string]string{},
	}
}

// handoffToken derives a deterministic token from (character, epoch) so a retried
// Prepare converges on the same token and the same pending entity rather than
// creating a duplicate (PROTOCOL.md §5).
func handoffToken(character string, epoch uint64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", character, epoch)))
	return hex.EncodeToString(sum[:16])
}

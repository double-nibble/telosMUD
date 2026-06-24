package world

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
)

// Locator is the slice of the directory the world needs for cross-shard handoff:
// resolving which shard hosts a zone, and recording a player's new placement with a
// monotonic epoch (docs/ARCHITECTURE.md §4, PROTOCOL.md §5). directory.Redis
// implements it.
type Locator interface {
	ShardForZone(ctx context.Context, zoneID string) (string, error)
	SetPlayerShard(ctx context.Context, playerID, shardAddr string, epoch uint64) (bool, error)
}

// parseRef splits a qualified exit reference "zone:room" into its parts. An
// unqualified ref (no colon) is a local room with an empty zone.
func parseRef(ref string) (zone, room string) {
	if i := strings.IndexByte(ref, ':'); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return "", ref
}

// buildSnapshot serializes a player's authoritative in-memory state for transfer to
// the destination shard. Phase 2 carries the minimum — identity plus the applied-
// input high-water mark (the linchpin that ties the source's freeze-point to the
// gate's replay-point, PROTOCOL.md §5). Full character state fills in as the mudlib
// grows. Built on the zone goroutine, so the read of player state is race-free.
func buildSnapshot(p *player) *handoffv1.PlayerSnapshot {
	return &handoffv1.PlayerSnapshot{
		CharacterId: p.id,
		Name:        p.name,
		AppliedSeq:  p.appliedSeq,
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

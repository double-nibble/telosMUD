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
	// PlayerEpoch reads the player's last-recorded ownership epoch from the directory so a
	// fresh login can RESUME it (instead of restarting at 1). The directory's placement
	// persists across logout/crash/restart, so the next cross-shard move must compute
	// stored+1 to satisfy the placement CAS — otherwise a relog after any handoff hits an
	// "ownership conflict". found=false (not an error) when the player has no placement yet
	// (a brand-new character). Read OFF the zone goroutine (server.go), never on it.
	PlayerEpoch(ctx context.Context, playerID string) (epoch uint64, found bool, err error)
}

// buildSnapshot serializes a player's authoritative in-memory state for transfer to
// the destination shard. Slice 1 keeps it behavior-preserving: the same minimal fields
// — identity plus the applied-input high-water mark (the linchpin that ties the source's
// freeze-point to the gate's replay-point, PROTOCOL.md §5) — now sourced from the
// session and its entity instead of the old player struct. Inventory/equipment/affects
// (proto fields 6–11) stay unset until later slices make those components carry real
// state (PHASE3-PLAN.md §4); cross-shard inventory is deferred past Phase 3. As of Phase
// 5.1 a player also carries content-defined attribute bases + resource currents (Living,
// character.go StateJSON) — those are NOT in this snapshot either, so a wound/resource
// state resolves from defaults across a cross-shard hop, exactly like inventory. As of 5.2
// a player ALSO carries active affects (the Affected component, character.go AffectJSON);
// those round-trip through the SAVE/LOAD (restart) path but, like attributes/resources/
// inventory, are STILL NOT on this handoff snapshot — the same deferred set. Because affect
// durations only decrement on the OWNING zone's pulse (the resolve-by-id/skip-frozen tick),
// a frozen in-flight entity does not tick, so durations are conserved up to the seam; the
// destination simply resolves from defaults until the handoff carry lands. Carry attributes/
// resources/affects HERE together when cross-shard inventory lands. Built on the zone
// goroutine, so the read of session/entity state is race-free.
func buildSnapshot(s *session) *handoffv1.PlayerSnapshot {
	// DEFERRED (PHASE3-PLAN.md §4): as of slice 4 a player entity CAN carry items
	// (s.entity.contents) and wear them (the *Wearer slot map, components.go). A future
	// cross-shard handoff that supports a player carrying inventory would walk
	// s.entity.contents + the Wearer slots HERE and have prepare rehydrate them — which needs
	// the common.v1.Item shape to reference a ProtoRef plus the instance's COW delta (so a
	// unique/enchanted item survives the hop). That is a proto change scoped OUT of Phase 3
	// (the ROADMAP milestone is single-zone); no Phase-3 test transfers a player with items,
	// so the snapshot stays minimal — identity + the applied-input high-water mark only.
	// Carry the durable PersistID so the DESTINATION can flush this player to the SAME durable
	// row (characters.id). Without it the handed-off session has no id to CAS on, so a quit on the
	// destination is silently skipped (leave's pid==nil guard) — the location never advances and the
	// next login routes back to the stale home_zone (the cross-shard reconnect bug). Empty only in
	// the async-create window: a brand-new char can hand off BEFORE its CreateCharacter returned the
	// PID at the source, so e.pid is still nil here; the destination then re-resolves it by name on
	// bind. Paired with StateVersion below so the id and the CAS base cross the seam together.
	var pid string
	if s.entity.pid != nil {
		pid = string(*s.entity.pid)
	}
	return &handoffv1.PlayerSnapshot{
		CharacterId: s.character,
		Name:        s.entity.Name(),
		AppliedSeq:  s.appliedSeq,
		PersistId:   pid,
		// Carry the optimistic-concurrency version through the handoff so a later save on the
		// destination CASes from the right base and a handoff + a save stay monotonic (slice 4.2,
		// docs/PERSISTENCE.md §7). The destination seeds its session.stateVersion from this on
		// Prepare, exactly as it seeds appliedSeq.
		StateVersion: s.stateVersion,
		Flags:        map[string]string{},
	}
}

// handoffToken derives a deterministic token from (character, epoch) so a retried
// Prepare converges on the same token and the same pending entity rather than
// creating a duplicate (PROTOCOL.md §5).
func handoffToken(character string, epoch uint64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", character, epoch)))
	return hex.EncodeToString(sum[:16])
}

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
	// PlayerShard resolves which shard a player currently lives on (Phase 8.5 tell routing, P8-D5)
	// — a thin world-facing wrapper over directory.PlayerPlacement. It is the EPOCH-AUTHORITATIVE
	// player->shard map — NEVER the presence roster (a stale
	// presence entry must never route or validate a tell; P8-A4). For durable-always tells (OQ-1) the
	// sender's world does not need the shard to DELIVER (it publishes to the per-target durable subject
	// and the target's own world drains it); it reads this only to VALIDATE the target exists — a
	// resolve MISS (found=false) refuses the tell to the sender ("no player by that name"). A character
	// that has ever logged in has a placement; a never-seen name does not. Read OFF the zone goroutine.
	PlayerShard(ctx context.Context, playerID string) (shardID string, found bool, err error)
}

// buildSnapshot serializes a player's authoritative in-memory state for transfer to
// the destination shard. It carries identity + the applied-input high-water mark (the
// linchpin that ties the source's freeze-point to the gate's replay-point, PROTOCOL.md §5)
// + the durable PersistID + the optimistic-concurrency version + the comms-state subtree
// (field 14) AND — as of the cross-shard full-state-carry fix — the player's REMAINING
// entity state in state_json (field 15): inventory, equipment, attribute bases, resource
// currents, affects (with remaining durations), flags, cooldowns (remaining), and the
// data-only self.state. dumpStateJSON reuses dumpCharacter's component dumpers so the
// handoff carry and the durable save are byte-identical for the entity subtree (one shape).
// The comms subtree rides field 14 (its single authority — NOT duplicated in state_json),
// and the tell delivered-cursor is intentionally out of scope (FOLLOW-UPS). AppliedSeq
// rides the dedicated field (12), never the embedded state, so the linchpin stays correct
// (a fresh login restarts the fence; a handoff keeps the same gate session). Because affect
// durations + cooldowns only decrement on the OWNING zone's pulse (resolve-by-id/skip-frozen
// tick), a frozen in-flight entity does not tick, so remaining values are conserved up to the
// seam and the destination re-attaches/re-arms them without reset. Built on the zone goroutine,
// so the read of session/entity state is race-free (the same single-writer safety as
// dumpCharacter; ItemJSON.Delta must be OWNED bytes, never aliasing a live COW buffer).
func buildSnapshot(s *session) *handoffv1.PlayerSnapshot {
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
		// Carry the receiver-side comms-state subtree (Phase 8.6, P8-D7) across the handoff so a
		// cross-shard walk is comms-transparent: channel toggles, the ignore list, and AFK do not reset
		// when the player changes shard. Empty for an all-default player (the common case); the
		// destination then loads defaults (identical to a pre-8.6 snapshot). The destination re-publishes
		// the EFFECTIVE hear-set (recomputed against ITS channel_defs + the live entity) to the gate on
		// arrival, so the gate's receiver HEAR-filter is correct for the destination's content.
		CommsState: dumpCommsStateJSON(s),
		// Carry the player's REMAINING entity state (inventory, equipment, attribute bases, resource
		// currents, affects with remaining durations, flags, cooldowns with remaining, self.state) across
		// the seam so a midgaard->darkwood walk no longer drops gear/stats/affects (the full-state-carry
		// fix). It reuses dumpCharacter's component dumpers (dumpStateJSON -> dumpStateComponents), MINUS
		// the comms subtree (CommsState above is the SINGLE comms authority) and the tell delivered-cursor
		// (session state, out of scope). AppliedSeq is NOT in here — it rides the dedicated field above.
		// Built at the freeze point on the zone goroutine, so the read is race-free and affect/cooldown
		// remaining values are conserved (the frozen source does not tick). Empty for a bare player.
		StateJson: dumpStateJSON(s),
	}
}

// handoffToken derives a deterministic token from (character, epoch) so a retried
// Prepare converges on the same token and the same pending entity rather than
// creating a duplicate (PROTOCOL.md §5).
func handoffToken(character string, epoch uint64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", character, epoch)))
	return hex.EncodeToString(sum[:16])
}

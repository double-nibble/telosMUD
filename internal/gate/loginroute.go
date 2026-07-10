package gate

import (
	"context"
	"log/slog"
	"time"

	"github.com/double-nibble/telosmud/internal/directory"
)

// loginroute.go owns the gate's LOGIN ROUTING POLICY: given a returning character, which world shard do
// we dial first? It lives here, not in cmd/telos-gate, so the integration tests exercise the SHIPPING
// resolver instead of a hand-synced twin that silently drifts from it.
//
// Cross-shard moves after login do not come through here — the destination address rides the Redirect
// frame. This is the first hop only.

// LoginLocator is the slice of the directory login routing needs. directory.Redis implements it.
type LoginLocator interface {
	PlayerPlacement(ctx context.Context, playerID string) (directory.Placement, error)
	ShardForZone(ctx context.Context, zoneID string) (string, error)
	EndpointForShard(ctx context.Context, shardID string) (string, error)
}

// loginResolveTimeout bounds the whole resolution (up to three Redis round trips). Without it a
// partitioned Redis wedges the per-connection login goroutine indefinitely, holding the socket open.
// Mirrors the world's 2s directory-read budget (internal/world/server.go).
const loginResolveTimeout = 3 * time.Second

// ResolveLoginShard picks the dial endpoint for characterID, in descending order of authority:
//
//  1. **The placement's ZONE, resolved to its CURRENT owner** (#320). The zone is the stable routing key.
//     A recorded shard id only says where the player *was*: the moment that zone is rebalanced onto
//     another shard, the id is stale. ShardForZone always names the live owner, so a rebalance that
//     happened while the player was offline is transparent.
//  2. **The placement's SHARD, but only for a LEGACY record** (one written before the zone field existed).
//     Deliberately NOT used when a zone IS recorded but did not resolve: that means the directory is
//     mid-rebalance or the owner's lease lapsed, and the recorded shard is precisely the shard that no
//     longer owns the zone. Dialing it lands the player on a shard that cannot host their durable
//     zone_ref, which start-rooms them (server.go) — the very loss #320 exists to prevent — or, if that
//     shard is draining, gets them disconnected outright (#324). Falling through is the safer guess.
//  3. **The home zone's owner.** A brand-new character, or a placement we could not act on.
//  4. **The configured world target.** A single-shard dev stack with no directory records at all.
//
// A returned endpoint is a dial address, not a shard id. ok=false means the gate should refuse the login.
func ResolveLoginShard(ctx context.Context, dir LoginLocator, characterID, homeZone, fallback string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, loginResolveTimeout)
	defer cancel()

	log := slog.With("component", "gate", "character", characterID)

	// 1 + 2: the player's own placement record. The world writes it whenever a player becomes resident in
	// a zone (internal/world/placement.go), so it exists for every character who has ever logged in — not,
	// as before #320, only for those who happened to be handed off across shards.
	if place, err := dir.PlayerPlacement(ctx, characterID); err == nil {
		if place.ZoneID != "" {
			if shardID, zerr := dir.ShardForZone(ctx, place.ZoneID); zerr == nil && shardID != "" {
				if endpoint, eerr := dir.EndpointForShard(ctx, shardID); eerr == nil && endpoint != "" {
					log.Debug("login routed by placement zone",
						"zone", place.ZoneID, "shard_id", shardID, "epoch", place.Epoch, "endpoint", endpoint)
					return endpoint, true
				}
			}
			// The zone is recorded but has no reachable owner right now. Do NOT fall back to place.ShardID
			// here (see the doc comment): it is the stale former owner by construction.
			log.Warn("placement zone has no reachable owner; falling back to the home zone",
				"zone", place.ZoneID, "stale_shard", place.ShardID)
		} else if place.ShardID != "" {
			// A legacy record: no zone was ever written for this player. Its shard id is the only routing
			// information we have, and it is correct as long as that shard still hosts them — the common
			// case. No backfill needed; the player's next login rewrites the record with a zone.
			if endpoint, eerr := dir.EndpointForShard(ctx, place.ShardID); eerr == nil && endpoint != "" {
				log.Debug("login routed by placement shard (legacy record, no zone)",
					"shard_id", place.ShardID, "epoch", place.Epoch, "endpoint", endpoint)
				return endpoint, true
			}
		}
	}

	// 3: the home zone's owner. Two hops: who owns the home zone, then where that shard is reachable.
	// A home-zone landing is not automatically a lost location — the destination still attaches the player
	// into whichever zone their durable zone_ref names, if it hosts it (server.go, #320 slice 1).
	shardID, err := dir.ShardForZone(ctx, homeZone)
	if err == nil && shardID != "" {
		endpoint, eerr := dir.EndpointForShard(ctx, shardID)
		if eerr == nil && endpoint != "" {
			log.Debug("login routed by home zone", "home_zone", homeZone, "shard_id", shardID, "endpoint", endpoint)
			return endpoint, true
		}
		err = eerr
	}

	// 4: the configured target, so a single-shard dev stack works without Redis at all.
	log.Debug("login routing fell through to the configured target",
		"home_zone", homeZone, "shard_id", shardID, "err", err, "fallback", fallback)
	if fallback == "" {
		return "", false
	}
	return fallback, true
}

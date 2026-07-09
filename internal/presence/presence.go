// Package presence is the cross-shard "who" roster (docs/PHASE8-PLAN.md slice 8.4, P8-D4). It is a
// best-effort, self-healing online list — NOT a routing source. Each world shard publishes a presence
// entry per RESIDENT player, with a per-player TTL refreshed by a batched heartbeat; `who` reads the
// whole roster across all shards. A crashed shard simply stops heartbeating, so its players' entries
// EXPIRE and age out of `who` with no explicit cleanup (the crashed-shard recovery story).
//
// # Why this is separate from the directory
//
// The directory (internal/directory) is the EPOCH-AUTHORITATIVE player->shard map: it answers "which
// shard owns this player right now" for tell routing and handoff, with a monotonic CAS. Presence is the
// opposite: a deliberately loose, TTL-leased roster that answers only "is this name in `who`". Conflating
// them is the P8-A4 bug — a stale presence entry must NEVER route a tell to a dead shard. So presence is
// its own package with its own store, mirroring the directory's Redis key/TTL/lease discipline (lease
// expiry == age-out) without sharing its authority.
//
// # Write authority (P8-A4 / the 8.1-carried security obligation)
//
// A shard writes ONLY its own residents' presence. The store enforces this: a Set/Remove names the
// CALLING shard, and a write to a key currently owned by a DIFFERENT, still-live shard is refused. So a
// shard (or a forged frame, were one possible) cannot mark an arbitrary player online or evict a real
// one — the write authority is "the shard that hosts the player", enforced at the store, not by trust.
package presence

import (
	"context"
	"errors"
	"time"
)

// ErrNotOwner is returned when a shard tries to Set/Remove a presence key that a DIFFERENT, still-live
// shard currently owns — the write-authority guard (P8-A4). A batched Set reports it once if ANY entry
// in the batch was refused; the owned/unowned entries in the same batch still apply.
var ErrNotOwner = errors.New("presence: key owned by another live shard")

// DefaultTTL is how long a presence entry survives without a heartbeat refresh. It is set to ~2x the
// directory's lease so a crashed shard's players age out of `who` within ~30s (OQ-2), while a live
// shard — heartbeating well within the directory's ~15s lease — never lets a resident's entry lapse.
const DefaultTTL = 30 * time.Second

// DefaultHeartbeat is the batched-refresh cadence: one pipelined write per shard per beat (NOT one per
// player per beat — the write rate is O(shards/interval), not O(players)). It is well within DefaultTTL
// so a live player's entry never flickers out of `who` between beats.
const DefaultHeartbeat = 10 * time.Second

// Entry is one player's presence as it appears in `who`: the display name, the shard that hosts them,
// the AFK flag (8.4 carries the field; the `afk` command is 8.6), a concealment bit, and a server-stamped
// last-seen. Concealed (#98) is set by the hosting shard when the player is invisible/hidden/wizinvis, so
// the cross-shard `who` reader (renderWho) can omit them from an ordinary viewer's roster — the roster
// counterpart to the zone-local canSee filter (which the cross-shard path could not previously honor because
// the Entry carried no concealment state).
type Entry struct {
	PlayerID  string
	Name      string
	ShardID   string
	AFK       bool
	Concealed bool
	// Channels is the player's effective hear-set (the sorted {enabled ∩ hearable} channel refs), carried so
	// the cross-shard roster is ALSO the per-channel membership source (#90): "who hears channel X" is List()
	// inverted by Channels. Written by the owning shard on the heartbeat, same as the rest of the entry.
	Channels []string
	LastSeen time.Time
}

// Roster is the cross-shard presence store. A shard SET/REMOVEs its own residents (write authority keyed
// by shardID) and LISTs the whole roster for `who`. Implementations: Redis (cross-process, TTL age-out)
// and Mem (hermetic, in-process — the two-shard test harness shares one Mem roster).
//
// All writes name the calling shard so the store can enforce write authority (P8-A4): a write to a key a
// DIFFERENT live shard owns is refused with ErrNotOwner.
type Roster interface {
	// Set writes/refreshes presence for every entry in this batch under the given TTL, in a single
	// pipelined round-trip (the batched heartbeat). The caller's own shardID is the write authority; the
	// store refuses any entry a different live shard currently owns (ErrNotOwner) but still applies the
	// entries the caller owns or that are unowned.
	Set(ctx context.Context, shardID string, entries []Entry, ttl time.Duration) error

	// Remove eagerly deletes playerID's presence on a clean quit/leave — but only if shardID still owns
	// the key (so a stale removal can't evict a player a newer shard has since taken over).
	Remove(ctx context.Context, shardID, playerID string) error

	// List returns every live (un-expired) presence entry across all shards — the `who` read.
	List(ctx context.Context) ([]Entry, error)
}

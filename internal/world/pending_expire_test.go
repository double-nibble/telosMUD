package world

import (
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// pending_expire_test.go — coverage for the pending-TTL reaper (handoff_server.go pendingTTL /
// Zone.pendingExpire), which had ZERO tests. A destination Prepare parks a PENDING player and indexes its
// handoff token; the gate then re-dials to BIND it. If that bind never arrives — the gate crashed mid-redial,
// or the re-dial to this shard failed after the source already committed ownership — the pendingExpireMsg TTL
// must REAP the pending player AND DROP its token. Otherwise the pending entity is a permanent phantom and its
// tokenIndex entry leaks (a memory leak at scale). These drive Zone.pendingExpire directly — the same
// direct-construction style as the freezeExpire tests — rather than waiting out the real 60s pendingTTL.

// newPendingPlayer parks a pending player in z the way prepare() does: a pending session carrying a handoff
// token, present in z.players, with the token indexed on the shard. Returns the session + its attachGen (the
// value a pendingExpireMsg carries to guard against expiring a since-rebound session).
func newPendingPlayer(t *testing.T, shard *Shard, z *Zone, name, token string) (*session, uint64) {
	t.Helper()
	out := make(chan *playv1.ServerFrame, 16)
	s := &session{character: name, out: out, epoch: 5, pending: true, token: token}
	z.newPlayerEntity(s, name)
	z.players[name] = s
	shard.indexToken(token, z)
	return s, s.attachGen
}

func TestPendingExpireReapsUnboundPlayerAndDropsToken(t *testing.T) {
	shard, z, _ := persistShard(t)
	s, gen := newPendingPlayer(t, shard, z, "Pending", "tok-reap")

	// Precondition: parked + indexed.
	if !s.pending || z.players["Pending"] == nil {
		t.Fatal("setup: player should be present and pending")
	}
	if shard.zoneForToken("tok-reap") != z {
		t.Fatal("setup: token should be indexed to the zone")
	}

	// The TTL fires and the gate never bound the player: reap it and drop the token.
	z.pendingExpire("Pending", gen)

	if z.players["Pending"] != nil {
		t.Error("an unbound pending player must be reaped when its TTL expires (phantom-entity leak)")
	}
	if shard.zoneForToken("tok-reap") != nil {
		t.Error("the expired pending player's token must be dropped from the index (tokenIndex leak)")
	}
}

func TestPendingExpireStaleGenIsNoOp(t *testing.T) {
	shard, z, _ := persistShard(t)
	_, gen := newPendingPlayer(t, shard, z, "Kept", "tok-keep")

	// A stale-gen expire (the pending player was rebound/rebuilt after the timer was armed) must NOT reap it —
	// the gen guard prevents a late timer from evicting a session that has since been activated.
	z.pendingExpire("Kept", gen+1)

	if z.players["Kept"] == nil {
		t.Error("a stale-gen pendingExpire must not reap a still-pending player (gen guard)")
	}
	if shard.zoneForToken("tok-keep") == nil {
		t.Error("a no-op stale-gen expire must leave the token indexed")
	}
}

func TestPendingExpireBoundPlayerIsNoOp(t *testing.T) {
	shard, z, _ := persistShard(t)
	s, gen := newPendingPlayer(t, shard, z, "Bound", "tok-bound")
	// The gate bound the player: attach clears `pending` on a successful bind.
	s.pending = false

	// A late TTL fire after a successful bind must NOT reap the now-live player (the pending guard).
	z.pendingExpire("Bound", gen)

	if z.players["Bound"] == nil {
		t.Error("a pendingExpire after the player was bound (pending=false) must be a no-op")
	}
}

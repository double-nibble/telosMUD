package world

import (
	"context"
	"sync/atomic"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// tier_test.go — #27 Slice 2: a fresh-login attach records the account trust tier (from the VERIFIED session
// assertion, threaded through attachMsg) onto the session, so Slice 3 can apply the matching flags on spawn.

func TestAttachRecordsVerifiedTier(t *testing.T) {
	shard, z, _ := persistShard(t)
	out := make(chan *playv1.ServerFrame, 64)
	var cz atomic.Pointer[Zone]
	loaded, loadedOK := shard.loadCharacterSnapshot(context.Background(), "Buildy")

	z.post(attachMsg{character: "Buildy", out: out, curZone: &cz, loaded: loaded, loadedOK: loadedOK, tier: "admin"})
	waitPlayer(t, z, "Buildy", true)

	if s := z.players["Buildy"]; s == nil || s.tier != "admin" {
		got := "<no session>"
		if s != nil {
			got = s.tier
		}
		t.Fatalf("session tier = %q, want admin (recorded from the verified assertion)", got)
	}
}

// TestAttachDefaultsTierToPlayer: with no tier claim (the dev/unverified path — attachMsg.tier == ""), the
// session tier is empty, which the flag application (Slice 3) treats as player. Fail-safe: no elevation
// without a signed claim.
func TestAttachDefaultsTierToPlayer(t *testing.T) {
	shard, z, _ := persistShard(t)
	out := make(chan *playv1.ServerFrame, 64)
	var cz atomic.Pointer[Zone]
	loaded, loadedOK := shard.loadCharacterSnapshot(context.Background(), "Plain")

	z.post(attachMsg{character: "Plain", out: out, curZone: &cz, loaded: loaded, loadedOK: loadedOK}) // no tier
	waitPlayer(t, z, "Plain", true)

	if s := z.players["Plain"]; s == nil || s.tier != "" {
		t.Fatalf("session tier should be empty (player) with no signed claim, got %q", s.tier)
	}
}

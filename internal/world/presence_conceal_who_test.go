package world

import (
	"strings"
	"testing"
	"time"

	roster "github.com/double-nibble/telosmud/internal/presence"
)

// presence_conceal_who_test.go — #98: the cross-shard `who` roster now carries a concealment bit, so an
// invisible/hidden/wizinvis player is omitted from another shard's `who` (the roster counterpart to the
// zone-local canSee filter, which the cross-shard path previously couldn't honor).

// TestConcealedForRoster pins the predicate that stamps the roster bit: any target-side concealment marks a
// player concealed; the viewer-side pierce senses and a plain player do not.
func TestConcealedForRoster(t *testing.T) {
	z, _, room := harmZone(t)
	p := harmPlayer(z, room, "P")

	if concealedForRoster(p) {
		t.Fatal("a plain player must not be roster-concealed")
	}
	for _, f := range []string{flagInvisible, flagHidden, flagWizinvis} {
		setFlag(p, f, true)
		if !concealedForRoster(p) {
			t.Fatalf("flag %q must mark the player roster-concealed", f)
		}
		setFlag(p, f, false)
	}
	// The viewer-side pierce senses are NOT concealment — they don't hide their bearer.
	for _, f := range []string{flagDetectInvis, flagSenseHidden, flagHolylight} {
		setFlag(p, f, true)
		if concealedForRoster(p) {
			t.Fatalf("viewer-side flag %q must NOT mark its bearer roster-concealed", f)
		}
		setFlag(p, f, false)
	}
}

// TestRenderWhoFiltersConcealed pins the render filter: a concealed Entry is omitted for an ordinary viewer
// and shown to a see-all (holylight) viewer; a visible Entry always shows.
func TestRenderWhoFiltersConcealed(t *testing.T) {
	entries := []roster.Entry{
		{PlayerID: "Alice", Name: "Alice"},
		{PlayerID: "Sneak", Name: "Sneak", Concealed: true},
	}
	ordinary := renderWho(entries, false)
	if !strings.Contains(ordinary, "Alice") {
		t.Fatalf("a visible player must appear for an ordinary viewer: %q", ordinary)
	}
	if strings.Contains(ordinary, "Sneak") {
		t.Fatalf("a concealed player must be omitted for an ordinary viewer: %q", ordinary)
	}
	seeAll := renderWho(entries, true)
	if !strings.Contains(seeAll, "Alice") || !strings.Contains(seeAll, "Sneak") {
		t.Fatalf("a holylight viewer must see every player, concealed included: %q", seeAll)
	}
}

// TestPresenceRepublishUpdatesConcealBit: a republish (the effect-op/wizinvis hook) re-stamps the roster bit
// from the player's current flags, so a mid-session invisibility toggle is reflected without a re-login.
func TestPresenceRepublishUpdatesConcealBit(t *testing.T) {
	tr := &presenceTracker{shardID: "s", roster: roster.NewMem(), residents: map[string]roster.Entry{}, ttl: roster.DefaultTTL}
	tr.eager = make(chan eagerOp, 8)

	tr.join("Bob", "Bob", false, false) // joins visible
	if tr.snapshot()[0].Concealed {
		t.Fatal("a visible join must not be roster-concealed")
	}
	tr.join("Bob", "Bob", false, true) // republish now-concealed
	if !tr.snapshot()[0].Concealed {
		t.Fatal("a republish must update the roster concealment bit")
	}
}

// TestCrossShardWhoOmitsConcealedPlayer is the end-to-end #98 done-when: an invisible player on one shard does
// not leak into another shard's `who`, while a visible player on that same shard does.
func TestCrossShardWhoOmitsConcealedPlayer(t *testing.T) {
	shared := roster.NewMem()
	za := presenceShard(t, shared, "shard-a")
	zb := presenceShard(t, shared, "shard-b")

	alice := joinPlayer(t, za, "Alice")

	// Bob joins shard-b already invisible (set before the entity is handed to the zone — race-free), so his
	// roster entry is stamped concealed at join.
	bob := newTestPlayerEntity(zb, "Bob")
	setFlag(bob.entity, flagInvisible, true)
	zb.post(joinMsg{s: bob})
	waitMarkup(t, bob, "The Temple Square")

	joinPlayer(t, zb, "Carol") // a VISIBLE control on the same shard

	// Once the visible control (Carol) shows up in Alice's cross-shard who, the roster has settled — the
	// invisible Bob must be absent from that very same list.
	deadline := time.After(2 * time.Second)
	for {
		who := waitWho(t, za, alice)
		if strings.Contains(who, "Carol") {
			if strings.Contains(who, "Bob") {
				t.Fatalf("an invisible player leaked into cross-shard who: %q", who)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("cross-shard who never listed the visible control (last: %q)", who)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

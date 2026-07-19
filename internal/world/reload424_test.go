package world

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// reload424_test.go — #424: the content-bus pack filter must fail CLOSED on an empty pack.
//
// The old rule was `inv.Pack != "" && !r.packs[inv.Pack]`: an invalidation naming NO pack was accepted by
// every shard whatever it loads. On an unsigned single-subject bus that was a knowledge-free fleet-wide
// primitive — and a DESTRUCTIVE one, because the definition re-read matches on (ref, pack), so an empty
// pack never resolves a row, comes back Found:false, and Found:false is the DELETION path.
//
// So these tests are not "is a filter applied". They drive the real bus → onInvalidation wiring and assert
// the DAMAGE does not happen: a prototype is not evicted, a channel is not unregistered, a zone is not
// reshaped. Per the #421 lesson, asserting the predicate alone would pass against a shard that never
// consults it.

// TestEmptyPackInvalidationDoesNotEvictAPrototype is the headline. Before the fix this evicted the item
// from every shard's cache fleet-wide, for the cost of one small unsigned message.
func TestEmptyPackInvalidationDoesNotEvictAPrototype(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer bus.Close()

	s := newReloadShard(t, src, bus)
	require.NotNil(t, s.protos.get("rt:obj:torch"), "precondition: the torch is cached")

	require.NoError(t, bus.Publish(context.Background(), contentbus.Invalidation{
		Kind: content.KindItem, Ref: "rt:obj:torch", Pack: "",
	}))

	// Give the subscription goroutine time to have done the damage if it were going to. A poll-until-gone
	// would be the wrong shape here: the assertion is that nothing EVER happens, so it has to be a settle.
	settleInvalidation(t, s)
	require.NotNil(t, s.protos.get("rt:obj:torch"),
		"a pack-less invalidation must not evict a prototype (the re-read misses and Found:false is the deletion path)")
	require.NotNil(t, s.Zone().spawn("rt:obj:torch"), "and the prototype must still spawn")
}

// TestEmptyPackChannelInvalidationDoesNotRemoveAChannel covers the second destructive path: a channel
// dropped from the registry stops resolving its verb AND drops live subscribers' subscriptions.
func TestEmptyPackChannelInvalidationDoesNotRemoveAChannel(t *testing.T) {
	pack := reloadTestPack()
	pack.Channels = []content.ChannelDTO{{Ref: "gossip", Name: "Gossip", Words: []string{"gossip"}}}
	src := content.NewMemSource()
	src.SetPack(pack)
	bus := contentbus.NewMemBus()
	defer bus.Close()

	s := newReloadShard(t, src, bus)
	require.NotNil(t, s.defs.channel.get("gossip"), "precondition: the channel is registered")

	require.NoError(t, bus.Publish(context.Background(), contentbus.Invalidation{
		Kind: content.KindChannel, Ref: "gossip", Pack: "",
	}))

	settleInvalidation(t, s)
	require.NotNil(t, s.defs.channel.get("gossip"),
		"a pack-less invalidation must not unregister a channel fleet-wide")
}

// TestEmptyPackZoneInvalidationDoesNotReconcile covers the shape-reconcile path — the most destructive of
// the three, since a reconcile TEARS DOWN rooms absent from the desired set.
//
// It asserts on the zone's INBOX rather than on the resulting room graph, deliberately. The fixture does
// not run the zone loop, so a posted message would never be applied and a room-set assertion would pass
// whether or not the invalidation was accepted — a vacuous test that still goes green when the filter is
// reverted. (It did, on the first draft.) Draining the inbox asserts the decision the reloader actually
// makes, and the positive control below proves the drain can see a reconcile when one is posted.
func TestEmptyPackZoneInvalidationDoesNotReconcile(t *testing.T) {
	pack := reloadTestPack()
	pack.Zones[0].Rooms = append(pack.Zones[0].Rooms, content.RoomDTO{
		Ref: "rt:room:cellar", Name: "The Cellar", Long: "A damp cellar.", Exits: map[string]string{},
	})
	src := content.NewMemSource()
	src.SetPack(pack)
	bus := contentbus.NewMemBus()
	defer bus.Close()

	s := newReloadShard(t, src, bus)
	z := s.Zone()

	// A desired set naming ONE of the two rooms: applied, this would tear the cellar down. A max version so
	// the last-writer-wins guard cannot be what saves us — only the pack filter can.
	zoneInv := contentbus.Invalidation{
		Kind: content.KindZone, Ref: "rt", Pack: "",
		Version: ^uint64(0), Rooms: []string{"rt:room:hall"}, StartRoom: "rt:room:hall",
	}
	require.NoError(t, bus.Publish(context.Background(), zoneInv))
	settleInvalidation(t, s)
	require.False(t, drainForReconcile(z), "a pack-less zone invalidation must not post a shape reconcile")

	// POSITIVE CONTROL: the same message naming the real pack MUST post one. Without this the test above
	// could pass because the drain is broken rather than because the filter works.
	zoneInv.Pack = "reloadtest"
	require.NoError(t, bus.Publish(context.Background(), zoneInv))
	settleInvalidation(t, s)
	require.True(t, drainForReconcile(z), "control: a properly-packed zone invalidation must post a reconcile")
}

// drainForReconcile empties z's inbox and reports whether a reconcileZoneMsg was among the messages. Safe
// to call only on a zone whose loop is NOT running (the fixtures here do not run it), which makes the test
// goroutine the single consumer.
func drainForReconcile(z *Zone) bool {
	found := false
	for {
		select {
		case m := <-z.inbox:
			if _, ok := m.(reconcileZoneMsg); ok {
				found = true
			}
		default:
			return found
		}
	}
}

// TestVersionCompleteSentinelIsStillAcceptedWithoutAPack pins the ONE legitimate exemption. The sentinel is
// content-less by construction and every shard must process it — it advances the applied-version cursor and
// is the only signal covering a zone DELETION. A later "tighten this further" must fail here, loudly,
// rather than silently disabling reconcile-on-join.
func TestVersionCompleteSentinelIsStillAcceptedWithoutAPack(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer bus.Close()

	s := newReloadShard(t, src, bus)
	require.NoError(t, contentbus.PublishVersionComplete(context.Background(), bus, 42))

	require.Eventually(t, func() bool { return s.reloader.appliedContentVersion.Load() == 42 },
		2*time.Second, time.Millisecond,
		"the pack-less version-complete sentinel must still be accepted by every shard")
}

// TestMalformedSentinelCarryingContentIsRejected makes the exemption STRUCTURAL rather than kind-based. A
// real sentinel carries kind+version and nothing else; one carrying a ref or a room set is not a sentinel,
// and accepting it would leave the exemption usable as a carrier for content a future refactor might start
// reading off the message.
func TestMalformedSentinelCarryingContentIsRejected(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer bus.Close()

	s := newReloadShard(t, src, bus)
	// Versions ABOVE the settle barrier below, deliberately: advanceApplied is monotone, so a malformed
	// sentinel stamped BELOW the barrier would be indistinguishable from one that was correctly rejected.
	const malformed = uint64(10_000_000)
	for _, inv := range []contentbus.Invalidation{
		{Kind: content.KindVersionComplete, Version: malformed, Ref: "rt"},
		{Kind: content.KindVersionComplete, Version: malformed, Rooms: []string{"rt:room:hall"}},
		{Kind: content.KindVersionComplete, Version: malformed, StartRoom: "rt:room:hall"},
		{Kind: content.KindVersionComplete, Version: malformed, Pack: "reloadtest"},
	} {
		require.NoError(t, bus.Publish(context.Background(), inv))
	}
	settleInvalidation(t, s)
	// settleInvalidation's barrier is monotone, so if a malformed sentinel WAS accepted the cursor sits at
	// `malformed` (or just above) and this names the defect directly, rather than the barrier timing out and
	// blaming the bus.
	require.EqualValues(t, settleVersion, s.reloader.appliedContentVersion.Load(),
		"a sentinel carrying content is not a sentinel and must not advance the applied-version cursor")
}

// TestAcceptsIsFailClosedForEveryKind is the re-widening regression catcher, and the reason `accepts` is a
// separate pure predicate at all. The rule it guards is one line, and one line is exactly what gets
// "simplified" back.
//
// Two properties make it hard to defeat by accident: the table's expectation for an empty pack is REJECT
// for every kind except the sentinel, and the length assertion below fails when someone adds a content kind
// without deciding what this filter should do with it.
func TestAcceptsIsFailClosedForEveryKind(t *testing.T) {
	// Iterate the REAL registry, not a literal copied into this file. The first version of this test
	// hand-wrote the kind list and then asserted its length — an assertion about a slice the same function
	// had just declared, which could never fire. Adding content.KindQuest to definition.go left it green.
	// Ranging content.AllKinds means a new kind lands in this loop automatically, and the classification
	// below has to be updated for it.
	r := &reloader{packs: map[string]bool{"loaded": true}}
	for _, kind := range content.AllKinds {
		sentinel := kind == content.KindVersionComplete

		// No pack at all: accepted ONLY for the content-less sentinel.
		require.Equal(t, sentinel, r.accepts(contentbus.Invalidation{Kind: kind}),
			"kind %q with an empty pack", kind)

		// A pack this shard loads: accepted for everything EXCEPT the sentinel, which must carry no pack.
		require.Equal(t, !sentinel, r.accepts(contentbus.Invalidation{Kind: kind, Ref: "x", Pack: "loaded"}),
			"kind %q with a loaded pack", kind)

		// A pack another shard loads but this one does not: always rejected. Keeping this in the table is
		// what stops the fix from being "accept everything".
		require.False(t, r.accepts(contentbus.Invalidation{Kind: kind, Ref: "x", Pack: "foreign"}),
			"kind %q with a foreign pack", kind)
	}
}

// TestAcceptsRejectsAnUnknownKind covers the whitelist. An unrecognised kind reaches the single-ref
// re-read, whose store dispatch returns Found:false for a kind it does not know — and Found:false is the
// DELETION path. So before the whitelist, a made-up kind naming a real ref in a real pack evicted that
// prototype fleet-wide, which is the same destructive primitive #424 is about, reached by a different door.
func TestAcceptsRejectsAnUnknownKind(t *testing.T) {
	r := &reloader{packs: map[string]bool{"loaded": true}}
	for _, kind := range []string{"", "quest", "totally-made-up", "ROOM", "room "} {
		require.False(t, r.accepts(contentbus.Invalidation{Kind: kind, Ref: "rt:obj:torch", Pack: "loaded"}),
			"kind %q is outside the closed wire vocabulary and must be rejected", kind)
	}
}

// TestUnknownKindInvalidationDoesNotEvictAPrototype drives that through the real bus wiring, because the
// predicate passing proves nothing about whether onInvalidation consults it.
func TestUnknownKindInvalidationDoesNotEvictAPrototype(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer bus.Close()

	s := newReloadShard(t, src, bus)
	require.NoError(t, bus.Publish(context.Background(), contentbus.Invalidation{
		Kind: "totally-made-up", Ref: "rt:obj:torch", Pack: "reloadtest",
	}))
	settleInvalidation(t, s)
	require.NotNil(t, s.protos.get("rt:obj:torch"),
		"an unknown kind must not reach the re-read, whose Found:false would evict the prototype")
}

// TestAcceptsDoesNotDependOnTheEnabledPackMapContents pins the explicit empty-pack check. r.packs is built
// by ranging the configured enabled-pack list, and an empty entry is reachable: a YAML
// `content_packs: [reference, ”]` survives config load (the env path drops empties, YAML does not). If
// accepts leaned on the map alone, that one authoring slip would silently restore the pre-#424 hole with
// nothing logged. The security property must not depend on config contents.
func TestAcceptsDoesNotDependOnTheEnabledPackMapContents(t *testing.T) {
	r := &reloader{packs: map[string]bool{"loaded": true, "": true}} // note the empty key
	for _, kind := range content.AllKinds {
		if kind == content.KindVersionComplete {
			continue // the content-less sentinel legitimately carries no pack
		}
		require.False(t, r.accepts(contentbus.Invalidation{Kind: kind, Ref: "x", Pack: ""}),
			"an empty pack must be rejected for kind %q even when \"\" is a key in the enabled-pack map", kind)
	}
}

// settleVersion is the FIRST barrier version. Each call publishes a STRICTLY HIGHER one, which is
// load-bearing rather than cosmetic: appliedContentVersion is monotone, so re-using one version would make
// the second settle observe the cursor already at its target and return WITHOUT waiting — a barrier that
// silently stops being a barrier the second time it is used. (That is exactly how the first draft of the
// zone test's positive control failed, which is the argument for having written the control at all.)
const settleVersion = uint64(999_999)

func settleInvalidation(t *testing.T, s *Shard) {
	t.Helper()
	want := s.reloader.appliedContentVersion.Load() + 1
	if want < settleVersion {
		want = settleVersion
	}
	require.NoError(t, contentbus.PublishVersionComplete(context.Background(), s.reloader.bus, want))
	require.Eventually(t, func() bool { return s.reloader.appliedContentVersion.Load() == want },
		2*time.Second, time.Millisecond, "bus subscription goroutine did not drain")
}

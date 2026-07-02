package gate

// comms_journey_test.go is the black-box (through the gate, player-visible) test set for Phase-8
// slice 8.2 — the gate-side comms client + the writer-path producer (docs/PHASE8-PLAN.md 8.2). It
// drives the real connection lifecycle (login → in-world → disconnect, and a cross-shard handoff)
// against a shared commbus.MemBus whose WORLD handle stands in for the source world, and asserts the
// three done-whens:
//
//   - subscription lifecycle: a synthetic comms message reaches a connected gate's socket, and the
//     subscription is torn down on disconnect (no leak).
//   - HANDOFF-COMMS-TRANSPARENCY (the central P8-D1 proof): a player walks A→B (a real cross-shard
//     handoff = a gate re-dial) and a comms message published AFTER the handoff still reaches their
//     socket — the subscription lives on the connection and survives the re-dial untouched.
//   - slow-consumer-no-stall: a blocked/slow socket does not stall comms delivery to a sibling.
//
// The world publisher is a commbus.MemBus WORLD handle; the gate is wired with the SAME MemBus's GATE
// handle (RoleGate, subscribe-only) — exactly the role split cmd/telos-gate wires via commbus.OpenGate.

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/directory"
)

// publishSyntheticTell publishes a synthetic comms message to a player's personal tell subject over a
// WORLD-role bus (the only role allowed to publish chan/tell — the impersonation gate). It stands in
// for the source world's tell-drain emit: as of slice 8.5 the world renders the FULL line into Body
// ("X tells you, '…'") and the gate writes it VERBATIM (a pure sink), so this helper renders the same
// full line here. The author is engine-set on the message (as a real source world sets it from the live
// *Entity; P8-A2).
func publishSyntheticTell(t *testing.T, world commbus.Bus, target, author, body string, seq uint64) {
	t.Helper()
	msg := commbus.Message{
		AuthorID:       author,
		AuthorName:     author,
		Seq:            seq,
		IdempotencyKey: commbus.NewIdempotencyKey(author, seq),
		Body:           author + " tells you, '" + body + "'", // the world renders the full line; the gate is a verbatim sink
	}
	if err := world.Publish(context.Background(), commbus.TellSubject(target), msg); err != nil {
		t.Fatalf("world publish to %s: %v", commbus.TellSubject(target), err)
	}
}

// TestCommsReachesSocketAndUnsubscribesOnDisconnect is the subscription-lifecycle done-when: a comms
// message published by the world reaches a connected player's socket (the producer renders onto the
// existing writer path), and on disconnect the gate unsubscribes so a later publish reaches NOBODY (no
// leak). The "no leak" half is observable: after the player closes, a second publish must NOT panic /
// must reach no subscriber — we assert it by confirming the connection's render path is gone (the
// socket is closed, and the gate's handle goroutine has returned, which is where the defer cc.close()
// ran).
func TestCommsReachesSocketAndUnsubscribesOnDisconnect(t *testing.T) {
	const addr = "addr-a"
	worldBus, gateBus := commbus.NewWorldBus() // shared core; world publishes, gate subscribes
	t.Cleanup(func() { _ = worldBus.Close() })

	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGateWithComms(directory.Static{Addr: addr}, gateBus)

	term := h.dial(t)
	term.login(t, "Alice")
	term.expect(t, "Temple Square") // in-world: openComms has run (it precedes the re-dial loop)

	// The world publishes a tell to Alice; the gate's comms client must render it onto her socket.
	publishSyntheticTell(t, worldBus, "Alice", "Bob", "across the shards", 1)
	term.expect(t, "Bob tells you, 'across the shards'")

	// Disconnect: the gate's deferred cc.close() unsubscribes. After the handle goroutine returns,
	// the subscription is gone — a later publish has no subscriber to reach.
	term.close(t)

	// No-leak: a publish AFTER teardown must be a clean no-op (no panic, no goroutine left rendering
	// to a closed socket). We cannot observe "nobody received it" through a closed socket, so we assert
	// the publish itself is harmless and that re-login gets a FRESH subscription (the old one did not
	// linger and double-deliver).
	publishSyntheticTell(t, worldBus, "Alice", "Bob", "after logout", 2)

	term2 := h.dial(t)
	term2.login(t, "Alice")
	term2.expect(t, "Temple Square")
	// The post-logout message (seq 2) must NOT arrive on the new session — it was published while no
	// one was subscribed (transient at-most-once), so a fresh login starts clean. A NEW publish does
	// arrive, proving the new session's subscription is live and independent of the old one.
	publishSyntheticTell(t, worldBus, "Alice", "Bob", "fresh session", 3)
	term2.expect(t, "Bob tells you, 'fresh session'")
	if got := term2.acc.String(); strings.Contains(got, "after logout") {
		t.Fatalf("a message published while logged out leaked into the new session: %q", got)
	}
	term2.close(t)
}

// TestCommsTransparentAcrossHandoff is THE central P8-D1 proof: comms is transparent across a
// cross-shard handoff. A player walks A→B (a real handoff = a gate re-dial of the Play stream). A comms
// message published AFTER the handoff still reaches the player's socket — because the comms
// subscription lives on the CONNECTION (established in handle, outside the re-dial loop) and the
// re-dial (runStream) never touches it. A regression that moved the subscription into the per-shard
// stream loop would tear it down on the A→B re-dial and this publish would never land.
func TestCommsTransparentAcrossHandoff(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")

	ctx := context.Background()
	for _, sh := range []struct{ id, addr string }{{"shard-a", "addr-a"}, {"shard-b", "addr-b"}} {
		if err := dir.RegisterShard(ctx, sh.id, sh.addr, directory.DefaultShardLease); err != nil {
			t.Fatal(err)
		}
	}
	if err := dir.RegisterZone(ctx, "midgaard", "shard-a"); err != nil {
		t.Fatal(err)
	}
	if err := dir.RegisterZone(ctx, "darkwood", "shard-b"); err != nil {
		t.Fatal(err)
	}

	worldBus, gateBus := commbus.NewWorldBus()
	t.Cleanup(func() { _ = worldBus.Close() })

	h := newHarness(t)
	h.addShard("darkwood", "addr-b", dir, nil)
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		if addr != "addr-b" {
			return nil, errUnknownShard(addr)
		}
		return h.dialHandoff("addr-b")
	}
	h.addShard("midgaard", "addr-a", dir, peers)
	h.serveGateWithComms(homeZoneDir{redis: dir, zone: "midgaard"}, gateBus)

	term := h.dial(t)
	term.login(t, "Walker")
	term.expect(t, "Temple Square") // live on A; the comms subscription is established

	// A comms message BEFORE the handoff arrives (sanity: the subscription is live on shard A).
	publishSyntheticTell(t, worldBus, "Walker", "Bob", "before the walk", 1)
	term.expect(t, "Bob tells you, 'before the walk'")

	// Walk A→B: temple → market → (cross-shard) darkwood. The gate catches the Redirect and re-dials
	// B; this is the handoff the comms subscription must survive untouched.
	term.send(t, "north")
	term.expect(t, "Market Square")
	term.send(t, "north")
	term.expect(t, "Moonlit Grove") // re-dialed B: the player now lives on shard B

	// THE PROOF: publish AFTER the handoff. The connection-scoped subscription survived the re-dial,
	// so this still reaches Walker's socket even though their zone (and shard) moved.
	publishSyntheticTell(t, worldBus, "Walker", "Bob", "after the walk", 2)
	term.expect(t, "Bob tells you, 'after the walk'")

	term.close(t)
}

// TestCommsSlowConsumerDoesNotStallSibling is the edge slow-consumer done-when: a blocked/slow socket
// does not stall comms delivery to a sibling connection. One player's terminal stops reading (its byte
// reader is paused), so its socket write back-pressures; a second player's comms must still arrive
// promptly. The bus's bounded per-subscription buffer (drop-on-full) + the per-subscription delivery
// goroutine mean the slow socket stalls only its OWN delivery goroutine, never the fan-out to siblings.
func TestCommsSlowConsumerDoesNotStallSibling(t *testing.T) {
	const addr = "addr-a"
	worldBus, gateBus := commbus.NewWorldBus()
	t.Cleanup(func() { _ = worldBus.Close() })

	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGateWithComms(directory.Static{Addr: addr}, gateBus)

	slow := h.dial(t)
	slow.login(t, "Slow")
	slow.expect(t, "Temple Square")

	fast := h.dial(t)
	fast.login(t, "Fast")
	fast.expect(t, "Temple Square")

	// Stall the slow player's socket: pause its byte reader so the gate's writes to it block once the
	// pipe buffer fills. The slow terminal's comms-delivery goroutine will park on tc.Write.
	slow.pauseReader()

	// Flood the slow player with comms (more than the OS pipe buffer can absorb) so its delivery
	// goroutine is firmly blocked on Write.
	for i := uint64(1); i <= 64; i++ {
		publishSyntheticTell(t, worldBus, "Slow", "Bob", "stuck", i)
	}

	// The fast player's comms must still arrive promptly — its delivery is independent of the slow
	// socket. If a single slow socket stalled the fan-out, this would time out.
	publishSyntheticTell(t, worldBus, "Fast", "Bob", "still flowing", 1)
	fast.expect(t, "Bob tells you, 'still flowing'")

	// Resume the slow reader and clean up so no goroutine is left blocked at teardown.
	slow.resumeReader()
	fast.close(t)
	slow.close(t)
}

// TestGateCommsRoleIsGate is the wiring assertion the security review folds in (PHASE8-PLAN 8.1
// review / 8.2 obligation): the gate's comms bus is a GATE-role handle, structurally subscribe-only on
// chan/tell. It mirrors what cmd/telos-gate guarantees by calling commbus.OpenGate (never OpenWorld /
// never MemBus.WorldHandle()): a gate handed a world handle would let a forged client author a
// channel/tell line and defeat the impersonation gate. We assert the role on the handle the gate is
// wired with, and that a gate handle's publish on a chan/tell subject is refused.
func TestGateCommsRoleIsGate(t *testing.T) {
	_, gateBus := commbus.NewWorldBus()
	t.Cleanup(func() { _ = gateBus.Close() })

	srv := newServer(":0", directory.Static{Addr: "addr-a"}, newPool(), gateBus)
	if got := srv.comms.Role(); got != commbus.RoleGate {
		t.Fatalf("gate comms role = %v, want %v (the gate must be subscribe-only on chan/tell)", got, commbus.RoleGate)
	}
	// Defense in depth: a gate handle's publish on a chan/tell subject is refused (the impersonation
	// gate). This is what makes a gate handed a world handle a detectable wiring bug.
	err := srv.comms.Publish(context.Background(), commbus.TellSubject("Victim"), commbus.Message{AuthorName: "Forger"})
	if err != commbus.ErrPublishForbidden {
		t.Fatalf("gate publish on a tell subject = %v, want ErrPublishForbidden", err)
	}
}

// TestCommsUnavailableNoticeAtLogin pins the #61 notice: when comms are CONFIGURED (commsExpected)
// but the bus is unavailable (a down/Disabled broker), the player gets a one-line "chat is offline"
// notice at login and still enters the world normally. The complement — an AVAILABLE bus emits NO
// notice — guards against nagging a healthy gate.
func TestCommsUnavailableNoticeAtLogin(t *testing.T) {
	const notice = "chat is currently offline"

	t.Run("configured but down -> notice", func(t *testing.T) {
		const addr = "addr-down"
		h := newHarness(t)
		h.addShard("midgaard", addr, nil, nil)
		// A Disabled gate bus is exactly what OpenGate returns when a configured broker is unreachable.
		h.serveGateWithComms(directory.Static{Addr: addr}, commbus.Disabled(commbus.RoleGate))
		h.srv.WithCommsExpected(true)

		term := h.dial(t)
		term.login(t, "Alice")
		term.expect(t, notice)
		term.expect(t, "Temple Square") // the notice does not block entering the world
	})

	t.Run("bus available -> no notice", func(t *testing.T) {
		const addr = "addr-up"
		worldBus, gateBus := commbus.NewWorldBus() // an available (in-process) gate handle
		t.Cleanup(func() { _ = worldBus.Close() })
		h := newHarness(t)
		h.addShard("midgaard", addr, nil, nil)
		h.serveGateWithComms(directory.Static{Addr: addr}, gateBus)
		h.srv.WithCommsExpected(true) // configured AND up: still no notice

		term := h.dial(t)
		term.login(t, "Bob")
		// The notice (if any) is written before the world's room output, so once "Temple Square"
		// is in the buffer a notice would be too — its absence here is race-free.
		term.expect(t, "Temple Square")
		if strings.Contains(term.acc.String(), notice) {
			t.Fatalf("an available comms bus emitted the offline notice: %q", term.acc.String())
		}
	})
}

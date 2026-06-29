package gate

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/directory"
)

// handoff_state_test.go is the Wave-2 BLACK-BOX (through the gate) coverage for two
// distributed-correctness GAPs flagged in docs/TEST-COVERAGE.md:
//   - cross-shard handoff preserves player state + INPUT CONTINUITY (the ack_input_seq /
//     doReplay exactly-once-and-ordered path), and
//   - handoff INTERRUPTED mid-move: the destination is unreachable, and the player must end up
//     cleanly in EXACTLY ONE place (back on the source), not lost / duplicated / stuck frozen.
//
// Both reuse the existing two-shard miniredis harness pattern (handoff_integration_test.go);
// nothing about the multi-shard plumbing is reinvented.

// twoShardDir is the shared two-shard directory setup (midgaard->shard-a, darkwood->shard-b)
// every cross-shard test here needs. It returns the directory and registers cleanup of redis.
func twoShardDir(t *testing.T) *directory.Redis {
	t.Helper()
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
	return dir
}

// TestCrossShardHandoffInputContinuity pins INPUT CONTINUITY across a cross-shard handoff at the
// GATE level, from the PLAYER's seat: a player keeps issuing commands AFTER crossing the boundary,
// and every one must land on the DESTINATION shard, in order, with no loss and no spurious echo —
// the player never sees the handoff. It is the player-visible, black-box COMPANION to the world
// unit test TestCrossShardHandoff (handoff_test.go), which crafts raw input seqs to prove the
// EXACTLY-ONCE dedup MECHANISM; this test owns no seqs (the gate mints them) and asserts the
// OBSERVABLE instead — an ordered far-side burst arrives intact on B and the directory placement
// transferred.
//
// SCOPE NOTE (honest): the in-process gate timing forwards a post-move input live rather than
// reliably buffering-during-freeze, so this tier does NOT reproduce (and does not claim to catch) a
// replay/dedup-fence regression — that is the unit test's job, verified there. What this tier
// uniquely guards is the END-TO-END player experience: after the gate's self-driven re-dial, the
// player is live on B and a sequence of commands round-trips in order on the new shard.
func TestCrossShardHandoffInputContinuity(t *testing.T) {
	dir := twoShardDir(t)

	h := newHarness(t)
	h.addShard("darkwood", "addr-b", dir, nil) // destination up first so A can reach its Handoff service
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		if addr != "addr-b" {
			return nil, errUnknownShard(addr)
		}
		return h.dialHandoff("addr-b")
	}
	h.addShard("midgaard", "addr-a", dir, peers)
	h.serveGate(homeZoneDir{redis: dir, zone: "midgaard"})

	term := h.dial(t)
	term.login(t, "Burst")
	term.expect(t, "Temple Square")

	// A distinct say BEFORE the boundary (lands on A) — proves the pre-handoff session is live.
	term.send(t, "say pre-a")
	term.expect(t, "You say, 'pre-a'")

	// Cross to market, then north across the shard boundary into darkwood. The gate handles the
	// Redirect on its own (re-dial B with the token); the player just keeps typing.
	term.send(t, "north") // temple -> market (still on A)
	term.expect(t, "Market Square")
	term.send(t, "north")           // market -> darkwood: the cross-shard handoff fires
	term.expect(t, "Moonlit Grove") // gate re-dialed B; activation look landed -> we are LIVE on B

	// A burst of distinct, ORDERED commands on the far side. Each must echo, in order, on B.
	for _, word := range []string{"far-1", "far-2", "far-3"} {
		term.send(t, "say "+word)
		term.expect(t, "You say, '"+word+"'")
	}

	// ORDER + NO-LOSS: the three far-side echoes appear in send order in the transcript, each exactly
	// once. A lost input (the command never reached B) or a reorder fails here.
	acc := term.acc.String()
	i1, i2, i3 := indexAfter(acc, "You say, 'far-1'", 0), 0, 0
	if i1 < 0 {
		t.Fatalf("far-1 echo missing on the destination; transcript:\n%s", acc)
	}
	i2 = indexAfter(acc, "You say, 'far-2'", i1)
	if i2 < 0 {
		t.Fatalf("far-2 echo missing or out of order after far-1; transcript:\n%s", acc)
	}
	i3 = indexAfter(acc, "You say, 'far-3'", i2)
	if i3 < 0 {
		t.Fatalf("far-3 echo missing or out of order after far-2; transcript:\n%s", acc)
	}
	for _, word := range []string{"far-1", "far-2", "far-3"} {
		if n := countOccurrences(acc, "You say, '"+word+"'"); n != 1 {
			t.Fatalf("far-side echo %q appeared %d times, want exactly 1 (replay double-apply / dedup regression); transcript:\n%s", word, n, acc)
		}
	}

	// And the directory records the player on shard-b at the bumped epoch (placement crossed the seam).
	place, err := dir.PlayerPlacement(context.Background(), "Burst")
	if err != nil {
		t.Fatalf("placement after handoff: %v", err)
	}
	if place.ShardID != "shard-b" {
		t.Fatalf("placement = %+v, want shard-b (the move did not transfer ownership)", place)
	}

	term.close(t)
}

// TestHandoffInterruptedDestinationUnreachable pins the INTERRUPTED-HANDOFF contract (the flagged
// GAP): the destination shard's Handoff service is UNREACHABLE when the player tries to cross, so
// the source-side handoff cannot even Prepare. The player must NOT be lost, duplicated, or stuck
// frozen — they must be cleanly thawed and RESTORED to the room they tried to leave, on the SAME
// (source) shard, still live and in EXACTLY ONE place.
//
// We inject the failure at the source's peer dialer: dialing addr-b returns an error, so
// beginHandoff takes its fail("destination unreachable") path (world.go), which posts
// handoffFailMsg -> the source zone thaws + re-Moves the player into the source room and sends
// "The way is barred." This is the deterministic interruption (no timeout wait): the dialer fails
// immediately. We REPRODUCE the crossing attempt, then assert the clean single-place outcome.
//
// CONTRACT pinned: a destination-unreachable cross-shard move is a NON-EVENT for the player's
// location — they stay put with a "barred" notice, never half-transferred. (Contrast the
// redirect-target-unreachable case below, where Prepare SUCCEEDED and ownership already moved.)
func TestHandoffInterruptedDestinationUnreachable(t *testing.T) {
	dir := twoShardDir(t)

	h := newHarness(t)
	// Destination B is NOT served (or, equivalently, its dialer errors). We only bring up A, and
	// give A a peer dialer that always fails to reach B — the interruption.
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		return nil, fmt.Errorf("injected: destination shard %q unreachable", addr)
	}
	h.addShard("midgaard", "addr-a", dir, peers)
	h.serveGate(homeZoneDir{redis: dir, zone: "midgaard"})

	term := h.dial(t)
	term.login(t, "Stranded")
	term.expect(t, "Temple Square")
	term.send(t, "north") // temple -> market (still on A)
	term.expect(t, "Market Square")

	// Attempt the cross-shard move. The handoff cannot reach B, so the player is thawed + restored.
	term.send(t, "north") // market -> darkwood: the handoff FAILS to initiate
	term.expect(t, "The way is barred.")

	// CLEAN SINGLE PLACE: the player is restored to the room they tried to leave (Market Square) and
	// is LIVE there — a command round-trips. A lost/frozen player would not respond; a duplicated one
	// would (this asserts the recovery, not the duplication directly — the directory check below
	// covers duplication of OWNERSHIP).
	term.send(t, "say still on a")
	term.expect(t, "You say, 'still on a'")

	// And they can still move WITHIN shard A — proving they were truly thawed, not soft-locked.
	term.send(t, "south") // market -> temple
	term.expect(t, "Temple Square")

	// The directory must NOT have transferred ownership to B: a failed Prepare never reaches the
	// SetPlayerShard CAS, so the player either has no placement yet or is still on shard-a — never
	// shard-b. This is the "no duplication / no phantom ownership" half of the contract.
	place, err := dir.PlayerPlacement(context.Background(), "Stranded")
	if err == nil && place.ShardID == "shard-b" {
		t.Fatalf("a FAILED handoff still transferred directory ownership to shard-b (phantom placement): %+v", place)
	}

	term.close(t)
}

// TestCrossShardHandoffCarriesWornGear is the PLAYER-VISIBLE regression for the live bug the user hit:
// pick up + equip gear, cross a shard boundary, and the gear is GONE on the far side (`equip` showed
// "Nothing"). It reproduces that exact journey end-to-end through the gate — get/wear/wield on shard A,
// walk north across the seam into shard B, and assert the worn items survived — pinning the
// full-state-carry fix (handoff.go buildSnapshot StateJson → zone.go prepare). Before the fix
// buildSnapshot carried only identity, so equipment/inventory were rebuilt EMPTY on the destination.
//
// The demo pack resets the helmet (head slot) and the steel longsword (wield slot) onto the MARKET
// floor — one room north of the temple on shard A — and the market→north exit crosses into darkwood on
// shard B, so the whole journey is on the natural path with no content scaffolding.
//
// FALSE-POSITIVE GUARD: the gate transcript (term.acc) is cumulative, so a pre-cross `equipment` listing
// the worn items would trivially satisfy a post-cross Contains. We instead COUNT occurrences — the
// baseline is occurrence #1, the post-cross listing must reach occurrence #2 — and add a FUNCTIONAL
// clincher (remove + drop on B succeed, and darkwood's grove has no such floor items) so the test only
// passes if the gear is genuinely present on the destination, not merely echoed once and lost.
func TestCrossShardHandoffCarriesWornGear(t *testing.T) {
	dir := twoShardDir(t)

	h := newHarness(t)
	h.addShard("darkwood", "addr-b", dir, nil) // destination up first so A can reach its Handoff service
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		if addr != "addr-b" {
			return nil, errUnknownShard(addr)
		}
		return h.dialHandoff("addr-b")
	}
	h.addShard("midgaard", "addr-a", dir, peers)
	h.serveGate(homeZoneDir{redis: dir, zone: "midgaard"})

	term := h.dial(t)
	term.login(t, "Geared")
	term.expect(t, "Temple Square")

	// The helmet + longsword reset onto the MARKET floor, one room north on shard A.
	term.send(t, "north")
	term.expect(t, "Market Square")

	// Pick up + equip a WORN (head) and a WIELDED item.
	term.send(t, "get helmet")
	term.expect(t, "You get an iron helmet.")
	term.send(t, "wear helmet")
	term.expect(t, "You wear an iron helmet on your head.")
	term.send(t, "get sword")
	term.expect(t, "You get a steel longsword.")
	term.send(t, "wield sword")
	term.expect(t, "You wield a steel longsword.")

	// BASELINE (pre-cross): equipment lists both worn items — occurrence #1 of each.
	term.send(t, "equipment")
	term.expectCount(t, "<head> an iron helmet", 1)
	term.expectCount(t, "<wielded> a steel longsword", 1)

	// Cross the seam: market→north is darkwood:grove on shard-b → the cross-shard handoff fires and the
	// gate self-re-dials B. The player just keeps typing.
	term.send(t, "north")
	term.expect(t, "Moonlit Grove") // activation look on B landed → we are LIVE on the destination

	// REGRESSION: the worn gear MUST have crossed. equipment on B lists both AGAIN — occurrence #2.
	// Before the full-state-carry fix this said "Nothing." Counting to 2 makes the stale pre-cross line
	// in the cumulative transcript unable to satisfy this post-cross assertion.
	term.send(t, "equipment")
	term.expectCount(t, "<head> an iron helmet", 2)
	term.expectCount(t, "<wielded> a steel longsword", 2)

	// FUNCTIONAL clincher: the items are really HERE, not just re-echoed. remove unslots the helmet and
	// drop puts the sword on darkwood's floor — both succeed ONLY if the gear actually transferred (the
	// grove has no helmet/sword of its own to satisfy these).
	term.send(t, "remove helmet")
	term.expect(t, "You stop using an iron helmet.")
	term.send(t, "drop sword")
	term.expect(t, "You drop a steel longsword.")

	term.close(t)
}

// indexAfter returns the index of the first occurrence of sub in s at or after `from`, or -1.
// Used to assert ORDERING of echoes in the accumulated transcript.
func indexAfter(s, sub string, from int) int {
	if from < 0 || from > len(s) {
		return -1
	}
	idx := indexOf(s[from:], sub)
	if idx < 0 {
		return -1
	}
	return from + idx
}

// indexOf is a tiny strings.Index (kept local so the assertion reads as intent, not stdlib noise).
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// countOccurrences counts non-overlapping occurrences of sub in s (exactly-once assertions).
func countOccurrences(s, sub string) int {
	if sub == "" {
		return 0
	}
	n, i := 0, 0
	for {
		j := indexOf(s[i:], sub)
		if j < 0 {
			return n
		}
		n++
		i += j + len(sub)
	}
}

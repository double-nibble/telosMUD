package directory

import (
	"context"
	"testing"
	"time"
)

// TestReserveDrainTargetCountingAndCeiling pins the #41 counting reservation: reservations SUM per target,
// a reserve that would exceed the caller's headroom is refused, and a release frees that drainer's hold.
func TestReserveDrainTargetCountingAndCeiling(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()
	const target, ttl = "shard-b", 30 * time.Second

	// First drainer reserves 60 of a 100 headroom: admitted.
	ok, err := d.ReserveDrainTarget(ctx, target, "shard-a", 100, 60, ttl)
	if err != nil || !ok {
		t.Fatalf("first reserve = %v, %v; want true", ok, err)
	}
	// Second drainer wants 60 more, but 60+60 > 100: refused (would overload the target).
	ok, err = d.ReserveDrainTarget(ctx, target, "shard-c", 100, 60, ttl)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("second reserve admitted past the ceiling; want refused (60+60 > 100)")
	}
	// A smaller reserve that fits the remaining headroom is admitted (40 fits under 100).
	ok, err = d.ReserveDrainTarget(ctx, target, "shard-c", 100, 40, ttl)
	if err != nil || !ok {
		t.Fatalf("fitting reserve = %v, %v; want true (60+40 == 100)", ok, err)
	}
	// Release the first drainer's 60; now a fresh 60 fits again (40 + 60 == 100).
	if err := d.ReleaseDrainTarget(ctx, target, "shard-a"); err != nil {
		t.Fatal(err)
	}
	ok, err = d.ReserveDrainTarget(ctx, target, "shard-d", 100, 60, ttl)
	if err != nil || !ok {
		t.Fatalf("post-release reserve = %v, %v; want true (40 + 60 == 100)", ok, err)
	}
}

// TestReserveDrainTargetNonPositiveHeadroom: a target already at/over its ceiling (headroom <= 0) refuses
// any reservation — the caller then re-selects or proceeds over the soft ceiling.
func TestReserveDrainTargetNonPositiveHeadroom(t *testing.T) {
	d := newTestRedis(t)
	ok, err := d.ReserveDrainTarget(context.Background(), "shard-b", "shard-a", 0, 1, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("reserve admitted with zero headroom; want refused")
	}
}

// TestDrainingMarker pins SetDraining/ListDraining/ClearDraining — the drain-target selector uses it to
// exclude a peer that is itself draining.
func TestDrainingMarker(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if set, err := d.ListDraining(ctx); err != nil || len(set) != 0 {
		t.Fatalf("ListDraining on empty = %v, %v; want {}", set, err)
	}
	if err := d.SetDraining(ctx, "shard-a", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.SetDraining(ctx, "shard-b", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	set, err := d.ListDraining(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !set["shard-a"] || !set["shard-b"] || len(set) != 2 {
		t.Fatalf("ListDraining = %v; want {shard-a, shard-b}", set)
	}
	if err := d.ClearDraining(ctx, "shard-a"); err != nil {
		t.Fatal(err)
	}
	set, _ = d.ListDraining(ctx)
	if set["shard-a"] || !set["shard-b"] || len(set) != 1 {
		t.Fatalf("after clear: ListDraining = %v; want {shard-b}", set)
	}
}

// TestReserveDrainTargetExpiresAStaleHoldPerField is the #284 fix, and the scenario is the fleet rollout the
// reservation guard exists for.
//
// The old shape stored a bare count and refreshed the WHOLE key's PEXPIRE on every reserve. So a drainer that
// crashed mid-drain left a hold that never expired, as long as OTHER drainers kept reserving onto the same
// hot target: the reserved sum stayed inflated, the target refused real reservations, and drainers spilled to
// the soft-ceiling fallback. The guard was weakest exactly under the concurrency it is for.
//
// Now each field carries its own expiry, so the crashed drainer's hold stops counting after ITS ttl no matter
// what its peers do to the key.
func TestReserveDrainTargetExpiresAStaleHoldPerField(t *testing.T) {
	d, mr := newTestRedisWithClock(t)
	ctx := context.Background()
	const target = "shard-b"
	const ttl = 15 * time.Second

	base := time.Unix(1_700_000_000, 0)
	mr.SetTime(base)

	// A drainer reserves 60 of a 100 headroom, then crashes (never releases).
	if ok, err := d.ReserveDrainTarget(ctx, target, "crashed", 100, 60, ttl); err != nil || !ok {
		t.Fatalf("crashed drainer's reserve: ok=%v err=%v", ok, err)
	}
	// A live peer keeps reserving onto the same target, refreshing the whole key's TTL under the old shape.
	for i := 1; i <= 3; i++ {
		mr.SetTime(base.Add(time.Duration(i) * 5 * time.Second))
		if ok, err := d.ReserveDrainTarget(ctx, target, "live", 100, 10, ttl); err != nil || !ok {
			t.Fatalf("live peer reserve %d: ok=%v err=%v", i, ok, err)
		}
	}

	// Past the crashed drainer's OWN ttl, its 60 must stop counting — even though the key is alive and the
	// live peer holds 30. A 60-player reserve now fits under the 100 ceiling (30 + 60 <= 100).
	mr.SetTime(base.Add(ttl + time.Second))
	ok, err := d.ReserveDrainTarget(ctx, target, "newcomer", 100, 60, ttl)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a crashed drainer's stale hold still counted against the ceiling — a live peer's reserves " +
			"were refreshing the whole key's TTL, which is exactly the #284 inflation")
	}
}

// TestReserveDrainTargetRefreshesItsOwnFieldExpiry: a drainer that is demonstrably alive (it keeps reserving)
// must not have its own hold expire out from under it mid-drain.
func TestReserveDrainTargetRefreshesItsOwnFieldExpiry(t *testing.T) {
	d, mr := newTestRedisWithClock(t)
	ctx := context.Background()
	const target = "shard-b"
	const ttl = 10 * time.Second

	base := time.Unix(1_700_000_000, 0)
	mr.SetTime(base)

	if ok, _ := d.ReserveDrainTarget(ctx, target, "alive", 100, 40, ttl); !ok {
		t.Fatal("first reserve refused")
	}
	// 8s later (inside ttl), the same drainer reserves another 40. Its accumulated hold is 80.
	mr.SetTime(base.Add(8 * time.Second))
	if ok, _ := d.ReserveDrainTarget(ctx, target, "alive", 100, 40, ttl); !ok {
		t.Fatal("the same drainer's second reserve was refused (40+40 <= 100)")
	}
	// 12s from base: past the FIRST reservation's expiry, but the second refreshed the field. The hold of 80
	// must still count, so a 30-player reserve by a peer is refused (80 + 30 > 100).
	mr.SetTime(base.Add(12 * time.Second))
	if ok, _ := d.ReserveDrainTarget(ctx, target, "peer", 100, 30, ttl); ok {
		t.Fatal("a live drainer's accumulated hold expired mid-drain — each reserve must refresh its own field")
	}
}

// TestReserveDrainTargetTreatsALegacyFieldAsExpired: a bare-count field written by the pre-#284 shape has no
// expiry to check. Rather than trust it forever, treat it as already expired and prune it. Worst case a
// single in-flight reservation is forgotten during an upgrade; the soft ceiling is the real backstop.
func TestReserveDrainTargetTreatsALegacyFieldAsExpired(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()
	const target = "shard-b"

	// Hand-write the old shape: a bare count, no expiry.
	if err := d.rdb.HSet(ctx, d.drainResvKey(target), "legacy", "90").Err(); err != nil {
		t.Fatal(err)
	}
	ok, err := d.ReserveDrainTarget(ctx, target, "newcomer", 100, 60, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a legacy bare-count field blocked a reservation; it should be treated as expired and pruned")
	}
	if n, _ := d.rdb.HLen(ctx, d.drainResvKey(target)).Result(); n != 1 {
		t.Fatalf("the legacy field was not pruned: %d fields remain", n)
	}
}

// TestReserveDrainTargetKeyOutlivesEveryLiveField is distsys review Finding 3. `PEXPIRE` SETS an expiry, it
// does not extend one — so a blanket `PEXPIRE key ttl` on every reserve let a SHORT-ttl reserve stomp the
// key's expiry below a longer-lived field's, evicting a live hold early. The key's expiry is now the max live
// field expiry plus a grace.
func TestReserveDrainTargetKeyOutlivesEveryLiveField(t *testing.T) {
	d, mr := newTestRedisWithClock(t)
	ctx := context.Background()
	const target = "shard-b"

	base := time.Unix(1_700_000_000, 0)
	mr.SetTime(base)

	// A long-lived reserve, then a short-lived one from a different drainer.
	if ok, err := d.ReserveDrainTarget(ctx, target, "long", 100, 10, 60*time.Second); err != nil || !ok {
		t.Fatalf("long reserve: ok=%v err=%v", ok, err)
	}
	if ok, err := d.ReserveDrainTarget(ctx, target, "short", 100, 10, 5*time.Second); err != nil || !ok {
		t.Fatalf("short reserve: ok=%v err=%v", ok, err)
	}

	// 30s on: the short field is long gone, but the long one has 30s left and the KEY must still exist.
	mr.SetTime(base.Add(30 * time.Second))
	mr.FastForward(30 * time.Second) // advance miniredis's own TTL machinery too
	if !mr.Exists(d.drainResvKey(target)) {
		t.Fatal("the reservation key was evicted while a live field still had 30s to run — a short-ttl " +
			"reserve stomped the key's expiry below a longer-lived field's (#284)")
	}
	// And the long hold still counts: 10 + 95 > 100 must refuse.
	if ok, _ := d.ReserveDrainTarget(ctx, target, "peer", 100, 95, 5*time.Second); ok {
		t.Fatal("the surviving long hold did not count against the ceiling")
	}
}

// TestExpireDrainTargetSoonShortensButNeverExtends is the #284 retire path for a target whose handover
// SUCCEEDED. The hold must survive about one presence heartbeat — long enough for the target to report the
// migrated players' weight — and then go, rather than running out its full TTL and double-counting them.
func TestExpireDrainTargetSoonShortensButNeverExtends(t *testing.T) {
	d, mr := newTestRedisWithClock(t)
	ctx := context.Background()
	const target = "shard-b"

	base := time.Unix(1_700_000_000, 0)
	mr.SetTime(base)

	if ok, err := d.ReserveDrainTarget(ctx, target, "drainer", 100, 60, 15*time.Second); err != nil || !ok {
		t.Fatalf("reserve: ok=%v err=%v", ok, err)
	}
	// Shorten to 8s.
	if ok, err := d.ExpireDrainTargetSoon(ctx, target, "drainer", 8*time.Second); err != nil || !ok {
		t.Fatalf("expire-soon: ok=%v err=%v", ok, err)
	}
	// Still counting at 5s: the hold is bridging the presence-heartbeat blind window.
	mr.SetTime(base.Add(5 * time.Second))
	if ok, _ := d.ReserveDrainTarget(ctx, target, "peer", 100, 60, 15*time.Second); ok {
		t.Fatal("the hold was dropped before the presence heartbeat could reflect the migrated players — a " +
			"concurrent drainer would read a stale low load and over-commit (#284)")
	}
	// Gone at 9s: it must not linger for the full 15s TTL double-counting players presence now reports.
	mr.SetTime(base.Add(9 * time.Second))
	if ok, err := d.ReserveDrainTarget(ctx, target, "peer", 100, 60, 15*time.Second); err != nil || !ok {
		t.Fatalf("the shortened hold outlived its new expiry: ok=%v err=%v", ok, err)
	}

	// It NEVER extends: a longer request on an already-shorter field is a no-op.
	mr.SetTime(base)
	if ok, _ := d.ReserveDrainTarget(ctx, "shard-c", "drainer", 100, 10, 5*time.Second); !ok {
		t.Fatal("seed reserve refused")
	}
	if ok, err := d.ExpireDrainTargetSoon(ctx, "shard-c", "drainer", 60*time.Second); err != nil || ok {
		t.Fatalf("expire-soon must not EXTEND a shorter expiry: ok=%v err=%v", ok, err)
	}
}

// TestExpireDrainTargetSoonOnAMissingFieldIsANoOp: nothing to shorten, no error.
func TestExpireDrainTargetSoonOnAMissingFieldIsANoOp(t *testing.T) {
	d := newTestRedis(t)
	ok, err := d.ExpireDrainTargetSoon(context.Background(), "shard-b", "ghost", 5*time.Second)
	if err != nil {
		t.Fatalf("missing field must not error: %v", err)
	}
	if ok {
		t.Fatal("reported a rewrite of a field that does not exist")
	}
}

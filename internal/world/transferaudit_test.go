package world

import (
	"testing"
)

// transferaudit_test.go — hermetic (MemStore-backed) tests for the #443 cross-character item-transfer audit:
// an unbound item one player drops/puts and a DIFFERENT player picks up records exactly one item_transferred
// row (subject = acquirer, actor = releaser); a self-pickup, a never-player-released item, and a mob-released
// item record nothing. Reuses the audit_test.go harness (withAuditor / setPID / waitAuditKind / settle).

// twoPlayersWithAudit sets up Alice + Bob in the SAME room, both saved (a PID each), behind an audit-enabled
// shard. Returns the zone, the two sessions, and the sink.
func twoPlayersWithAudit(t *testing.T) (*Zone, *session, *session, *MemStore) {
	t.Helper()
	z, _ := abilityTestZone(t)
	ms := withAuditor(t, z)
	alice := makeRoomPlayer(z, "Alice")
	setPID(alice, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	bob := makePlayerTargetInRoom(z, alice.entity, "Bob")
	setPID(bob, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	return z, alice, bob, ms
}

// TestItemTransferCrossCharacterRecorded: Alice drops an item, Bob picks it up — exactly one item_transferred
// row, subject = the ACQUIRER (Bob), actor = the RELEASER (Alice, a character actor carrying Alice's PID),
// payload carrying the item ref + both names + the room.
func TestItemTransferCrossCharacterRecorded(t *testing.T) {
	z, alice, bob, ms := twoPlayersWithAudit(t)
	addTestItem(z, alice.entity, "sword", []string{"sword"})

	z.dispatch(alice, "drop sword") // -> floor, stampReleased(sword, Alice)
	z.dispatch(bob, "get sword")    // -> Bob, crosses a character boundary

	rows := waitAuditKind(t, ms, "Bob", AuditKindItemTransferred, 1)
	if len(rows) != 1 {
		t.Fatalf("one cross-char pickup -> %d item_transferred rows, want 1", len(rows))
	}
	r := rows[0]
	if r.SubjectType != AuditSubjectCharacter || r.SubjectName != "Bob" {
		t.Errorf("subject = %s/%q, want character/Bob (the acquirer)", r.SubjectType, r.SubjectName)
	}
	if r.ActorType != AuditActorCharacter || r.ActorID != "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" {
		t.Errorf("actor = %s/%q, want character/Alice's PID (the releaser)", r.ActorType, r.ActorID)
	}
	if r.Payload["from_name"] != "Alice" || r.Payload["to_name"] != "Bob" {
		t.Errorf("payload from/to = %v/%v, want Alice/Bob", r.Payload["from_name"], r.Payload["to_name"])
	}
	if r.Payload["item_ref"] != "test:obj:sword" {
		t.Errorf("payload item_ref = %v, want test:obj:sword", r.Payload["item_ref"])
	}
}

// TestItemTransferSelfPickupNotRecorded: Alice drops and Alice picks it back up — no boundary was crossed, so
// nothing is recorded (the releaser == getter gate).
func TestItemTransferSelfPickupNotRecorded(t *testing.T) {
	z, alice, _, ms := twoPlayersWithAudit(t)
	addTestItem(z, alice.entity, "ring", []string{"ring"})

	before := ms.auditCount()
	z.dispatch(alice, "drop ring")
	z.dispatch(alice, "get ring")
	settle()
	if after := ms.auditCount(); after != before {
		t.Fatalf("a self drop+pickup recorded %d rows, want 0", after-before)
	}
}

// TestItemTransferNeverReleasedNotRecorded: an item that materializes on the floor without any PLAYER having
// released it (a spawn / reset / a mob drop) carries no Released marker, so picking it up records nothing —
// only a player-to-player handoff is a "transfer" here.
func TestItemTransferNeverReleasedNotRecorded(t *testing.T) {
	z, _, bob, ms := twoPlayersWithAudit(t)
	addTestItem(z, bob.entity.location, "gem", []string{"gem"}) // straight onto the floor, never player-released

	before := ms.auditCount()
	z.dispatch(bob, "get gem")
	settle()
	if after := ms.auditCount(); after != before {
		t.Fatalf("picking up a never-released floor item recorded %d rows, want 0", after-before)
	}
}

// TestItemTransferViaContainerRecorded: the get-FROM-container path — Alice puts an item into a bag on the
// floor, Bob takes it from the bag. Still a cross-character transfer, recorded once.
func TestItemTransferViaContainerRecorded(t *testing.T) {
	z, alice, bob, ms := twoPlayersWithAudit(t)
	addTestItem(z, alice.entity.location, "a leather bag", []string{"bag"}, &Container{capacity: 5, closed: false})
	addTestItem(z, alice.entity, "coin", []string{"coin"})

	z.dispatch(alice, "put coin in bag") // -> stampReleased(coin, Alice)
	z.dispatch(bob, "get coin from bag") // -> Bob, cross-character

	rows := waitAuditKind(t, ms, "Bob", AuditKindItemTransferred, 1)
	if len(rows) != 1 {
		t.Fatalf("get-from-container cross-char pickup -> %d rows, want 1", len(rows))
	}
	if rows[0].Payload["item_ref"] != "test:obj:coin" {
		t.Errorf("payload item_ref = %v, want test:obj:coin", rows[0].Payload["item_ref"])
	}
}

// TestItemTransferStackCountInPayload: a material stack carries its COUNT into the payload, so a reconciler
// sees how many units moved (one drop can externalize a whole 999-stack — the #443 dupe-amplifier note).
func TestItemTransferStackCountInPayload(t *testing.T) {
	z, alice, bob, ms := twoPlayersWithAudit(t)
	addTestItem(z, alice.entity, "ore", []string{"ore"}, &Stack{count: 7})

	z.dispatch(alice, "drop ore")
	z.dispatch(bob, "get ore")

	rows := waitAuditKind(t, ms, "Bob", AuditKindItemTransferred, 1)
	// Payload round-trips through JSONB (the MemStore mirrors the pgx contract), so a stored int reads back
	// as float64 — the shape any audit-payload reader must expect.
	if got, _ := rows[0].Payload["stack"].(float64); got != 7 {
		t.Fatalf("payload stack = %v, want float64(7) (the whole stack moved)", rows[0].Payload["stack"])
	}
}

// TestItemTransferFullContainerContentsRecorded (security review Finding A): dropping a FULL container and
// having another player take the whole container records the CONTENTS too — closing the "stuff loot in a bag,
// drop the bag" externalization bypass. The coin here is placed directly inside the bag (never individually
// `put` by a player), so only the recursive stamp-on-drop gives it a marker.
func TestItemTransferFullContainerContentsRecorded(t *testing.T) {
	z, alice, bob, ms := twoPlayersWithAudit(t)
	bag := addTestItem(z, alice.entity, "a leather bag", []string{"bag"}, &Container{capacity: 5, closed: false})
	addTestItem(z, bag, "coin", []string{"coin"}) // inside the bag, never player-`put` -> no marker of its own

	z.dispatch(alice, "drop bag") // recursive stamp: bag + coin both marked Alice
	z.dispatch(bob, "get bag")    // recursive record: bag + coin both cross Alice->Bob

	rows := waitAuditKind(t, ms, "Bob", AuditKindItemTransferred, 2)
	refs := map[string]bool{}
	for _, r := range rows {
		refs[r.Payload["item_ref"].(string)] = true
	}
	if !refs["test:obj:coin"] {
		t.Fatalf("the coin INSIDE the dropped bag must be recorded (the bypass); got refs %v", refs)
	}
}

// TestItemTransferStorelessShardEmitsNothing: a storeless shard (nil audit sink) audits nothing AND stamps no
// marker — the bare-engine invariant (byte-identical to a pre-#443 shard). A cross-character transfer runs the
// whole drop->get path and no-ops, never panicking.
func TestItemTransferStorelessShardEmitsNothing(t *testing.T) {
	z, _ := abilityTestZone(t) // NO withAuditor: the shard carries a nil audit sink
	if z.auditEnabled() {
		t.Fatal("test setup: expected a storeless (audit-disabled) shard")
	}
	alice := makeRoomPlayer(z, "Alice")
	setPID(alice, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	bob := makePlayerTargetInRoom(z, alice.entity, "Bob")
	setPID(bob, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	item := addTestItem(z, alice.entity, "sword", []string{"sword"})

	z.dispatch(alice, "drop sword")
	if Has[*Released](item) {
		t.Fatal("a storeless shard must not even stamp the Released marker (byte-identical to pre-#443)")
	}
	z.dispatch(bob, "get sword") // must no-op the audit (nil sink), not panic
}

// TestBoundItemNeverTransferAudited: a BOUND item can't be parted with (transferBlocked), so it never reaches
// the floor to be stamped/picked-up — no transfer is ever audited. Documents that the bound gate sits upstream.
func TestBoundItemNeverTransferAudited(t *testing.T) {
	z, alice, _, ms := twoPlayersWithAudit(t)
	item := addTestItem(z, alice.entity, "amulet", []string{"amulet"})
	bindItem(item) // now soulbound

	before := ms.auditCount()
	z.dispatch(alice, "drop amulet")
	if item.location != alice.entity {
		t.Fatalf("a bound item was dropped despite transferBlocked (location %v)", item.location)
	}
	settle()
	if after := ms.auditCount(); after != before {
		t.Fatalf("a blocked bound-item drop recorded %d rows, want 0", after-before)
	}
}

// TestItemTransferMarkerClearedOnPickup: the Released marker is CLEARED when an item is acquired. The
// structural `Has[*Released]` assertion is what pins the clear-on-pickup behavior (it fails if Remove is
// dropped). The end-to-end second leg (Bob drops -> Alice gets) then confirms the NEXT release re-stamps
// correctly and attributes to Bob — note that leg alone can't prove the clear (cmdDrop's Add would overwrite
// the marker to Bob regardless); the Has check is the real guard.
func TestItemTransferMarkerClearedOnPickup(t *testing.T) {
	z, alice, bob, ms := twoPlayersWithAudit(t)
	item := addTestItem(z, alice.entity, "torch", []string{"torch"})

	z.dispatch(alice, "drop torch")
	z.dispatch(bob, "get torch") // Alice -> Bob (row 1)
	waitAuditKind(t, ms, "Bob", AuditKindItemTransferred, 1)
	if Has[*Released](item) {
		t.Fatalf("Released marker should be cleared once the item is acquired")
	}

	z.dispatch(bob, "drop torch")
	z.dispatch(alice, "get torch") // Bob -> Alice (row 2, under Alice's subject)
	rows := waitAuditKind(t, ms, "Alice", AuditKindItemTransferred, 1)
	if rows[0].ActorID != "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" {
		t.Fatalf("second transfer actor = %q, want Bob's PID (no stale Alice attribution)", rows[0].ActorID)
	}
}

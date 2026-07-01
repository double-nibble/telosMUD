package world

import (
	"encoding/json"
	"testing"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
)

// TestPrepareRejectsUnknownItemPrototype pins the pack-set validation (security hardening): a cross-shard
// handoff whose carried inventory names a prototype this destination's packs don't define is REJECTED at
// Prepare — no pending entity is parked — so the source thaws the player in place WITH their items, instead
// of accepting the move and silently dropping the item post-commit (the old data-loss window). A carry with
// only known prototypes (or none) is still accepted.
func TestPrepareRejectsUnknownItemPrototype(t *testing.T) {
	dst := newDemoZone("midgaard", newProtoCache())

	// Carry an item whose prototype does not exist in the demo pack.
	st := StateJSON{Inventory: []ItemJSON{{ProtoRef: "ghost:item:doesnotexist"}}}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	snap := &handoffv1.PlayerSnapshot{CharacterId: "Ghosty", Name: "Ghosty", StateJson: string(raw)}

	reply := make(chan error, 1)
	dst.prepare(prepareMsg{snap: snap, room: "", epoch: 1, token: "tok", reply: reply})
	if err := <-reply; err == nil {
		t.Fatal("prepare accepted a handoff carrying an unknown item prototype; want rejection")
	}
	if dst.players["Ghosty"] != nil {
		t.Fatal("prepare parked a pending entity despite rejecting the handoff (item must not be dropped)")
	}

	// A clean carry (no items) is accepted and parks a pending entity.
	snapOK := &handoffv1.PlayerSnapshot{CharacterId: "Cleanly", Name: "Cleanly"}
	replyOK := make(chan error, 1)
	dst.prepare(prepareMsg{snap: snapOK, room: "", epoch: 1, token: "tok2", reply: replyOK})
	if err := <-replyOK; err != nil {
		t.Fatalf("prepare rejected a clean no-carry handoff: %v", err)
	}
	if dst.players["Cleanly"] == nil {
		t.Fatal("prepare did not park a pending entity for a clean handoff")
	}
}

// TestPrepareRejectsOversizedCarry pins the WIDTH guard: a carry past maxCarryItemNodes (a wide-but-shallow
// spawn-bomb the depth cap doesn't catch) is rejected at Prepare before any rehydration, while a small carry
// of the SAME known item is accepted (so the rejection is the node cap, not the pack-set check).
func TestPrepareRejectsOversizedCarry(t *testing.T) {
	dst := newDemoZone("midgaard", newProtoCache())
	const known = "midgaard:obj:torch"

	small := StateJSON{Inventory: []ItemJSON{{ProtoRef: known}, {ProtoRef: known}}}
	rawSmall, _ := json.Marshal(small)
	replyS := make(chan error, 1)
	dst.prepare(prepareMsg{
		snap:  &handoffv1.PlayerSnapshot{CharacterId: "SmallBag", Name: "SmallBag", StateJson: string(rawSmall)},
		epoch: 1, token: "s", reply: replyS,
	})
	if err := <-replyS; err != nil {
		t.Fatalf("small carry of a known item rejected (torch must be a real demo proto): %v", err)
	}

	big := StateJSON{Inventory: make([]ItemJSON, maxCarryItemNodes+1)}
	for i := range big.Inventory {
		big.Inventory[i] = ItemJSON{ProtoRef: known}
	}
	rawBig, _ := json.Marshal(big)
	replyB := make(chan error, 1)
	dst.prepare(prepareMsg{
		snap:  &handoffv1.PlayerSnapshot{CharacterId: "BigBag", Name: "BigBag", StateJson: string(rawBig)},
		epoch: 1, token: "b", reply: replyB,
	})
	if err := <-replyB; err == nil {
		t.Fatal("prepare accepted a carry past the node cap; want rejection")
	}
	if dst.players["BigBag"] != nil {
		t.Fatal("prepare parked a pending entity despite the node-cap rejection")
	}
}

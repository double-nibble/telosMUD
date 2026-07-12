package world

import (
	"testing"
)

// drain_wedged_test.go pins #336: a drain must not report a player Redirected (zero-drop, socket kept open)
// when that player's gate is wedged and cannot receive the Redirect frame. A wedged gate is skipped by
// drainPlayer, left resident, and reclaimed at the deadline as a CLIENT-fault straggler (a clean reconnect
// from durable state), never counted as a phantom Redirected.

// TestIsGateWedgedThreshold pins the exact boundary: a full outbound buffer's worth of consecutive drops
// (slowClientWedgedDrops) is wedged; one fewer is not. This is the same threshold the send path uses to log
// "gate write-deadline will reclaim the connection".
func TestIsGateWedgedThreshold(t *testing.T) {
	s := &session{character: "Limping"}
	s.consecutiveDrops = slowClientWedgedDrops - 1
	if isGateWedged(s) {
		t.Fatalf("consecutiveDrops=%d is below the wedged threshold (%d) — a limping-but-draining client must "+
			"still be redirectable", s.consecutiveDrops, slowClientWedgedDrops)
	}
	s.consecutiveDrops = slowClientWedgedDrops
	if !isGateWedged(s) {
		t.Fatalf("consecutiveDrops=%d must be wedged (threshold %d)", s.consecutiveDrops, slowClientWedgedDrops)
	}
}

// TestDrainPlayerSkipsAWedgedGate is the core of #336 at the redirect decision: drainPlayer must NOT freeze /
// hand off a player whose gate is wedged. Freezing them would drive a handoff whose Redirect frame the gate
// can never receive, reporting a zero-drop that never happened. The player must be left resident (frozen
// stays false, still in z.players) so the deadline reclaims it honestly.
func TestDrainPlayerSkipsAWedgedGate(t *testing.T) {
	z := newZone("midgaard")
	z.draining = true
	wedged := makeRoomPlayer(z, "Wedged")
	wedged.consecutiveDrops = slowClientWedgedDrops

	z.drainPlayer("Wedged")

	if wedged.frozen {
		t.Fatal("a wedged gate was frozen for handoff — its Redirect would be dropped, a phantom zero-drop (#336)")
	}
	if _, ok := z.players["Wedged"]; !ok {
		t.Fatal("a wedged gate must be left resident so the deadline reclaims it, not silently removed")
	}
}

// TestReclaimStragglersCountsAWedgedGateAsClient: at the deadline, a wedged-gate straggler is counted as a
// CLIENT fault (the client's stalled socket is the reason it couldn't be redirected — the same category as
// link death), NOT infra. Misfiling it as infra would inflate the metric ops watch to decide whether the
// drain/handoff machinery itself is struggling. It still gets a clean reconnect Disconnect (it is neither
// detached nor frozen), which harmlessly drops on the wedged socket while the gate's write-deadline reclaims.
func TestReclaimStragglersCountsAWedgedGateAsClient(t *testing.T) {
	z := newZone("midgaard")

	// A wedged (client-fault) straggler alongside a healthy (infra-fault) one, so we prove the split rather
	// than a blanket count.
	wedged := makeRoomPlayer(z, "Wedged")
	wedged.consecutiveDrops = slowClientWedgedDrops
	_ = makeRoomPlayer(z, "Healthy") // consecutiveDrops == 0 → infra

	resp := make(chan reclaimTally, 1)
	z.reclaimStragglers(resp)
	got := <-resp

	if got.client != 1 {
		t.Errorf("client tally = %d, want 1 (the wedged gate)", got.client)
	}
	if got.infra != 1 {
		t.Errorf("infra tally = %d, want 1 (the healthy player the drain simply couldn't move)", got.infra)
	}
	// The wedged straggler still gets the clean reconnect Disconnect (live, non-frozen socket).
	if !hasReconnectDisconnect(drainOutFrames(wedged)) {
		t.Error("a wedged straggler must still be sent a reconnect Disconnect (its gate's write-deadline reclaims the socket)")
	}
}

// TestWedgedGateClassifierEdge extends the classifier: a healthy in-room player is infra, the same player
// once wedged is client — the ONLY thing that changed is the gate's drop state, isolating the #336 signal.
func TestWedgedGateClassifierEdge(t *testing.T) {
	z := newZone("midgaard")
	s := makeRoomPlayer(z, "Turner")

	if isClientFaultStraggler(s) {
		t.Fatal("premise: a healthy in-room player is an infra fault")
	}
	s.consecutiveDrops = slowClientWedgedDrops
	if !isClientFaultStraggler(s) {
		t.Fatal("a wedged in-room player must be a client fault (#336)")
	}
}

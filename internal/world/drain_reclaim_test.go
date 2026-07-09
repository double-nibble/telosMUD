package world

import (
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// drainOutFrames non-blockingly drains a session's out channel and returns every frame queued on it.
func drainOutFrames(s *session) []*playv1.ServerFrame {
	var out []*playv1.ServerFrame
	for {
		select {
		case f := <-s.out:
			out = append(out, f)
		default:
			return out
		}
	}
}

// hasDisconnect reports whether any frame is a reconnectable drain Disconnect with the expected reason.
func hasReconnectDisconnect(frames []*playv1.ServerFrame) bool {
	for _, f := range frames {
		if d := f.GetDisconnect(); d != nil && d.GetReconnectable() && d.GetReason() == drainReclaimReason {
			return true
		}
	}
	return false
}

// TestReclaimStragglersClassifiesAndDisconnects pins #43: reclaimStragglers splits the deadline stragglers
// into infra- vs client-fault and sends a clean reconnect Disconnect to exactly the ones with a live,
// non-frozen socket (a detached/link-dead client has no reader; a frozen session is owned by the handoff
// machinery; a pending inbound-handoff arrival is not one of our residents at all).
func TestReclaimStragglersClassifiesAndDisconnects(t *testing.T) {
	z := newZone("midgaard")

	// infra-fault #1: a healthy, in-world, connected player the drain simply couldn't move in time.
	healthy := makeRoomPlayer(z, "Healthy")

	// infra-fault #2: a frozen (mid-handoff) player whose handoff did NOT commit — the RPC was too slow; its
	// entity is detached (location nil), so the classifier MUST check frozen before the location check.
	frozen := makeRoomPlayer(z, "Frozen")
	frozen.frozen = true

	// NOT reclaimed: a frozen player whose handoff CAS COMMITTED (handedOff) — the destination owns it, so it
	// was in effect redirected. It must be excluded from the tally entirely (counts as Redirected), and gets
	// no Disconnect (its socket is reserved for the pending Redirect frame).
	handedOff := makeRoomPlayer(z, "HandedOff")
	handedOff.frozen = true
	handedOff.handedOff = true

	// client-fault #1: a link-dead (detached) player — no reader on the socket.
	detached := makeRoomPlayer(z, "Detached")
	detached.detached = true

	// client-fault #2: a player that never finished connecting (entity built but never placed in a room).
	unplaced := &session{character: "Unplaced", out: make(chan *playv1.ServerFrame, 8), epoch: 1}
	z.newPlayerEntity(unplaced, "Unplaced")
	z.players["Unplaced"] = unplaced

	// not a straggler of ours: an inbound handoff arrival (pending, no bound stream) — skipped entirely.
	pending := &session{character: "Pending", pending: true}
	z.players["Pending"] = pending

	resp := make(chan reclaimTally, 1)
	z.reclaimStragglers(resp)
	got := <-resp

	if got.infra != 2 {
		t.Errorf("infra tally = %d, want 2 (healthy + frozen-not-committed; handedOff excluded)", got.infra)
	}
	if got.client != 2 {
		t.Errorf("client tally = %d, want 2 (detached + unplaced)", got.client)
	}

	// A handed-off (committed) frozen straggler is neither counted nor sent a Disconnect.
	if hasReconnectDisconnect(drainOutFrames(handedOff)) {
		t.Error("handed-off frozen straggler must NOT be sent a Disconnect (destination owns it)")
	}

	// Only the live, non-frozen sockets get the clean reconnect Disconnect.
	if !hasReconnectDisconnect(drainOutFrames(healthy)) {
		t.Error("healthy straggler did not get a reconnect Disconnect")
	}
	if !hasReconnectDisconnect(drainOutFrames(unplaced)) {
		t.Error("unplaced straggler did not get a reconnect Disconnect")
	}
	if hasReconnectDisconnect(drainOutFrames(frozen)) {
		t.Error("frozen (mid-handoff) straggler must NOT be sent a Disconnect (handoff machinery owns it)")
	}
	if hasReconnectDisconnect(drainOutFrames(detached)) {
		t.Error("detached (link-dead) straggler must NOT be sent a Disconnect (no reader)")
	}
}

// TestIsClientFaultStraggler pins the classifier edges directly, especially the frozen-before-location order.
func TestIsClientFaultStraggler(t *testing.T) {
	z := newZone("midgaard")

	healthy := makeRoomPlayer(z, "Healthy")
	if isClientFaultStraggler(healthy) {
		t.Error("a healthy, in-room player is an infra fault, not client")
	}

	frozen := makeRoomPlayer(z, "Frozen")
	frozen.frozen = true
	if isClientFaultStraggler(frozen) {
		t.Error("a frozen mid-handoff player is an infra fault (handoff too slow), not client")
	}

	detached := makeRoomPlayer(z, "Detached")
	detached.detached = true
	if !isClientFaultStraggler(detached) {
		t.Error("a link-dead (detached) player is a client fault")
	}

	unplaced := &session{character: "Unplaced"}
	z.newPlayerEntity(unplaced, "Unplaced")
	if !isClientFaultStraggler(unplaced) {
		t.Error("a never-placed (location nil) player is a client fault")
	}
}

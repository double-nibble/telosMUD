package gate

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/directory"
)

// handoff_neither_own_test.go pins the worst-consequence cross-shard window that had NO test
// (issue #138): the source COMMITS ownership of the player to shard B (the placement CAS
// succeeds) and sends the Redirect, then B becomes undialable before the gate re-dials its
// Play service. The player is now OWNED by B but LIVE nowhere — the "neither-own" gap.
//
// The failure is injected with faultyPlayDialer — the gate-side half of the #123 fault
// enabler (the Locator/CAS half lives in internal/world). It fails the gate's POST-REDIRECT
// Play dial to B while B's Handoff service stays reachable for the cross-shard Prepare, which
// is exactly what distinguishes this from a plain shard drop (there, ownership never moved;
// here it did).

// faultyPlayDialer wraps the harness's bufconn Play dialer and FAILS dials to failAddr while
// failing is set, delegating every other address. A test arms it to open the neither-own
// window, then flips failing off to let a later reconnect through. failing is atomic: it is
// toggled on the test goroutine and read on the gate's dial goroutine.
type faultyPlayDialer struct {
	h        *harness
	failAddr string
	failing  atomic.Bool
}

func (f *faultyPlayDialer) dial(addr string) (playv1.PlayClient, error) {
	if addr == f.failAddr && f.failing.Load() {
		return nil, fmt.Errorf("harness: injected Play-dial failure for %q (neither-own window)", addr)
	}
	return f.h.dialPlay(addr)
}

// serveGateFaultyPlayDial builds the gate over a faultyPlayDialer that begins by FAILING Play
// dials to failAddr. It returns the dialer so the test can flip the fault off (recovery leg).
func (h *harness) serveGateFaultyPlayDial(dir directory.Directory, failAddr string) *faultyPlayDialer {
	h.t.Helper()
	fd := &faultyPlayDialer{h: h, failAddr: failAddr}
	fd.failing.Store(true)
	h.srv = newServer(":0", dir, newPoolWithDialer(fd.dial), nil)
	return fd
}

// TestHandoffRedirectDestinationDialFails drives the neither-own window end-to-end through the
// gate and pins two things:
//
//  1. THE FAILURE MODE: after ownership commits to B and B's Play becomes undialable, the gate
//     does NOT hang the socket forever — it surfaces the error (the "world is unreachable"
//     notice) and closes the connection within a deadline. Ownership is confirmed to have moved
//     (directory placement = shard-b at the bumped epoch), which is what makes this the
//     neither-own gap rather than the pre-redirect drop (shard_drop_chaos_test.go).
//
//  2. THE RECOVERY CONTRACT (as it is TODAY): with B reachable again, a fresh reconnect is
//     routed by placement to the OWNING shard B — but B still holds the orphaned PENDING copy
//     the failed handoff left behind, so the fresh (token-less) bind is REJECTED
//     ("handoff token invalid") until that pending is reaped. Recovery therefore waits out the
//     destination's pending-TTL (handoff_server.go pendingTTL) — the documented cost of this
//     window. This pins the current behavior and flags the contract, exactly as
//     shard_drop_chaos_test.go does for a plain shard crash: a gate that ABORTED B's pending on
//     a failed re-dial (or reconnected with the redirect token) would recover the player
//     immediately, and this assertion should then be updated.
func TestHandoffRedirectDestinationDialFails(t *testing.T) {
	dir := twoShardDir(t)

	h := newHarness(t)
	// B is served (its Handoff service answers Prepare) and registered in the directory, so the
	// ownership CAS commits to shard-b. A reaches B's Handoff via the peer dialer.
	h.addShard("darkwood", "addr-b", dir, nil)
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		if addr != "addr-b" {
			return nil, errUnknownShard(addr)
		}
		return h.dialHandoff("addr-b")
	}
	h.addShard("midgaard", "addr-a", dir, peers)

	// The gate dials A's Play normally, but its Play re-dial to addr-b FAILS — the post-commit
	// neither-own window. Routing is placement-aware (production-faithful): a fresh login with no
	// placement falls back to the home zone (A); after the handoff commits, placement points at B.
	fd := h.serveGateFaultyPlayDial(placementDir{redis: dir, homeZone: "midgaard"}, "addr-b")

	term := h.dial(t)
	term.login(t, "Marooned")
	term.expect(t, "Temple Square") // fresh login routed to A (home fallback)
	term.send(t, "north")           // temple -> market (still on A)
	term.expect(t, "Market Square")

	// Cross the seam: A Prepares B (OK), commits ownership to shard-b, sends Redirect. The gate
	// re-dials addr-b's Play -> the injected failure. The gate must SURFACE it, not hang: it
	// writes the unreachable notice and closes the socket within expect/expectClose's deadline.
	term.send(t, "north") // market -> darkwood: handoff commits, then the re-dial fails
	term.expect(t, "The world is unreachable")
	term.expectClose(t)
	term.close(t)

	// OWNERSHIP DID MOVE: the directory records the player on shard-b at the bumped epoch. The
	// commit happened before the (failed) re-dial — this is the crux of the neither-own window
	// and the contrast with a pre-redirect shard drop (where ownership never transfers).
	place, err := dir.PlayerPlacement(context.Background(), "Marooned")
	if err != nil {
		t.Fatalf("placement after committed handoff: %v", err)
	}
	if place.ShardID != "shard-b" || place.Epoch != 2 {
		t.Fatalf("placement = %+v, want {shard-b epoch 2} (ownership committed before the re-dial failed)", place)
	}

	// RECOVERY LEG: B is dialable again. A fresh reconnect is placement-routed to the owning
	// shard B, but B's orphaned pending copy (never bound, not yet TTL-reaped) rejects the
	// token-less bind. This is today's contract — recovery is deferred to the pending-TTL reaper.
	fd.failing.Store(false)
	recon := h.dial(t)
	recon.login(t, "Marooned")
	recon.expect(t, "handoff token invalid") // routed to B, but B's lingering pending blocks the fresh bind
	recon.expectClose(t)
	recon.close(t)
}

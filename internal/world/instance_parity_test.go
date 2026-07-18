package world

import (
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// instance_parity_test.go — the two PARITY guards for #411, in the house style (internal/luasandbox): a
// duplicated fact with no link between its two ends is a silent break waiting to happen, so the link is a
// build-failing test.
//
// This file holds the charset half. The director half must live in internal/director (world cannot import
// director) — see internal/director/instance_parity_test.go.

// TestInstanceSeparatorIsOutsideTheRefCharset is the keystone assertion for the whole instance model.
//
// EVERYTHING rests on `#` being impossible in an authored ref. isInstanceID is a pure SHAPE test — it asks
// "does this id contain the separator", never "is this in a registry" — precisely so the answer is the same
// on a shard that hosts the zone and one that does not. That is only sound while no real authored zone can
// contain a `#`.
//
// If refCharset ever widens to admit `#`, a legitimately authored zone named "boss#1" starts misclassifying
// as an instance, and every fail-closed guard in this change inverts on it, SILENTLY:
//
//   - drain.go drops it out of the handover set: its lease is never flipped and its players are never
//     redirected on SIGTERM (they reconnect instead of migrating seamlessly).
//   - reset.go refuses its persistent resets: its durable objects stop loading, forever.
//   - reset.go suppresses its timed repop; luascope.go refuses its signal_world/signal_region.
//   - reload.go freezes it: it stops taking room reconciles and Lua recompiles entirely.
//   - unhost.go skips the lease-OWNERSHIP check when tearing it down — the one that is a real safety check
//     for a leased zone.
//   - handoff_server.go refuses to Prepare into it or AdoptZone it from any peer.
//
// Not one of those produces an error. The zone simply becomes quietly wrong. So this asserts the charset
// side of the invariant against the LIVE separator constant, through the real lint, and it is built to fail
// the build if instanceSep is renamed.
func TestInstanceSeparatorIsOutsideTheRefCharset(t *testing.T) {
	// The exact composition mintInstanceID performs. If the charset ever admits this, the whole shape-based
	// model is unsound and the guards above invert on real content.
	minted := "darkwood" + instanceSep + "deadbeef"

	packs := []content.Pack{{
		Pack:  "parity",
		Zones: []content.ZoneDTO{{Ref: minted}},
	}}
	violations := content.LintRefCharset(packs)
	found := false
	for _, v := range violations {
		if v.Value == minted {
			found = true
		}
	}
	if !found {
		t.Fatalf("content.LintRefCharset accepted %q as an authored zone ref: the instance separator %q is "+
			"INSIDE the authored ref charset. isInstanceID is a pure shape test, so a real authored zone in "+
			"this shape is now silently treated as an instance — dropped from the drain handover, refused its "+
			"persistent resets, frozen out of hot reload, and skipped by UnhostZone's lease-ownership check, "+
			"with no error on any of those paths", minted, instanceSep)
	}

	// The CONTROL: an ordinary ref of the same shape MINUS the separator must pass, so the assertion above is
	// about the separator and not about the lint rejecting everything.
	ok := content.LintRefCharset([]content.Pack{{
		Pack:  "parity",
		Zones: []content.ZoneDTO{{Ref: "darkwood-deadbeef"}},
	}})
	if len(ok) != 0 {
		t.Fatalf("the control ref was rejected too (%v); this test proves nothing about the separator", ok)
	}

	// And the separator is exactly one character, which splitInstanceID's index arithmetic assumes.
	if len(instanceSep) != 1 || strings.ContainsAny(instanceSep, "abcdefghijklmnopqrstuvwxyz0123456789:-_") {
		t.Fatalf("instanceSep = %q; it must be a single character outside the ref charset", instanceSep)
	}
}

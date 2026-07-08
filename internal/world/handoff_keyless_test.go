package world

import (
	"context"
	"crypto/ed25519"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
)

// handoff_keyless_test.go — #260: a KEYLESS world port must REFUSE inbound handoff RPCs by default. A single-
// shard world never legitimately receives a handoff, so a reachable keyless port that accepted an unsigned
// Prepare would be a known-prototype item-injection vector (an econ dupe) — and AdoptZone a forced-host DoS.
// The refusal is lifted only by the explicit insecure opt-in (WithInsecureHandoff, from TELOS_ALLOW_INSECURE).

// TestPrepareRefusesKeylessHandoffByDefault: a keyless, non-insecure shard rejects Prepare with
// PermissionDenied BEFORE any state work — even for a well-formed snapshot — so the refusal is independent of
// whether the target zone exists (contrast the INSECURE keyless path, which falls through to NotFound).
func TestPrepareRefusesKeylessHandoffByDefault(t *testing.T) {
	keyless := &handoffServer{shard: NewDemoShard()} // keyless, NOT insecure (default)
	req := newSignedPrepare()                        // a plausible unsigned Prepare

	_, err := keyless.Prepare(context.Background(), req)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("keyless non-insecure shard must REFUSE Prepare with PermissionDenied, got %v", err)
	}
}

// TestPrepareRefusesKeylessBeforeTierStrip: the refusal must fire BEFORE the #106 tier-strip / any request
// mutation, so a refused Prepare leaves the request untouched and never reaches state work.
func TestPrepareRefusesKeylessBeforeTierStrip(t *testing.T) {
	keyless := &handoffServer{shard: NewDemoShard()}
	req := newSignedPrepare() // Tier == "admin"

	if _, err := keyless.Prepare(context.Background(), req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	// The handler returned before the strip; the request is unmodified (nothing was processed).
	if got := req.Snapshot.GetTier(); got != "admin" {
		t.Fatalf("a refused keyless Prepare must not mutate the request, tier now %q", got)
	}
}

// TestAdoptZoneRefusesKeylessHandoffByDefault: AdoptZone is guarded by the same #260 gate — a keyless,
// non-insecure shard refuses to host a zone on an unauthenticated request.
func TestAdoptZoneRefusesKeylessHandoffByDefault(t *testing.T) {
	keyless := &handoffServer{shard: NewDemoShard()}

	_, err := keyless.AdoptZone(context.Background(), &handoffv1.AdoptZoneRequest{ZoneId: "midgaard"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("keyless non-insecure shard must REFUSE AdoptZone with PermissionDenied, got %v", err)
	}
}

// TestInsecureKeylessHandoffAccepted: the explicit opt-in lifts the refusal — an insecure keyless shard
// processes the Prepare (falling through to NotFound for a zone it doesn't host, i.e. auth was skipped, not
// refused). This is the dev/test rig posture the serveShard harness and cfg.AllowInsecure select.
func TestInsecureKeylessHandoffAccepted(t *testing.T) {
	insecure := &handoffServer{shard: NewDemoShard().WithInsecureHandoff(true)}
	req := newSignedPrepare()

	if _, err := insecure.Prepare(context.Background(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("insecure keyless shard must ACCEPT (skip auth) and reach NotFound, got %v", err)
	}
}

// TestKeyedHandoffIgnoresInsecureFlag: a KEYED shard always enforces the signature regardless of the insecure
// flag — an unsigned Prepare is PermissionDenied even if allowInsecureHandoff were somehow set.
func TestKeyedHandoffIgnoresInsecureFlag(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	keyed := &handoffServer{shard: NewDemoShard().WithHandoffKeys(nil, pub).WithInsecureHandoff(true)}
	req := newSignedPrepare() // unsigned

	if _, err := keyed.Prepare(context.Background(), req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a keyed shard must enforce the signature regardless of the insecure flag, got %v", err)
	}
}

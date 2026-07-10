package world

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/directory"
)

// handoffsig_adoptzone_test.go — #262 (authenticate AdoptZone) and #315 (fence it).
//
// AdoptZone drives HostZone + lease renewal, so it was the weakest link in the handoff surface: #260 refused
// the KEYLESS case, but a KEYED cluster still adopted on a wholly unauthenticated request. #262 signed it.
//
// A signature alone is not enough. A signature over zone_id + destination is a PERMANENT, transferable
// capability to force that shard to host that zone, so #262 also bound an issue time and expired the request
// after 60s. That was weak in three separate ways: the window is a full minute of live replay; it depends on
// two machines' wall clocks agreeing (a skewed peer is undrainable-to); and it says nothing about whether the
// handover it authorizes has already happened.
//
// #315 replaces the clock with the zone's monotonic lease GENERATION, read from the directory. The source
// signs the generation it observes while still holding the lease; HandoverZone increments it. A captured
// request is therefore valid exactly until the handover it authorizes lands, and dead forever after — a
// replay is not "tolerated because HostZone is idempotent", it is unrepresentable.

func adoptKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// signedAdopt builds a request as the real source does: bind the destination + the lease generation it holds
// the zone at, then sign.
func signedAdopt(priv ed25519.PrivateKey, zone, from, to string, gen uint64) *handoffv1.AdoptZoneRequest {
	req := &handoffv1.AdoptZoneRequest{ZoneId: zone, FromShardId: from, ToShardId: to, LeaseGen: gen}
	req.ZoneSig = signAdoptZone(priv, req)
	return req
}

// stubLeaser answers ZoneLease from fixed values so a server-level test can place the directory at any
// generation (or make it fail) without a Redis. The mutating methods are no-ops: these tests never flip a
// lease, they only exercise the verify gate in front of one.
type stubLeaser struct {
	owner string
	gen   uint64
	err   error
}

func (stubLeaser) ClaimZone(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (stubLeaser) ReleaseZone(context.Context, string, string) error { return nil }
func (stubLeaser) HandoverZone(context.Context, string, string, string, time.Duration) (bool, error) {
	return true, nil
}

func (s stubLeaser) ZoneLease(context.Context, string) (string, uint64, error) {
	return s.owner, s.gen, s.err
}

// TestVerifyAdoptZoneAcceptsRequestAtTheCurrentGeneration: the happy path — signed by the cluster key, naming
// this shard, carrying the generation the directory currently reports.
func TestVerifyAdoptZoneAcceptsRequestAtTheCurrentGeneration(t *testing.T) {
	pub, priv := adoptKeys(t)
	req := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)
	if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", 7); err != nil {
		t.Fatalf("a correctly-signed request at the live generation must verify, got %v", err)
	}
}

// TestVerifyAdoptZoneRejectsForgeryAndTampering: every field in the digest is load-bearing. A request with no
// signature, a signature from the wrong key, or ANY mutated field after signing must be refused.
func TestVerifyAdoptZoneRejectsForgeryAndTampering(t *testing.T) {
	pub, priv := adoptKeys(t)
	_, otherPriv := adoptKeys(t)

	t.Run("unsigned", func(t *testing.T) {
		req := &handoffv1.AdoptZoneRequest{ZoneId: "midgaard", ToShardId: "shard-b", LeaseGen: 7}
		if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", 7); err == nil {
			t.Fatal("a keyed shard must refuse an unsigned AdoptZone — that is the whole #262 hole")
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		req := signedAdopt(otherPriv, "midgaard", "shard-a", "shard-b", 7)
		if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", 7); err == nil {
			t.Fatal("a signature from outside the cluster keypair must be refused")
		}
	})
	for _, tc := range []struct {
		name   string
		mutate func(*handoffv1.AdoptZoneRequest)
	}{
		{"zone_id", func(r *handoffv1.AdoptZoneRequest) { r.ZoneId = "darkwood" }},
		{"from_shard_id", func(r *handoffv1.AdoptZoneRequest) { r.FromShardId = "shard-evil" }},
		{"signature", func(r *handoffv1.AdoptZoneRequest) { r.ZoneSig[0] ^= 0xff }},
	} {
		t.Run("tampered "+tc.name, func(t *testing.T) {
			req := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)
			tc.mutate(req)
			if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", 7); err == nil {
				t.Fatalf("mutating %s after signing must invalidate the request", tc.name)
			}
		})
	}

	// lease_gen needs its own shape. Rewriting it to the generation the directory now holds is EXACTLY the
	// attack the fence exists to stop: a captured request re-stamped to survive the flip. It must fail on the
	// DIGEST, so verify it against the generation the attacker wrote — if lease_gen sat outside the signed
	// digest, this would sail through.
	t.Run("tampered lease_gen (re-stamped past the flip)", func(t *testing.T) {
		req := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)
		req.LeaseGen = 8
		if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", 8); !errors.Is(err, ErrAdoptZoneSig) {
			t.Fatalf("re-stamping lease_gen must break the signature, got %v", err)
		}
	})

	// to_shard_id likewise: rewriting it to REDIRECT the capability must fail on the digest, not merely on the
	// destination-binding check. So verify at the shard the attacker rewrote it to.
	t.Run("tampered to_shard_id (redirected at another shard)", func(t *testing.T) {
		req := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)
		req.ToShardId = "shard-c"
		if err := verifyAdoptZone(pub, req, "shard-c", "shard-a", 7); err == nil {
			t.Fatal("rewriting to_shard_id must break the signature — otherwise the capability is redirectable")
		}
	})
}

// TestVerifyAdoptZoneBindsDestinationShard is the anti-replay-across-the-fleet property: a signature minted to
// make shard-b host a zone is worthless at shard-c. Without this, one captured request would be a cluster-wide
// capability to force ANY shard to host that zone.
func TestVerifyAdoptZoneBindsDestinationShard(t *testing.T) {
	pub, priv := adoptKeys(t)
	req := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)

	if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", 7); err != nil {
		t.Fatalf("the named destination must accept it, got %v", err)
	}
	if err := verifyAdoptZone(pub, req, "shard-c", "shard-a", 7); err == nil {
		t.Fatal("a request naming shard-b must NOT be replayable at shard-c")
	}

	// An unbound request (no destination) is refused even with a valid signature over it.
	unbound := &handoffv1.AdoptZoneRequest{ZoneId: "midgaard", FromShardId: "shard-a", LeaseGen: 7}
	unbound.ZoneSig = signAdoptZone(priv, unbound)
	if err := verifyAdoptZone(pub, unbound, "shard-b", "shard-a", 7); err == nil {
		t.Fatal("a request that names no destination must be refused (it would be replayable anywhere)")
	}
}

// TestVerifyAdoptZoneIsDeadOnceTheHandoverLands is the #315 headline. The signature is valid FOREVER — Ed25519
// has no expiry — so what kills the request must be the world moving on, not a clock. The generation the
// source signed is the generation the flip consumes.
//
// This also pins the retry semantics the drain depends on: BEFORE the flip, a re-sent AdoptZone at the same
// generation is still good (the source retries a dropped RPC; HostZone is idempotent).
func TestVerifyAdoptZoneIsDeadOnceTheHandoverLands(t *testing.T) {
	pub, priv := adoptKeys(t)
	req := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)

	if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", 7); err != nil {
		t.Fatalf("before the flip the source may retry at the same generation, got %v", err)
	}
	if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", 8); !errors.Is(err, ErrAdoptZoneStale) {
		t.Fatal("after the flip bumps the generation, the captured request must be refused as stale")
	}
	// And it never comes back. Under the old issue-time scheme a captured request was live for a full 60s
	// window regardless of what had happened to the zone; here every later generation is dead ground.
	for _, cur := range []uint64{9, 10, 1 << 20} {
		if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", cur); !errors.Is(err, ErrAdoptZoneStale) {
			t.Fatalf("a consumed request must stay dead at generation %d, got %v", cur, err)
		}
	}
	// A generation BEHIND the request is refused too. The source cannot pre-sign a future handover, and a
	// directory that somehow reads backwards (a restored-from-backup Redis) must not resurrect the capability.
	if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", 6); !errors.Is(err, ErrAdoptZoneStale) {
		t.Fatalf("a request ahead of the live generation must be refused, got %v", err)
	}
}

// TestVerifyAdoptZoneRefusesAnUnleasedZone: generation 0 means the directory has never seen this zone claimed,
// so no handover exists for the request to authorize. Accepting it would let a signature bound to nothing pull
// an arbitrary zone id onto this shard — the forced-host vector #262 opened the door on, sneaking back through
// the fence.
func TestVerifyAdoptZoneRefusesAnUnleasedZone(t *testing.T) {
	pub, priv := adoptKeys(t)
	req := signedAdopt(priv, "nowhere", "shard-a", "shard-b", 0)
	if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", 0); !errors.Is(err, ErrAdoptZoneStale) {
		t.Fatalf("an unleased zone (gen 0) must be refused, got %v", err)
	}
}

// TestVerifyAdoptZoneRefusesAKeyedSourceThatSendsNoGeneration is the mixed-version rung, and the one an
// unsigned-source test does NOT cover. During a rolling upgrade an OLD keyed source still holds the cluster
// key, so its request is perfectly signed — it simply carries no lease_gen (the field did not exist), which
// decodes as 0. Against a CLAIMED zone (curGen > 0) that must be refused, so the drain fails closed and the
// source keeps the zone rather than the destination adopting on an unfenced request.
//
// (It also fails the signature check, since the old digest covered issued_at — but the fence must refuse it on
// its own, or a future digest change would silently reopen the hole.)
func TestVerifyAdoptZoneRefusesAKeyedSourceThatSendsNoGeneration(t *testing.T) {
	pub, priv := adoptKeys(t)
	req := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 0)
	if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", 5); !errors.Is(err, ErrAdoptZoneStale) {
		t.Fatalf("a request carrying no lease generation must not adopt a claimed zone, got %v", err)
	}
}

// TestVerifyAdoptZoneRefusesASourceThatDoesNotOwnTheZone is #316: the check the generation fence cannot make.
//
// The fence's transitive argument — "gen unchanged since the source read it, therefore the owner is unchanged"
// — silently assumes the source truthfully NAMED itself. A shard that misnames its source (desynced, lagging,
// mid-partition, buggy) satisfies the generation and would otherwise make this shard build the zone: rooms,
// resets, mob spawns, an actor goroutine. Renewal is owner-fenced so it could never take ownership, and
// ShardForZone would never route to it — but nothing un-adopts it either (#327), so it lingers as an orphan.
//
// Be honest about the security value: this is NOT a barrier against a leaked cluster key. `owner` and `gen`
// come from the same directory hash in the same read, so anyone who can satisfy the fence has already learned
// the owner and can name it. What #316 buys is enforcing, at the destination, the precondition the source
// already asserts — and failing closed BEFORE any state work rather than after the flip is refused.
func TestVerifyAdoptZoneRefusesASourceThatDoesNotOwnTheZone(t *testing.T) {
	pub, priv := adoptKeys(t)

	t.Run("misnamed source", func(t *testing.T) {
		// Correctly signed, and at the live generation. Only from_shard_id names a shard that does not own
		// the zone.
		req := signedAdopt(priv, "midgaard", "shard-evil", "shard-b", 7)
		if err := verifyAdoptZoneLease(req, "shard-a", 7); !errors.Is(err, ErrAdoptZoneNotOwner) {
			t.Fatalf("a source that does not own the zone has nothing to hand over, got %v", err)
		}
		if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", 7); !errors.Is(err, ErrAdoptZoneNotOwner) {
			t.Fatalf("the composed verifier must refuse it too, got %v", err)
		}
	})

	t.Run("unattributed source", func(t *testing.T) {
		// from_shard_id is inside the digest, so an empty one is a legitimately signable request. It can never
		// match a real owner, and it must not be treated as a wildcard.
		req := signedAdopt(priv, "midgaard", "", "shard-b", 7)
		if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", 7); !errors.Is(err, ErrAdoptZoneNotOwner) {
			t.Fatalf("a request naming no source must be refused, got %v", err)
		}
	})

	t.Run("lapsed lease", func(t *testing.T) {
		// The zone has a generation but no live owner: the source crashed, or was partitioned long enough for
		// its lease to expire. Nobody can hand over a zone nobody owns — the flip would be refused anyway, and
		// building the zone here would leave an orphan.
		req := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)
		if err := verifyAdoptZone(pub, req, "shard-b", "", 7); !errors.Is(err, ErrAdoptZoneNotOwner) {
			t.Fatalf("a lapsed lease has no owner to hand the zone over, got %v", err)
		}
	})

	t.Run("the live owner is accepted", func(t *testing.T) {
		req := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)
		if err := verifyAdoptZone(pub, req, "shard-b", "shard-a", 7); err != nil {
			t.Fatalf("the zone's live owner must still be able to hand it over, got %v", err)
		}
	})
}

// TestVerifyAdoptZoneChecksTheGenerationBeforeTheOwner pins the refusal ORDER, which is what makes the two
// log lines mean what they say. A stale request from a source that has since lost the zone (the ordinary
// post-flip replay) must report STALENESS — the operator should not go hunting for a forged source when what
// actually happened is that the handover completed.
func TestVerifyAdoptZoneChecksTheGenerationBeforeTheOwner(t *testing.T) {
	_, priv := adoptKeys(t)
	// A's request, captured; the flip has since moved the zone to B and bumped the generation. Both checks
	// would refuse it. The generation is the more specific truth.
	replayed := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)
	if err := verifyAdoptZoneLease(replayed, "shard-b", 8); !errors.Is(err, ErrAdoptZoneStale) {
		t.Fatalf("a post-flip replay must be reported as stale, not as a forged source, got %v", err)
	}
}

// TestVerifyAdoptZoneDistinguishesStaleFromForgery: the server logs the two apart — a stale-but-authentic
// request means the handover already landed (or another shard won the flip), while a bad signature means a
// wrong key or a forgery, and an operator staring at a stalled drain needs to tell those apart. Both become
// the same opaque PermissionDenied on the wire.
func TestVerifyAdoptZoneDistinguishesStaleFromForgery(t *testing.T) {
	pub, priv := adoptKeys(t)

	stale := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)
	if err := verifyAdoptZone(pub, stale, "shard-b", "shard-a", 8); !errors.Is(err, ErrAdoptZoneStale) || errors.Is(err, ErrAdoptZoneSig) {
		t.Fatalf("an authentic but consumed request must report staleness, got %v", err)
	}

	forged := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)
	forged.ZoneSig[0] ^= 0xff
	if err := verifyAdoptZone(pub, forged, "shard-b", "shard-a", 7); !errors.Is(err, ErrAdoptZoneSig) || errors.Is(err, ErrAdoptZoneStale) {
		t.Fatalf("a bad signature must report forgery, not staleness, got %v", err)
	}

	notOwner := signedAdopt(priv, "midgaard", "shard-evil", "shard-b", 7)
	if err := verifyAdoptZone(pub, notOwner, "shard-b", "shard-a", 7); !errors.Is(err, ErrAdoptZoneNotOwner) ||
		errors.Is(err, ErrAdoptZoneSig) || errors.Is(err, ErrAdoptZoneStale) {
		t.Fatalf("a misnamed source must report its own reason, not staleness or forgery, got %v", err)
	}
}

// TestAdoptZoneSignatureIsDomainSeparated: Prepare and AdoptZone share one cluster keypair, so their digests
// must live in different domains. Otherwise a captured Prepare signature could be presented as an AdoptZone
// authorization (or vice versa) — a cross-protocol forgery with no key compromise at all.
func TestAdoptZoneSignatureIsDomainSeparated(t *testing.T) {
	pub, priv := adoptKeys(t)

	// A Prepare signature over the same key must not authenticate an AdoptZone.
	prep := newSignedPrepare()
	prep.SnapshotSig = signSnapshot(priv, prep)
	borrowed := &handoffv1.AdoptZoneRequest{
		ZoneId: "midgaard", FromShardId: "shard-a", ToShardId: "shard-b",
		LeaseGen: 7, ZoneSig: prep.GetSnapshotSig(),
	}
	if err := verifyAdoptZone(pub, borrowed, "shard-b", "shard-a", 7); err == nil {
		t.Fatal("a Prepare signature must not authenticate an AdoptZone (domain separation)")
	}

	// …and an AdoptZone signature must not authenticate a Prepare.
	adopt := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)
	prep2 := newSignedPrepare()
	prep2.SnapshotSig = adopt.GetZoneSig()
	if err := verifySnapshot(pub, prep2); err == nil {
		t.Fatal("an AdoptZone signature must not authenticate a Prepare (domain separation)")
	}
}

// TestKeyedAdoptZoneRejectsUnsignedRequest is the server-level pin: the #262 hole was that a KEYED shard
// reached HostZone on an unauthenticated request. It must be PermissionDenied before any state work — and
// unlike the keyless gate, the insecure opt-in cannot lift it.
func TestKeyedAdoptZoneRejectsUnsignedRequest(t *testing.T) {
	pub, _ := adoptKeys(t)
	shard := NewDemoShard().WithHandoffKeys(nil, pub).WithInsecureHandoff(true).
		WithZoneLeasing(stubLeaser{owner: "shard-a", gen: 7}, "shard-b", 0, 0, nil)
	keyed := &handoffServer{shard: shard}

	_, err := keyed.AdoptZone(context.Background(), &handoffv1.AdoptZoneRequest{ZoneId: "midgaard"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a keyed shard must refuse an unsigned AdoptZone with PermissionDenied, got %v", err)
	}
}

// TestKeyedAdoptZoneAcceptsSignedRequest: the same shard adopts when the request is properly signed, bound, and
// at the live generation — proving the gate authenticates rather than simply blocking AdoptZone outright.
func TestKeyedAdoptZoneAcceptsSignedRequest(t *testing.T) {
	pub, priv := adoptKeys(t)
	shard := NewDemoShard().WithHandoffKeys(priv, pub).
		WithZoneLeasing(stubLeaser{owner: "shard-a", gen: 7}, "shard-b", 0, 0, nil)
	keyed := &handoffServer{shard: shard}

	resp, err := keyed.AdoptZone(context.Background(), signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7))
	if err != nil {
		t.Fatalf("a correctly signed AdoptZone must be accepted, got %v", err)
	}
	if !resp.GetHosted() {
		t.Fatal("the zone should be hosted after a successful adopt")
	}
}

// TestKeyedAdoptZoneRejectsStaleGenerationOverRPC closes the gap a unit test of verifyAdoptZone alone leaves:
// it does not prove the SERVER reads the live generation from the directory and passes it in. A server that
// forwarded req.LeaseGen as curGen would pass every verifier test above and fence nothing.
//
// Name a zone this shard does NOT already host, so "did any state work happen?" is observable — a pre-hosted
// zone would hit HostZone's idempotent early return and tell us nothing.
func TestKeyedAdoptZoneRejectsStaleGenerationOverRPC(t *testing.T) {
	pub, priv := adoptKeys(t)
	const zone = "darkwood"

	shard := NewDemoShard().WithHandoffKeys(priv, pub).
		WithZoneLeasing(stubLeaser{owner: "shard-a", gen: 8}, "shard-b", 0, 0, nil)
	if shard.ZoneByID(zone) != nil {
		t.Fatalf("precondition: %s must not already be hosted", zone)
	}
	keyed := &handoffServer{shard: shard}

	// Authentic, correctly bound — but signed at generation 7 while the directory has moved to 8. This is a
	// replay of a handover that already completed.
	stale := signedAdopt(priv, zone, "shard-a", "shard-b", 7)
	if _, err := keyed.AdoptZone(context.Background(), stale); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("the server must refuse a stale-generation request with PermissionDenied, got %v", err)
	}
	if shard.ZoneByID(zone) != nil {
		t.Fatal("the zone was hosted despite a refused AdoptZone — auth must precede all state work")
	}

	// The very same request at the generation the directory actually holds gets PAST auth and reaches
	// HostZone, which refuses for its own reason (this bare shard isn't running). Landing on
	// FailedPrecondition rather than PermissionDenied is what proves the earlier refusal was the fence.
	fresh := signedAdopt(priv, zone, "shard-a", "shard-b", 8)
	if _, err := keyed.AdoptZone(context.Background(), fresh); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a request at the live generation must clear auth and reach HostZone, got %v", err)
	}
}

// TestKeyedAdoptZoneRefusesAMisnamedSourceOverRPC is #316 at the RPC boundary. A verifier unit test alone
// would pass on a server that read the generation but threw the owner away — which is exactly what the server
// did before this change.
//
// The request is correctly signed and carries the live generation; only from_shard_id names a shard that does
// not own the zone. Name a zone this shard does NOT already host, so "did any state work happen?" is
// observable: a pre-hosted zone would hit HostZone's idempotent early return and prove nothing.
func TestKeyedAdoptZoneRefusesAMisnamedSourceOverRPC(t *testing.T) {
	pub, priv := adoptKeys(t)
	const zone = "darkwood"

	shard := NewDemoShard().WithHandoffKeys(priv, pub).
		WithZoneLeasing(stubLeaser{owner: "shard-a", gen: 7}, "shard-b", 0, 0, nil)
	if shard.ZoneByID(zone) != nil {
		t.Fatalf("precondition: %s must not already be hosted", zone)
	}
	keyed := &handoffServer{shard: shard}

	misnamed := signedAdopt(priv, zone, "shard-evil", "shard-b", 7)
	if _, err := keyed.AdoptZone(context.Background(), misnamed); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a request from a source that does not own the zone must be refused, got %v", err)
	}
	if shard.ZoneByID(zone) != nil {
		t.Fatal("the zone was BUILT for a source that never owned it — an orphan with no un-adopt path (#316)")
	}

	// The control: the same request from the zone's real owner clears auth and reaches HostZone, which refuses
	// for its own reason (this bare shard isn't running). Landing on FailedPrecondition rather than
	// PermissionDenied is what proves the earlier refusal was the owner check.
	honest := signedAdopt(priv, zone, "shard-a", "shard-b", 7)
	if _, err := keyed.AdoptZone(context.Background(), honest); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("the zone's live owner must clear auth and reach HostZone, got %v", err)
	}
}

// TestKeyedAdoptZoneFailsClosedWithoutAReadableGeneration: the fence is only as good as the read behind it.
// A keyed shard that cannot learn the zone's current generation must refuse, never fall back to an unfenced
// verify — that fallback would be the whole #315 window, reachable by anyone who can break the destination's
// path to Redis.
func TestKeyedAdoptZoneFailsClosedWithoutAReadableGeneration(t *testing.T) {
	pub, priv := adoptKeys(t)
	req := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)

	t.Run("directory unreachable", func(t *testing.T) {
		shard := NewDemoShard().WithHandoffKeys(priv, pub).
			WithZoneLeasing(stubLeaser{err: errors.New("redis: connection refused")}, "shard-b", 0, 0, nil)
		if _, err := (&handoffServer{shard: shard}).AdoptZone(context.Background(), req); status.Code(err) != codes.Unavailable {
			t.Fatalf("a directory read failure must fail closed as Unavailable (retryable), got %v", err)
		}
	})
	t.Run("no leaser configured", func(t *testing.T) {
		// A keyed shard is by definition in a multi-shard cluster, which leases its zones. A keyed shard with
		// no leaser cannot check the fence, so it must not adopt at all.
		shard := NewDemoShard().WithHandoffKeys(priv, pub)
		shard.shardID = "shard-b"
		if _, err := (&handoffServer{shard: shard}).AdoptZone(context.Background(), req); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("a keyed shard with no leaser must refuse, got %v", err)
		}
	})
}

// TestKeyedZoneHandoverEndToEnd is the integration proof: two KEYED shards, a real gRPC AdoptZone over
// bufconn, a real Redis directory, and a full lease flip. It exercises the source's signing path
// (lease.go handoverZoneTo) and the destination's verification path together — a unit test of verifyAdoptZone
// alone would pass even if the source never attached a signature, or attached one over the wrong fields.
//
// The centerpiece is the CAPTURED-REQUEST REPLAY (#315). We mint exactly the request the source sends,
// complete the handover, and then present that byte-identical request to B's handoff server. Under #262 it
// would be accepted (inside its 60s window, HostZone idempotent). Now the flip has consumed its generation, so
// B refuses it.
//
// It also pins the FAIL-CLOSED direction: an UNKEYED source cannot hand a zone to a keyed destination (it
// signs nothing), so the drain aborts and the source keeps the zone.
func TestKeyedZoneHandoverEndToEnd(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	pub, priv := adoptKeys(t)

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")

	ctx := context.Background()
	mustReg(t, dir.RegisterShard(ctx, "shard-a", "addr-a", directory.DefaultShardLease))
	mustReg(t, dir.RegisterShard(ctx, "shard-b", "addr-b", directory.DefaultShardLease))
	mustReg(t, dir.RegisterZone(ctx, "midgaard", "shard-a"))

	lisA := bufconn.Listen(1 << 20)
	lisB := bufconn.Listen(1 << 20)
	lisByAddr := map[string]*bufconn.Listener{"addr-a": lisA, "addr-b": lisB}
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		lis := lisByAddr[addr]
		if lis == nil {
			return nil, fmt.Errorf("unknown shard %q", addr)
		}
		return handoffv1.NewHandoffClient(dialBuf(t, lis)), nil
	}
	noFence := func() {}

	// Both shards hold the shared cluster keypair. B is a keyed standby: it will only adopt an authenticated,
	// in-generation request. A signs with the same key.
	shardA := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "addr-a", dir, peers).
		WithHandoffKeys(priv, pub).
		WithZoneLeasing(dir, "shard-a", time.Second, 80*time.Millisecond, noFence)
	shardB := NewShardFromContent(lc, nil, "", "addr-b", dir, peers).
		WithHandoffKeys(priv, pub).
		WithZoneLeasing(dir, "shard-b", time.Second, 80*time.Millisecond, noFence)
	serveShard(t, shardA, lisA)
	serveShard(t, shardB, lisB)

	// Mint the request A is about to send, at the generation A holds midgaard's lease at. Keeping a copy is
	// exactly what an attacker who can read one AdoptZone off the wire has.
	var captured *handoffv1.AdoptZoneRequest
	waitCond(t, "shard-a owns midgaard's lease", func() bool {
		owner, gen, lerr := dir.ZoneLease(ctx, "midgaard")
		if lerr != nil || owner != "shard-a" || gen == 0 {
			return false
		}
		captured = signedAdopt(priv, "midgaard", "shard-a", "shard-b", gen)
		return true
	})

	// The captured request is good RIGHT NOW: B would adopt on it. (Proving it is live before the flip is what
	// makes the post-flip refusal below mean something — otherwise a typo'd request would "pass" this test.)
	bSrv := &handoffServer{shard: shardB}
	if _, err := bSrv.AdoptZone(ctx, captured); err != nil {
		t.Fatalf("before the flip, an in-generation request must be adoptable (retry path), got %v", err)
	}

	// Call the handover exactly ONCE. It flips ownership as a side effect, so retrying it in a waitCond would
	// turn "it worked on the second try" into a pass, and a first-attempt flip that then errored into a
	// confusing timeout instead of a clear failure. The ownership precondition is what deserved the wait.
	if err := shardA.handoverZoneTo(ctx, "midgaard", "shard-b", "addr-b"); err != nil {
		t.Fatalf("the keyed handover A->B must succeed, got %v", err)
	}
	if shardB.ZoneByID("midgaard") == nil {
		t.Fatal("B does not host midgaard after a signed AdoptZone")
	}
	owner, err := dir.ShardForZone(ctx, "midgaard")
	if err != nil || owner != "shard-b" {
		t.Fatalf("owner after handover = %q (err %v), want shard-b", owner, err)
	}

	// #315: THE REPLAY. Byte-identical to the request B accepted moments ago, still perfectly signed. The flip
	// incremented midgaard's lease generation, so the capability it carried is spent.
	if _, err := bSrv.AdoptZone(ctx, captured); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a captured AdoptZone replayed after the flip must be refused, got %v", err)
	}
	// PermissionDenied is deliberately uniform on the wire, so pin the REASON directly: the refusal must come
	// from the generation fence, not from some incidental signature problem. (The pre-flip accept above already
	// proves the signature is good; this proves what changed.)
	postFlipOwner, postFlipGen, err := dir.ZoneLease(ctx, "midgaard")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyAdoptZoneLease(captured, postFlipOwner, postFlipGen); !errors.Is(err, ErrAdoptZoneStale) {
		t.Fatalf("the replay must be refused BY THE FENCE (request_gen=%d, current_gen=%d), got %v",
			captured.GetLeaseGen(), postFlipGen, err)
	}

	// And the source-side retry is refused too: A is no longer the live owner, so it aborts before making B
	// build anything, and ownership is untouched.
	if err := shardA.handoverZoneTo(ctx, "midgaard", "shard-b", "addr-b"); err == nil {
		t.Fatal("a second handover after the flip must be refused, not silently re-run")
	}
	owner, err = dir.ShardForZone(ctx, "midgaard")
	if err != nil || owner != "shard-b" {
		t.Fatalf("owner after a replayed handover = %q (err %v), want shard-b — ownership must be untouched", owner, err)
	}

	// REBALANCE-BACK (A->B->A). The generation fence has to compose with #288's re-adoption path, where the
	// zone object is still in A's s.zones and HostZone takes its idempotent early return before restarting the
	// lease renewal. B signs the generation it now holds; A adopts and re-adopts its own old zone.
	_, genAtB, err := dir.ZoneLease(ctx, "midgaard")
	if err != nil {
		t.Fatal(err)
	}
	if err := shardB.handoverZoneTo(ctx, "midgaard", "shard-a", "addr-a"); err != nil {
		t.Fatalf("handing midgaard back to A must succeed, got %v", err)
	}
	owner, backGen, err := dir.ZoneLease(ctx, "midgaard")
	if err != nil || owner != "shard-a" {
		t.Fatalf("owner after the return leg = %q (err %v), want shard-a", owner, err)
	}
	if backGen != genAtB+1 {
		t.Fatalf("the return leg must bump the generation exactly once: %d -> %d", genAtB, backGen)
	}
	// And the FIRST leg's captured request is still dead — two ownership changes later, it has not come back
	// around. This is the property the clock-seeded counter exists to guarantee.
	if _, err := bSrv.AdoptZone(ctx, captured); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("the first leg's captured AdoptZone must stay dead across a rebalance-back, got %v", err)
	}

	// #316 END-TO-END: a correctly signed request at darkwood's real current generation, naming a source that
	// does not own it, asks B to adopt a zone B does not host. B must refuse before building anything.
	mustReg(t, dir.RegisterZone(ctx, "darkwood", "shard-a"))
	if _, err := dir.ClaimZone(ctx, "darkwood", "shard-a", time.Second); err != nil {
		t.Fatal(err)
	}
	darkOwner, darkGen, err := dir.ZoneLease(ctx, "darkwood")
	if err != nil || darkOwner != "shard-a" {
		t.Fatalf("precondition: darkwood must be owned by shard-a, got %q (err %v)", darkOwner, err)
	}
	misnamed := signedAdopt(priv, "darkwood", "shard-evil", "shard-b", darkGen)
	if _, err := bSrv.AdoptZone(ctx, misnamed); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a request naming a source that does not own darkwood must be refused, got %v", err)
	}
	if shardB.ZoneByID("darkwood") != nil {
		t.Fatal("B built darkwood for a source that never owned it — an orphan zone with no un-adopt path (#316)")
	}

	// FAIL-CLOSED: an UNKEYED source attaches no signature, so the keyed destination refuses. Hand darkwood
	// from an unkeyed A' to the keyed B.
	unkeyedA := NewShardFromContent(lc, []string{"darkwood"}, "darkwood", "addr-a", dir, peers).
		WithZoneLeasing(dir, "shard-a", time.Second, 80*time.Millisecond, noFence)
	err = unkeyedA.handoverZoneTo(ctx, "darkwood", "shard-b", "addr-b")
	if err == nil {
		t.Fatal("a keyed destination must REFUSE an unsigned AdoptZone from an unkeyed source (fail closed)")
	}
	// …and it must fail at the AUTH gate, not incidentally (a missing lease, an undialable peer). Otherwise
	// this assertion would pass even with #262 still open.
	if status.Code(errors.Unwrap(err)) != codes.PermissionDenied {
		t.Fatalf("the unsigned handover must be refused with PermissionDenied from AdoptZone, got %v", err)
	}
	if shardB.ZoneByID("darkwood") != nil {
		t.Fatal("B adopted darkwood on an unauthenticated request — the #262 hole is still open")
	}
}

// TestHandoverAbortsWhenTheGenerationCannotBeRead is the source-side half of failing closed. The source signs
// the generation it READS; if that read fails, or reports someone else as owner, it must not fabricate a
// generation (0, or a remembered one) and send the request anyway — a signature over a guessed generation is
// either dead on arrival or, worse, accidentally live.
func TestHandoverAbortsWhenTheGenerationCannotBeRead(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	pub, priv := adoptKeys(t)

	// A peer dialer that records every AdoptZone it is asked to make. If the source aborts correctly, it is
	// never called at all.
	var mu sync.Mutex
	dialed := 0
	peers := func(string) (handoffv1.HandoffClient, error) {
		mu.Lock()
		dialed++
		mu.Unlock()
		return nil, errors.New("dial should never be reached")
	}
	dials := func() int { mu.Lock(); defer mu.Unlock(); return dialed }

	for _, tc := range []struct {
		name   string
		leaser ZoneLeaser
	}{
		{"read fails", stubLeaser{err: errors.New("redis down")}},
		{"another shard owns it", stubLeaser{owner: "shard-c", gen: 9}},
		{"nobody owns it", stubLeaser{owner: "", gen: 9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := dials()
			sh := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "addr-a", nil, peers).
				WithHandoffKeys(priv, pub).
				WithZoneLeasing(tc.leaser, "shard-a", time.Second, 0, nil)
			if err := sh.handoverZoneTo(context.Background(), "midgaard", "shard-b", "addr-b"); err == nil {
				t.Fatal("the handover must abort when the source cannot confirm the generation it owns")
			}
			if dials() != before {
				t.Fatal("the source contacted the target before confirming its own lease generation")
			}
		})
	}
}

// countingLeaser records how many times the directory was consulted.
type countingLeaser struct {
	stubLeaser
	mu    sync.Mutex
	reads int
}

func (c *countingLeaser) ZoneLease(ctx context.Context, zone string) (string, uint64, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.stubLeaser.ZoneLease(ctx, zone)
}
func (c *countingLeaser) count() int { c.mu.Lock(); defer c.mu.Unlock(); return c.reads }

// TestForgedAdoptZoneNeverReachesTheDirectory pins the CHECK ORDER, which is a security property in its own
// right, not a micro-optimization.
//
// The handoff port takes unauthenticated connections — that is the premise of #262. The generation fence needs
// a read from the cluster's shared directory Redis, the store every shard, the placement coordinator and
// leader election all depend on. If that read happened before the signature check, anyone with network reach
// to a world port could turn a stream of garbage AdoptZone requests into amplified load on the one component
// whose failure takes the whole fleet's control plane down. A forged request must cost this shard a local
// Ed25519 verify and nothing else.
func TestForgedAdoptZoneNeverReachesTheDirectory(t *testing.T) {
	pub, priv := adoptKeys(t)
	_, otherPriv := adoptKeys(t)

	for _, tc := range []struct {
		name string
		req  *handoffv1.AdoptZoneRequest
	}{
		{"unsigned", &handoffv1.AdoptZoneRequest{ZoneId: "midgaard", ToShardId: "shard-b", LeaseGen: 7}},
		{"no source named", signedAdopt(otherPriv, "midgaard", "", "shard-b", 7)},
		{"wrong key", signedAdopt(otherPriv, "midgaard", "shard-a", "shard-b", 7)},
		{"garbage signature", func() *handoffv1.AdoptZoneRequest {
			r := signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)
			r.ZoneSig[0] ^= 0xff
			return r
		}()},
		{"addressed to another shard", signedAdopt(priv, "midgaard", "shard-a", "shard-c", 7)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaser := &countingLeaser{stubLeaser: stubLeaser{owner: "shard-a", gen: 7}}
			shard := NewDemoShard().WithHandoffKeys(priv, pub).WithZoneLeasing(leaser, "shard-b", 0, 0, nil)

			_, err := (&handoffServer{shard: shard}).AdoptZone(context.Background(), tc.req)
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("a forged request must be refused with PermissionDenied, got %v", err)
			}
			if n := leaser.count(); n != 0 {
				t.Fatalf("a forged request hit the shared directory %d time(s) — unauthenticated input must "+
					"never trigger a remote read", n)
			}
		})
	}

	// A forged request must also never learn that the directory is DOWN. The `Unavailable` code exists so a
	// legitimate source can tell a transient directory fault from an auth verdict; if an unauthenticated caller
	// could elicit it, the handoff port would double as a Redis health oracle for anyone with network reach.
	// The signature check running first is what prevents that — this pins it against a refactor.
	down := &countingLeaser{stubLeaser: stubLeaser{err: errors.New("redis: connection refused")}}
	shard := NewDemoShard().WithHandoffKeys(priv, pub).WithZoneLeasing(down, "shard-b", 0, 0, nil)
	unsigned := &handoffv1.AdoptZoneRequest{ZoneId: "midgaard", ToShardId: "shard-b", LeaseGen: 7}
	if _, err := (&handoffServer{shard: shard}).AdoptZone(context.Background(), unsigned); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("an unsigned request must get the uniform auth verdict even when the directory is down, got %v",
			status.Code(err))
	}
	if down.count() != 0 {
		t.Fatal("an unsigned request probed the directory")
	}

	// The control: an AUTHENTIC request does consult the directory. Without this, the assertions above would
	// pass on a server that never reads the generation at all.
	leaser := &countingLeaser{stubLeaser: stubLeaser{owner: "shard-a", gen: 7}}
	ok := NewDemoShard().WithHandoffKeys(priv, pub).WithZoneLeasing(leaser, "shard-b", 0, 0, nil)
	if _, err := (&handoffServer{shard: ok}).AdoptZone(context.Background(), signedAdopt(priv, "midgaard", "shard-a", "shard-b", 7)); err != nil {
		t.Fatalf("an authentic request must be accepted, got %v", err)
	}
	if leaser.count() != 1 {
		t.Fatalf("an authentic request must read the live generation exactly once, got %d reads", leaser.count())
	}
}

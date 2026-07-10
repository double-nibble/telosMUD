package world

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math"
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

// handoffsig_adoptzone_test.go — #262. AdoptZone drives HostZone + lease renewal, so before this it was the
// weakest link in the handoff surface: #260 refused the KEYLESS case, but a KEYED cluster still adopted on a
// wholly unauthenticated request. Anyone with network reach to a world port could forge AdoptZone(zoneID) —
// a forced-host / lease-takeover / resource-exhaustion vector.
//
// Unlike Prepare (whose digest binds a monotonically-rejected epoch), AdoptZone has no natural anti-replay
// field, so the signature binds the DESTINATION and an ISSUE TIME. These tests pin both, because a signature
// over zone_id alone would be a permanent, transferable capability to force any shard to host that zone.

func adoptKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// signedAdopt builds a request as the real source does: bind the destination + now, then sign.
func signedAdopt(priv ed25519.PrivateKey, zone, from, to string) *handoffv1.AdoptZoneRequest {
	req := &handoffv1.AdoptZoneRequest{
		ZoneId: zone, FromShardId: from, ToShardId: to, IssuedAtUnixMs: time.Now().UnixMilli(),
	}
	req.ZoneSig = signAdoptZone(priv, req)
	return req
}

// TestVerifyAdoptZoneAcceptsFreshSignedRequest: the happy path — a request signed by the cluster key, naming
// this shard, issued now.
func TestVerifyAdoptZoneAcceptsFreshSignedRequest(t *testing.T) {
	pub, priv := adoptKeys(t)
	req := signedAdopt(priv, "midgaard", "shard-a", "shard-b")
	if err := verifyAdoptZone(pub, req, "shard-b", time.Now()); err != nil {
		t.Fatalf("a fresh, correctly-signed request for this shard must verify, got %v", err)
	}
}

// TestVerifyAdoptZoneRejectsForgeryAndTampering: every field in the digest is load-bearing. A request with no
// signature, a signature from the wrong key, or ANY mutated field after signing must be refused.
func TestVerifyAdoptZoneRejectsForgeryAndTampering(t *testing.T) {
	pub, priv := adoptKeys(t)
	_, otherPriv := adoptKeys(t)

	t.Run("unsigned", func(t *testing.T) {
		req := &handoffv1.AdoptZoneRequest{ZoneId: "midgaard", ToShardId: "shard-b", IssuedAtUnixMs: time.Now().UnixMilli()}
		if err := verifyAdoptZone(pub, req, "shard-b", time.Now()); err == nil {
			t.Fatal("a keyed shard must refuse an unsigned AdoptZone — that is the whole #262 hole")
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		req := signedAdopt(otherPriv, "midgaard", "shard-a", "shard-b")
		if err := verifyAdoptZone(pub, req, "shard-b", time.Now()); err == nil {
			t.Fatal("a signature from outside the cluster keypair must be refused")
		}
	})
	for _, tc := range []struct {
		name   string
		mutate func(*handoffv1.AdoptZoneRequest)
	}{
		{"zone_id", func(r *handoffv1.AdoptZoneRequest) { r.ZoneId = "darkwood" }},
		{"from_shard_id", func(r *handoffv1.AdoptZoneRequest) { r.FromShardId = "shard-evil" }},
		{"issued_at", func(r *handoffv1.AdoptZoneRequest) { r.IssuedAtUnixMs++ }},
		{"signature", func(r *handoffv1.AdoptZoneRequest) { r.ZoneSig[0] ^= 0xff }},
	} {
		t.Run("tampered "+tc.name, func(t *testing.T) {
			req := signedAdopt(priv, "midgaard", "shard-a", "shard-b")
			tc.mutate(req)
			if err := verifyAdoptZone(pub, req, "shard-b", time.Now()); err == nil {
				t.Fatalf("mutating %s after signing must invalidate the request", tc.name)
			}
		})
	}

	// to_shard_id needs its own shape: rewriting it to REDIRECT the capability must fail on the DIGEST, not
	// merely on the destination-binding check. So verify at the shard the attacker rewrote it to — if
	// to_shard_id sat outside the signed digest, that shard would happily adopt.
	t.Run("tampered to_shard_id (redirected at another shard)", func(t *testing.T) {
		req := signedAdopt(priv, "midgaard", "shard-a", "shard-b")
		req.ToShardId = "shard-c"
		if err := verifyAdoptZone(pub, req, "shard-c", time.Now()); err == nil {
			t.Fatal("rewriting to_shard_id must break the signature — otherwise the capability is redirectable")
		}
	})
}

// TestVerifyAdoptZoneBindsDestinationShard is the anti-replay-across-the-fleet property: a signature minted
// to make shard-b host a zone is worthless at shard-c. Without this, one captured request would be a
// cluster-wide capability to force ANY shard to host that zone.
func TestVerifyAdoptZoneBindsDestinationShard(t *testing.T) {
	pub, priv := adoptKeys(t)
	req := signedAdopt(priv, "midgaard", "shard-a", "shard-b")

	if err := verifyAdoptZone(pub, req, "shard-b", time.Now()); err != nil {
		t.Fatalf("the named destination must accept it, got %v", err)
	}
	if err := verifyAdoptZone(pub, req, "shard-c", time.Now()); err == nil {
		t.Fatal("a request naming shard-b must NOT be replayable at shard-c")
	}

	// An unbound request (no destination) is refused even with a valid signature over it.
	unbound := &handoffv1.AdoptZoneRequest{ZoneId: "midgaard", FromShardId: "shard-a", IssuedAtUnixMs: time.Now().UnixMilli()}
	unbound.ZoneSig = signAdoptZone(priv, unbound)
	if err := verifyAdoptZone(pub, unbound, "shard-b", time.Now()); err == nil {
		t.Fatal("a request that names no destination must be refused (it would be replayable anywhere)")
	}
}

// TestVerifyAdoptZoneExpiresOutsideSkewWindow is the anti-replay-over-time property: a captured request stops
// working once it ages out, in EITHER direction (a far-future stamp must not buy a long-lived capability).
// Inside the window a replay is deliberately accepted — HostZone is idempotent, so re-adopting the same zone
// at the same shard is a no-op.
func TestVerifyAdoptZoneExpiresOutsideSkewWindow(t *testing.T) {
	pub, priv := adoptKeys(t)
	req := signedAdopt(priv, "midgaard", "shard-a", "shard-b")
	issued := time.UnixMilli(req.GetIssuedAtUnixMs())

	if err := verifyAdoptZone(pub, req, "shard-b", issued.Add(adoptZoneMaxSkew-time.Second)); err != nil {
		t.Fatalf("a replay INSIDE the window is accepted (HostZone is idempotent), got %v", err)
	}
	if err := verifyAdoptZone(pub, req, "shard-b", issued.Add(adoptZoneMaxSkew+time.Second)); err == nil {
		t.Fatal("a captured request must expire once it ages past the skew window")
	}
	if err := verifyAdoptZone(pub, req, "shard-b", issued.Add(-(adoptZoneMaxSkew + time.Second))); err == nil {
		t.Fatal("a far-future issue time must be refused, not granted a long life")
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
		IssuedAtUnixMs: time.Now().UnixMilli(), ZoneSig: prep.GetSnapshotSig(),
	}
	if err := verifyAdoptZone(pub, borrowed, "shard-b", time.Now()); err == nil {
		t.Fatal("a Prepare signature must not authenticate an AdoptZone (domain separation)")
	}

	// …and an AdoptZone signature must not authenticate a Prepare.
	adopt := signedAdopt(priv, "midgaard", "shard-a", "shard-b")
	prep2 := newSignedPrepare()
	prep2.SnapshotSig = adopt.GetZoneSig()
	if err := verifySnapshot(pub, prep2); err == nil {
		t.Fatal("an AdoptZone signature must not authenticate a Prepare (domain separation)")
	}
}

// TestKeyedAdoptZoneRejectsUnsignedRequest is the server-level pin: the #262 hole was that a KEYED shard
// reached HostZone on an unauthenticated request. It must now be PermissionDenied before any state work — and
// unlike the keyless gate, the insecure opt-in cannot lift it.
func TestKeyedAdoptZoneRejectsUnsignedRequest(t *testing.T) {
	pub, _ := adoptKeys(t)
	shard := NewDemoShard().WithHandoffKeys(nil, pub).WithInsecureHandoff(true)
	shard.shardID = "shard-b"
	keyed := &handoffServer{shard: shard}

	_, err := keyed.AdoptZone(context.Background(), &handoffv1.AdoptZoneRequest{ZoneId: "midgaard"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a keyed shard must refuse an unsigned AdoptZone with PermissionDenied, got %v", err)
	}
}

// TestKeyedAdoptZoneAcceptsSignedRequest: the same shard adopts when the request is properly signed and bound
// — proving the gate authenticates rather than simply blocking AdoptZone outright.
func TestKeyedAdoptZoneAcceptsSignedRequest(t *testing.T) {
	pub, priv := adoptKeys(t)
	shard := NewDemoShard().WithHandoffKeys(priv, pub)
	shard.shardID = "shard-b"
	keyed := &handoffServer{shard: shard}

	req := signedAdopt(priv, "midgaard", "shard-a", "shard-b")
	resp, err := keyed.AdoptZone(context.Background(), req)
	if err != nil {
		t.Fatalf("a correctly signed AdoptZone must be accepted, got %v", err)
	}
	if !resp.GetHosted() {
		t.Fatal("the zone should be hosted after a successful adopt")
	}
}

// TestKeyedZoneHandoverEndToEnd is the integration proof for #262: two KEYED shards, a real gRPC AdoptZone
// over bufconn, and a full lease flip. It exercises the source's signing path (lease.go handoverZoneTo) and
// the destination's verification path together — a unit test of verifyAdoptZone alone would pass even if the
// source never attached a signature, or attached one over the wrong fields.
//
// It also pins the FAIL-CLOSED direction: an UNKEYED source cannot hand a zone to a keyed destination (it
// signs nothing), so the drain aborts and the source keeps the zone rather than the destination adopting on
// an unauthenticated request.
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

	// Both shards hold the shared cluster keypair. B is a keyed standby: it will only adopt an authenticated
	// request. A signs with the same key.
	shardA := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "addr-a", dir, peers).
		WithHandoffKeys(priv, pub).
		WithZoneLeasing(dir, "shard-a", time.Second, 80*time.Millisecond, noFence)
	shardB := NewShardFromContent(lc, nil, "", "addr-b", dir, peers).
		WithHandoffKeys(priv, pub).
		WithZoneLeasing(dir, "shard-b", time.Second, 80*time.Millisecond, noFence)
	serveShard(t, shardA, lisA)
	serveShard(t, shardB, lisB)

	waitCond(t, "keyed zone handover A->B succeeds", func() bool {
		return shardA.handoverZoneTo(ctx, "midgaard", "shard-b", "addr-b") == nil
	})
	if shardB.ZoneByID("midgaard") == nil {
		t.Fatal("B does not host midgaard after a signed AdoptZone")
	}
	owner, err := dir.ShardForZone(ctx, "midgaard")
	if err != nil || owner != "shard-b" {
		t.Fatalf("owner after handover = %q (err %v), want shard-b", owner, err)
	}

	// REPLAY AFTER THE FLIP: re-running the whole handover (a retry racing a completed one, or an attacker
	// replaying a captured AdoptZone inside its window) must not disturb ownership. AdoptZone is idempotent, and
	// the ownership-moving step is a FENCED CAS keyed on from==current-owner — which A no longer is — so the
	// second attempt is refused and B stays the owner. The single-writer spine holds.
	if err := shardA.handoverZoneTo(ctx, "midgaard", "shard-b", "addr-b"); err == nil {
		t.Fatal("a second handover after the flip must be refused by the fenced CAS, not silently re-run")
	}
	owner, err = dir.ShardForZone(ctx, "midgaard")
	if err != nil || owner != "shard-b" {
		t.Fatalf("owner after a replayed handover = %q (err %v), want shard-b — ownership must be untouched", owner, err)
	}

	// FAIL-CLOSED: an UNKEYED source attaches no signature, so the keyed destination refuses. Hand darkwood
	// (still on A per the directory default) from an unkeyed A' to the keyed B.
	mustReg(t, dir.RegisterZone(ctx, "darkwood", "shard-a"))
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

// TestVerifyAdoptZoneRejectsAbsurdFutureTimestamp is the regression for a real bug the security review caught.
// The natural way to write a symmetric skew check —
//
//	skew := now.Sub(time.UnixMilli(issuedAt)); if skew < 0 { skew = -skew }; if skew > max { reject }
//
// silently ACCEPTS an absurd far-future timestamp: time.Sub SATURATES at minDuration, and negating
// minDuration overflows back to minDuration (still negative), so the `> max` test is false and the request
// passes. That inverts the check's entire purpose. verifyAdoptZone therefore compares raw milliseconds.
func TestVerifyAdoptZoneRejectsAbsurdFutureTimestamp(t *testing.T) {
	pub, priv := adoptKeys(t)
	for _, issuedMs := range []int64{math.MaxInt64, math.MaxInt64 / 2, math.MinInt64, math.MinInt64 / 2} {
		req := &handoffv1.AdoptZoneRequest{
			ZoneId: "midgaard", FromShardId: "shard-a", ToShardId: "shard-b", IssuedAtUnixMs: issuedMs,
		}
		req.ZoneSig = signAdoptZone(priv, req)
		if err := verifyAdoptZone(pub, req, "shard-b", time.Now()); !errors.Is(err, ErrAdoptZoneSkew) {
			t.Fatalf("issued_at=%d must be refused as out-of-window, got %v", issuedMs, err)
		}
	}
}

// TestVerifyAdoptZoneDistinguishesSkewFromForgery: the server logs the two apart (a stale-but-authentic
// request means the peers' clocks disagree; a bad signature means a wrong key or a forgery), so the verifier
// must return distinguishable errors even though both become the same opaque PermissionDenied on the wire.
func TestVerifyAdoptZoneDistinguishesSkewFromForgery(t *testing.T) {
	pub, priv := adoptKeys(t)

	stale := signedAdopt(priv, "midgaard", "shard-a", "shard-b")
	staleErr := verifyAdoptZone(pub, stale, "shard-b", time.UnixMilli(stale.GetIssuedAtUnixMs()).Add(adoptZoneMaxSkew+time.Second))
	if !errors.Is(staleErr, ErrAdoptZoneSkew) {
		t.Fatalf("an authentic but stale request must report skew, got %v", staleErr)
	}

	forged := signedAdopt(priv, "midgaard", "shard-a", "shard-b")
	forged.ZoneSig[0] ^= 0xff
	if err := verifyAdoptZone(pub, forged, "shard-b", time.Now()); !errors.Is(err, ErrAdoptZoneSig) || errors.Is(err, ErrAdoptZoneSkew) {
		t.Fatalf("a bad signature must report forgery, not skew, got %v", err)
	}
}

// TestKeyedAdoptZoneRejectsSkewedRequestOverRPC closes the gap the distsys review named: the skew bound was
// only unit-tested with an injected clock, which does not prove the SERVER consults it. Clock is the one new
// dependency this change introduces into the drain path, so pin the rejection at the RPC boundary — with the
// zone left un-hosted, since auth must run before any state work.
func TestKeyedAdoptZoneRejectsSkewedRequestOverRPC(t *testing.T) {
	pub, priv := adoptKeys(t)
	shard := NewDemoShard().WithHandoffKeys(priv, pub)
	shard.shardID = "shard-b"
	keyed := &handoffServer{shard: shard}

	// Name a zone this shard does NOT already host, so "did any state work happen?" is observable. (A
	// pre-hosted zone would hit HostZone's idempotent early return and tell us nothing.)
	const zone = "darkwood"
	if shard.ZoneByID(zone) != nil {
		t.Fatalf("precondition: %s must not already be hosted", zone)
	}
	req := signedAdopt(priv, zone, "shard-a", "shard-b")

	// Fast-forward the SERVER's clock past the window. The request is perfectly authentic — only stale.
	old := adoptZoneNow
	adoptZoneNow = func() time.Time { return time.Now().Add(adoptZoneMaxSkew + time.Minute) }
	t.Cleanup(func() { adoptZoneNow = old })

	if _, err := keyed.AdoptZone(context.Background(), req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("the server must refuse an out-of-window request with PermissionDenied, got %v", err)
	}
	if shard.ZoneByID(zone) != nil {
		t.Fatal("the zone was hosted despite a refused AdoptZone — auth must precede all state work")
	}

	// Restore the clock: the very same request now gets PAST auth and reaches HostZone, which refuses for its
	// own reason (this bare shard isn't running). Landing on FailedPrecondition rather than PermissionDenied is
	// exactly what proves the earlier refusal was the clock, not the signature.
	adoptZoneNow = old
	if _, err := keyed.AdoptZone(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("the same request must clear auth once inside the window (reaching HostZone), got %v", err)
	}
}

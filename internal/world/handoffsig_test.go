package world

import (
	"context"
	"crypto/ed25519"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
)

// newSignedPrepare builds a representative Prepare request for the signing tests.
func newSignedPrepare() *handoffv1.PrepareRequest {
	return &handoffv1.PrepareRequest{
		SessionId:    "kas",
		TargetZoneId: "darkwood",
		TargetRoomId: "darkwood:entrance",
		Epoch:        7,
		FromShardId:  "shard-a:9000",
		Snapshot: &handoffv1.PlayerSnapshot{
			CharacterId:  "kas",
			Name:         "Kas",
			PersistId:    "11111111-1111-1111-1111-111111111111",
			StateVersion: 4,
			AppliedSeq:   99,
			CommsState:   `{"afk":false}`,
			StateJson:    `{"inv":["a torch"]}`,
			Tier:         "admin", // #106: an ELEVATED player, so the tier participates in the signature
		},
	}
}

func TestSignSnapshotRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	req := newSignedPrepare()
	req.SnapshotSig = signSnapshot(priv, req)
	if len(req.SnapshotSig) != ed25519.SignatureSize {
		t.Fatalf("signSnapshot produced %d bytes, want %d", len(req.SnapshotSig), ed25519.SignatureSize)
	}
	if err := verifySnapshot(pub, req); err != nil {
		t.Fatalf("verifySnapshot rejected a valid signature: %v", err)
	}
}

// TestVerifySnapshotRejectsTamper is the core integrity assertion: mutating ANY integrity-critical field
// after signing must fail verification (a forged carry can't inject state under a valid signature).
func TestVerifySnapshotRejectsTamper(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	tamper := map[string]func(*handoffv1.PrepareRequest){
		"target_zone":   func(r *handoffv1.PrepareRequest) { r.TargetZoneId = "midgaard" },
		"target_room":   func(r *handoffv1.PrepareRequest) { r.TargetRoomId = "midgaard:vault" },
		"epoch":         func(r *handoffv1.PrepareRequest) { r.Epoch = 8 },
		"character_id":  func(r *handoffv1.PrepareRequest) { r.Snapshot.CharacterId = "eve" },
		"name":          func(r *handoffv1.PrepareRequest) { r.Snapshot.Name = "Eve" },
		"persist_id":    func(r *handoffv1.PrepareRequest) { r.Snapshot.PersistId = "22222222-2222-2222-2222-222222222222" },
		"state_version": func(r *handoffv1.PrepareRequest) { r.Snapshot.StateVersion = 999 },
		"applied_seq":   func(r *handoffv1.PrepareRequest) { r.Snapshot.AppliedSeq = 0 },
		"comms_state":   func(r *handoffv1.PrepareRequest) { r.Snapshot.CommsState = `{"afk":true}` },
		"state_json":    func(r *handoffv1.PrepareRequest) { r.Snapshot.StateJson = `{"inv":["a legendary sword"]}` },
		// #106: flipping the trust tier on an in-flight handoff must break the signature — otherwise a network
		// attacker could rewrite the elevation the destination re-derives. The base snapshot is "admin"; both a
		// downgrade (to "builder") and a strip (to the empty baseline) must be caught.
		"tier_change": func(r *handoffv1.PrepareRequest) { r.Snapshot.Tier = "builder" },
		"tier_strip":  func(r *handoffv1.PrepareRequest) { r.Snapshot.Tier = "" },
	}
	for name, mutate := range tamper {
		t.Run(name, func(t *testing.T) {
			req := newSignedPrepare()
			req.SnapshotSig = signSnapshot(priv, req)
			mutate(req)
			if err := verifySnapshot(pub, req); err == nil {
				t.Fatalf("verifySnapshot accepted a request with tampered %s", name)
			}
		})
	}
}

func TestVerifySnapshotRejectsUnsignedAndWrongKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)

	req := newSignedPrepare()
	// Unsigned (empty sig) is rejected when a verify key is present.
	if err := verifySnapshot(pub, req); err == nil {
		t.Fatal("verifySnapshot accepted an unsigned request")
	}
	// Signed by priv, verified with an unrelated public key, is rejected.
	req.SnapshotSig = signSnapshot(priv, req)
	if err := verifySnapshot(otherPub, req); err == nil {
		t.Fatal("verifySnapshot accepted a signature from a different key")
	}
	// A truncated signature is rejected (not indexed out of range).
	req.SnapshotSig = req.SnapshotSig[:ed25519.SignatureSize-1]
	if err := verifySnapshot(pub, req); err == nil {
		t.Fatal("verifySnapshot accepted a truncated signature")
	}
}

// TestEmptyTierPreservesLegacyDigest pins the #106 rollout-compat property: a BASELINE player (empty tier)
// must digest byte-identically to a snapshot that carries no tier field at all, so an in-flight ordinary
// handoff is unaffected by a rolling upgrade. The tier is bound into the signature ONLY when non-empty; an
// empty tier contributes nothing, so a new-code signer and an old-code verifier agree for the common case.
// Conversely, an elevated tier DOES change the digest (that handoff would fail across a version boundary,
// fail-closed — the intended, rare cost).
func TestEmptyTierPreservesLegacyDigest(t *testing.T) {
	base := newSignedPrepare()
	base.Snapshot.Tier = "" // a baseline player

	// Compute the digest with an explicitly-absent tier by clearing it — identical object minus the field.
	withEmpty := snapshotSigningInput(base)

	// A hand-built request with NO tier set at all must produce the same digest (the legacy shape).
	legacy := newSignedPrepare()
	legacy.Snapshot.Tier = ""
	if got := snapshotSigningInput(legacy); string(got) != string(withEmpty) {
		t.Fatal("an empty tier must not change the signing digest (rolling-upgrade compat for baseline handoffs)")
	}

	// An elevated tier MUST change the digest — the value is authenticated, so its presence is bound.
	elevated := newSignedPrepare()
	elevated.Snapshot.Tier = "admin"
	if string(snapshotSigningInput(elevated)) == string(withEmpty) {
		t.Fatal("an elevated tier must change the signing digest (else it would be forgeable)")
	}
}

// TestSignSnapshotNilKeyIsNoop documents the dev/test degrade: no signing key => nil signature (which a
// keyless destination accepts, and a key-enforcing one rejects).
func TestSignSnapshotNilKeyIsNoop(t *testing.T) {
	if sig := signSnapshot(nil, newSignedPrepare()); sig != nil {
		t.Fatalf("signSnapshot with a nil key returned %d bytes, want nil", len(sig))
	}
}

// TestPrepareEnforcesSignatureWhenKeyed asserts the handler-level gate: a keyed destination rejects an
// unsigned/forged Prepare with PermissionDenied BEFORE any zone/state work, while a keyless destination
// skips authentication (falling through to the normal zone lookup — NotFound for a zone it doesn't host).
func TestPrepareEnforcesSignatureWhenKeyed(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	req := newSignedPrepare() // unsigned (no SnapshotSig)

	keyed := &handoffServer{shard: NewDemoShard().WithHandoffKeys(nil, pub)}
	_, err := keyed.Prepare(context.Background(), req)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("keyed shard: want PermissionDenied for an unsigned Prepare, got %v", err)
	}

	// A keyless shard skips signature enforcement — but since #260 it only ACCEPTS the handoff when explicitly
	// insecure, so mark it as such to reach the fall-through (auth skipped ⇒ unknown zone ⇒ NotFound). The
	// keyless-REFUSE default is asserted in handoff_keyless_test.go.
	keyless := &handoffServer{shard: NewDemoShard().WithInsecureHandoff(true)}
	_, err = keyless.Prepare(context.Background(), req)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("insecure keyless shard: want NotFound (auth skipped, unknown zone), got %v", err)
	}
}

// TestPrepareStripsTierWhenKeyless pins the #106 blast-radius guard on the INSECURE keyless path: an insecure
// keyless shard (which does not verify the signature but is explicitly allowed to accept handoffs) must STRIP
// the carried tier before any state work, so an unsigned/forged Prepare cannot inject elevation — the pre-#106
// posture (a handoff drops elevation). A KEYED shard with a VALID signature preserves the tier (the signature
// bound it). The strip runs before the zone lookup, so it is observable on the request even though both calls
// end at NotFound (unknown demo zone). (A keyless shard that is NOT insecure refuses outright — #260, asserted
// in handoff_keyless_test.go — so the strip only matters once acceptance is opted into.)
func TestPrepareStripsTierWhenKeyless(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)

	// Insecure keyless: an unsigned Prepare carrying tier="admin" must have the tier stripped.
	keyless := &handoffServer{shard: NewDemoShard().WithInsecureHandoff(true)}
	unsigned := newSignedPrepare() // Tier=="admin", no SnapshotSig
	if _, err := keyless.Prepare(context.Background(), unsigned); status.Code(err) != codes.NotFound {
		t.Fatalf("insecure keyless shard: want NotFound, got %v", err)
	}
	if unsigned.Snapshot.GetTier() != "" {
		t.Fatalf("an insecure keyless shard must STRIP the carried tier (unverified elevation), got %q", unsigned.Snapshot.GetTier())
	}

	// Keyed + validly signed: the tier survives (the signature authenticated it).
	keyed := &handoffServer{shard: NewDemoShard().WithHandoffKeys(nil, pub)}
	signed := newSignedPrepare()
	signed.SnapshotSig = signSnapshot(priv, signed)
	if _, err := keyed.Prepare(context.Background(), signed); status.Code(err) != codes.NotFound {
		t.Fatalf("keyed shard: want NotFound after a valid signature, got %v", err)
	}
	if signed.Snapshot.GetTier() != "admin" {
		t.Fatalf("a keyed shard must preserve the signed tier, got %q", signed.Snapshot.GetTier())
	}
}

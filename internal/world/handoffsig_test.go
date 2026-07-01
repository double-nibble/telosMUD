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

	keyless := &handoffServer{shard: NewDemoShard()}
	_, err = keyless.Prepare(context.Background(), req)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("keyless shard: want NotFound (auth skipped, unknown zone), got %v", err)
	}
}

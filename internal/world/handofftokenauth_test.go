package world

import (
	"context"
	"crypto/ed25519"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
)

// handofftokenauth_test.go — #314: Handoff.Commit/Abort are authenticated with the shared cluster handoff
// keypair, exactly as Prepare/AdoptZone are. Before this, their only credential was the handoff_token, and
// that token is derived deterministically from (character, epoch) — both public — so anyone with network
// reach to a world port could compute it and Abort a pending handoff mid-flight (a targeted disruption).

const (
	testShardD  = "shard-d:9000"
	testShardD2 = "shard-d2:9000"
)

func TestSignHandoffTokenRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	const token = "deadbeefdeadbeefdeadbeefdeadbeef"
	for _, domain := range []string{handoffCommitDomain, handoffAbortDomain} {
		sig := signHandoffToken(priv, domain, token, testShardD)
		if len(sig) != ed25519.SignatureSize {
			t.Fatalf("%s: signHandoffToken produced %d bytes, want %d", domain, len(sig), ed25519.SignatureSize)
		}
		if err := verifyHandoffToken(pub, domain, token, testShardD, testShardD, sig); err != nil {
			t.Fatalf("%s: verifyHandoffToken rejected a valid signature: %v", domain, err)
		}
	}
}

// TestVerifyHandoffTokenRejects covers every rejection axis: unsigned, wrong key, a tampered token, a
// truncated signature, a signature under the WRONG operation domain (cross-operation replay), and — the
// #314 security-review finding — a signature minted for a DIFFERENT destination shard (cross-shard replay).
func TestVerifyHandoffTokenRejects(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)
	const token = "cafebabecafebabecafebabecafebabe"

	// Unsigned (empty sig) is rejected when a verify key is present.
	if err := verifyHandoffToken(pub, handoffAbortDomain, token, testShardD, testShardD, nil); err == nil {
		t.Fatal("verifyHandoffToken accepted an unsigned request")
	}

	good := signHandoffToken(priv, handoffAbortDomain, token, testShardD)

	// A different key is rejected.
	if err := verifyHandoffToken(otherPub, handoffAbortDomain, token, testShardD, testShardD, good); err == nil {
		t.Fatal("verifyHandoffToken accepted a signature from a different key")
	}
	// A different token is rejected.
	if err := verifyHandoffToken(pub, handoffAbortDomain, "0000000000000000", testShardD, testShardD, good); err == nil {
		t.Fatal("verifyHandoffToken accepted a signature over a different token")
	}
	// The SAME signature under the OTHER operation's domain is rejected — no cross-op replay.
	if err := verifyHandoffToken(pub, handoffCommitDomain, token, testShardD, testShardD, good); err == nil {
		t.Fatal("an Abort signature must not verify under the Commit domain (cross-operation replay)")
	}
	// CROSS-SHARD REPLAY: a signature minted for destination D, presented at a DIFFERENT receiver D2, is
	// rejected — this is the split-brain same-token replay the security review found. (Here the digest was
	// signed with toShardID=D but the receiver is D2, so both the signature check and the self-shard check
	// would fail; the signature check fires first.)
	if err := verifyHandoffToken(pub, handoffAbortDomain, token, testShardD, testShardD2, good); err == nil {
		t.Fatal("a signature bound to shard D must not verify at shard D2 (cross-shard replay)")
	}
	// And the pure destination-check axis: even a well-formed signature whose bound to_shard_id matches the
	// digest is refused when that destination is not THIS receiver. Sign for D2, verify at D.
	sigForD2 := signHandoffToken(priv, handoffAbortDomain, token, testShardD2)
	if err := verifyHandoffToken(pub, handoffAbortDomain, token, testShardD2, testShardD, sigForD2); err == nil {
		t.Fatal("a signature addressed to D2 must be refused at receiver D (destination binding)")
	}
	// An empty destination binding is refused (can never match a real shard id).
	sigEmpty := signHandoffToken(priv, handoffAbortDomain, token, "")
	if err := verifyHandoffToken(pub, handoffAbortDomain, token, "", testShardD, sigEmpty); err == nil {
		t.Fatal("an unbound (empty to_shard_id) signature must be refused")
	}
	// A truncated signature is rejected (not indexed out of range).
	if err := verifyHandoffToken(pub, handoffAbortDomain, token, testShardD, testShardD, good[:ed25519.SignatureSize-1]); err == nil {
		t.Fatal("verifyHandoffToken accepted a truncated signature")
	}
}

// TestSignHandoffTokenNilKeyIsNoop documents the dev/test degrade: no signing key => nil signature (which a
// keyless destination accepts, and a key-enforcing one rejects).
func TestSignHandoffTokenNilKeyIsNoop(t *testing.T) {
	if sig := signHandoffToken(nil, handoffAbortDomain, "tok", testShardD); sig != nil {
		t.Fatalf("signHandoffToken with a nil key returned %d bytes, want nil", len(sig))
	}
}

// TestCommitAbortEnforceSignatureWhenKeyed is the handler-level gate (mirrors TestPrepareEnforcesSignature):
// a keyed destination rejects an unsigned/forged Commit or Abort with PermissionDenied BEFORE any state work
// (for Abort, before the pending discard is ever posted), and accepts a validly-signed one.
func TestCommitAbortEnforceSignatureWhenKeyed(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	const token = "1234567890abcdef1234567890abcdef"

	shard := NewDemoShard().WithHandoffKeys(nil, pub)
	shard.shardID = testShardD // the receiver's own id; a valid signature must be bound to it
	keyed := &handoffServer{shard: shard}

	// Commit: unsigned => PermissionDenied; validly signed (bound to this shard) => ok.
	if _, err := keyed.Commit(context.Background(), &handoffv1.CommitRequest{HandoffToken: token}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("keyed shard: want PermissionDenied for an unsigned Commit, got %v", err)
	}
	commitSig := signHandoffToken(priv, handoffCommitDomain, token, testShardD)
	if _, err := keyed.Commit(context.Background(), &handoffv1.CommitRequest{HandoffToken: token, ToShardId: testShardD, Sig: commitSig}); err != nil {
		t.Fatalf("keyed shard: a validly-signed Commit must be accepted, got %v", err)
	}

	// Abort: unsigned => PermissionDenied; a token-only forgery (right token, no sig) => PermissionDenied;
	// validly signed => ok.
	if _, err := keyed.Abort(context.Background(), &handoffv1.AbortRequest{HandoffToken: token}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("keyed shard: want PermissionDenied for an unsigned Abort, got %v", err)
	}
	// A Commit signature must not be replayable as an Abort (distinct domains).
	if _, err := keyed.Abort(context.Background(), &handoffv1.AbortRequest{HandoffToken: token, ToShardId: testShardD, Sig: commitSig}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("keyed shard: a Commit signature must not authenticate an Abort, got %v", err)
	}
	// An Abort validly signed for a DIFFERENT destination (D2) must be refused at this receiver (D) — the
	// cross-shard replay the destination binding closes. The token/reason are legitimate; only the binding differs.
	abortSigD2 := signHandoffToken(priv, handoffAbortDomain, token, testShardD2)
	if _, err := keyed.Abort(context.Background(), &handoffv1.AbortRequest{HandoffToken: token, ToShardId: testShardD2, Sig: abortSigD2}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("keyed shard D: an Abort bound to D2 must be refused (cross-shard replay), got %v", err)
	}
	abortSig := signHandoffToken(priv, handoffAbortDomain, token, testShardD)
	if _, err := keyed.Abort(context.Background(), &handoffv1.AbortRequest{HandoffToken: token, ToShardId: testShardD, Sig: abortSig}); err != nil {
		t.Fatalf("keyed shard: a validly-signed Abort must be accepted, got %v", err)
	}
}

// TestCommitAbortRefuseKeylessByDefault: a keyless, non-insecure shard refuses both RPCs with
// PermissionDenied (the #260 posture — a single-shard world never legitimately receives a handoff RPC).
func TestCommitAbortRefuseKeylessByDefault(t *testing.T) {
	keyless := &handoffServer{shard: NewDemoShard()} // keyless, NOT insecure

	if _, err := keyless.Commit(context.Background(), &handoffv1.CommitRequest{HandoffToken: "tok"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("keyless non-insecure shard must REFUSE Commit, got %v", err)
	}
	if _, err := keyless.Abort(context.Background(), &handoffv1.AbortRequest{HandoffToken: "tok"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("keyless non-insecure shard must REFUSE Abort, got %v", err)
	}
}

// TestInsecureKeylessCommitAbortAccepted: the explicit insecure opt-in lifts the refusal — an unsigned
// Commit/Abort is processed (the dev/test rig posture serveShard selects). Abort over an unknown token is a
// harmless broadcast no-op, so both return without error.
func TestInsecureKeylessCommitAbortAccepted(t *testing.T) {
	insecure := &handoffServer{shard: NewDemoShard().WithInsecureHandoff(true)}

	if _, err := insecure.Commit(context.Background(), &handoffv1.CommitRequest{HandoffToken: "tok"}); err != nil {
		t.Fatalf("insecure keyless shard must ACCEPT Commit, got %v", err)
	}
	if _, err := insecure.Abort(context.Background(), &handoffv1.AbortRequest{HandoffToken: "tok"}); err != nil {
		t.Fatalf("insecure keyless shard must ACCEPT Abort, got %v", err)
	}
}

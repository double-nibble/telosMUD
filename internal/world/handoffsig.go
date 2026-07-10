package world

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
)

// Cross-shard handoff snapshot authentication (docs/REMAINING.md §1, MEDIUM).
//
// Handoff.Prepare rehydrates an incoming player from an attacker-influenceable snapshot. Without
// authentication, a reachable inter-shard port + a character name + a plausible epoch lets a forged
// Prepare carry an arbitrary state_json; the pack-set audit only rejects UNKNOWN prototypes, so a
// forged carry can still inject any KNOWN prototype (item dupe / econ break). The size caps + pack-set
// audit harden availability, not integrity — this closes the integrity hole.
//
// The signature is Ed25519 over a canonical digest of the integrity-critical Prepare fields. All shards
// in a cluster are mutually-trusting peers that share the handoff keypair (config, like the session-
// assertion keys): the source signs an outgoing Prepare with the shared private key and the destination
// verifies with the shared public key, so a party without the key cannot forge a Prepare. Enforcement is
// gated on the destination having a verify key configured — a keyless shard (dev/test) does not enforce,
// mirroring the WithVerifyKey session-assertion seam so the existing keyless test suite is unaffected.

// ErrSnapshotSig is returned when a Prepare's snapshot_sig fails verification against the shard's handoff
// verify key (missing, malformed, or over tampered fields).
var ErrSnapshotSig = errors.New("handoff: snapshot signature verification failed")

// snapshotSigningInput builds the canonical byte string the handoff signature covers. It length-prefixes
// every field (8-byte big-endian length + bytes) so no concatenation of one field's tail with the next's
// head can collide with a different field split — the classic canonicalization pitfall. It binds exactly
// the fields that decide WHERE a player lands and WHAT state they carry: the routing tuple
// (character/epoch/target zone+room) and the whole carried entity subtree (persist id, version, applied
// seq, comms + entity state json). The volatile transport fields (session id, from-shard, target addr)
// are intentionally out of scope — they do not affect integrity and would only make the signature brittle.
func snapshotSigningInput(req *handoffv1.PrepareRequest) []byte {
	snap := req.GetSnapshot()
	h := sha256.New()
	var lenbuf [8]byte
	write := func(b []byte) {
		binary.BigEndian.PutUint64(lenbuf[:], uint64(len(b)))
		h.Write(lenbuf[:])
		h.Write(b)
	}
	writeStr := func(s string) { write([]byte(s)) }
	writeU64 := func(v uint64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], v)
		write(b[:])
	}

	// Domain separator so a handoff signature can never be mistaken for another Ed25519 use of the key.
	writeStr("telosmud/handoff/v1")
	// Routing tuple.
	writeStr(snap.GetCharacterId())
	writeU64(req.GetEpoch())
	writeStr(req.GetTargetZoneId())
	writeStr(req.GetTargetRoomId())
	// Carried player state (the injection surface).
	writeStr(snap.GetName())
	writeStr(snap.GetPersistId())
	writeU64(snap.GetStateVersion())
	writeU64(snap.GetAppliedSeq())
	writeStr(snap.GetCommsState())
	writeStr(snap.GetStateJson())
	// The account trust tier (#106) is elevation-bearing, so it MUST be bound by the signature — otherwise a
	// network attacker could flip an in-flight handoff's tier and the destination would re-derive admin from it.
	// It is appended ONLY WHEN NON-EMPTY: a baseline player (tier=="", the overwhelming common case) then digests
	// byte-identically to a pre-#106 signer, so a rolling upgrade does not break ordinary handoffs — only an
	// ELEVATED player (staff, rare) handed off across a mixed-version boundary sees a signature mismatch, which
	// fails CLOSED (the source keeps them; they retry). This is safe canonicalization, not a length-prefix
	// footgun: the field's presence is a pure function of the value being authenticated, so a party without the
	// key cannot make a tier="" and a tier="admin" snapshot collide (the latter includes the extra bytes; the
	// digest differs), and the same-code verifier always recomputes the identical presence decision.
	if t := snap.GetTier(); t != "" {
		writeStr(t)
	}

	return h.Sum(nil)
}

// signSnapshot returns the Ed25519 signature the source shard attaches to an outgoing Prepare. A nil
// private key returns nil (signing unconfigured — dev/test); a wrong-sized key is treated the same rather
// than panicking, so a misconfigured source degrades to an unsigned Prepare (which a key-enforcing
// destination then rejects) instead of crashing the handoff goroutine.
func signSnapshot(priv ed25519.PrivateKey, req *handoffv1.PrepareRequest) []byte {
	if len(priv) != ed25519.PrivateKeySize {
		return nil
	}
	return ed25519.Sign(priv, snapshotSigningInput(req))
}

// verifySnapshot checks a Prepare's snapshot_sig against pub. It is called only when the destination has a
// verify key (the caller gates on pub != nil), so a missing/short/tampered signature is a hard reject —
// there is no "accept unsigned when a key is present" fallback (that would defeat the whole control).
func verifySnapshot(pub ed25519.PublicKey, req *handoffv1.PrepareRequest) error {
	if len(pub) != ed25519.PublicKeySize {
		return ErrSnapshotSig
	}
	sig := req.GetSnapshotSig()
	if len(sig) != ed25519.SignatureSize {
		return ErrSnapshotSig
	}
	if !ed25519.Verify(pub, snapshotSigningInput(req), sig) {
		return ErrSnapshotSig
	}
	return nil
}

// --- AdoptZone authentication (#262) -----------------------------------------------------------
//
// AdoptZone drives HostZone + lease renewal on the destination. Before this, only the KEYLESS case was
// refused (#260): a KEYED cluster still adopted a zone on a wholly unauthenticated request, so anyone with
// network reach to a world port could forge AdoptZone(zoneID) — a forced-host / lease-takeover /
// resource-exhaustion vector, and after #260 the weakest link in the handoff surface (Prepare is signed).
//
// It is authenticated with the SAME shared handoff keypair, but the threat model differs from Prepare's in
// one way that matters: Prepare's digest binds an EPOCH the destination monotonically rejects, so a captured
// Prepare cannot be replayed. AdoptZone has no such monotonic field. A signature over zone_id alone would
// therefore be a permanent, transferable capability to force any shard to host that zone. So the digest
// binds the DESTINATION (to_shard_id — a signature for shard A is worthless at shard B) and an ISSUE TIME
// the verifier bounds (adoptZoneMaxSkew), giving the capability an expiry.
//
// A replay inside that window re-adopts the same zone at the same shard, which HostZone treats as idempotent
// (see the security-load-bearing note on HostZone's early return). That is the common case and is a true
// no-op. It is NOT unconditional: if the destination has since handed the zone ONWARD (a cascading rebalance)
// it no longer holds it, and a replay would rebuild the zone actor. Such a zombie can never win the lease —
// its renewal self-terminates on the handedOff fence, and ShardForZone never routes to it — so single-writer
// holds and it is a bounded resource leak, not a correctness break. Binding the zone's monotonic directory
// lease generation instead of a wall clock would collapse the window entirely and remove the clock dependence
// this check introduces into the drain path; that is the recommended hardening, tracked as a follow-up.
//
// from_shard_id is bound by the digest as an AUDIT SUBJECT, not an authorization input: the verifier does not
// check that the named source actually owns the zone. Under the mutually-trusting shared-key peer model any
// key-holder is already authorized; a source-ownership check would narrow the residual key-holder surface.

// ErrAdoptZoneSig is returned when an AdoptZone request fails authentication: a missing/malformed signature,
// a digest over tampered fields, or a signature minted for a different destination shard.
var ErrAdoptZoneSig = errors.New("handoff: adopt-zone authentication failed")

// ErrAdoptZoneSkew is returned when the signature is VALID and bound to this shard, but its issue time falls
// outside the accepted clock-skew window. It is separated from ErrAdoptZoneSig only so the server can log
// which it was: an authentic-but-stale request means the two peers' clocks disagree (or a genuine replay of
// an old capture), not that the cluster key is wrong. Both map to the same opaque PermissionDenied on the wire.
var ErrAdoptZoneSkew = errors.New("handoff: adopt-zone issue time outside the accepted clock-skew window")

// adoptZoneMaxSkew bounds how far an AdoptZone's issued_at may sit from the verifier's clock, in either
// direction. It is the replay window: a captured request is useless after it, and useless at any shard but
// the one it names. Generous enough to absorb ordinary NTP drift between cluster peers, short enough that a
// captured request is not a standing capability. A package var so a test can shrink it.
var adoptZoneMaxSkew = 60 * time.Second

// adoptZoneNow is the verifier's clock seam. The server reads it rather than calling time.Now directly so an
// integration test can pin the skew REJECTION at the RPC boundary — clock is the one novel dependency this
// change introduces into the drain path, and a unit test with an injected `now` does not prove the server
// actually consults it.
var adoptZoneNow = time.Now

// adoptZoneSigningInput builds the canonical digest an AdoptZone signature covers, length-prefixing every
// field exactly as snapshotSigningInput does, under its OWN domain separator — so an AdoptZone signature can
// never be replayed as a Prepare signature (or vice versa) even though both use the same key.
func adoptZoneSigningInput(req *handoffv1.AdoptZoneRequest) []byte {
	h := sha256.New()
	var lenbuf [8]byte
	write := func(b []byte) {
		binary.BigEndian.PutUint64(lenbuf[:], uint64(len(b)))
		h.Write(lenbuf[:])
		h.Write(b)
	}
	writeStr := func(s string) { write([]byte(s)) }
	writeI64 := func(v int64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(v)) //nolint:gosec // two's-complement round-trip; the verifier reads it back the same way
		write(b[:])
	}

	writeStr("telosmud/handoff/v1/adoptzone") // domain separator, distinct from the Prepare digest's
	writeStr(req.GetZoneId())
	writeStr(req.GetFromShardId())
	writeStr(req.GetToShardId())
	writeI64(req.GetIssuedAtUnixMs())
	return h.Sum(nil)
}

// signAdoptZone returns the signature the draining source attaches to an outgoing AdoptZone. A nil or
// wrong-sized private key returns nil (signing unconfigured — dev/test), degrading to an unsigned request
// that a key-enforcing destination then rejects, rather than panicking on the drain goroutine.
func signAdoptZone(priv ed25519.PrivateKey, req *handoffv1.AdoptZoneRequest) []byte {
	if len(priv) != ed25519.PrivateKeySize {
		return nil
	}
	return ed25519.Sign(priv, adoptZoneSigningInput(req))
}

// verifyAdoptZone authenticates an inbound AdoptZone against pub. Called only when the destination has a
// verify key, so a missing or malformed signature is a hard reject — there is no "accept unsigned when a key
// is present" fallback. selfShardID is this shard's id: a request naming a DIFFERENT destination is refused
// even with a valid signature, which is what stops a captured request from being replayed across the fleet.
// now is injected so the skew check is testable.
//
// It distinguishes ErrAdoptZoneSkew from ErrAdoptZoneSig so the SERVER can log the difference. Clock skew is
// the one rejection cause here that is environmental, silent and self-healing — an operator debugging a
// stalled drain must be able to tell "chrony died on this node" from "you configured the wrong key". The RPC
// response stays deliberately uniform; only the local log distinguishes them.
func verifyAdoptZone(pub ed25519.PublicKey, req *handoffv1.AdoptZoneRequest, selfShardID string, now time.Time) error {
	if len(pub) != ed25519.PublicKeySize {
		return ErrAdoptZoneSig
	}
	sig := req.GetZoneSig()
	if len(sig) != ed25519.SignatureSize {
		return ErrAdoptZoneSig
	}
	if !ed25519.Verify(pub, adoptZoneSigningInput(req), sig) {
		return ErrAdoptZoneSig
	}
	// Bind the destination: a signature minted for another shard must not be usable here. An empty
	// to_shard_id can never match a real shardID, so an unbound request is refused too.
	if req.GetToShardId() == "" || req.GetToShardId() != selfShardID {
		return ErrAdoptZoneSig
	}
	// Bound the replay window in BOTH directions: a stale capture expires, and a far-future issue time is
	// refused rather than granted a long life.
	//
	// Compare RAW MILLISECONDS, never time.Duration. `now.Sub(time.UnixMilli(v))` SATURATES at ±maxDuration
	// for an absurd v, and negating minDuration overflows back to minDuration (still negative) — so the
	// natural `if d < 0 { d = -d }; if d > max` form silently ACCEPTS a far-future timestamp, exactly
	// inverting this check's purpose. `now` is a real clock (~1.7e12 ms) and the window is small, so the
	// bounds below cannot overflow, and issuedMs is compared as the signed integer the digest actually bound.
	nowMs, skewMs := now.UnixMilli(), adoptZoneMaxSkew.Milliseconds()
	if issuedMs := req.GetIssuedAtUnixMs(); issuedMs < nowMs-skewMs || issuedMs > nowMs+skewMs {
		return ErrAdoptZoneSkew
	}
	return nil
}

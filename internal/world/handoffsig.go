package world

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"

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

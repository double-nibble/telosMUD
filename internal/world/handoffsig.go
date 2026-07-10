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

// --- AdoptZone authentication (#262) -----------------------------------------------------------
//
// AdoptZone drives HostZone + lease renewal on the destination. Before this, only the KEYLESS case was
// refused (#260): a KEYED cluster still adopted a zone on a wholly unauthenticated request, so anyone with
// network reach to a world port could forge AdoptZone(zoneID) — a forced-host / lease-takeover /
// resource-exhaustion vector, and after #260 the weakest link in the handoff surface (Prepare is signed).
//
// It is authenticated with the SAME shared handoff keypair, but the threat model differs from Prepare's in
// one way that matters: Prepare's digest binds an EPOCH the destination monotonically rejects, so a captured
// Prepare cannot be replayed. A signature over zone_id alone would therefore be a permanent, transferable
// capability to force any shard to host that zone.
//
// So the digest binds two things beyond the zone:
//
//   - to_shard_id — a signature minted for shard A is worthless at shard B.
//   - lease_gen (#315) — the zone lease's MONOTONIC GENERATION, read from the directory by the source while
//     it still holds the lease. The verifier checks it against the directory's current generation. Every
//     ownership change bumps it, and the HandoverZone flip that ends this very drain is an ownership change,
//     so the request stops being honored the instant the handover it authorizes completes. A captured
//     AdoptZone is not a time-bounded capability; it is a SINGLE-USE fence token for one specific handover.
//
// This replaces #262's issued_at_unix_ms + clock-skew window, which was weaker on three counts. A replay
// inside the window was safe only by accident — it rested on HostZone's early return being a pure no-op and
// on s.zones never being pruned, emergent properties rather than enforced ones. It was NOT unconditionally
// safe: if the destination had since handed the zone ONWARD, a replay rebuilt the zone actor as a zombie.
// And it dragged a clock dependence into the drain control plane, where a peer with more than 60s of drift
// became silently undrainable-to. The generation removes all three: there is no window to be inside, no
// clock to trust, and a post-flip replay is rejected outright rather than tolerated.
//
// from_shard_id is bound by the digest as an AUDIT SUBJECT, not an authorization input: the verifier does not
// check that the named source actually owns the zone. Under the mutually-trusting shared-key peer model any
// key-holder is already authorized; a source-ownership check would narrow the residual key-holder surface
// (tracked as #316).

// ErrAdoptZoneSig is returned when an AdoptZone request fails authentication: a missing/malformed signature,
// a digest over tampered fields, or a signature minted for a different destination shard.
var ErrAdoptZoneSig = errors.New("handoff: adopt-zone authentication failed")

// ErrAdoptZoneStale is returned when the signature is VALID and bound to this shard, but the lease generation
// it carries is not the zone's current one. It is separated from ErrAdoptZoneSig only so the server can log
// which it was: an authentic-but-stale request is a REPLAY of a handover that has already completed (or a
// racing one that lost), not a wrong cluster key. Both map to the same opaque PermissionDenied on the wire.
var ErrAdoptZoneStale = errors.New("handoff: adopt-zone lease generation is stale (the handover it authorized is over)")

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
	writeU64 := func(v uint64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], v)
		write(b[:])
	}

	writeStr("telosmud/handoff/v1/adoptzone") // domain separator, distinct from the Prepare digest's
	writeStr(req.GetZoneId())
	writeStr(req.GetFromShardId())
	writeStr(req.GetToShardId())
	writeU64(req.GetLeaseGen())
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

// verifyAdoptZoneSig is the LOCAL half of authenticating an inbound AdoptZone on a keyed shard: a well-formed
// Ed25519 signature over the canonical digest (zone, from, to, lease_gen), and a request that names THIS shard
// as its destination.
//
// It is deliberately separate from the generation fence, and it runs FIRST. The fence needs a directory read,
// and the handoff port takes unauthenticated connections — so if the read came first, anyone with network
// reach could make every shard hammer the cluster's shared Redis with a stream of garbage AdoptZone requests.
// Nothing remote happens until a request proves it was signed by a cluster key and is addressed here.
func verifyAdoptZoneSig(pub ed25519.PublicKey, req *handoffv1.AdoptZoneRequest, selfShardID string) error {
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
	return nil
}

// verifyAdoptZoneGen is the FENCE (#315), checked once the signature is known good. The source signs the
// generation it observes while it still holds the lease; its HandoverZone flip then increments it. So a
// captured request is honored only until the handover it authorizes lands, and never afterwards — a replay is
// rejected outright rather than tolerated as "idempotent".
//
// curGen is read from the directory by the caller, never derived from the request. A generation of 0 means the
// zone has never been claimed, so there is no handover to authorize; refuse rather than let an unclaimed zone
// be adopted on a signature bound to nothing.
//
// WHY EQUALITY IS ENOUGH, and why this does not also have to check that from_shard_id owns the zone: the
// directory bumps `gen` on EVERY ownership change (a new-owner claim, the handover flip) and on NOTHING else
// (a renewal by the current owner does not move it; a refused claim or flip does not move it). So
// `req.LeaseGen == curGen` transitively proves the owner has not changed since the source read it while
// holding the lease. The bump rule in directory/redis.go and this comparison are load-bearing for each other:
// weaken either — bump on a renewal, or accept a near-miss generation — and this stops proving anything.
func verifyAdoptZoneGen(req *handoffv1.AdoptZoneRequest, curGen uint64) error {
	if curGen == 0 || req.GetLeaseGen() != curGen {
		return ErrAdoptZoneStale
	}
	return nil
}

// verifyAdoptZone runs both halves in the order the server runs them. The wire response is a uniform
// PermissionDenied; the split error types exist only so the server can log which check failed.
func verifyAdoptZone(pub ed25519.PublicKey, req *handoffv1.AdoptZoneRequest, selfShardID string, curGen uint64) error {
	if err := verifyAdoptZoneSig(pub, req, selfShardID); err != nil {
		return err
	}
	return verifyAdoptZoneGen(req, curGen)
}

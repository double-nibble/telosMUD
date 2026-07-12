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
// from_shard_id is bound by the digest AND checked against the directory (#316): the named source must be the
// zone's live owner. Be precise about what that is and is not worth.
//
// It is NOT an adversarial barrier. Every shard holds the same cluster key, and `owner` and `gen` live in the
// same directory hash, returned by the same read. Any attacker who can satisfy the generation fence — by
// reading the directory, or by capturing an AdoptZone off the plaintext handoff wire — has thereby learned the
// true owner and can simply name it. A leaked key or a compromised shard defeats both checks together.
//
// What it IS: destination-side enforcement of a precondition the source already asserts (handoverZoneTo bails
// unless it is the live owner), so a shard that is merely WRONG — desynced, lagging, mid-partition, buggy —
// is refused at the door instead of building a zone nobody handed it. Without it such a request builds rooms,
// resets, mob spawns and an actor goroutine, and only then fails at the flip; renewal is owner-fenced so the
// zone can never take ownership and ShardForZone never routes to it, but nothing un-adopts it either (#327),
// so it lingers as an orphan. The check moves that failure before any state work, and it is free — the fence
// already reads the owner in the same round trip.

// ErrAdoptZoneSig is returned when an AdoptZone request fails authentication: a missing/malformed signature,
// a digest over tampered fields, or a signature minted for a different destination shard.
var ErrAdoptZoneSig = errors.New("handoff: adopt-zone authentication failed")

// ErrAdoptZoneStale is returned when the signature is VALID and bound to this shard, but the lease generation
// it carries is not the zone's current one. It is separated from ErrAdoptZoneSig only so the server can log
// which it was: an authentic-but-stale request is a REPLAY of a handover that has already completed (or a
// racing one that lost), not a wrong cluster key. Both map to the same opaque PermissionDenied on the wire.
var ErrAdoptZoneStale = errors.New("handoff: adopt-zone lease generation is stale (the handover it authorized is over)")

// ErrAdoptZoneNotOwner is returned when the signature is VALID and the generation matches, but from_shard_id
// is not the zone's live owner in the directory (#316) — so the named source has no zone to give away. Also
// opaque PermissionDenied on the wire; the split exists for the log.
var ErrAdoptZoneNotOwner = errors.New("handoff: adopt-zone source does not own the zone")

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

// verifyAdoptZoneLease is the DIRECTORY half, checked once the signature is known good. It takes the zone's
// live (owner, generation) as the caller read them, never anything derived from the request.
//
// The GENERATION is the fence (#315). The source signs the generation it observes while it still holds the
// lease; its HandoverZone flip then increments it. So a captured request is honored only until the handover it
// authorizes lands, and never afterwards — a replay is rejected outright rather than tolerated as
// "idempotent". A generation of 0 means the zone has never been claimed, so there is no handover to authorize;
// refuse rather than let an unclaimed zone be adopted on a signature bound to nothing.
//
// Equality is what makes the fence work, and it is load-bearing with the directory's bump rule: `gen` moves on
// EVERY ownership change (a new-owner claim, the handover flip) and on NOTHING else (a renewal by the current
// owner does not move it; a refused claim or flip does not move it). So `req.LeaseGen == curGen` transitively
// proves the owner has not changed since the source read it. Weaken either side — bump on a renewal, or accept
// a near-miss generation — and this stops proving anything.
//
// The OWNER check (#316) closes what that transitive argument cannot: it assumes the source truthfully named
// itself. A request that misnames its source satisfies the generation and would otherwise make this shard
// build a zone nobody handed it — an orphan with no un-adopt path. It buys correctness and an early
// fail-closed, not secrecy: an attacker who can read `gen` reads `owner` from the same hash, so this does not
// harden the fence against a leaked cluster key. See the file header.
func verifyAdoptZoneLease(req *handoffv1.AdoptZoneRequest, curOwner string, curGen uint64) error {
	if curGen == 0 || req.GetLeaseGen() != curGen {
		return ErrAdoptZoneStale
	}
	// An empty curOwner means the lease has LAPSED — nobody owns the zone, so nobody can hand it over.
	// Aborting here is intended, not an oversight: the source's own HandoverZone flip is fenced on a LIVE
	// lease and would be refused a moment later anyway, so all we lose is the wasted zone build. The zone
	// stays with the source and the drain degrades to reclaim-from-durable. Reaching this at all means the
	// source missed three consecutive renewals — it is already unhealthy.
	//
	// An empty from_shard_id can never match a real owner, so an unattributed request is refused here too.
	if req.GetFromShardId() == "" || req.GetFromShardId() != curOwner {
		return ErrAdoptZoneNotOwner
	}
	return nil
}

// verifyAdoptZone runs both halves in the order the server runs them. The wire response is a uniform
// PermissionDenied; the split error types exist only so the server can log which check failed.
func verifyAdoptZone(pub ed25519.PublicKey, req *handoffv1.AdoptZoneRequest, selfShardID, curOwner string, curGen uint64) error {
	if err := verifyAdoptZoneSig(pub, req, selfShardID); err != nil {
		return err
	}
	return verifyAdoptZoneLease(req, curOwner, curGen)
}

// --- Commit / Abort authentication (#314) ------------------------------------------------------
//
// Handoff.Commit and Handoff.Abort mutate a pending handoff's lifecycle (Abort discards the rehydrated
// pending player; Commit exists for the explicit-lifecycle path). Their only credential used to be the
// handoff_token, and that token is NOT a secret: handoffToken derives it as sha256(character/epoch), and
// both inputs are guessable — a character name is public (`who`) and the epoch is a small monotonic
// integer. So anyone with network reach to a world port could compute a valid token for a player
// mid-handoff and Abort it (a targeted disruption). The port being private was the only barrier — the same
// trusted-network assumption #260/#262 removed for Prepare/AdoptZone.
//
// The fix mirrors Prepare/AdoptZone: an Ed25519 signature under the shared cluster handoff keypair, gated
// on the destination having a verify key (a keyless shard is governed by the #260 refusal instead). We keep
// the token DETERMINISTIC — the retried-Prepare convergence and the zoneForToken index both depend on it —
// and add the signature as the thing an attacker cannot forge. The token stays public; possession of the
// cluster key is what the signature proves.
//
// The digest binds the token AND the destination shard id, under an operation-specific domain separator.
// The DESTINATION BINDING is load-bearing, not decoration (security review of #314): the handoff wire is
// plaintext (world.go dials peers with insecure creds), so an on-path attacker can CAPTURE a legitimately
// signed Abort. It is NOT enough that the token is a per-(character, epoch) capability, because two
// concurrent handoffs of the SAME character in a split-brain race both read the same base epoch M, both
// derive the identical token(character, M+1), and both Prepare — parking a pending under one token at TWO
// different destinations D and D2. The loser's genuine, correctly-signed Abort goes to D; captured and
// replayed to D2 (or broadcast), it would discard the WINNER's live pending. Binding to_shard_id and
// checking it against the receiver's own shard id (exactly as verifyAdoptZoneSig does) makes the loser's
// Abort worthless anywhere but D — the replay at D2 fails the destination check before any pending is
// touched.
//
// (The SAME-destination variant of that race — both handoffs target D, Prepare idempotency parks one shared
// pending, and the loser's genuine Abort discards the pending the winner needs — is a pre-existing
// token-idempotency-vs-rollback interaction, NOT an authentication gap; it is tracked separately and is out
// of scope here. Destination binding does not and need not address it.)
//
// The distinct domain separators keep a Commit signature from being replayable as an Abort signature (or
// vice versa) even though both cover the same token bytes — the same hygiene AdoptZone's separator gives.

// ErrHandoffTokenSig is returned when a Commit/Abort request fails authentication: a missing/malformed
// signature, a digest over a different token/operation, or a signature minted for a different destination
// shard.
var ErrHandoffTokenSig = errors.New("handoff: commit/abort authentication failed")

// Domain separators for the token-capability signatures. Distinct from each other and from the Prepare /
// AdoptZone digests, so no signature minted for one operation verifies for another under the shared key.
const (
	handoffCommitDomain = "telosmud/handoff/v1/commit"
	handoffAbortDomain  = "telosmud/handoff/v1/abort"
)

// handoffTokenSigningInput builds the canonical digest a Commit/Abort signature covers: the operation's
// domain separator, the handoff token, and the destination shard id — each length-prefixed exactly as the
// other handoff digests are (8-byte big-endian length + bytes), so no field can be reassociated by a
// boundary shift.
func handoffTokenSigningInput(domain, token, toShardID string) []byte {
	h := sha256.New()
	var lenbuf [8]byte
	write := func(b []byte) {
		binary.BigEndian.PutUint64(lenbuf[:], uint64(len(b)))
		h.Write(lenbuf[:])
		h.Write(b)
	}
	write([]byte(domain))
	write([]byte(token))
	write([]byte(toShardID))
	return h.Sum(nil)
}

// signHandoffToken returns the signature the source attaches to an outgoing Commit/Abort, binding the token
// to its destination shard. A nil or wrong-sized private key returns nil (signing unconfigured — dev/test),
// degrading to an unsigned request that a key-enforcing destination then rejects, rather than panicking on
// the caller's goroutine.
func signHandoffToken(priv ed25519.PrivateKey, domain, token, toShardID string) []byte {
	if len(priv) != ed25519.PrivateKeySize {
		return nil
	}
	return ed25519.Sign(priv, handoffTokenSigningInput(domain, token, toShardID))
}

// verifyHandoffToken checks a Commit/Abort sig against pub AND binds the request to this shard. Called only
// when the destination has a verify key (the server gates on pub != nil), so a missing/short/tampered
// signature is a hard reject — there is no "accept unsigned when a key is present" fallback, mirroring
// verifySnapshot. selfShardID is the RECEIVER's shard id: a signature whose bound to_shard_id is not this
// shard (or is empty) is refused, so a signature minted for a different destination cannot be replayed here
// (the split-brain cross-shard replay in the header).
func verifyHandoffToken(pub ed25519.PublicKey, domain, token, toShardID, selfShardID string, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return ErrHandoffTokenSig
	}
	if len(sig) != ed25519.SignatureSize {
		return ErrHandoffTokenSig
	}
	if !ed25519.Verify(pub, handoffTokenSigningInput(domain, token, toShardID), sig) {
		return ErrHandoffTokenSig
	}
	// Bind the destination: a signature minted for another shard must not be usable here. An empty
	// to_shard_id can never match a real shardID, so an unbound request is refused too (same rule as
	// verifyAdoptZoneSig).
	if toShardID == "" || toShardID != selfShardID {
		return ErrHandoffTokenSig
	}
	return nil
}

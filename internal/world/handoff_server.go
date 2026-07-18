package world

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
)

// pendingTTL bounds how long a rehydrated-but-unbound player waits for the gate's
// re-dial before the destination gives up on it (docs/PROTOCOL.md §5).
const pendingTTL = 60 * time.Second

// handoffServer is the destination side of the cross-shard handoff. Prepare
// rehydrates the incoming player as a PENDING entity in the target zone; the gate's
// re-dial — an Attach carrying the handoff token — binds and activates it (the
// self-commit). Commit/Abort cover the explicit lifecycle. All zone state changes go
// through the zone inbox, never touched directly from these RPC goroutines.
type handoffServer struct {
	handoffv1.UnimplementedHandoffServer
	shard *Shard
}

func registerHandoff(gs *grpc.Server, s *Shard) {
	handoffv1.RegisterHandoffServer(gs, &handoffServer{shard: s})
}

// keylessHandoffRefused is the #260 request-time gate for a KEYLESS shard: an inbound handoff RPC that cannot
// be authenticated by signature is refused (PermissionDenied) unless the operator explicitly opted into
// insecure handoffs (WithInsecureHandoff, from cfg.AllowInsecure). It returns nil when acceptance may proceed
// — i.e. the shard is either keyed (callers verify the signature themselves) or explicitly insecure. Callers
// on the keyless path use it to fail closed BEFORE any state work, so a forged Prepare/AdoptZone on a
// reachable keyless world port can never mutate state (a known-prototype item dupe, or a forced zone host).
func (s *Shard) keylessHandoffRefused() error {
	if s.handoffVerifyKey == nil && !s.allowInsecureHandoff {
		return status.Error(codes.PermissionDenied,
			"handoff refused: this world has no handoff verify key and does not accept unauthenticated "+
				"cross-shard handoffs — a single-shard world never receives one; a multi-shard cluster must "+
				"configure the shared handoff keypair (WithHandoffKeys), or set TELOS_ALLOW_INSECURE on a trusted rig")
	}
	return nil
}

// Prepare rehydrates the snapshot as a pending player in the target zone. It is
// idempotent on (character, epoch) via the deterministic token, and rejects an epoch
// at or below one this shard has already seen for the player.
func (h *handoffServer) Prepare(ctx context.Context, req *handoffv1.PrepareRequest) (*handoffv1.PrepareResponse, error) {
	snap := req.GetSnapshot()
	if snap == nil || snap.GetCharacterId() == "" {
		return nil, status.Error(codes.InvalidArgument, "missing snapshot")
	}
	// Authenticate the handoff BEFORE any state work (docs/REMAINING.md §1): when this shard has a handoff
	// verify key, an unsigned or tampered Prepare is rejected outright, so a forged carry can never reach
	// the pack-set audit / rehydrate path.
	if h.shard.handoffVerifyKey != nil {
		if err := verifySnapshot(h.shard.handoffVerifyKey, req); err != nil {
			return nil, status.Error(codes.PermissionDenied, "handoff snapshot authentication failed")
		}
	} else if err := h.shard.keylessHandoffRefused(); err != nil {
		// #260: a KEYLESS shard cannot authenticate the snapshot, so it REFUSES the handoff outright unless
		// the operator explicitly opted into insecure handoffs (TELOS_ALLOW_INSECURE). A reachable keyless
		// port that accepted an unsigned Prepare is a known-prototype item-injection vector (an econ dupe);
		// a single-shard world never legitimately receives a handoff, so refusing is the correct fail-closed.
		return nil, err
	} else if snap.GetTier() != "" {
		// #106 blast-radius guard, on the INSECURE keyless path only: the carried TIER re-derives elevation
		// (holylight/builder/admin) at the destination, so it must be trusted ONLY from an authenticated
		// snapshot. An insecure keyless shard did not verify the signature, so an unsigned/forged Prepare
		// could otherwise inject tier="admin" and the attach path would grant it. Strip the tier here so an
		// insecure keyless deployment applies NO elevation from a handoff — the pre-#106 fail-closed posture.
		// A keyed shard (above) already had the tier bound by the verified signature.
		snap.Tier = ""
	}
	// FAIL CLOSED on an instance-shaped target (#411). An instance is shard-local by construction: it takes no
	// directory lease, is never in the placement pool, and no peer can resolve it — so a cross-shard Prepare
	// naming one is either a poisoned/pre-migration record or a probe. Refusing by SHAPE (not by "do we host
	// it") is what makes the answer the same on every shard, and it is what stops a peer from injecting a
	// player into a live private instance whose id it guessed or observed.
	if isInstanceID(req.GetTargetZoneId()) {
		slog.Warn("handoff prepare refused: instance-shaped target zone", "zone", req.GetTargetZoneId())
		return nil, status.Errorf(codes.InvalidArgument, "zone %q is not a valid handoff destination", req.GetTargetZoneId())
	}
	z := h.shard.zoneByID(req.GetTargetZoneId())
	if z == nil {
		return nil, status.Errorf(codes.NotFound, "zone %q not hosted on this shard", req.GetTargetZoneId())
	}

	token := handoffToken(snap.GetCharacterId(), req.GetEpoch())
	reply := make(chan error, 1)
	m := prepareMsg{
		snap:  snap,
		room:  ProtoRef(req.GetTargetRoomId()),
		epoch: req.GetEpoch(),
		token: token,
		reply: reply,
	}
	// Honor the RPC context for both the post and the reply so a client cancellation,
	// deadline, or a stopped zone can't wedge this handler goroutine forever.
	select {
	case z.inbox <- m:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-reply:
		if err != nil {
			return nil, err
		}
	}
	return &handoffv1.PrepareResponse{
		HandoffToken:    token,
		TargetShardAddr: h.shard.addr,
		PendingTtlMs:    uint64(pendingTTL / time.Millisecond),
	}, nil
}

// authTokenRPC gates Commit/Abort exactly as Prepare gates itself (#314): a KEYED destination requires a
// valid signature over the token under the operation's domain separator and rejects otherwise; a KEYLESS
// destination is governed by the #260 refusal (fail closed unless the operator opted into insecure
// handoffs). It returns a gRPC status error to return verbatim, or nil to proceed. Runs BEFORE any state
// work, so a forged Abort can never reach the pending-discard path.
func (h *handoffServer) authTokenRPC(domain, token, toShardID string, sig []byte) error {
	if h.shard.handoffVerifyKey != nil {
		if err := verifyHandoffToken(h.shard.handoffVerifyKey, domain, token, toShardID, h.shard.shardID, sig); err != nil {
			return status.Error(codes.PermissionDenied, "handoff authentication failed")
		}
		return nil
	}
	return h.shard.keylessHandoffRefused()
}

// Commit is an idempotent no-op: the destination self-commits when the gate's stream
// binds the pending player (see Zone.attach). Kept for the explicit-lifecycle path. Authenticated (#314)
// so the surface is uniform — every mutating handoff RPC proves cluster-key possession — even though the
// no-op body has nothing to protect today; a future Commit that gains real behavior inherits the gate.
func (h *handoffServer) Commit(_ context.Context, req *handoffv1.CommitRequest) (*handoffv1.CommitResponse, error) {
	if err := h.authTokenRPC(handoffCommitDomain, req.GetHandoffToken(), req.GetToShardId(), req.GetSig()); err != nil {
		return nil, err
	}
	return &handoffv1.CommitResponse{}, nil
}

// Abort discards a pending player whose handoff was cancelled by the source. The token
// index names the zone that prepared it; absent that, broadcast to every hosted zone
// (the matching one discards, the rest no-op). Authenticated (#314): before this, the guessable token was
// the only credential, so anyone with network reach could discard a pending mid-handoff.
func (h *handoffServer) Abort(_ context.Context, req *handoffv1.AbortRequest) (*handoffv1.AbortResponse, error) {
	token := req.GetHandoffToken()
	if err := h.authTokenRPC(handoffAbortDomain, token, req.GetToShardId(), req.GetSig()); err != nil {
		return nil, err
	}
	if z := h.shard.zoneForToken(token); z != nil {
		z.post(abortPendingMsg{token: token})
	} else {
		for _, z := range h.shard.zonesList() { // mu-guarded: safe against a runtime HostZone (16.4a)
			z.post(abortPendingMsg{token: token})
		}
	}
	return &handoffv1.AbortResponse{}, nil
}

// AdoptZone runtime-hosts a draining peer's zone on this shard (Phase 16.4b): it builds + runs the zone
// (HostZone, idempotent) and starts its lease renewal, which waits out the draining source's ownership and
// takes over the instant the source's HandoverZone flip lands. It is BUILD-ONLY — it does NOT claim the
// lease itself; the caller (the draining source) owns the atomic flip so ShardForZone never observes an
// ownerless gap. Errors (FailedPrecondition) if this shard can't host the zone (not running / no content).
func (h *handoffServer) AdoptZone(ctx context.Context, req *handoffv1.AdoptZoneRequest) (*handoffv1.AdoptZoneResponse, error) {
	// FAIL CLOSED on an instance-shaped zone id (#411), BEFORE the signature verify. An instance is never
	// leased, so it can never legitimately be handed over; and this ordering is deliberate — the shape check
	// is a local string test, strictly cheaper than a signature verify and far cheaper than the directory
	// round trip the fence below needs, so an unauthenticated peer cannot use this id class to make us do
	// either. It leaks nothing: instance ids are never valid here for anyone.
	if isInstanceID(req.GetZoneId()) {
		slog.Warn("adopt zone refused: instance-shaped zone id (instances are shard-local and unleased)",
			"zone", req.GetZoneId(), "from", req.GetFromShardId(), "to", req.GetToShardId())
		return nil, status.Errorf(codes.InvalidArgument, "zone %q cannot be adopted", req.GetZoneId())
	}
	// Authenticate BEFORE any state work, exactly as Prepare does. #262: a KEYED shard used to adopt on a
	// wholly unauthenticated request, so anyone with network reach to a world port could force it to host a
	// zone (lease takeover / resource exhaustion). #315 then made the authorization SINGLE-USE.
	if h.shard.handoffVerifyKey != nil {
		// LOCAL checks first: signature + destination binding. The handoff port is unauthenticated, so a
		// forged request must cost this shard a signature verify and nothing more — never a round trip to the
		// cluster's shared directory Redis, which every shard, the placement coordinator and leader election
		// all depend on. This ordering is the whole defense against unauthenticated read amplification.
		if err := verifyAdoptZoneSig(h.shard.handoffVerifyKey, req, h.shard.shardID); err != nil {
			slog.Warn("adopt zone refused: signature authentication failed",
				"zone", req.GetZoneId(), "from", req.GetFromShardId(), "to", req.GetToShardId())
			return nil, status.Error(codes.PermissionDenied, "adopt zone authentication failed")
		}

		// Authentic, and addressed to us. Now the fence (#315): read the zone's CURRENT lease generation from
		// the directory. The source signed the generation it saw while holding the lease, and its HandoverZone
		// flip increments it, so this request is honored only until the handover it authorizes lands.
		//
		// A keyed shard with no leaser cannot perform this check — and a keyed shard is by definition part of
		// a multi-shard cluster, which leases its zones. Refuse rather than fall back to an unfenced verify.
		if h.shard.leaser == nil {
			slog.Error("adopt zone refused: this shard has a handoff verify key but no zone leaser, so the "+
				"lease-generation fence cannot be checked", "zone", req.GetZoneId())
			return nil, status.Error(codes.PermissionDenied, "adopt zone authentication failed")
		}
		curOwner, curGen, gerr := h.shard.leaser.ZoneLease(ctx, req.GetZoneId())
		if gerr != nil {
			// Fail closed. Unavailable is the honest code (transient, not an auth verdict), though nothing
			// consumes the distinction automatically: a graceful drain does not retry a failed handover, it
			// degrades that zone to reclaim-from-durable (drain.go); a rebalance directive is what gets
			// re-issued on backoff. Either way, adopting on an unverifiable generation would reinstate exactly
			// the replay window #315 removes.
			slog.Warn("adopt zone refused: could not read the zone's lease generation",
				"zone", req.GetZoneId(), "err", gerr)
			return nil, status.Error(codes.Unavailable, "adopt zone: directory unavailable")
		}
		if err := verifyAdoptZoneLease(req, curOwner, curGen); err != nil {
			// The wire response stays uniform — an attacker learns nothing about WHY. The local log is
			// specific, because an operator staring at a stalled drain needs to tell these apart: a STALE
			// request is a replay of a handover that already completed (or a racing one that lost the flip);
			// a NOT-OWNER request means the named source does not hold the zone, which for a correctly-signed
			// request means a key-holder forged the source (#316); and a bad signature means a wrong key.
			if errors.Is(err, ErrAdoptZoneNotOwner) {
				slog.Warn("adopt zone refused: the named source does not own this zone",
					"zone", req.GetZoneId(), "from", req.GetFromShardId(), "to", req.GetToShardId(),
					"current_owner", curOwner)
			} else {
				slog.Warn("adopt zone refused: stale lease generation — the handover this request authorized "+
					"has already completed, or another shard won the flip",
					"zone", req.GetZoneId(), "from", req.GetFromShardId(), "to", req.GetToShardId(),
					"request_gen", req.GetLeaseGen(), "current_gen", curGen)
			}
			return nil, status.Error(codes.PermissionDenied, "adopt zone authentication failed")
		}
	} else if err := h.shard.keylessHandoffRefused(); err != nil {
		// #260: a KEYLESS shard cannot authenticate the request at all, so it refuses outright unless the
		// operator explicitly opted into insecure handoffs (TELOS_ALLOW_INSECURE).
		return nil, err
	}
	// Thread the CALLER's context into HostZone (#280): building the zone now includes a blocking scope-snapshot
	// read, and the draining source is blocking on this RPC. If it gives up, we must too, rather than running
	// the read out on our own clock while the source has already moved on.
	//
	// THE DEADLINE INVERSION (#327) is handled NOT here but by the renewer. HostZone starts this zone's lease
	// renewal in the ADOPTING state, so if the source's deadline elapses mid-RPC (it returns before its
	// HandoverZone flip and keeps the lease), the zone we just built is left hosted-but-unowned — and its
	// renewer un-adopts it after adoptConfirmDeadline. Compensating HERE, synchronously, on ctx.Err() would be
	// racy: a concurrent sibling AdoptZone could have legitimately flipped ownership to us while this call's
	// context died, and we would tear down a zone we now own. The renewer is immune to that: a landed flip
	// makes its ClaimZone succeed, which sets confirmed and stops it ever un-adopting. One compensation point,
	// gated on the fact that actually matters (did the flip land?), not on this RPC's context.
	z, err := h.shard.HostZone(ctx, req.GetZoneId())
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "adopt zone %q: %v", req.GetZoneId(), err)
	}
	return &handoffv1.AdoptZoneResponse{Hosted: z != nil}, nil
}

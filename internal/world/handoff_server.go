package world

import (
	"context"
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

// Commit is an idempotent no-op: the destination self-commits when the gate's stream
// binds the pending player (see Zone.attach). Kept for the explicit-lifecycle path.
func (h *handoffServer) Commit(_ context.Context, _ *handoffv1.CommitRequest) (*handoffv1.CommitResponse, error) {
	return &handoffv1.CommitResponse{}, nil
}

// Abort discards a pending player whose handoff was cancelled by the source. The token
// index names the zone that prepared it; absent that, broadcast to every hosted zone
// (the matching one discards, the rest no-op).
func (h *handoffServer) Abort(_ context.Context, req *handoffv1.AbortRequest) (*handoffv1.AbortResponse, error) {
	token := req.GetHandoffToken()
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
func (h *handoffServer) AdoptZone(_ context.Context, req *handoffv1.AdoptZoneRequest) (*handoffv1.AdoptZoneResponse, error) {
	// #260: same keyless refusal as Prepare — a keyless shard must not host a zone on an unauthenticated,
	// attacker-forgeable request (a resource-exhaustion / disruption vector) unless explicitly insecure.
	// keylessHandoffRefused is a no-op on a keyed shard (it proceeds). NOTE: AdoptZone carries no per-request
	// signature, so a KEYED shard still adopts on an unauthenticated request — a separate, pre-existing gap
	// (the drain control plane leans on transport trust); authenticating keyed AdoptZone is tracked in #262.
	if err := h.shard.keylessHandoffRefused(); err != nil {
		return nil, err
	}
	z, err := h.shard.HostZone(req.GetZoneId())
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "adopt zone %q: %v", req.GetZoneId(), err)
	}
	return &handoffv1.AdoptZoneResponse{Hosted: z != nil}, nil
}

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

// Prepare rehydrates the snapshot as a pending player in the target zone. It is
// idempotent on (character, epoch) via the deterministic token, and rejects an epoch
// at or below one this shard has already seen for the player.
func (h *handoffServer) Prepare(ctx context.Context, req *handoffv1.PrepareRequest) (*handoffv1.PrepareResponse, error) {
	snap := req.GetSnapshot()
	if snap == nil || snap.GetCharacterId() == "" {
		return nil, status.Error(codes.InvalidArgument, "missing snapshot")
	}
	z := h.shard.zone
	if req.GetTargetZoneId() != z.id {
		return nil, status.Errorf(codes.NotFound, "zone %q not hosted on this shard", req.GetTargetZoneId())
	}

	token := handoffToken(snap.GetCharacterId(), req.GetEpoch())
	reply := make(chan error, 1)
	m := prepareMsg{
		snap:  snap,
		room:  req.GetTargetRoomId(),
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
func (h *handoffServer) Commit(ctx context.Context, req *handoffv1.CommitRequest) (*handoffv1.CommitResponse, error) {
	return &handoffv1.CommitResponse{}, nil
}

// Abort discards a pending player whose handoff was cancelled by the source.
func (h *handoffServer) Abort(ctx context.Context, req *handoffv1.AbortRequest) (*handoffv1.AbortResponse, error) {
	h.shard.zone.post(abortPendingMsg{token: req.GetHandoffToken()})
	return &handoffv1.AbortResponse{}, nil
}

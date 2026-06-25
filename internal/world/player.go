package world

import (
	"log/slog"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// player is a connected character's in-zone presence. All fields are zone-owned:
// only the zone goroutine reads or writes them, so they need no locks. The out
// channel is the single bridge from the zone to the player's gRPC stream writer
// goroutine (server.go) — the zone enqueues frames, the writer drains them. This
// split is what keeps the zone loop from ever blocking on a slow or dead socket.
type player struct {
	id   string
	name string
	room string
	out  chan *playv1.ServerFrame // buffered; drained by the writer goroutine in server.go

	// Redirect/replay substrate (docs/PROTOCOL.md §5).
	//
	// appliedSeq is the highest InputLine.seq this zone has applied for the player —
	// the dedup high-water mark. Any input with seq <= appliedSeq is a replay and is
	// dropped before dispatch, giving exactly-once apply across a re-dial. It is
	// stamped onto every outgoing frame (send) as ServerFrame.ack_input_seq so the
	// gate knows how far the world has consumed.
	appliedSeq uint64

	// detached/attachGen support re-attach (the gate re-dialing after a Redirect, or
	// a link-death + reconnect). On stream loss the player is NOT removed; it is
	// marked detached and reaped after a grace period unless a new stream re-binds.
	// attachGen is bumped on every (re-)attach so a stale reap timer for an older
	// generation is ignored once the player has re-attached.
	detached  bool
	attachGen uint64

	// quitting marks a clean, player-initiated disconnect ("quit"), so the stream
	// dropping removes the player immediately rather than entering the link-death
	// grace window.
	quitting bool

	// Cross-shard handoff (docs/PROTOCOL.md §3, §5). When a player walks through an
	// exit into a zone this shard does not own, the source FREEZES them: it stops
	// applying their input and refuses to remove them on stream-drop until the
	// handoff commits or aborts. epoch is the player's current ownership epoch,
	// bumped on each handoff so the directory's compare-and-set can reject stale
	// routing.
	frozen bool
	epoch  uint64

	// Destination side: a PENDING player has been rehydrated by Prepare and is waiting
	// for the gate to re-dial. It is not yet in its room's occupant set and applies no
	// input until an Attach carrying the matching token activates it. token is the
	// handoff token that re-dial must present.
	pending bool
	token   string
}

// send queues a frame for delivery to this player's stream writer, stamping the
// current input high-water mark onto it (ServerFrame.ack_input_seq) so the gate can
// prune its replay buffer. Called only from the zone goroutine, which owns
// appliedSeq, so the read is race-free.
//
// It is deliberately non-blocking: if the out buffer is full (client can't keep up,
// or the writer has already stopped) the frame is dropped rather than stalling the
// zone goroutine. Real backpressure/flow control is Phase 14. A single slow client
// must never wedge the whole zone — hence the default branch. Dropped frames are
// logged at Debug so DEBUG=1 surfaces client-can't-keep-up situations.
func (p *player) send(f *playv1.ServerFrame) {
	f.AckInputSeq = p.appliedSeq
	select {
	case p.out <- f:
	default:
		slog.Debug("frame dropped: player out buffer full", "player", p.id, "room", p.room)
	}
}


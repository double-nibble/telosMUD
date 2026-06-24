package world

import (
	"log/slog"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// player is a connected character's in-zone presence. Its fields (id, name, room)
// are zone-owned: only the zone goroutine reads or writes them, so they need no
// locks. The out channel is the single bridge from the zone to the player's gRPC
// stream writer goroutine (server.go) — the zone enqueues frames, the writer
// drains them. This split is what keeps the zone loop from ever blocking on a slow
// or dead socket.
type player struct {
	id   string
	name string
	room string
	out  chan *playv1.ServerFrame // buffered; drained by the writer goroutine in server.go
}

// send queues a frame for delivery to this player's stream writer. It is
// deliberately non-blocking: if the out buffer is full (client can't keep up, or
// the writer has already stopped) the frame is dropped rather than stalling the
// zone goroutine. Real backpressure/flow control is Phase 14.
//
// Because the zone goroutine calls send for every line of output, a single slow
// client must never be allowed to wedge the whole zone — hence the default branch.
// Dropped frames are logged at Debug so DEBUG=1 surfaces client-can't-keep-up
// situations during troubleshooting.
func (p *player) send(f *playv1.ServerFrame) {
	select {
	case p.out <- f:
	default:
		// Buffer full or writer gone: drop the frame instead of blocking the zone.
		slog.Debug("frame dropped: player out buffer full", "player", p.id, "room", p.room)
	}
}

package world

import playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"

// player is a connected character's in-zone presence. Its fields are owned by the
// zone goroutine; the out channel is the single bridge to the player's gRPC
// stream writer (server.go).
type player struct {
	id   string
	name string
	room string
	out  chan *playv1.ServerFrame
}

// send queues a frame for delivery. Non-blocking: if the client can't keep up the
// frame is dropped (Phase 1; real backpressure is Phase 14). Safe to call from the
// zone goroutine even after the stream writer has stopped.
func (p *player) send(f *playv1.ServerFrame) {
	select {
	case p.out <- f:
	default:
	}
}

package world

import (
	"log/slog"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// playServer implements the gRPC Play service for one zone. It is the bridge
// between the network (a player's bidirectional stream) and the zone actor: it
// never touches zone state, it only posts messages to the zone inbox.
type playServer struct {
	playv1.UnimplementedPlayServer
	zone *Zone
	log  *slog.Logger // scoped logger: component=play
}

func registerPlay(gs *grpc.Server, z *Zone) {
	playv1.RegisterPlayServer(gs, &playServer{
		zone: z,
		log:  slog.With("component", "play"),
	})
}

// Connect runs one player's bidirectional gRPC stream and is the bridge between the
// socket and the zone actor. The control flow, end to end:
//
//  1. The first client frame must be Attach; it names the character.
//  2. A *writer goroutine* is spawned. It is the SINGLE goroutine that ever calls
//     stream.Send — gRPC streams are not safe for concurrent Send, so all output
//     funnels through this one goroutine, fed by the player's out channel. The zone
//     enqueues frames with player.send; the writer drains them onto the wire.
//  3. This goroutine then becomes the *reader loop*: every client frame it receives
//     it translates into a zone inbox message (inputMsg) via zone.post. It never
//     mutates player/world state itself — that is the zone goroutine's job.
//
// So the full path for one command is:
//
//	wire -> reader loop (here) -> zone.post(inputMsg) -> zone inbox
//	     -> Zone.Run -> dispatch -> player.send -> out channel
//	     -> writer goroutine -> stream.Send -> wire
//
// On EOF/error or Detach, the reader loop exits and a leaveMsg is posted so the zone
// removes the player. The writer goroutine stops when the stream context is done.
func (s *playServer) Connect(stream playv1.Play_ConnectServer) error {
	s.log.Debug("play stream connect")
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	attach := first.GetAttach()
	if attach == nil {
		return status.Error(codes.InvalidArgument, "first frame must be Attach")
	}

	name := attach.GetCharacterId()
	if name == "" {
		// No character id supplied: invent an anonymous one.
		name = "Wanderer-" + uuid.NewString()[:8]
	}
	s.log.Debug("attach parsed", "character", name)
	p := &player{
		id:   name,
		name: name,
		out:  make(chan *playv1.ServerFrame, 256),
	}

	ctx := stream.Context()

	// Writer goroutine: the ONLY goroutine that calls stream.Send. It drains the
	// player's out channel until the stream context is cancelled or Send fails.
	go func() {
		for {
			select {
			case <-ctx.Done():
				s.log.Debug("stream writer stop (ctx done)", "player", p.id)
				return
			case f := <-p.out:
				if err := stream.Send(f); err != nil {
					s.log.Debug("stream writer stop (send error)", "player", p.id, "err", err)
					return
				}
			}
		}
	}()

	// Acknowledge the attach, then hand the player to the zone to be placed.
	p.send(attachedFrame(s.zone.id))
	s.zone.post(joinMsg{p: p})
	s.log.Debug("player stream ready", "player", p.id, "zone", s.zone.id)

	// Reader loop: translate client frames into zone inbox messages.
	var seq int
	for {
		f, err := stream.Recv()
		if err != nil {
			s.log.Debug("stream recv ended", "player", p.id, "err", err)
			break // EOF or transport error
		}
		switch pl := f.Payload.(type) {
		case *playv1.ClientFrame_Input:
			seq++
			text := pl.Input.GetText()
			s.log.Debug("input received", "player", p.id, "seq", seq, "text", text)
			s.zone.post(inputMsg{id: p.id, line: text})
		case *playv1.ClientFrame_Detach:
			s.log.Debug("detach received", "player", p.id)
			goto done
		default:
			// Phase 1 ignores gmcp/resize/pong/attach-after-first.
		}
	}
done:
	// Stream is closing: tell the zone to remove the player.
	s.log.Debug("stream closing", "player", p.id)
	s.zone.post(leaveMsg{id: p.id})
	return nil
}

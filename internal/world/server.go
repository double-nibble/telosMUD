package world

import (
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// playServer implements the gRPC Play service for one zone.
type playServer struct {
	playv1.UnimplementedPlayServer
	zone *Zone
}

func registerPlay(gs *grpc.Server, z *Zone) {
	playv1.RegisterPlayServer(gs, &playServer{zone: z})
}

// Connect runs one player's bidirectional stream. The first frame must be Attach.
// A writer goroutine drains the player's out channel to the stream; this goroutine
// reads input and posts it to the zone. All player/world mutation happens in the
// zone goroutine, never here.
func (s *playServer) Connect(stream playv1.Play_ConnectServer) error {
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
		name = "Wanderer-" + uuid.NewString()[:8]
	}
	p := &player{
		id:   name,
		name: name,
		out:  make(chan *playv1.ServerFrame, 256),
	}

	ctx := stream.Context()

	// Writer: the only goroutine that calls stream.Send.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-p.out:
				if err := stream.Send(f); err != nil {
					return
				}
			}
		}
	}()

	p.send(attachedFrame(s.zone.id))
	s.zone.post(joinMsg{p: p})

	// Reader loop.
	for {
		f, err := stream.Recv()
		if err != nil {
			break // EOF or transport error
		}
		switch pl := f.Payload.(type) {
		case *playv1.ClientFrame_Input:
			s.zone.post(inputMsg{id: p.id, line: pl.Input.GetText()})
		case *playv1.ClientFrame_Detach:
			goto done
		default:
			// Phase 1 ignores gmcp/resize/pong/attach-after-first.
		}
	}
done:
	s.zone.post(leaveMsg{id: p.id})
	return nil
}

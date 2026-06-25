package world

import (
	"log/slog"
	"sync/atomic"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// playServer implements the gRPC Play service for one shard (which may host several
// zones). It is the bridge between the network (a player's bidirectional stream) and
// the zone actors: it never touches zone state, it only posts messages to a zone
// inbox. Each connection routes its input to the player's CURRENT zone, which can
// change when the player walks between zones this shard hosts (see currentZone below).
type playServer struct {
	playv1.UnimplementedPlayServer
	shard *Shard
	log   *slog.Logger // scoped logger: component=play
}

func registerPlay(gs *grpc.Server, s *Shard) {
	playv1.RegisterPlayServer(gs, &playServer{
		shard: s,
		log:   slog.With("component", "play"),
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
// The zone, not the server, owns the player: the server posts attachMsg (which
// creates a new player or re-binds an existing one on a re-dial) and forwards each
// input with the gate's session-scoped seq so the zone can dedup replays. On a clean
// Detach the player is removed at once; on an unexpected drop a detachMsg starts the
// link-death grace (a reconnect/handoff may resume). The writer goroutine stops when
// the stream context is done.
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

	character := attach.GetCharacterId()
	if character == "" {
		// No character id supplied: invent an anonymous one.
		character = "Wanderer-" + uuid.NewString()[:8]
	}
	token := attach.GetHandoffToken()
	s.log.Debug("attach parsed", "character", character)

	// Decide which hosted zone this connection starts in: a handoff re-dial binds to
	// whichever zone holds the matching pending player; everything else (fresh login,
	// link-dead reconnect) routes to the shard's home zone. currentZone is this
	// connection's routing pointer — the zone Stores itself into it on attach, and the
	// reader loop Loads it for every input so a later intra-shard move follows the player.
	zone := s.shard.zones[s.shard.home]
	if token != "" {
		if z := s.shard.zoneForToken(token); z != nil {
			zone = z
		}
		// If no zone holds the token, fall through with home zone; the zone's attach
		// rejects the unknown token rather than spawning a fresh character.
	}
	var currentZone atomic.Pointer[Zone]
	currentZone.Store(zone)

	ctx := stream.Context()
	// out is this stream's outbound channel; the zone binds it to the character's
	// player. The writer goroutine below is the ONLY caller of stream.Send.
	out := make(chan *playv1.ServerFrame, 256)
	go func() {
		for {
			select {
			case <-ctx.Done():
				s.log.Debug("stream writer stop (ctx done)", "character", character)
				return
			case f := <-out:
				if err := stream.Send(f); err != nil {
					s.log.Debug("stream writer stop (send error)", "character", character, "err", err)
					return
				}
			}
		}
	}()

	// Hand the stream to the chosen zone: it creates a new player, re-binds an existing
	// one (a re-dial within the link-death window), or activates a pending player when
	// the Attach carries a handoff token (a cross-shard re-dial). It Stores itself into
	// currentZone and then sends Attached. We pass &currentZone so the zone — and a
	// later destination zone after an intra-shard move — can repoint the connection.
	zone.post(attachMsg{character: character, token: token, out: out, curZone: &currentZone})
	s.log.Debug("player stream ready", "character", character, "zone", zone.id)

	// Reader loop: translate client frames into zone inbox messages, posting each to
	// the player's CURRENT zone (which can change as they walk between this shard's
	// zones). detach/leave likewise go to the current zone.
	cleanQuit := false
	for {
		f, err := stream.Recv()
		if err != nil {
			s.log.Debug("stream recv ended", "character", character, "err", err)
			break // EOF or transport error
		}
		switch pl := f.Payload.(type) {
		case *playv1.ClientFrame_Input:
			in := pl.Input
			s.log.Debug("input received", "character", character, "seq", in.GetSeq(), "text", in.GetText())
			currentZone.Load().post(inputMsg{id: character, seq: in.GetSeq(), line: in.GetText()})
		case *playv1.ClientFrame_Detach:
			s.log.Debug("detach received (clean)", "character", character)
			cleanQuit = true
			goto done
		default:
			// Phase 1 ignores gmcp/resize/pong/attach-after-first.
		}
	}
done:
	if cleanQuit {
		// Explicit client disconnect: remove now.
		currentZone.Load().post(leaveMsg{id: character})
	} else {
		// Unexpected loss: enter link-death (the zone removes immediately only if the
		// player was quitting; otherwise it waits out the grace for a re-attach).
		currentZone.Load().post(detachMsg{id: character, out: out})
	}
	s.log.Debug("stream closing", "character", character, "clean", cleanQuit)
	return nil
}

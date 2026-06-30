package world

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/assertion"
	"github.com/double-nibble/telosmud/internal/sessionlock"
	"github.com/double-nibble/telosmud/internal/textsan"
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

	// Defense-in-depth at the world's gRPC boundary (mirrors the CleanLine call in the
	// reader loop): the gate validates the login name, but a compromised/buggy gate or a
	// direct-shard client could send a control-laden or over-long character id. It seeds
	// the player's display name AND targeting keyword (newPlayerEntity), so an
	// un-sanitized id is a terminal-injection vector into other players' clients (who,
	// "$n arrives", says). Sanitize BEFORE the empty-check so an all-control id that
	// cleans to empty falls through to the anonymous fallback rather than rendering blank.
	character := textsan.CleanName(attach.GetCharacterId(), maxPlayerNameRunes)
	if character == "" {
		// No (usable) character id supplied: invent an anonymous one.
		character = "Wanderer-" + uuid.NewString()[:8]
	}
	token := attach.GetHandoffToken()
	s.log.Debug("attach parsed", "character", character)

	// Phase 14.3: on a FRESH login (no handoff token) verify the gate's signed session assertion against
	// account's public key — OFFLINE, no per-connect RPC. A handoff re-dial is trusted via its handoff token
	// (and the short assertion TTL would falsely reject a late cross-shard walk), so it is NOT re-verified.
	// When no verify key is configured the shard trusts the gate's asserted identity directly (dev/pre-14.3).
	if token == "" && s.shard.verifyKey != nil {
		claims, err := assertion.Verify(s.shard.verifyKey, attach.GetSessionAssertion(), time.Now())
		if err != nil {
			s.log.Warn("session assertion rejected", "err", err, "character", character)
			return status.Error(codes.Unauthenticated, "invalid session assertion")
		}
		// The token must match THIS connection: the session it was issued for + the character it names. This
		// is what stops a compromised gate replaying one account's assertion to attach as a different identity.
		if claims.Session != attach.GetSessionId() || claims.Character != attach.GetCharacterId() {
			s.log.Warn("session assertion identity mismatch",
				"claim_session", claims.Session, "claim_character", claims.Character)
			return status.Error(codes.Unauthenticated, "session assertion mismatch")
		}
	}

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

	// Resume the player's ownership epoch from the directory BEFORE handing the stream to
	// the zone. Only for a fresh/link-dead login (token == ""), never a handoff re-dial
	// (that carries its own epoch through the pending session). The directory's placement
	// persists across logout/crash/restart, so without this a relog after a prior cross-
	// shard move would restart at epoch 1 and its next move's CAS would be rejected as stale
	// ("ownership conflict"). This read runs on THIS stream goroutine — never the zone
	// goroutine — so directory I/O can't block the actor loop. Errors degrade to not-found
	// (epoch 0 -> the zone seeds 1), so a directory hiccup can only cost a fresh-character
	// epoch, never a crash. A read-only lookup never writes/lowers the directory epoch, so
	// there is no both-own risk.
	var resumeEpoch uint64
	if token == "" && s.shard.dir != nil {
		ectx, ecancel := context.WithTimeout(stream.Context(), 2*time.Second)
		if ep, found, err := s.shard.dir.PlayerEpoch(ectx, character); err != nil {
			s.log.Debug("epoch resume read failed; treating as fresh", "character", character, "err", err)
		} else if found {
			resumeEpoch = ep
			s.log.Debug("epoch resumed from directory", "character", character, "epoch", ep)
		}
		ecancel()
	}

	// Load the character's durable snapshot, sibling to the epoch read above and on the SAME
	// stream goroutine — never the zone goroutine — so the (blocking) Postgres/Redis reads can't
	// stall the actor loop (docs/PHASE4-PLAN.md §4). Only for a fresh/link-dead login (token=="");
	// a handoff re-dial carries its state in the pending session. loadCharacterSnapshot picks the
	// FRESHER of {Postgres row, Redis checkpoint} by state_version (the crash-rehydrate freshness
	// check). loadedOK distinguishes a found row (rehydrate) from a brand-new name (create). A
	// disabled store yields loadedOK=false with no I/O, so a storeless boot is unchanged.
	var loaded CharSnapshot
	var loadedOK bool
	if token == "" {
		lctx, lcancel := context.WithTimeout(stream.Context(), 2*time.Second)
		loaded, loadedOK = s.shard.loadCharacterSnapshot(lctx, character)
		lcancel()
	}

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
	zone.post(attachMsg{
		character:   character,
		token:       token,
		out:         out,
		curZone:     &currentZone,
		resumeEpoch: resumeEpoch,
		inputSeq:    attach.GetInputSeq(),
		loaded:      loaded,
		loadedOK:    loadedOK,
	})
	s.log.Debug("player stream ready", "character", character, "zone", zone.id)

	// Phase 14.4 single-session lock: on a FRESH login (a handoff re-dial already holds the lock under the
	// same character), ACQUIRE the cross-shard lock (takeover) and start a renewer. A session displaced by a
	// newer login anywhere in the fleet sees its renew fail and self-kicks; the lock is released on teardown.
	// A Redis hiccup at acquire degrades to unlocked (we never block login on the lock).
	if token == "" && s.shard.sessionLock != nil {
		lockToken := uuid.NewString()
		key := sessionlock.Key(character)
		actx, acancel := context.WithTimeout(ctx, 2*time.Second)
		_, lerr := s.shard.sessionLock.Acquire(actx, key, lockToken, s.shard.lockTTL)
		acancel()
		if lerr != nil {
			s.log.Warn("session lock acquire failed (continuing unlocked)", "character", character, "err", lerr)
		} else {
			go s.runSessionLockRenewer(ctx, character, lockToken, out)
			defer func() {
				rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = s.shard.sessionLock.Release(rctx, key, lockToken)
				rcancel()
			}()
		}
	}

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
			// Defense-in-depth: the edge (telnet codec) already caps+sanitizes every
			// line, but the world is a separate trust domain reachable over gRPC — a
			// compromised/buggy gate or a direct-shard client could deliver an unbounded
			// or control-laden line. Re-cap and re-strip control runes here, at the
			// world's own ingress, before it reaches a zone inbox and fans out to other
			// players' terminals. CleanLine is a no-op for legitimate, already-clean input.
			line := textsan.CleanLine(in.GetText())
			s.log.Debug("input received", "character", character, "seq", in.GetSeq(), "text", line)
			currentZone.Load().post(inputMsg{id: character, seq: in.GetSeq(), line: line})
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

// runSessionLockRenewer heartbeats this connection's single-session lock (Phase 14.4). On each tick it
// renews the lock; if the renew reports the lock was LOST (a newer login took it over, anywhere in the
// fleet), it kicks THIS connection (displacedKick -> the gate closes the socket -> the reader loop ends ->
// the deferred Release runs). A transient renew error does NOT kick (one Redis blip shouldn't drop a live
// player); only a definitive "not owned" does. Runs until the stream ctx is cancelled (the connection ended).
func (s *playServer) runSessionLockRenewer(ctx context.Context, character, token string, out chan *playv1.ServerFrame) {
	t := time.NewTicker(s.shard.lockRenew)
	defer t.Stop()
	key := sessionlock.Key(character)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			owned, err := s.shard.sessionLock.Renew(rctx, key, token, s.shard.lockTTL)
			cancel()
			if err != nil {
				s.log.Warn("session lock renew failed (not kicking on a transient error)", "character", character, "err", err)
				continue
			}
			if !owned {
				s.log.Debug("session lock lost to a newer login; kicking this connection", "character", character)
				displacedKick(out, 0)
				return
			}
		}
	}
}

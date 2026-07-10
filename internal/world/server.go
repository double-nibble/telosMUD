package world

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/assertion"
	"github.com/double-nibble/telosmud/internal/metrics"
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
	// loginTier is the account trust tier (#27) from the VERIFIED assertion, carried to the session so the
	// zone can apply the matching builder/admin flags on spawn (Slice 3). Empty on the dev/unverified path
	// and on a handoff re-dial (token != "" — the tier's applied flags ride the entity snapshot, not this
	// claim), which correctly means "player, unless the verified claim elevated it". Never trusted from an
	// unverified source: only a signature-checked claim sets it.
	var loginTier string
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
		loginTier = claims.Tier
	}

	// Phase 16.4b: a draining shard refuses a FRESH login (it is handing its zones + players to a peer and is
	// about to exit) so a new arrival doesn't land here and become a drain straggler; the gate re-resolves
	// via the directory (whose zone leases have flipped to the peer) and dials there. A handoff BIND
	// (token != "") is still accepted so an in-flight cross-shard move completes.
	if token == "" && s.shard.isDraining() {
		s.log.Info("refusing fresh login: shard draining", "character", character)
		return status.Error(codes.Unavailable, "shard draining; reconnect")
	}

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

	// Decide which hosted zone this connection starts in. This runs AFTER the snapshot load because the
	// durable record is what names the zone (#320). currentZone is this connection's routing pointer — the
	// zone Stores itself into it on attach, and the reader loop Loads it for every input so a later
	// intra-shard move follows the player.
	//
	//   - handoff re-dial (token != "")  -> whichever zone holds the matching pending player.
	//   - rehydrating login              -> the zone named by the durable zone_ref.
	//   - brand-new character, or a zone_ref this shard does not host -> the shard's home zone.
	//
	// Before #320 EVERY non-handoff login attached to `home`, and loginRoom's resolveRoom silently fell
	// back to that zone's start room when the saved room_ref named a room some other zone hosts. A shard
	// hosts many zones (the demo pack ships three), so a player who walked from midgaard into darkwood and
	// logged out there came back standing in midgaard's temple, with their durable location gone. No
	// rebalance and no cross-shard hop required — an ordinary intra-shard walk was enough.
	zone := s.shard.zoneByID(s.shard.home)
	switch {
	case token != "":
		if z := s.shard.zoneForToken(token); z != nil {
			zone = z
		}
		// If no zone holds the token, fall through with the home zone; the zone's attach
		// rejects the unknown token rather than spawning a fresh character.
	case loadedOK && loaded.ZoneRef != "":
		if z := s.shard.zoneByID(loaded.ZoneRef); z != nil {
			zone = z
		} else {
			// This shard does not host the player's durable zone, so we cannot honor it. Falling back to
			// home preserves the pre-#320 behavior (they land in the home start room) rather than refusing
			// the login outright — the gate cannot yet re-resolve, because the directory placement records
			// a SHARD, not a zone. Fixing that is the second half of #320; until then this WARN is the
			// operator's signal that a rebalance stranded someone's durable location.
			s.log.Warn("durable zone not hosted on this shard; falling back to the home zone (the player's saved location is lost)",
				"character", character, "zone_ref", loaded.ZoneRef, "home", s.shard.home)
		}
	}
	if zone == nil {
		s.log.Error("no zone to attach to", "character", character, "home", s.shard.home)
		return status.Error(codes.Unavailable, "no hosted zone; reconnect")
	}
	var currentZone atomic.Pointer[Zone]
	currentZone.Store(zone)

	ctx := stream.Context()
	// out is this stream's outbound channel; the zone binds it to the character's
	// player. The writer goroutine below is the ONLY caller of stream.Send.
	out := make(chan *playv1.ServerFrame, sessionOutBuffer)

	// #274: a WRITER-STALL watchdog. gRPC keepalive (cmd/telos-world) reclaims TRANSPORT death — a gate that
	// crashed, partitioned, or wedged its HTTP/2 stack stops acking PINGs and the connection closes. It
	// structurally cannot see the other failure: a gate whose transport happily acks our PINGs (the HTTP/2
	// stack answers them independently of application flow control) while its APPLICATION has stopped reading
	// the Play stream. The stream's flow-control window then exhausts, the stream.Send below blocks with no
	// deadline, `out` fills, and session.send starts dropping. The zone stays healthy — that drop is why it
	// never blocks — but this stream's writer goroutine, its reader goroutine, the session, the entity's zone
	// ownership, and the session-lock renewer all leak until the GATE's own write-deadline closes its side.
	// Which is exactly the dependency on gate correctness that #46's keepalive set out to remove.
	//
	// sendStarted is nil when no Send is in flight, else the instant the current one began. The watchdog
	// samples it; a Send blocked past streamSendStallTimeout means the peer is not reading, and we tear the
	// stream down ourselves.
	//
	// A *time.Time, not a UnixNano int64: round-tripping through UnixNano strips Go's monotonic reading, so
	// the bound would ride the wall clock and an NTP step could trip or mask it.
	var sendStarted atomic.Pointer[time.Time]
	stalled := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				s.log.Debug("stream writer stop (ctx done)", "character", character)
				return
			case f := <-out:
				now := time.Now()
				sendStarted.Store(&now)
				err := stream.Send(f)
				sendStarted.Store(nil)
				if err != nil {
					s.log.Debug("stream writer stop (send error)", "character", character, "err", err)
					return
				}
			}
		}
	}()
	// The peer address labels the metric and the log. A wedged gate stalls EVERY player it serves — the
	// HTTP/2 connection-level window is shared across their streams — so an unlabeled counter would show a
	// burst of increments with nothing to attribute them to. `gate` is the actionable dimension (which gate
	// to restart) and has fleet-bounded cardinality; character and zone would not.
	gateAddr := "unknown"
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		gateAddr = p.Addr.String()
	}
	go watchSendStall(ctx, &sendStarted, stalled, streamSendStallTimeout, streamStallCheckInterval, func(blocked time.Duration) {
		s.log.Warn("play stream reclaimed: a frame has been blocked in Send past the stall bound; "+
			"the gate is answering keepalives but is not reading this stream",
			"character", character, "gate", gateAddr, "blocked", blocked)
		metrics.StreamStalled(context.Background(), gateAddr)
	})

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
		tier:        loginTier,
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
	// Recv runs on its own goroutine so this loop can also select on the writer-stall signal (#274). A bare
	// `stream.Recv()` here is uninterruptible: when the writer is wedged, nothing would ever return the
	// handler, and only the handler returning closes the stream and unblocks that Send. The goroutine exits
	// when Recv errors, or when the stream context is cancelled — which happens the moment we return.
	type recvResult struct {
		f   *playv1.ClientFrame
		err error
	}
	frames := make(chan recvResult, 1)
	go func() {
		for {
			f, err := stream.Recv()
			select {
			case frames <- recvResult{f: f, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	cleanQuit := false
	reclaimed := false // the watchdog tore this stream down; the peer gets a distinct status, not a clean EOF
	for {
		var f *playv1.ClientFrame
		select {
		case <-ctx.Done():
			// The stream ended (client cancelled, transport died, server stopping). This case is REQUIRED, not
			// belt-and-braces: the Recv goroutine races us on the same ctx, and when it wins it returns without
			// ever delivering its error to `frames`. Without this case the handler would block here forever and
			// never post the detach below — the player would stay resident with a dead stream.
			s.log.Debug("stream ctx done", "character", character, "err", ctx.Err())
			goto done
		case <-stalled:
			// The peer is not reading. Returning closes the stream, which unblocks the writer's Send and ends
			// the Recv goroutine. The player enters link-death below and reconnects, exactly as they would if
			// the gate had closed the socket itself.
			s.log.Info("stream torn down by the writer-stall watchdog", "character", character, "gate", gateAddr)
			reclaimed = true
			goto done
		case r := <-frames:
			if r.err != nil {
				s.log.Debug("stream recv ended", "character", character, "err", r.err)
				goto done // EOF or transport error
			}
			f = r.f
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
		case *playv1.ClientFrame_Gmcp:
			// Inbound GMCP request (#92): a rich client ASKS the world for data (Char.Items.Contents).
			// Treated as hostile input from a separate trust domain: bound the payload here at the world
			// ingress (the gate already whitelisted the package), then route it to the owning zone, which
			// resolves any named entity only within the requester's own reach.
			g := pl.Gmcp
			if raw := g.GetJson(); len(raw) <= maxInboundGMCPBytes {
				s.log.Debug("gmcp request received", "character", character, "pkg", g.GetPkg())
				currentZone.Load().post(gmcpRequestMsg{id: character, pkg: g.GetPkg(), json: raw})
			} else {
				s.log.Debug("gmcp request dropped: oversized payload", "character", character, "bytes", len(raw))
			}
		case *playv1.ClientFrame_Detach:
			s.log.Debug("detach received (clean)", "character", character)
			cleanQuit = true
			goto done
		default:
			// Phase 1 ignores resize/pong/attach-after-first.
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
	if reclaimed {
		// A distinct status, not a clean EOF: the gate can tell "the world reclaimed me because I stopped
		// reading" from "the world shut down cleanly", and log accordingly instead of guessing (#274).
		return status.Error(codes.Aborted, "play stream reclaimed: the peer stopped reading")
	}
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

// watchSendStall closes `stalled` when a single stream.Send has been in flight longer than
// streamSendStallTimeout, having first called onStall (#274).
//
// started carries the UnixNano at which the current Send began, or 0 when none is in flight — written by the
// writer goroutine, sampled here. A Send blocked that long means the peer's HTTP/2 flow-control window is
// exhausted and it is not reading. That is the failure gRPC keepalive structurally cannot see: the peer's
// transport keeps acking PINGs independently of application flow control, so the world would otherwise wait
// for the GATE's write-deadline to reclaim the stream — the dependency on gate correctness that keepalive was
// added to remove.
//
// timeout and interval are parameters rather than reads of the package vars so the watchdog is testable
// without mutating shared state under a running goroutine.
//
// It returns on ctx.Done (the stream ended for any other reason), and closes `stalled` at most once.
func watchSendStall(ctx context.Context, started *atomic.Pointer[time.Time], stalled chan struct{}, timeout, interval time.Duration, onStall func(time.Duration)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			begun := started.Load()
			if begun == nil {
				continue // no Send in flight; the peer is keeping up
			}
			if blocked := time.Since(*begun); blocked >= timeout {
				if onStall != nil {
					onStall(blocked)
				}
				close(stalled)
				return
			}
		}
	}
}

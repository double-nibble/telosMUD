package world

import (
	"context"
	"errors"
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

// attachRoute names WHICH branch of the attach-routing decision was taken. It exists so the decision can run
// entirely under s.mu (see resolveAttachZoneLocked) while its two operator-facing WARNs are emitted by the
// caller AFTER the lock is released: s.mu is the shard's hot routing mutex — every zoneByID, every claim,
// every UnhostZone quiescence check takes it — and a log handler is arbitrary caller-supplied code that may
// do I/O. Logging under it would put a disk write on the critical section of the whole shard's routing.
type attachRoute int

const (
	attachRouteHome     attachRoute = iota // the fallback: brand-new character, or nothing better resolved
	attachRouteToken                       // a pending handoff token names the zone holding the pending player
	attachRouteResident                    // a session for this character is still held on this shard (#321)
	attachRouteDurable                     // the character's durable zone_ref, hosted here and honored
	// attachRouteDurableUnhosted and attachRouteDurableInstance both END at the home zone; they are distinct
	// so the caller can report WHY the player's saved location was not honored.
	attachRouteDurableUnhosted // the durable zone_ref names a zone this shard does not host (#320)
	attachRouteDurableInstance // the durable zone_ref is instance-shaped and refused by shape (#411)
)

// resolveAttachZoneLocked decides which hosted zone an attaching connection is handed to, in priority order:
// a pending handoff token, then a session this shard still HOLDS (the #321 residency index), then the
// character's durable zone_ref, then the home zone as the fallback. It returns nil only when this shard hosts
// no home zone at all, plus the route taken so the caller can log it.
//
// THE CALLER MUST HOLD s.mu. It takes s.residentMu inside, which is the established order (UnhostZone does
// exactly this; residentMu is a leaf and never reaches back for s.mu). Running the WHOLE four-branch decision
// under one hold is what lets claimAttachTarget resolve and claim atomically (#413) — and it is a
// strengthening in its own right: before this the decision took THREE separate acquisitions across TWO
// mutexes (home, then residency, then token/zoneByID), so it was not even atomic with itself, and a zone
// could be unhosted between the branch that chose it and the branch that read it.
//
// Kept as its own function, separate from the claim, so the routing decision stays directly testable — it is
// a security boundary (it answers "which live zone does this identity get to enter"), and driving it through
// a full Play stream to exercise one branch is how a branch ends up untested.
func (s *Shard) resolveAttachZoneLocked(character, token string, loaded CharSnapshot, loadedOK bool) (*Zone, attachRoute) {
	zone := s.zones[s.home]
	var resident *Zone
	if token == "" {
		s.residentMu.Lock()
		resident = s.residentZone[character]
		s.residentMu.Unlock()
	}
	switch {
	case token != "":
		if z := s.tokenIndex[token]; z != nil {
			return z, attachRouteToken
		}
		// If no zone holds the token, fall through with the home zone; the zone's attach
		// rejects the unknown token rather than spawning a fresh character.
	case resident != nil:
		// A session for this character is still held on this shard: route to its actual zone so attach
		// re-binds it. This is authoritative over the (possibly stale) durable zone_ref (#321).
		return resident, attachRouteResident
	case loadedOK && loaded.ZoneRef != "" && isInstanceID(loaded.ZoneRef):
		// FAIL CLOSED on an instance-shaped durable location (#411). No write path stores one — a player's
		// durable location while inside an instance is the exit ANCHOR, never the instance itself (#72) — so a
		// row in this shape is a poisoned record or a pre-migration artifact. Honoring it would log a
		// reconnecting player straight into a live private instance, one they may never have entered and whose
		// occupants are somebody else's party. Falling back to home is the same degraded outcome an
		// unhostable durable zone already gets. Note this branch must stay ABOVE the zones[] branch below:
		// the instance IS hosted and IS resolvable, so the shape check is the only thing standing in the way.
		return zone, attachRouteDurableInstance
	case loadedOK && loaded.ZoneRef != "":
		if z := s.zones[loaded.ZoneRef]; z != nil {
			return z, attachRouteDurable
		}
		// This shard does not host the player's durable zone, so we cannot honor it. Falling back to
		// home preserves the pre-#320 behavior (they land in the home start room) rather than refusing
		// the login outright — the gate cannot yet re-resolve, because the directory placement records
		// a SHARD, not a zone. Fixing that is the second half of #320; until then the caller's WARN is the
		// operator's signal that a rebalance stranded someone's durable location.
		return zone, attachRouteDurableUnhosted
	}
	return zone, attachRouteHome
}

// claimAttachTarget resolves the attach destination AND claims the arrival on it, in ONE hold of mu (#413).
// It is the login path's analog of claimTransferTarget, and it exists for the same reason: server.go resolves
// the zone and then delivers an attachMsg through the zone's INBOX, with pop bumped by the handler's
// setPlayer. Between the two a concurrent UnhostZone — which checks quiescence under this SAME mutex — could
// pass its check, delete the zone and close(z.dead), abandoning the post. The player's stream then never
// receives Attached and the gate does not re-resolve on it (#324): a login black hole.
//
// Returns nil (claiming nothing) only when this shard hosts no home zone at all, which the caller turns into
// an Unavailable. Zone.attach releases the claim on every path.
//
// WHY IT TAKES NO LEGITIMACY REFUSALS. It deliberately does NOT reuse claimTransferTarget, whose two refusals
// are both wrong here — the same reason claimEjectTarget is a separate function rather than a boolean
// parameter:
//
//   - `draining`: a draining shard still admits a handoff RE-DIAL (server.go refuses only a FRESH login while
//     draining), so refusing here would break the in-flight cross-shard move that re-dial completes;
//   - `handedOff`: a player already prepared into a zone whose lease flipped mid-drain has their pending
//     session in THAT zone and nowhere else; refusing the bind would strand them.
func (s *Shard) claimAttachTarget(character, token string, loaded CharSnapshot, loadedOK bool) (*Zone, attachRoute) {
	s.mu.Lock()
	defer s.mu.Unlock()
	z, route := s.resolveAttachZoneLocked(character, token, loaded, loadedOK)
	if z == nil {
		return nil, route
	}
	z.claimInboundArrival()
	return z, route
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
	// loginAccount is the ACCOUNT id from the same VERIFIED assertion (#72). It is carried to the session
	// because the instanced-zone caps are charged per ACCOUNT (instance.go) — a per-character cap is routed
	// around by alts and a per-script cap by one script minting for many players — so an unattributable mint
	// is refused rather than sharing a bucket. Like the tier it is set ONLY from a signature-checked claim:
	// never from attach.GetCharacterId()'s neighbourhood of client-supplied fields, and never from Lua. Empty
	// on the dev/unverified path (no verify key), which correctly means "this session may not mint".
	var loginTier, loginAccount string
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
		// The identity check above (claims.Character == the attaching character) is what makes this account
		// binding trustworthy: the claim is signed, and it is signed for THIS character, so a compromised gate
		// cannot replay one account's assertion to charge another account's instance quota.
		loginAccount = claims.Account
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
		var unreadable bool
		loaded, loadedOK, unreadable = s.shard.loadCharacterSnapshot(lctx, character)
		lcancel()
		if unreadable {
			// The durable tier is configured but could not be read. Falling through would take the
			// BRAND-NEW-character branch for a character that very likely has a row — spawning a blank
			// copy in the start room, whose create then collides on the unique name, leaving the player
			// with an ephemeral session that persists nothing (#432). Refusing is both honest and the
			// same fail-closed rule the ownership claim below applies.
			s.log.Error("durable character read failed; refusing the login rather than spawning a blank copy",
				"character", character)
			return status.Error(codes.Unavailable, "character store unavailable; reconnect")
		}
	}

	// Decide which hosted zone this connection starts in. This runs AFTER the snapshot load because the
	// durable record is what names the zone (#320). currentZone is this connection's routing pointer — the
	// zone Stores itself into it on attach, and the reader loop Loads it for every input so a later
	// intra-shard move follows the player.
	//
	//   - handoff re-dial (token != "")  -> whichever zone holds the matching pending player.
	//   - a session STILL HELD on this shard (link-dead resume) -> its ACTUAL live zone (#321).
	//   - rehydrating login              -> the zone named by the durable zone_ref.
	//   - brand-new character, or a zone_ref this shard does not host -> the shard's home zone.
	//
	// Before #320 EVERY non-handoff login attached to `home`, and loginRoom's resolveRoom silently fell
	// back to that zone's start room when the saved room_ref named a room some other zone hosts. A shard
	// hosts many zones (the demo pack ships three), so a player who walked from midgaard into darkwood and
	// logged out there came back standing in midgaard's temple, with their durable location gone. No
	// rebalance and no cross-shard hop required — an ordinary intra-shard walk was enough.
	//
	// The in-memory residency index (#321) takes precedence over the durable zone_ref for a fresh/link-dead
	// login. The durable record LAGS an intra-shard walk (transferIn never flushes; detach's save drains
	// async), so a reconnect that beats the flush would read the stale pre-walk zone, find no session there,
	// take the fresh-login branch, and double-own the character while the detached copy still sits in the zone
	// they walked to. Routing to where the session is actually held re-binds it (attach's re-attach branch)
	// instead. If the session was already reaped (index miss), we fall through to the now-consistent durable
	// zone_ref.
	//
	// The REPORTED #321 case — link death AFTER the walk lands — is fully closed: detach forwards the detach
	// to the destination via z.forwarding, so the session is always held on (and indexed to) the zone it
	// walked to. A residual micro-window remains only for a reconnect landing DURING an in-flight intra-shard
	// transfer (between the source's delPlayer and the destination's setPlayer the session is in no zone's
	// players map), where the index misses and we fall back to the stale durable zone. That is intrinsic to
	// the message-passing handoff and strictly narrower than the pre-fix window; tracked as a follow-up.
	//
	// RESOLVE THE ATTACH ZONE AND CLAIM THE ARRIVAL, atomically under s.mu (#413). See claimAttachTarget:
	// resolving here and posting the attachMsg below is a resolve-then-deliver-async window, and a concurrent
	// UnhostZone could otherwise pass quiescence in it, tear the zone down and abandon the post — a login
	// whose stream never receives Attached, which the gate does not re-resolve on (#324).
	//
	// THE RESIDENCY READ IS SINGLE, AND THAT IS WHY THE ORDER IS THIS WAY (#432 + #413). Two things key off
	// "is this character still resident on this shard": the routing decision (route to the zone that HOLDS
	// the session) and the ownership-claim skip below (a re-attach must not mint a new epoch). They must
	// agree. Taking two separate reads with a blocking store round trip between them is what makes them
	// disagree, and the divergence is not symmetric:
	//
	//   - resident at the CLAIM check, gone by the ROUTING read: the claim is SKIPPED, then the routing
	//     falls through to the durable zone_ref, and Zone.attach takes its fresh-login default at the merely
	//     RESUMED directory epoch — unclaimed. That is precisely the pre-#432 posture: two live copies
	//     holding the same epoch, force-writing over each other. `unindexResident` runs from delPlayer, i.e.
	//     the ordinary link-dead reap, which is exactly what races a reconnect — this is reachable, not
	//     theoretical.
	//   - the mirror (gone at routing, resident at the claim check) is BENIGN: an over-claim, which attach's
	//     re-attach branch absorbs via `if resumeEpoch > s.epoch`.
	//
	// So the resolve runs FIRST and the claim reads `route == attachRouteResident` rather than taking its
	// own look at the index. There is then exactly ONE observation of residency for this login, taken under
	// one hold of s.mu, and the two consumers cannot disagree by construction.
	//
	// The consequence — the arrival claim is held across the ClaimCharacter round trip (up to 2s) — is
	// correct rather than merely tolerable: the destination zone is protected for the WHOLE span in which
	// this login is committed to it. The cost is that an unhost or drain of that zone is REFUSED for up to
	// 2s, which is a refusal (retryable, self-clearing) and not a wedge.
	zone, route := s.shard.claimAttachTarget(character, token, loaded, loadedOK)
	if zone == nil {
		// Decided BEFORE any epoch is minted, deliberately: a shard with no hosted zone would otherwise burn
		// a ClaimCharacter — and thus an ownership generation — on every retry of a login it always refuses.
		s.log.Error("no zone to attach to", "character", character, "home", s.shard.home)
		return status.Error(codes.Unavailable, "no hosted zone; reconnect")
	}
	// LIVE INSURANCE, registered before anything that can return or panic while the claim is held.
	//
	// It used to be dead by construction (the claim was taken below the ownership claim, with only
	// straight-line code after it). It is NOT dead any more: the ClaimCharacter fail-closed return below
	// runs with the claim held, and that is the path this exists for. It also covers the two WARNs — a log
	// handler is arbitrary caller-supplied code that may panic — which is why it is registered ABOVE them.
	//
	// The asymmetry is what justifies it: a leaked claim is PERMANENT — nothing else ever releases it, so
	// the zone can never be unhosted or rebalanced again and every BeginDrain on this process burns its
	// whole deadline — while a spurious release is impossible from here, because `posted` is set on the one
	// path that hands the claim to Zone.attach.
	posted := false
	defer func() {
		if !posted {
			zone.releaseInboundArrival("attach-not-posted")
		}
	}()
	// The routing decision's operator-facing WARNs, emitted OUT from under s.mu (the shard's hot routing
	// mutex: a log handler is arbitrary code that may do I/O, and every zone resolve on this shard queues
	// behind it). Both routes end at the home zone with the player's saved location not honored.
	switch route {
	case attachRouteDurableInstance:
		s.log.Warn("durable zone_ref names a zone INSTANCE; refusing it and falling back to the home zone",
			"character", character, "zone_ref", loaded.ZoneRef, "home", s.shard.home)
	case attachRouteDurableUnhosted:
		s.log.Warn("durable zone not hosted on this shard; falling back to the home zone (the player's saved location is lost)",
			"character", character, "zone_ref", loaded.ZoneRef, "home", s.shard.home)
	case attachRouteHome, attachRouteToken, attachRouteResident, attachRouteDurable:
		// Nothing to report: the route either honored the player's location or is the ordinary fallback.
	}

	// CLAIM OWNERSHIP (#432). A login is an ownership assertion, and until this existed it was not
	// expressed as one anywhere: the epoch was RESUMED at its stored value, so a second login on a
	// different shard came up holding the same epoch as the copy it was displacing, and the two
	// force-wrote over each other. ClaimCharacter mints the next epoch atomically from the durable row,
	// so every claimant gets a distinct, strictly greater value and an epoch names exactly one live copy.
	//
	// The floor is max(directory, row) and is a FLOOR, not a source. The directory contributes the
	// placement epoch of a character whose last handoff outran its last save; the row contributes the
	// authoritative high-water mark. Taking the max is what keeps an evicted or unreachable directory
	// (Redis, TTL'd, #340) from minting BELOW a live character's last epoch — which would wedge them
	// into a session whose every save is refused for its whole lifetime. A directory read failure
	// already degrades to 0 above; with the row in the floor, that costs nothing.
	//
	// WHEN WE DO NOT CLAIM:
	//   - token != ""  — a handoff re-dial. Its epoch was already minted by the source's beginHandoff
	//     and rides the signed Prepare; claiming here would bump the row past the arriving session and
	//     wedge the very player being handed to us.
	//   - route == attachRouteResident — a session for this character is STILL HELD on this shard (the #321
	//     residency index), so this is a re-attach, not a new claim. We already own it. Claiming would raise
	//     the row above the held session's epoch, and until it adopted the new value its in-flight saves
	//     would come back not-owner: a live player, unsaveable and (worse) evictable, from an ordinary
	//     reconnect. Read off the ROUTE, not off a second look at the index — see the single-read note above.
	//   - no durable row (a brand-new name, or a storeless/ephemeral boot) — there is nothing to fence
	//     and nothing to fence against. The row is created at owner_epoch 0 and the first save arms it.
	claimed := resumeEpoch
	if token == "" && loadedOK && loaded.PID != "" && s.shard.saver != nil && s.shard.saver.store != nil &&
		route != attachRouteResident {
		floor := resumeEpoch
		if loaded.OwnerEpoch > floor {
			floor = loaded.OwnerEpoch
		}
		cctx, ccancel := context.WithTimeout(stream.Context(), 2*time.Second)
		ep, cerr := s.shard.saver.store.ClaimCharacter(cctx, loaded.PID, floor)
		ccancel()
		switch {
		case cerr == nil:
			claimed = ep
			s.log.Debug("ownership claimed", "character", character, "epoch", ep, "floor", floor)
		case errors.Is(cerr, ErrNoCharacterRow):
			// The row vanished between the load and the claim (a soft delete). Nothing to fence; fall
			// through unclaimed and let the ordinary login path handle the missing row.
			s.log.Warn("ownership claim found no row; continuing unfenced", "character", character)
		default:
			// FAIL CLOSED. We could admit this login at the resumed epoch, as the code did before — but
			// a session that could not claim cannot be proven to be the owner, and the same store outage
			// that refused the claim will refuse its saves. Admitting it means hours of play that is
			// silently unpersistable, which is strictly worse for the player than being told to
			// reconnect. Unavailable is the code the gate already retries on.
			//
			// This return runs with the arrival claim HELD: the deferred release above is what keeps a
			// store outage from converting every refused login into a permanently un-unhostable zone.
			s.log.Error("ownership claim failed; refusing the login", "character", character, "err", cerr)
			return status.Error(codes.Unavailable, "could not claim character ownership; reconnect")
		}
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
		resumeEpoch: claimed,
		inputSeq:    attach.GetInputSeq(),
		loaded:      loaded,
		loadedOK:    loadedOK,
		tier:        loginTier,
		account:     loginAccount,
	})
	posted = true // the arrival claim is now Zone.attach's to release (#413)
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

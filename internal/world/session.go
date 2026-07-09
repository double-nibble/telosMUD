package world

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/metrics"
)

// sessionOutBuffer is the depth of a session's outbound frame channel (server.go binds it). It absorbs a
// burst of output while the client's writer goroutine drains it; once full, send drops (the zone never
// blocks). slowClientWedgedDrops — a full buffer's worth of CONSECUTIVE drops — is the "this client has
// drained nothing" threshold at which the zone logs the client as wedged (Phase 16.3).
const (
	sessionOutBuffer      = 256
	slowClientWedgedDrops = sessionOutBuffer

	// dropRateWindow / dropRateWarnThreshold drive the windowed per-player drop-RATE warn (#46). Over each
	// window of dropRateWindow send attempts, if the fraction DROPPED reaches the threshold the client is
	// "limping" — draining some frames but falling behind — which the consecutiveDrops "fully wedged" warn
	// above never catches (any successful enqueue resets consecutiveDrops). The window is count-based (no
	// clock read on the hot send path); the warn latches once per limping episode and clears on a healthy
	// window.
	dropRateWindow        = 2 * sessionOutBuffer // 512 send attempts per evaluation window
	dropRateWarnThreshold = 0.5                  // >= 50% of a window dropped => limping
)

// session is a connected character's connection/handoff state — the Phase-2 exactly-once
// substrate (docs/PROTOCOL.md §5), lifted verbatim out of the old player struct
// (docs/PHASE3-PLAN.md §2). It is the value the zone's players map holds, keyed by
// character id, so every Phase-2 handler (attach/detach/reap/prepare/redirect/
// transferIn/forwarding) keeps its exact control flow and changes only in how it reaches
// the in-world object: through session.entity instead of the old player.room + room map.
//
// All fields are zone-owned: only the zone goroutine reads or writes them, so they need
// no locks (the lone exception is currentZone, an atomic.Pointer owned by the Play
// stream). The out channel is the single bridge from the zone to the player's gRPC
// stream writer goroutine (server.go) — the zone enqueues frames, the writer drains
// them — which is what keeps the zone loop from ever blocking on a slow or dead socket.
type session struct {
	character string                   // routing key; mirrors the entity's identity
	out       chan *playv1.ServerFrame // buffered; drained by the writer goroutine in server.go

	// entity is the in-world object this connection drives (its Living + PlayerControlled
	// entity). The PlayerControlled component points back here, so entity <-> session is a
	// two-way link. INVARIANT: entity is wired before the session is inserted into
	// z.players — every code path (attach, prepare) calls newPlayerEntity first — so any
	// session reachable through z.players has a non-nil entity (who/broadcast rely on this).
	// A purely pending entity has its location set but is not yet in a room's contents
	// (invisible) until attach Moves it in. The destination zone takes ownership of session
	// and entity together on a transfer, so only one zone goroutine ever touches the pair.
	entity *Entity

	// tier is the account trust tier (#27) from the VERIFIED session assertion, set on fresh-login attach.
	// player/builder/admin (content-defined ladder); "" (== baseline) on the dev/unverified path. It is the
	// input the zone uses to DERIVE the reserved builder/admin/holylight flags on spawn (applyTierFlags). The
	// FLAGS themselves are NOT persisted or carried on the handoff snapshot (security-audit H-1: a flag restore
	// bypasses the content op guard) — they are re-derived from the tier. The tier IS carried across a
	// cross-shard handoff, but only on the SIGNED snapshot (#106): it rides the authenticated payload and is
	// re-derived at the destination, so an admin/builder keeps elevation across a shard walk while a forged
	// snapshot cannot inject it. A keyless (unverified) shard strips the carried tier at Prepare, so elevation
	// is applied only from a verified source. Zone-goroutine-owned.
	tier string

	// lastVitals / lastStatus are the last GMCP Char.Vitals / Char.Status payloads emitted to this
	// session (Phase 9.2). sendPrompt re-emits a HUD frame only when its payload CHANGED, so an
	// unchanging vitals line isn't re-sent on every prompt. Zone-goroutine-owned (set only in
	// sendPrompt), nil until the first prompt (so the first prompt always emits the initial HUD).
	lastVitals      []byte
	lastStatus      []byte
	lastStats       []byte
	lastRoom        []byte // last GMCP Room.Info payload (Phase 9.3); re-emitted only on a room change
	lastRoomPlayers []byte // last GMCP Room.Players payload (#33); re-emitted only when the visible occupant set changes
	// lastInvItems / lastRoomItems are the last Char.Items snapshots (Phase 9.4 + #48) for the inventory
	// and room-ground panels, keyed by stable gmcpItem id. sendPrompt diffs the live set against these and
	// emits only Add/Remove/Update deltas; a nil map means "not sent yet" (login / reconnect / handoff
	// arrival), which forces a full Char.Items.List. Zone-goroutine-owned like the HUD buffers above.
	lastInvItems  map[string]gmcpItem
	lastRoomItems map[string]gmcpItem

	// vitalsLive is the player's live-vitals HUD pref (#40, vitals.go): when true the prompt carries a
	// "[hp: 80/100 …]" block and re-emits (with a GMCP Char.Vitals delta) at each combat-round boundary,
	// so a round's HP drain tracks live. Default false (the classic bare "> " prompt, update-on-command).
	// Zone-goroutine-owned like the HUD buffers; session-scoped (rides a zone transfer, resets on a
	// cross-shard handoff — a UI pref, not persisted).
	vitalsLive bool

	// showRolls is a STAFF debug pref (#30, toggles.go): when true, a check the player performs that would
	// otherwise be hidden by the engine DEFAULT surfaces its full roll math to them (emitCheck). Set live
	// by `rolls on|off`; only staff can reach the verb (MinRank). Session-scoped like vitalsLive (a debug
	// pref, not persisted); default false. An explicit content visHide is still respected (content intent).
	showRolls bool

	// debugEchoes is a STAFF debug pref (#116, toggles.go): when true, this staff session receives live
	// diagnostic echoes for the zone it is in — the first consumer is Lua script errors in that zone
	// (z.echoDebug, fired from the isolated-error log sites). Set live by `debug on|off`; only staff can
	// reach the verb (MinRank). Session-scoped like showRolls (a debug pref, not persisted); default false.
	debugEchoes bool

	// lastScreenPulse / screenFramesThisPulse rate-limit inbound Lua Screen frames (#120): a content script's
	// screen:show() to this session is capped at maxScreenFramesPerPulse per zone heartbeat, so a tight repaint
	// loop can't flood the terminal. Zone-goroutine-owned (screenShow runs there); reset lazily when the pulse
	// advances (admitScreenFrame). Not persisted — a per-tick counter.
	lastScreenPulse       uint64
	screenFramesThisPulse int

	// lastWho is when this session last ran `who` — the per-session cooldown mark (cmdWho reads and
	// writes it against zone.whoCooldown). Zone-goroutine-owned like the HUD buffers above; it rides
	// the session across an intra-shard zone transfer, so a cross-zone walk doesn't reset the cooldown.
	// A cross-SHARD handoff rebuilds the session and DOES reset it — deliberately not on the handoff
	// snapshot (protocol churn for zero risk: a crossing buys one who, dominated by the handoff cost).
	lastWho time.Time

	// currentZone is the per-connection routing pointer the Play stream owns (server.go):
	// it names the zone this player's input should be posted to right now. The zone that
	// owns the session Stores itself here on attach and on an intra-shard transfer
	// (transferIn), so the reader loop always posts to the CURRENT zone. nil for test-only
	// sessions created without a stream. Reading or Storing it is safe from any goroutine;
	// the pointer itself is the only shared mutable handoff between the source and
	// destination zone goroutines on a move.
	currentZone *atomic.Pointer[Zone]

	// appliedSeq is the highest InputLine.seq this zone has applied for the player — the
	// dedup high-water mark. Any input with seq <= appliedSeq is a replay and is dropped
	// before dispatch, giving exactly-once apply across a re-dial. It is stamped onto
	// every outgoing frame (send) as ServerFrame.ack_input_seq so the gate knows how far
	// the world has consumed.
	appliedSeq uint64

	// stateVersion is the optimistic-concurrency guard for this character's durable record
	// (docs/PERSISTENCE.md §7). It mirrors characters.state_version: a save CASes on it
	// (UPDATE ... WHERE state_version=$old) and the saver posts the bumped value back via
	// saveConflictMsg/the success path so subsequent saves stay monotonic. A stale (zombie)
	// owner saving with a lower version fails the CAS and is rejected — the §7 backstop behind
	// the directory epoch. 0 for a brand-new or storeless (ephemeral) character. Zone-owned:
	// only the zone goroutine reads/writes it (dumpCharacter reads it on-goroutine; the saver
	// posts the bumped value back as an inbox message, never mutating it off-goroutine).
	stateVersion uint64

	// detached/attachGen support re-attach (the gate re-dialing after a Redirect, or a
	// link-death + reconnect). On stream loss the session is NOT removed; it is marked
	// detached and reaped after a grace period unless a new stream re-binds. attachGen is
	// bumped on every (re-)attach so a stale reap timer for an older generation is ignored
	// once the session has re-attached.
	detached  bool
	attachGen uint64

	// quitting marks a clean, player-initiated disconnect ("quit"), so the stream
	// dropping removes the player immediately rather than entering the link-death grace.
	quitting bool

	// Cross-shard handoff (docs/PROTOCOL.md §3, §5). When a player walks through an exit
	// into a zone this shard does not own, the source FREEZES the session: it stops
	// applying input and refuses to remove on stream-drop until the handoff commits or
	// aborts. epoch is the current ownership epoch, bumped on each handoff so the
	// directory's compare-and-set can reject stale routing.
	frozen bool
	epoch  uint64

	// frozenFrom is the room entity the player tried to leave when the cross-shard handoff
	// was initiated. move() detaches the entity from its room while the handoff is in flight;
	// if the handoff FAILS, handoffFailed re-attaches the entity here. Cleared on success
	// (redirect) and after a failed-handoff restore. nil except during an in-flight handoff.
	frozenFrom *Entity

	// handedOff marks that the directory's ownership CAS has COMMITTED for an in-flight
	// handoff — the moment the destination shard becomes the truth (set on the zone goroutine
	// by Zone.markHandedOff, posted by the coordinator the instant SetPlayerShard succeeds,
	// BEFORE the redirectMsg). It is the discriminator the freeze-timeout reaper uses: a frozen
	// session that was handed off had its handoff SUCCEED (the directory points at the
	// destination), so its lingering source copy is an orphan to be REMOVED — thawing it would
	// be a both-own bug; a frozen session that was NOT handed off never committed, so on timeout
	// it is THAWED IN PLACE and restored to frozenFrom. Tying the flag to the CAS commit (not the
	// later Redirect frame) is what makes the reaper's choice independent of freeze-TTL timing.
	// Only meaningful while frozen.
	handedOff bool

	// tellCursor is the in-memory mirror of the durable delivered-cursor (Phase 8.5, OQ-4,
	// character.go TellCursorJSON): tellCursor[authorID] is the highest tell Seq from that author
	// already RENDERED to this player. The durable-tell drain (tell.go) renders a message only when
	// its Seq strictly exceeds the stored value, then advances it — the per-sender idempotency that
	// suppresses a redelivery (<= the cursor) — exactly-once in steady state, never-lost always; the
	// only re-render is the bounded crash-window case (see tell.go's guarantee note). Zone-owned: only the zone
	// goroutine reads/writes it (the drain posts a tellDeliverMsg to the zone, which owns the session),
	// so it needs no lock, exactly like appliedSeq. Loaded from StateJSON on login, dumped on save.
	// nil until first touched (loadTellCursor / the drain lazily create it).
	tellCursor map[string]uint64

	// lastTellFrom is the author id of the most recent tell this player RECEIVED (Phase 8.5): the
	// `reply` target. Zone-owned (set on the zone goroutine when a tell is delivered). Empty until the
	// player has received a tell; `reply` with none tells them there is no one to reply to. It is
	// session-scoped runtime state (not persisted) — a relog clears who you would `reply` to, which is
	// the conventional MUD behavior.
	lastTellFrom string

	// comms is the player's in-memory receiver-side comms state (Phase 8.6, commsstate.go): channel
	// toggles, the ignore list, the AFK flag/message. Zone-owned (only the zone goroutine reads/writes
	// it), so it needs no lock — exactly like tellCursor. Loaded from StateJSON.Comms on login, carried
	// on the handoff snapshot (handoff-transparency), dumped on save. nil until first touched
	// (loadCommsState / a toggle lazily create it); a nil comms is all-default (every channel at its
	// default_on, no ignores, not AFK).
	comms *commsState

	// Destination side: a PENDING session has been rehydrated by Prepare and is waiting
	// for the gate to re-dial. Its entity is not yet in a room's contents and it applies
	// no input until an Attach carrying the matching token activates it. token is the
	// handoff token that re-dial must present.
	pending bool
	token   string

	// framesDropped / consecutiveDrops track slow-client backpressure (Phase 16.3). The zone never blocks on
	// a player's out channel (send drops when it's full); these count how often. consecutiveDrops resets on
	// any successful enqueue, so it only climbs while the client drains NOTHING — a full buffer's worth of
	// consecutive drops means the client is wedged. The zone does not tear the connection down itself (the
	// golden rule: no I/O on the zone goroutine); it relies on the gate's write-deadline to reclaim a wedged
	// socket. These are zone-owned (send runs on the zone goroutine), so they need no lock.
	framesDropped    uint64
	consecutiveDrops int
	// windowSends / windowDrops / dropRateWarned drive the #46 windowed per-player drop-RATE warn: they count
	// send attempts + drops within the current dropRateWindow, and latch the warn so a limping client is
	// flagged once per episode. Zone-owned (send runs on the zone goroutine), so no lock.
	windowSends    int
	windowDrops    int
	dropRateWarned bool

	// pendingNotice is a one-line system message queued while the session is PENDING (the
	// destination side of a cross-shard handoff, before the gate re-dials and s.out is bound).
	// A pending session has no out channel yet, so a notice raised during prepare (e.g. "some
	// items did not transfer" when the destination's enabled-pack set lacks a carried item's
	// prototype) cannot be sent immediately; it is stashed here and flushed once attach binds
	// the stream. Empty when there is nothing to tell the arriving player.
	pendingNotice string

	// gmcpReqTokens / gmcpReqRefill are a per-session TOKEN BUCKET rate-limiting inbound GMCP requests (#92):
	// a client request forces O(container) work on the shared zone goroutine, so a flood is throttled here
	// before the parse/scan/marshal (security review M1). Zone-owned (touched only on the zone goroutine).
	gmcpReqTokens float64
	gmcpReqRefill time.Time
}

// newPlayerEntity builds the in-world half of a player and links it to its session
// (docs/PHASE3-PLAN.md §2). The entity gets a Living component (it is alive) and a
// PlayerControlled component whose session pointer bridges back to the connection; the
// session's entity back-pointer completes the two-way link. character is the player's
// name and durable handle (its proto stands in for the eventual content/persist key);
// the entity is not yet placed in a room (location nil) — join/transferIn/attach do
// that via Move. Built on the zone goroutine, which then owns the pair.
func (z *Zone) newPlayerEntity(s *session, character string) *Entity {
	e := z.newEntity(ProtoRef(character))
	e.short = character
	e.keywords = []string{character}
	// A player fights with the pack's DEFAULT combat profile (Phase 6.3a) — set its combatRef so a
	// `kill` runs the same content to-hit/avoidance/damage pipeline a mob does. Empty when no pack (the
	// bare-engine case) => no profile => auto-hit. Players aren't prototyped, so this is the seam content
	// configures the player's baseline combat through (gear/affects then modify the derived attributes).
	Add(e, &Living{combatRef: z.defBundle().defaultCombat})
	Add(e, &PlayerControlled{session: s})
	s.entity = e
	return e
}

// send queues a frame for delivery to this player's stream writer, stamping the current
// input high-water mark onto it (ServerFrame.ack_input_seq) so the gate can prune its
// replay buffer. Called only from the zone goroutine, which owns appliedSeq, so the read
// is race-free.
//
// It is deliberately non-blocking: if the out buffer is full (client can't keep up, or
// the writer has already stopped) the frame is dropped rather than stalling the zone
// goroutine. Real backpressure/flow control is Phase 14. A single slow client must never
// wedge the whole zone — hence the default branch. Dropped frames are logged at Debug so
// DEBUG=1 surfaces client-can't-keep-up situations.
func (s *session) send(f *playv1.ServerFrame) {
	f.AckInputSeq = s.appliedSeq
	s.windowSends++
	select {
	case s.out <- f:
		s.consecutiveDrops = 0 // the writer drained the buffer; the client is keeping up again
	default:
		s.windowDrops++
		s.framesDropped++
		s.consecutiveDrops++
		slog.Debug("frame dropped: session out buffer full", "player", s.character,
			"consecutive", s.consecutiveDrops, "total", s.framesDropped)
		metrics.FrameDropped(context.Background()) // Phase 16.1: slow-client drop rate (shard-wide, label-free)
		// Phase 16.3: a client that hasn't drained a SINGLE frame for a full buffer's worth of sends is
		// FULLY wedged (dead/stalled TCP). The zone keeps running (this drop is why it never blocks); the
		// gate's write-deadline is what actually reclaims the socket+stream. Warn ONCE at the threshold so
		// ops get a per-player "reclaim incoming" line without one per dropped frame.
		if s.consecutiveDrops == slowClientWedgedDrops {
			slog.Warn("slow client wedged: outbound buffer full for a full buffer of frames; "+
				"gate write-deadline will reclaim the connection",
				"player", s.character, "drops", s.framesDropped)
		}
	}
	// #46: windowed per-player drop-RATE warn — catches a LIMPING client (drains a little, drops most) that
	// the consecutiveDrops "fully wedged" warn above misses (any successful enqueue resets consecutiveDrops).
	// Over each window of dropRateWindow send attempts, warn ONCE if the drop fraction crosses the threshold;
	// a healthy window clears the latch so a later limping episode warns again. Zone goroutine, no clock read.
	if s.windowSends >= dropRateWindow {
		if float64(s.windowDrops) >= dropRateWarnThreshold*float64(s.windowSends) {
			if !s.dropRateWarned {
				s.dropRateWarned = true
				slog.Warn("slow client: high outbound drop rate over the window; the client is falling behind "+
					"(dropping most or all frames)",
					"player", s.character, "dropped", s.windowDrops, "of", s.windowSends)
			}
		} else {
			s.dropRateWarned = false // a healthy window: clear the latch
		}
		s.windowSends, s.windowDrops = 0, 0
	}
}

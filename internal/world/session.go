package world

import (
	"log/slog"
	"sync/atomic"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
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

	// Destination side: a PENDING session has been rehydrated by Prepare and is waiting
	// for the gate to re-dial. Its entity is not yet in a room's contents and it applies
	// no input until an Attach carrying the matching token activates it. token is the
	// handoff token that re-dial must present.
	pending bool
	token   string
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
	Add(e, &Living{})
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
	select {
	case s.out <- f:
	default:
		slog.Debug("frame dropped: session out buffer full", "player", s.character)
	}
}

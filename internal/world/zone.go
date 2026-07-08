package world

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"runtime/debug"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/metrics"
	roster "github.com/double-nibble/telosmud/internal/presence"
	"github.com/double-nibble/telosmud/internal/textsan"
)

// maxPlayerNameRunes caps an externally-sourced player name at the world's gRPC
// boundary — both a Play-attach character id (server.go) and a rehydrated handoff
// snapshot's display name (prepare, below). It mirrors the gate's maxNameLen
// (internal/gate): the edge minted the name under that limit, so a longer one is
// malformed and only a bypassed/forged producer can exceed it.
const maxPlayerNameRunes = 20

// linkDeadGrace is how long a player's in-zone presence survives after its stream
// drops unexpectedly (no clean quit). A re-dial (handoff, docs/PROTOCOL.md §5) or a
// reconnect within this window re-binds to the same player and resumes; otherwise
// the player is reaped. Tunable later.
const linkDeadGrace = 60 * time.Second

// Zone is the actor (docs/ARCHITECTURE.md §3). A single goroutine — Run — owns all
// rooms and players within the zone and is the *only* code that ever reads or
// mutates their state, so game logic needs no locks. Every other goroutine (each
// player's gRPC stream handler in server.go, future cross-zone senders) interacts
// with the zone exclusively by posting messages to inbox; none of them touch zone
// state directly.
//
// Lifecycle of a message: a producer calls post (from any goroutine) -> the message
// lands on the buffered inbox channel -> Run pulls it off and calls handle, which
// runs on the single zone goroutine. From there everything (join/input/leave) is
// sequential and single-threaded.
type Zone struct {
	id        string
	rooms     map[ProtoRef]*Entity // room entities, keyed by their ProtoRef (MUDLIB §4)
	players   map[string]*session  // connection state, keyed by character id
	startRoom ProtoRef             // ProtoRef of the room a fresh login spawns in
	rids      ridAllocator         // per-zone RuntimeID source for entities (identity.go)
	inbox     chan msg             // message queue; the only ingress to zone state
	log       *slog.Logger         // scoped logger: component=zone, zone=<id>

	// lastReconciledPackVer is the version of the newest zone-SHAPE reconcile this zone has applied
	// (#191). A reconcileZoneMsg whose version is ≤ this is DROPPED — a racing reload's stale reconcile
	// must not reorder ahead of a newer one (last-writer-wins by version, not by arrival). Zone-goroutine
	// state (written only in reconcileZone).
	lastReconciledPackVer uint64

	// whoCooldown rate-limits `who` PER SESSION (the shared roster cache already collapses concurrent
	// reads; this blunts a single spammer). Zone-goroutine-read in cmdWho against session.lastWho.
	// Tests that poll `who` on one session set it to 0 at construction (before Run).
	whoCooldown time.Duration

	// pop mirrors len(players) as an atomic so an OFF-goroutine reader (BeginDrain's wait-until-empty poll)
	// can observe occupancy without posting a query. Written ONLY on the zone goroutine (at the join/leave
	// occupancy points), read anywhere. Phase 16.4b.
	pop atomic.Int64

	// protos is the per-SHARD prototype cache (prototype.go), shared READ-ONLY across all
	// the shard's zone goroutines. The zone reads it via spawn; it is never mutated after
	// shard construction, so the cross-goroutine sharing needs no lock. A bare test zone
	// (newZone alone) gets its own private cache so spawn still works standalone.
	protos *protoCache

	// defs is the per-SHARD bundle of pack-global definition registries (attributes/resources/
	// damage-types — defs.go), shared READ-ONLY across all the shard's zone goroutines exactly
	// like protos: each is an atomic-swap table read lock-free from any zone goroutine. A bare
	// test zone (newZone alone) gets its own empty bundle so attr()/resource reads work
	// standalone (returning 0 — no content defined). Set to the shared bundle by a shard.
	defs *defRegistries

	// bareComm is a PRIVATE comms-source fallback (comm.go) for a bare test zone built without a
	// shard: the per-author seq counter + rate-limit buckets the channel publish path needs. A shard-
	// hosted zone never uses it — it reaches the shared shard.comms. Lazily created; zone-goroutine
	// only (a bare zone has no sibling zones racing it). nil until a bare zone first emits on a channel.
	bareComm *commSource

	// shard, if set, is the world process hosting this zone. It is read (never
	// mutated through this field) by the zone goroutine to learn its sibling zones for
	// an intra-shard move and to populate/clear the shard token index. nil on a bare
	// test zone built via newZone/newDemoZone without a shard.
	shard *Shard

	// forwarding routes in-flight input for a player who has just left this zone via
	// an intra-shard transfer to the destination zone. The reader-loop goroutine is
	// separate, so a line it posted to THIS (source) zone in the window between the
	// transfer and its observing the new currentZone would otherwise hit a departed
	// player and be dropped. Instead handleInput re-posts it to the recorded
	// destination, which dedups by appliedSeq — nothing lost, nothing double-applied.
	// Written and read only by this zone's goroutine, so it needs no lock.
	forwarding map[string]*Zone

	// handoff, if set, initiates a cross-shard handoff when a player walks into a
	// zone NO shard on this process owns (set by the Shard). It runs asynchronously and
	// posts results back to the source zone as redirectMsg / handoffFailMsg. nil on a
	// single-shard zone, where cross-shard exits are sealed.
	handoff func(src *Zone, snap *handoffv1.PlayerSnapshot, destZone, destRoom string, epoch uint64)

	// pulses is the per-zone heartbeat scheduler (pulse.go). Its callbacks fire ON THIS
	// zone goroutine, driven by the ticker case in Run's select, so they have full
	// single-writer access to zone state — combat rounds (Phase 6) and affect ticks
	// (Phase 5) hang off it. Plain zone-owned data; only this goroutine touches it.
	pulses *pulseScheduler

	// scopes is this zone's read-only REPLICA of the region/world scope state it cares about
	// (Phase 10.3b, docs/WORLD-EVENTS.md §2). A director owns the authoritative region/world state
	// and broadcasts a change DOWN over the scoped event bus; the shard's subscription posts a
	// scopeDeltaMsg, and applyScopeDelta updates this replica — ALL on the zone goroutine, so the
	// Lua world.flag/world.get/region:get reads are lock-free and never cross a scope boundary (the
	// golden rule: reads are local & cached, writes signal UP). Never nil (newZone builds it empty).
	scopes *scopeReplica

	// eventCascadeDepth is the CAN'T-FORGET recursion backstop for the in-zone event bus
	// (event.go fireEvent). The per-fire effectCtx.depth/eventBudget guards bound a cascade ONLY
	// when every fire site threads its parent ctx — a forget-prone discipline (the 7.8 affect-
	// lifecycle fires were exactly such a forgotten site, resetting depth to 0 and recursing the Go
	// stack unbounded until a FATAL panic took the whole process down — no Lua VM, so no sandbox
	// defense). This zone-scoped counter trips REGARDLESS of whether a fire threaded its parent:
	// fireEvent increments it on entry, decrements on return, and bails (with a Warn) past
	// maxEventCascadeDepth. The zone is single-writer, so a plain int is race-free. It honors the
	// pillar: the engine ENFORCES the bound, it does not assume well-behaved content.
	eventCascadeDepth int

	// saver is the shard's async character writer (saver.go), shared read-only by every
	// hosted zone. The zone produces a CharSnapshot on its own goroutine (dumpCharacter) and
	// hands it to the saver over a buffered channel; the saver does the blocking Redis/Postgres
	// I/O OFF this goroutine and posts the result back as saveConflictMsg/saveOkMsg. nil (or a
	// disabled saver) means ephemeral characters — no durable state, exactly today's behavior.
	// Set by Shard.adopt; never mutated by the zone goroutine.
	saver *saver

	// savePulse holds the cancel handle for this zone's save-cadence pulse callback so the
	// scheduler can be torn down; nil until persistSave starts the cadence (first registered on
	// the first persisted login). Zone-owned, only the zone goroutine touches it.
	savePulse *pulseHandle

	// combatPulse holds the cancel handle for this zone's combat round driver (combat.go). nil until
	// the first startFight arms it (ensureCombatRound); the driver self-cancels and nils it when no
	// Fighting entities remain. ONE per zone drives ALL fights (the centralized [G-G] iteration order).
	// Zone-owned; only the zone goroutine touches it.
	combatPulse *pulseHandle

	// combatRand is the zone-owned SEEDED rng the combat resolver draws from (to-hit / avoidance / damage
	// and in-combat ability rolls). Seeded at newZone from seedFromZoneID(id) so a fight is reproducible
	// from the zone seed (#58) — production no longer draws combat from the process-global math/rand.
	// Mutated ONLY on the zone goroutine (single-writer), so it needs no lock. A test overrides it with a
	// fixed seed for a known sequence; a replay harness can inject a recorded seed the same way.
	combatRand *rand.Rand

	// repopPulse holds the cancel handle for this zone's repop-cadence pulse callback (reset.go).
	// nil until startRepop registers it at build time (skipped when reset_secs==0 — no timed
	// reset). The callback re-runs the reset script each stride, ON this goroutine (single-writer).
	// Zone-owned; only the zone goroutine touches it.
	repopPulse *pulseHandle

	// persistentDone records the reset ops (by their stable identity) whose persistent objects
	// have already been loaded, so a persistent-flagged op (docs/PERSISTENCE.md §4) loads its
	// durable instances from object_instances at MOST ONCE — never re-spawning on each repop tick.
	// Zone-owned, only the zone goroutine writes it (applyReset). Empty for the demo (it flags none).
	persistentDone map[string]bool

	// objects, if set, is the durable world-object loader for persistent reset ops (object_instances,
	// docs/PERSISTENCE.md §4). It is read OFF the zone goroutine (like the saver/login reads) and the
	// loaded objects are posted back as a loadObjectsMsg. nil today — the demo flags no persistent op
	// and no store wires it — so the persistent gate degrades to a logged no-op. Set by Shard.adopt.
	objects ObjectLoader

	// lua is this zone's Lua runtime (luart.go, Phase 7): one *lua.LState plus the restricted-
	// globals sandbox, constructed at zone build and called ONLY from this goroutine (the
	// single-writer invariant — gopher-lua is not goroutine-safe). It is torn down when Run
	// returns. A zone with NO scripted content compiles/runs no scripts: the VM exists but does
	// nothing (the bare-engine invariant — Phase-6 empty-boot behavior is byte-identical). Only
	// the zone goroutine touches it. Slice 7.1 builds the VM + sandbox skeleton; handles, effect
	// ops, and entry points hang off it in 7.2+.
	lua *luaRuntime

	// pendingFinalFlush stashes the LOGOUT snapshot of a brand-new character that quit BEFORE its
	// async CreateCharacter returned a PersistID (createCharacter's goroutine had not posted
	// createdMsg yet). At leave() time the durable flush would otherwise be SKIPPED (enqueueSave
	// guards on pid != nil) and then DROPPED when createdMsg finally arrives to a gone session —
	// silently losing every action the player took during the create round-trip (e.g. the room they
	// walked to). Instead leave() dumps the final snapshot here ON the zone goroutine (race-free:
	// e.location is current) keyed by character name; characterCreated stamps the freshly-minted PID
	// onto it and enqueues the saveFinal once the row exists, so logout stays a true flush point
	// (docs/PERSISTENCE.md §6) even across the create window. Keyed by character name (the create key).
	// Zone-owned; only the zone goroutine writes it.
	pendingFinalFlush map[string]CharSnapshot

	// gen is the zone's generation counter — the forward-looking guard a Lua handle captures
	// at creation (luahandle.go, §1.2) so a handle minted before a hot-reload swap can be
	// recognized as stale. In slice 7.2 it is captured into every handle and re-checked on
	// every method (handleResolve), but it is NEVER bumped yet — the cross-zone/no-dangling
	// guarantee in 7.2 rests on the zone-pointer match + RID-not-found-in-this-zone walk. The
	// BUMP POINT (on the hot-reload chunk swap) is deferred to slice 7.7, where wiring it makes
	// stale-gen handles no-op without any handle-layer change. Zone-owned; only the zone
	// goroutine touches it.
	gen uint64
}

// msg is anything the zone goroutine processes off its inbox. The interface keeps
// the inbox a single typed channel while letting handle switch on concrete type.
type msg interface{ zoneMsg() }

// joinMsg adds a pre-built session (carrying its entity) to the zone directly. Used by
// tests; the network path uses attachMsg (which creates or re-binds and then joins).
type joinMsg struct{ s *session }

// attachMsg binds a player's gRPC stream (its out channel) to a character. If the
// character is unknown it creates and joins a new player; if it already exists
// (a re-dial/reconnect within the link-death window) it re-binds the stream to the
// existing player, preserving appliedSeq so input replay dedups correctly.
type attachMsg struct {
	character string
	token     string // non-empty on a handoff re-dial; binds & activates a pending player
	out       chan *playv1.ServerFrame
	// curZone is the per-connection routing pointer the Play stream owns. The zone
	// Stores itself here once it binds the player, so the reader loop posts subsequent
	// input to this zone (and, after an intra-shard move, to the destination zone). nil
	// for test-only attaches that don't drive a real stream.
	curZone *atomic.Pointer[Zone]
	// resumeEpoch is the player's last-recorded ownership epoch, read from the directory
	// OFF the zone goroutine (server.go) before this attach was posted. Only the fresh-login
	// (default) branch of attach consults it, seeding s.epoch = max(1, resumeEpoch) so the
	// next cross-shard move computes resumeEpoch+1 — which the placement CAS accepts. 0 on a
	// brand-new character or a token re-dial (which carries its own epoch).
	resumeEpoch uint64

	// inputSeq is the gate's Attach.input_seq: the NEXT input seq this connection will send
	// (docs/PROTOCOL.md §5). It is the resume point the link-dead re-attach branch uses to
	// reconcile the dedup fence: when a connection presents a fresh, restarted numbering
	// (input_seq >= 1 and <= the carried appliedSeq), the gate minted a NEW session — a fresh
	// reconnect or a SECOND concurrent login — whose seq 1 would otherwise be wrongly dropped as
	// a replay against the stale high-water. attach clamps appliedSeq = inputSeq-1 so the new
	// connection starts clean. 0 means unspecified (the test/internal path and the redirect
	// replay path, which carry no restarted numbering and MUST preserve appliedSeq).
	inputSeq uint64

	// loaded is the character's durable snapshot, read OFF the zone goroutine (server.go) before
	// this attach was posted — sibling to resumeEpoch, the freshest of {Postgres row, Redis
	// checkpoint}. Only the fresh-login branch consults it: a present snapshot rehydrates the
	// entity (PersistID, room, inventory/equipment) via loadCharacter; a nil snapshot for a
	// known store means a brand-new name (attach mints a row), and a nil store means ephemeral
	// (today's blank-entity login). Never set on a token re-dial or a link-dead re-attach (those
	// already hold a live session). loadedOK distinguishes "no snapshot found" from "no store".
	loaded   CharSnapshot
	loadedOK bool // a durable snapshot was found for this name (rehydrate); else create-or-ephemeral

	// tier is the account trust tier (#27) from the VERIFIED session assertion (server.go), carried to the
	// session so a fresh login applies the matching builder/admin flags on spawn (Slice 3). Empty on the
	// dev/unverified path and on a handoff re-dial (the applied flags ride the entity snapshot), meaning
	// "player unless a signature-checked claim elevated it".
	tier string
}

// inputMsg carries one line of player input. seq is the gate's session-scoped input
// sequence (docs/PROTOCOL.md §5); seq==0 means unsequenced (tests/internal) and is
// always applied. A seq <= the player's appliedSeq is a replay and is dropped.
type inputMsg struct {
	id   string
	seq  uint64
	line string
}

// detachMsg signals that a player's stream dropped. out identifies which stream, so
// a stale detach from a superseded stream (after a re-attach) is ignored. A clean
// quit removes the player immediately; an unexpected drop starts the link-death grace.
type detachMsg struct {
	id  string
	out chan *playv1.ServerFrame
}

// reapMsg fires after the link-death grace to remove a player that never re-attached.
// gen guards against reaping a player that has since re-attached (new generation).
type reapMsg struct {
	id  string
	gen uint64
}

// leaveMsg removes a player from the zone immediately.
type leaveMsg struct{ id string }

// whoFallbackMsg is posted back by the async cross-shard `who` read (cmdWho) when the roster read FAILED:
// the fallback zone-local render runs on the zone goroutine (single-writer over z.players) and writes to
// the captured out channel. A roster miss thus degrades to the local list, never an error to the player.
type whoFallbackMsg struct {
	out    chan *playv1.ServerFrame
	viewer *Entity // the requester, for the #28 canSee visibility filter (captured at post time)
}

// whoRenderMsg carries a SUCCESSFUL cross-shard roster read (cmdWho) back onto the zone goroutine so the
// render happens under single-writer discipline (#24). The blocking Redis read must stay OFF the zone
// goroutine, but the render must be ON it: a content `who` display template enters the zone's Lua VM, which
// is one-per-zone and zone-goroutine-owned — rendering it in the async fetch goroutine would be a data race
// against every other script the zone runs. So the fetcher does I/O only and posts the raw entries here; the
// inbox handler renders (template, else the built-in renderWho) and writes the frame.
//
// entries are REMOTE data (a snapshot of other shards' rosters), not live entities, so carrying them across
// the goroutine boundary in a message is sound — the message IS the ownership transfer.
type whoRenderMsg struct {
	out     chan *playv1.ServerFrame
	viewer  *Entity        // the requester (the render's `self`), captured at post time
	entries []roster.Entry // the roster snapshot the async read returned
	seeAll  bool           // the viewer's holylight, captured ON the zone goroutine before the async read (#98)
}

// transferInMsg hands an existing session (and its entity) from a sibling zone on the
// SAME shard (an intra-shard cross-zone walk). The destination zone takes ownership: it
// Moves the entity into room, Stores itself into the session's currentZone pointer so
// input now routes here, and shows the new room. The SAME out channel and appliedSeq are
// carried, so there is no snapshot, no epoch bump, no directory change — and replayed
// in-flight input (forwarded by the source) still dedups by appliedSeq.
type transferInMsg struct {
	s    *session
	room ProtoRef
}

// redirectMsg is posted back by the async handoff coordinator once the destination
// shard is chosen and the directory updated: the zone sends the player a Redirect
// frame (the gate will re-dial the new shard). The player stays frozen.
type redirectMsg struct {
	id         string
	targetAddr string
	token      string
	epoch      uint64
}

// handedOffMsg is the commit-marker posted back the instant the directory ownership CAS
// (SetPlayerShard) succeeds, BEFORE redirectMsg. It flips the freeze-reaper's success
// discriminator (session.handedOff) at the exact point the both-own truth flips — the CAS
// commit — rather than one step later at Redirect-frame send. Enqueued ahead of redirectMsg,
// so a freezeExpire firing in the old gap (after the CAS commit, before the frame) now
// observes handedOff=true and reaps the orphan instead of thawing a player whose handoff
// already succeeded. See Zone.markHandedOff and Shard.beginHandoff.
type handedOffMsg struct{ id string }

// handoffFailMsg is posted back if the handoff could not be initiated, so the zone
// thaws the otherwise-stuck frozen player.
type handoffFailMsg struct {
	id     string
	reason string
}

// prepareMsg is the destination side: rehydrate the snapshot as a PENDING player in
// this zone and reply on the channel (nil on success). Posted by the Handoff server.
type prepareMsg struct {
	snap  *handoffv1.PlayerSnapshot
	room  ProtoRef
	epoch uint64
	token string
	reply chan error
}

// abortPendingMsg discards a pending player by handoff token (source cancelled).
type abortPendingMsg struct{ token string }

// pendingExpireMsg fires if a pending player is never bound by the gate within the
// TTL; gen guards against expiring one that has since been activated/rebuilt.
type pendingExpireMsg struct {
	id  string
	gen uint64
}

// freezeExpireMsg fires if a frozen (source-side, in-flight handoff) player is still
// frozen after freezeTTL — the backstop for a handoff that neither thawed (RPC timeout)
// nor was reclaimed. gen (the session's attachGen at freeze time) guards against acting
// on a session that has since rebound/rebuilt. See Zone.freezeExpire for thaw-vs-reap.
type freezeExpireMsg struct {
	id  string
	gen uint64
}

// saveConflictMsg is posted BACK to the zone by the async saver (saver.go) when a Postgres
// state_version CAS finds zero rows — a stale writer (a zombie/duplicated owner saved first).
// The zone reconciles ON its own goroutine: re-load the current durable version and re-dump at
// it, so the next save's CAS matches. Reconciliation is single-writer (only the zone touches the
// session) — the saver never mutates entity state off-goroutine (docs/PHASE4-PLAN.md §4).
type saveConflictMsg struct{ id string }

// saveOkMsg carries a successful Postgres flush's bumped state_version back to the zone, so the
// session's stateVersion advances in lockstep with the row (every save CASes on the prior value).
// Posted by the saver; applied on the zone goroutine.
type saveOkMsg struct {
	id         string
	newVersion uint64
}

// saveReconcileMsg carries the freshly RE-READ durable version back to the zone after a live
// player's CAS loss (Zone.saveConflict's spawned re-read). The handler adopts the version (like
// saveOk) AND re-dumps the player's current in-memory state, re-enqueuing the flush — so a conflict
// re-WRITES current state at the new version instead of silently dropping the data it was carrying.
// Distinct from saveOkMsg precisely because it re-enqueues a save; saveOk must not (it is also
// posted by the saver's own final-flush success, where re-enqueuing would loop). Applied on the
// zone goroutine, so the re-dump stays single-writer.
type saveReconcileMsg struct {
	id         string
	newVersion uint64
}

// drainFlushMsg requests an immediate durable flush of every persisted player in this zone — the
// shard-drain flush point (Shard.Drain, a rolling-redeploy hook). The dump runs on the zone
// goroutine; the write is the saver's job. Phase 4 builds it; Phase 10 wires the trigger.
type drainFlushMsg struct{}

// createdMsg carries a freshly-minted PersistID back to the zone after a brand-new character's
// row was INSERTed off the zone goroutine (createCharacter). The zone adopts the UUID onto the
// live entity here — PersistID becomes REAL — so subsequent cadence saves can CAS the row.
type createdMsg struct {
	id  string
	pid PersistID
}

// createFailedMsg signals that a brand-new character's async CreateCharacter (createCharacter's
// goroutine) FAILED PERMANENTLY — it returned an error and will never post a createdMsg. This is a
// clean one-shot terminal signal (the goroutine has a single error-return; there is no retry loop),
// so it is the precise eviction trigger for the create-window logout stash: if the player ALSO quit
// inside the create round-trip, leave() parked a snapshot in pendingFinalFlush that createdMsg would
// normally replay-or-drop. With no createdMsg ever coming, that entry would linger for the zone's
// lifetime (FOLLOW-UPS §2). The zone handles this by deleting the stash entry — delete-only, so it
// can never resurrect or mis-key a flush. It is a no-op when the player is still present (the live
// cadence/logout path owns the record) or when there was no stash (the common fast-create case).
type createFailedMsg struct {
	id string
}

// adoptPidMsg carries a PersistID + durable state_version RESOLVED BY NAME back to the zone after a
// cross-shard bind found the handed-off session had no PID (the async-create window: a brand-new
// char handed off BEFORE its CreateCharacter returned the PID at the source, so the snapshot carried
// none). The destination re-resolves the row by name OFF the zone goroutine (resolveHandoffPid) and
// adopts it here so it can flush the player to the SAME durable row. version is adopted WITH the PID
// so the destination's first save CASes against the right base (monotonic). Idempotent + guarded:
// adopted only if the entity still has no PID (a concurrent createdMsg or a carried PID wins first).
type adoptPidMsg struct {
	id      string
	pid     PersistID
	version uint64
}

// presenceMsg is a synchronous query answered ON the zone goroutine: it reports whether a player
// is currently registered and whether its entity has a durable PersistID yet (the brand-new-
// character create is async, so the PID lands a beat after login). Because z.players is
// zone-owned, an external caller must never read it directly; this routes the read through the
// inbox so it stays single-writer. It is the race-free probe the persistence tests (and a future
// ops/health endpoint) use to observe login/logout/create without touching zone state. reply is
// buffered by the caller so the handler never blocks.
type presenceMsg struct {
	id    string
	reply chan presence
}

// presence is the answer to a presenceMsg.
type presence struct {
	present bool // the player is registered in this zone
	pidSet  bool // its entity has a durable PersistID (the create returned)
	stashed bool // a create-window logout snapshot is parked in pendingFinalFlush for this name
}

// loadObjectsMsg carries durable world-objects (object_instances rows) read OFF the zone goroutine
// back to the zone, for a persistent-flagged reset op (reset.go, docs/PERSISTENCE.md §4). The zone
// spawns each object from its proto ref into the target room/container ON its goroutine (single-
// writer), exactly like loadCharacter rehydrates a player's inventory. Empty/absent today — the
// demo flags no persistent op — so this path is dormant until a persistent op + a wired loader.
type loadObjectsMsg struct {
	target  *Entity            // the room or container the objects belong in (resolved on-goroutine)
	objects []PersistentObject // the durable instances to rehydrate
}

// reloadLuaMsg tells a zone to apply a content Lua hot reload for a (kind, ref) whose prototype the
// shard reloader already swapped into the shared cache (slice 7.7). It is posted to EACH hosted
// zone's inbox so the chunk recompile + the per-instance handler re-registration run ON THE ZONE
// GOROUTINE (the per-zone LState + entityScripts are zone-owned — never written cross-goroutine).
type reloadLuaMsg struct {
	kind string // the content kind ("mob"/"room"/"item"/"ability"/"affect"/...)
	ref  string // the (kind, ref) whose Lua was edited
}

// republishCommsMsg tells a zone to RE-PUBLISH every hosted player's comms config after a channel_def hot
// reload changed a channel's access/hear_access (#75). The gate's per-player HEAR-filter is PUSHED (the world
// publishes the effective hear-set), so a channel retightened mid-session leaves an already-subscribed player
// with a stale, too-permissive subscription until their next toggle/handoff/relog — the security gap this
// closes. Posted to EACH hosted zone (a channel is shard-global) so the per-player republish runs ON THE ZONE
// GOROUTINE (sessions are zone-owned). Level-triggered + idempotent: publishCommsConfig recomputes the
// current effective hear-set, so a re-post (bounded-retry on a dropped fan-out) always emits correct state
// and a duplicate is harmless. ref is the reloaded channel, for logging only.
type republishCommsMsg struct {
	ref string
}

// reconcileZoneMsg tells a zone to converge its live room SHAPE to the reloaded content's DESIRED state
// (#191): the KindZone invalidation carries that state (rooms + start room + a monotonic version) on the
// wire, and the reloader hands it here so the diff-and-converge (spawn ADDs, resync UPDATEs, tear down
// DELETIONs, apply the start_room change) runs ON THE ZONE GOROUTINE (single-writer over z.rooms /
// z.startRoom / z.players). zoneRef is the zone the reconcile is FOR — the reloader posts only to the
// matching hosted zone, but the handler re-asserts z.id == zoneRef defensively. version orders reconciles
// (last-writer-wins by version, not arrival) so a racing reload's stale reconcile is dropped.
//
// INVARIANT for the bounded-retry follow-up (#194, PR 3/3): a retry of a DROPPED reconcile MUST re-post
// this SAME immutable message with its ORIGINAL version — never re-stamp a fresh version. The version
// guard advances the cursor only when a reconcile APPLIES (world.go reconcileZone), so a dropped message
// left the cursor untouched and a same-version retry re-applies cleanly; but a re-stamped (higher) version
// on retry could win last-writer-wins over a genuinely newer concurrent reload and RESURRECT a stale
// desired state — a split-brain. Retry = re-enqueue this value as-is (it also stays shard-local, off the
// bus, so it never crosses the wire version's clock-skew boundary).
type reconcileZoneMsg struct {
	zoneRef   string   // the zone this reconcile targets (== z.id)
	version   uint64   // monotonic reload stamp; a reconcile ≤ z.lastReconciledPackVer is dropped
	rooms     []string // the refs the reloaded content says SHOULD be live in this zone
	startRoom ProtoRef // the reloaded start/login room (applied before removals)
}

// reloadDoneMsg reports the outcome of a `reload` command's BACKGROUND fan-out back to the builder who
// triggered it (reloadcmd.go). The re-read + per-ref publish runs off the zone goroutine, so the result
// is posted here to be delivered ON the zone goroutine — where z.players is single-writer, so it sends
// only if the builder is STILL present. A builder who quit/moved mid-reload simply gets no readout rather
// than a send on a torn-down session (the channel-safety the async path needs).
type reloadDoneMsg struct {
	player  string // the character id that ran `reload` (z.players key)
	summary string // the finished-fan-out line to show them
}

// pullResultMsg reports the outcome of a director-coordinated `pull <version>` back to the builder who
// ran it (#230). Unlike reload (whose outcome is local to the issuing shard), a pull runs on the world
// DIRECTOR, which broadcasts the result DOWN on the world scope; the shard's scope replication fans this
// message to every hosted zone, and — exactly like reloadDoneMsg — the zone that STILL hosts the builder
// delivers it (single-writer over z.players). A builder who quit/moved gets no readout rather than a bad
// send. Every other zone is a clean no-op (the player isn't theirs).
type pullResultMsg struct {
	player  string // the builder character id that ran `pull` (z.players key)
	summary string // the pass/fail line to show them
}

func (joinMsg) zoneMsg()           {}
func (attachMsg) zoneMsg()         {}
func (inputMsg) zoneMsg()          {}
func (detachMsg) zoneMsg()         {}
func (reapMsg) zoneMsg()           {}
func (leaveMsg) zoneMsg()          {}
func (transferInMsg) zoneMsg()     {}
func (redirectMsg) zoneMsg()       {}
func (handedOffMsg) zoneMsg()      {}
func (handoffFailMsg) zoneMsg()    {}
func (prepareMsg) zoneMsg()        {}
func (abortPendingMsg) zoneMsg()   {}
func (pendingExpireMsg) zoneMsg()  {}
func (freezeExpireMsg) zoneMsg()   {}
func (saveConflictMsg) zoneMsg()   {}
func (saveOkMsg) zoneMsg()         {}
func (saveReconcileMsg) zoneMsg()  {}
func (drainFlushMsg) zoneMsg()     {}
func (createdMsg) zoneMsg()        {}
func (createFailedMsg) zoneMsg()   {}
func (adoptPidMsg) zoneMsg()       {}
func (presenceMsg) zoneMsg()       {}
func (loadObjectsMsg) zoneMsg()    {}
func (reloadLuaMsg) zoneMsg()      {}
func (reconcileZoneMsg) zoneMsg()  {}
func (republishCommsMsg) zoneMsg() {}
func (reloadDoneMsg) zoneMsg()     {}
func (pullResultMsg) zoneMsg()     {}
func (whoFallbackMsg) zoneMsg()    {}
func (whoRenderMsg) zoneMsg()      {}

func newZone(id string) *Zone {
	z := &Zone{
		id:                id,
		whoCooldown:       defaultWhoCooldown,
		rooms:             map[ProtoRef]*Entity{},
		players:           map[string]*session{},
		forwarding:        map[string]*Zone{},
		persistentDone:    map[string]bool{},
		pendingFinalFlush: map[string]CharSnapshot{},
		inbox:             make(chan msg, 256),
		// Zone-owned combat rng (#58): the combat resolver + player-cast ability rolls draw from THIS
		// instance instead of the process-global math/rand, so a fight is seedable/replayable. Seeded from
		// ENTROPY in production, deliberately: a zone-id-derived seed would make live crits predictable
		// every restart (the id is public) — worse than today's global rand — and would correlate this
		// stream with z.lua.rng (also id-seeded). A test or a replay harness reassigns z.combatRand with a
		// FIXED seed for a reproducible sequence. Mutated only on the zone goroutine (single-writer).
		combatRand: rand.New(rand.NewSource(rand.Int63())), //nolint:gosec // gameplay roll, not security
		// A private, empty prototype cache by default. A shard-hosted zone has this
		// replaced with the shared per-shard cache (newShard); a bare test zone keeps its
		// own so spawn works standalone.
		protos: newProtoCache(),
		// A private, empty definition-registry bundle by default (defs.go). A shard-hosted zone
		// has this replaced with the shared per-shard bundle; a bare test zone keeps its own so
		// attr()/resource reads work standalone (reporting 0/absent — no content defined).
		defs: newDefRegistries(),
		// Per-zone heartbeat scheduler (pulse.go). Empty until something registers a
		// callback; the ticker in Run is a cheap no-op until then.
		pulses: newPulseScheduler(),
		// Per-zone region/world scope-state replica (scope.go, Phase 10.3b). Empty until the shard's
		// scoped-bus subscription delivers a director broadcast; regionID is set when a shard adopts
		// the zone into a region (WithScopeBus). A bare/regionless zone keeps an empty replica.
		scopes: newScopeReplica(),
		// Scoped logger so every line this zone emits is tagged with its id; all
		// the verbose control-flow tracing below goes through z.log at Debug.
		log: slog.With("component", "zone", "zone", id),
	}
	// The per-zone Lua runtime (luart.go): the VM + the restricted-globals sandbox, built at
	// zone construction so it is live before Run starts, and torn down when Run returns. Built
	// for EVERY zone (including bare/empty ones) — it is inert until scripted content runs
	// through it, so the bare-engine invariant holds. Seeded deterministically from the zone id
	// so script RNG is reproducible (T9).
	z.lua = newLuaRuntime(id, z.log)
	// Wire the runtime's back-pointer to its owning zone so the mud.* world table (luamud.go)
	// can read/message/spawn into the zone and schedule on its pulse wheel. The runtime is
	// built from the zone id; the *Zone exists only now, so the back-pointer is set here.
	z.lua.zone = z
	return z
}

// post enqueues a message for the zone goroutine. Safe to call from any goroutine —
// this is the *only* sanctioned way to reach zone state from outside the loop.
func (z *Zone) post(m msg) { z.inbox <- m }

// postOrDrop is a NON-BLOCKING inbox post: it enqueues m if the inbox has room, else DROPS it and returns
// false. Only for RECOVERABLE notices — e.g. a hot-reload invalidation, where the shared prototype cache is
// ALREADY swapped so a dropped notice just means that zone recompiles its Lua chunk on the next
// invalidation/access — where a blocking post to ONE saturated zone must not head-of-line-stall a shard-wide
// fan-out. NEVER use it for state the zone must not miss (attach/handoff/leave/save/input).
func (z *Zone) postOrDrop(m msg) bool {
	select {
	case z.inbox <- m:
		return true
	default:
		return false
	}
}

// Run is the zone's single-threaded event loop and the heart of the actor model.
// It runs on one dedicated goroutine and serially handles inbox messages until ctx
// is cancelled. Because all state mutation funnels through here, no other goroutine
// ever races it.
func (z *Zone) Run(ctx context.Context) {
	z.log.Debug("zone loop start", "rooms", len(z.rooms), "start_room", z.startRoom)
	// The heartbeat: one ticker owned by the loop goroutine. On each tick the loop calls
	// pulses.tick INLINE — so every periodic/delayed callback runs on THIS goroutine with
	// the same single-writer access a command handler has (pulse.go). The ticker only fires
	// a select wakeup; it never touches entity state itself, and with no registered
	// callbacks tick is a no-op, so adding it cannot perturb the deterministic tests (they
	// register none) — it just costs one cheap wakeup per pulseInterval on an idle zone.
	ticker := time.NewTicker(pulseInterval)
	defer ticker.Stop()
	// Tear the Lua VM down when the loop exits — on THIS goroutine, the only one that ever
	// touched it (gopher-lua is not goroutine-safe). After this no script can run; the zone is
	// stopping anyway.
	defer z.lua.close()
	lastTick := time.Now() // Phase 16.1: tick-lag = how far past the budget each heartbeat fires.
	for {
		select {
		case <-ctx.Done():
			z.log.Debug("zone loop stop", "players", len(z.players))
			return
		case m := <-z.inbox:
			z.handle(m)
		case <-ticker.C:
			// tick-lag: the gap since the previous tick MINUS the budget. ~0 on a healthy zone; it grows
			// when the single-writer goroutine can't keep up (saturated by inbox + pulse work) — the headline
			// scale signal. The Go ticker coalesces missed ticks, so a slow zone shows a widening gap here.
			now := time.Now()
			if lag := now.Sub(lastTick) - pulseInterval; lag > 0 {
				metrics.RecordTickLag(ctx, z.id, float64(lag.Microseconds())/1000.0)
			}
			lastTick = now
			z.pulses.tick()
		}
	}
}

// handle dispatches one inbox message to the matching handler. Runs only on the
// zone goroutine (called from Run), so all handlers below are lock-free.
//
// It is wrapped in a recover() that is the process-survival net: an unrecovered panic
// in ANY handler (attach/prepare/redirect/handoffFailed/detach/reap/...) would otherwise
// propagate out of Zone.Run and crash the WHOLE world process — every zone, every player.
// On a panic we log the offending message type + stack and CONTINUE the loop. This layers
// with dispatchSafe (the COMMAND path's nicer per-player message); this outer net catches
// everything else. The underlying bug should still be fixed — the source nil-derefs below
// (attach pending-bind, prepare unknown-room) are guarded so this net is rarely tripped.
func (z *Zone) handle(m msg) {
	defer func() {
		if r := recover(); r != nil {
			z.log.Error("zone handler panicked; zone survived",
				"msg_type", fmt.Sprintf("%T", m), "panic", r, "stack", string(debug.Stack()))
		}
	}()
	switch v := m.(type) {
	case joinMsg:
		z.log.Debug("inbox: join", "player", v.s.character)
		z.join(v.s, "") // test/direct join: always the start room
	case attachMsg:
		z.log.Debug("inbox: attach", "player", v.character)
		z.attach(v)
	case transferInMsg:
		z.transferIn(v)
	case prepareMsg:
		z.prepare(v)
	case abortPendingMsg:
		z.abortPending(v.token)
	case pendingExpireMsg:
		z.pendingExpire(v.id, v.gen)
	case freezeExpireMsg:
		z.freezeExpire(v.id, v.gen)
	case inputMsg:
		z.handleInput(v)
	case detachMsg:
		z.log.Debug("inbox: detach", "player", v.id)
		z.detach(v.id, v.out)
	case reapMsg:
		z.reap(v.id, v.gen)
	case redirectMsg:
		z.redirect(v)
	case handedOffMsg:
		z.markHandedOff(v.id)
	case handoffFailMsg:
		z.handoffFailed(v)
	case leaveMsg:
		z.log.Debug("inbox: leave", "player", v.id)
		z.leave(v.id)
	case saveConflictMsg:
		z.saveConflict(v.id)
	case saveOkMsg:
		z.saveOk(v.id, v.newVersion)
	case saveReconcileMsg:
		z.saveReconcile(v.id, v.newVersion)
	case drainFlushMsg:
		z.saveAll(saveFlush)
	case drainZoneMsg:
		z.drainZone() // Phase 16.4b: hand every live player off to the zone's new (post-flip) owner
	case createdMsg:
		z.characterCreated(v.id, v.pid)
	case createFailedMsg:
		z.characterCreateFailed(v.id)
	case adoptPidMsg:
		z.adoptHandoffPid(v.id, v.pid, v.version)
	case presenceMsg:
		s, present := z.players[v.id]
		pidSet := present && s.entity != nil && s.entity.pid != nil
		_, stashed := z.pendingFinalFlush[v.id]
		v.reply <- presence{present: present, pidSet: pidSet, stashed: stashed}
	case loadObjectsMsg:
		z.rehydrateObjects(v)
	case reloadLuaMsg:
		// Recompile the (kind, ref)'s Lua chunk + re-register live instances' handlers from the swapped
		// source. Room SHAPE (add/update/remove of the room entity + start_room) is NOT handled here — it is
		// owned by the KindZone reconcile (reconcileZone, #191), the single authoritative zone-shape path;
		// this only refreshes Lua state, which is orthogonal to shape.
		z.reloadLua(v.kind, v.ref)
	case reconcileZoneMsg:
		// Defensive: only reconcile the zone this message targets (the reloader posts to the matching zone,
		// but a mis-post must never converge another zone against a foreign room set).
		if v.zoneRef == z.id {
			z.reconcileZone(v)
		}
	case republishCommsMsg:
		// A channel_def hot reload changed a channel's access/hear_access — re-publish EVERY hosted player's
		// comms config so a retightened channel drops (or a loosened one adds) their subscription now, not at
		// their next toggle/handoff/relog (#75). Unconditional over the current player set: the hear-set can
		// move in BOTH directions (a gate added → drop; a gate removed → add), so the anyChannelGatesHearing
		// short-circuit that guards the per-entity access-change path would MISS the loosened case here.
		z.republishAllComms(v.ref)
	case reloadDoneMsg:
		// Deliver a `reload` fan-out result to the builder if they are still in this zone (single-writer
		// over z.players); a builder who left mid-reload gets nothing rather than a bad send.
		if s, ok := z.players[v.player]; ok {
			s.send(textFrame(v.summary))
		}
	case pullResultMsg:
		// Deliver a coordinated `pull` outcome (#230) to the builder if they are still in this zone; every
		// other hosted zone the director's world-scope broadcast fanned to is a clean no-op.
		if s, ok := z.players[v.player]; ok {
			s.send(textFrame(v.summary))
		}
	case whoFallbackMsg:
		writeFrameTo(v.out, textFrame(z.whoLocalSheet(v.viewer)))
	case whoRenderMsg:
		// The async roster read landed. Render HERE, on the zone goroutine (single-writer): a content `who`
		// template enters the zone-owned Lua VM, so it may only run on this goroutine (#24). No template (or a
		// broken one) falls back to the built-in renderWho — the pre-#24 output, unchanged.
		if sheet, ok := z.renderWhoSheet(v.viewer, v.entries, v.seeAll); ok {
			writeFrameTo(v.out, textFrame(sheet))
		} else {
			writeFrameTo(v.out, textFrame(renderWho(v.entries, v.seeAll)))
		}
	case tellDeliverMsg:
		v.ack <- z.deliverDrainedTell(v) // drained durable tell: dedup-via-cursor, render+emit, ack/nak
	case tellCursorProbeMsg:
		z.probeTellCursor(v)
	case lastTellProbeMsg:
		z.probeLastTell(v)
	case scopeDeltaMsg:
		z.applyScopeDelta(v) // a director's region/world broadcast updates this zone's read-replica
	case scopeEventMsg:
		z.fireScopeEvent(v) // a director's remote-effect broadcast fires on_world/on_region handlers
	}
}

// handleInput applies one input line with exactly-once semantics. A sequenced line
// (seq>0) at or below the player's high-water mark is a replay — dropped before it
// can run a second time (docs/PROTOCOL.md §5). Otherwise the high-water advances and
// the line is dispatched.
func (z *Zone) handleInput(v inputMsg) {
	s := z.players[v.id]
	if s == nil {
		// The player may have just left via an intra-shard transfer while the separate
		// reader-loop goroutine was still posting to this (source) zone. Re-post the line
		// to the destination zone, which dedups by appliedSeq so it is neither lost nor
		// double-applied. Once the reader loop observes the new currentZone it posts
		// there directly and this forwarding entry is never consulted again.
		if dest := z.forwarding[v.id]; dest != nil {
			z.log.Debug("forwarding in-flight input to destination zone",
				"player", v.id, "seq", v.seq, "to_zone", dest.id)
			dest.post(v)
			return
		}
		// Input for a player the zone no longer knows about (e.g. leave/input race).
		z.log.Debug("inbox: input for unknown player", "player", v.id)
		return
	}
	if s.frozen {
		// A cross-shard handoff is in progress: this shard no longer acts for the
		// player. The gate buffers input typed during the redirect and replays it to
		// the destination shard (PROTOCOL.md §5); applying it here would double-act.
		z.log.Debug("input dropped: player frozen (handoff in progress)", "player", v.id, "seq", v.seq)
		return
	}
	if v.seq != 0 && v.seq <= s.appliedSeq {
		// Replay of an already-applied line: drop it. No dispatch, no output, so the
		// command's side effects happen exactly once across a re-dial.
		z.log.Debug("duplicate input dropped", "player", v.id, "seq", v.seq, "applied", s.appliedSeq)
		return
	}
	if v.seq != 0 {
		s.appliedSeq = v.seq
	}
	z.log.Debug("inbox: input", "player", v.id, "seq", v.seq, "line", v.line)
	z.dispatchSafe(s, v.line)
}

// dispatchSafe runs one command with panic recovery. A bug in a handler must NEVER crash the
// zone goroutine: an unrecovered panic there is fatal to the whole world process and every
// player on every zone it hosts (a single malformed command would be a DoS). On a panic we
// log the stack, tell the offending player their command failed, and the zone keeps serving
// everyone else. This is the safety net; the underlying bug should still be fixed.
func (z *Zone) dispatchSafe(s *session, line string) {
	defer func() {
		if r := recover(); r != nil {
			z.log.Error("command handler panicked; zone survived",
				"player", s.character, "line", line, "panic", r, "stack", string(debug.Stack()))
			s.send(textFrame("Something went wrong with that command."))
			z.sendPrompt(s)
		}
	}()
	z.dispatch(s, line)
}

// join places a newly connected player into the world at room (a fresh login uses the start room
// — room=="" — and a rehydrated login uses its SAVED room ref; resolveRoom falls back to the
// start room for an unknown ref): it registers the player, announces the arrival to the room,
// shows the player their surroundings, and primes the prompt.
func (z *Zone) join(s *session, room ProtoRef) {
	r := z.resolveRoom(room) // empty room => start-room fallback (resolveRoom)
	if r == nil {
		// Empty-world boot (bare-engine invariant, docs/PHASE4-PLAN.md §7.5): the zone hosts
		// no rooms (no content loaded / no start room), so there is nowhere to place the
		// player. Reject the login cleanly rather than registering a roomless player and then
		// null-deref'ing in lookRoom/act. The player is NOT added to z.players, so no later
		// command finds a placeless session.
		z.log.Warn("login rejected: zone has no rooms (empty world)", "player", s.character, "zone", z.id)
		s.send(textFrame("This world has no rooms yet. There is nowhere to enter."))
		// Close the stream (like transferIn's empty-dest rejection) rather than leave a
		// registered-nowhere session the player can type into a void — no prompt.
		s.send(disconnectFrame("world has no content"))
		return
	}
	z.setPlayer(s.character, s)
	delete(z.forwarding, s.character) // present here again; no stale forward
	Move(s.entity, r)
	z.actConceal("$n arrives.", s.entity, ToRoom) // #100: silent to those who can't see the arriver
	z.lookRoom(s)
	z.sendPrompt(s)
	// Publish to the cross-shard `who` roster: this shard now hosts the player (8.4). Off the zone
	// goroutine — presenceJoin only records + enqueues; the background loop does the Redis write.
	z.presenceJoin(s)
	// Start the player's durable-tell consumer (Phase 8.5): it drains any OFFLINE backlog (paced,
	// "while you were away…") and then delivers live tells. Idempotent; one per resident on this shard.
	z.startTellConsumer(s)
	// Publish the player's effective {enabled ∩ hearable} hear-set + ignore list to their gate (Phase
	// 8.6, the receiver HEAR-filter + the ignore funnel): the gate subscribes exactly these concrete
	// channel subjects and caches the ignore list. Recomputed from THIS shard's channel_defs + the live
	// entity + the loaded comms state. A disabled bus is a clean no-op.
	z.publishCommsConfig(s)
	z.log.Debug("player joined", "player", s.character, "room", r.proto, "population", len(z.players))
	metrics.SetOccupancy(context.Background(), z.id, int64(len(z.players)))
}

// resolveRoom returns the room entity for the given ProtoRef, falling back to the start
// room when ref is empty or names no room this zone hosts. This is the single place the
// old "if z.rooms[room] == nil { room = z.startRoom }" guard lives now that rooms are
// entities keyed by ProtoRef.
func (z *Zone) resolveRoom(ref ProtoRef) *Entity {
	if r := z.rooms[ref]; r != nil {
		return r
	}
	return z.rooms[z.startRoom]
}

// leave removes a player from the world: detaches them from their room, announces
// the departure, and forgets them. Safe to call for an unknown id (no-op).
func (z *Zone) leave(id string) {
	s := z.players[id]
	if s == nil {
		// Clean disconnect for a player who has since transferred to a sibling zone:
		// forward it to the current owner so the player is removed there, not leaked.
		if dest := z.forwarding[id]; dest != nil {
			dest.post(leaveMsg{id: id})
			return
		}
		z.log.Debug("leave: unknown player", "player", id)
		return
	}
	// Immediate durable flush on a clean leave/quit (docs/PERSISTENCE.md §6: logout is a flush
	// point). Dump BEFORE detaching from the room so room_ref reflects where they logged out;
	// the dump is on this goroutine (race-free) and the write is the saver's job (off-goroutine),
	// so removal does not wait on I/O. A storeless/ephemeral player is a no-op.
	if z.saver != nil && z.saver.enabled() && s.entity != nil {
		if s.entity.pid == nil {
			// Brand-new character that quit BEFORE its async create returned a PersistID. enqueueSave
			// cannot flush yet (no PID to CAS on — its guard would no-op) and the in-flight createdMsg,
			// when it finally lands on a now-gone session, would DROP the data — silently losing every
			// action the player took during the create round-trip (e.g. the room they walked to).
			// Instead, DUMP the final snapshot NOW (on this goroutine: e.location is current, room_ref
			// reflects the move) and stash it keyed by name; characterCreated stamps the freshly-minted
			// PID onto it and enqueues the saveFinal once the row exists. The CreateCharacter INSERT
			// starts the row at version 0, so this deferred snapshot CASes at version 0 (dumpCharacter
			// reads s.stateVersion, still 0 — no save has bumped it). This keeps logout a true flush
			// point across the create window. If the create ultimately FAILS (stays ephemeral), the
			// stash is evicted by characterCreateFailed (never replayed) — no worse than the prior
			// behavior, but the common case (a fast create that just hadn't returned yet) is saved.
			z.pendingFinalFlush[id] = dumpCharacter(s)
			z.log.Info("character logged out before its durable id was assigned; deferring final flush to create completion", "player", id)
		} else {
			// saveFinal (not saveFlush): the session is removed below in this same handler, so a CAS
			// miss must NOT bounce a conflict back (there would be no session to re-dump). The saver
			// instead re-reads, rebases this authoritative logout snapshot, and retries the CAS itself
			// — so a cadence flush winning the race can never strand the durable record at the pre-move
			// room (docs/PERSISTENCE.md §6, the TestQuitFlushReliableAfterMove regression).
			z.enqueueSave(id, s, saveFinal)
		}
	}
	if r := s.entity.location; r != nil {
		z.actConceal("$n leaves.", s.entity, ToRoom) // #100: silent to those who can't see the leaver
		Move(s.entity, nil)
	}
	// Drop the player's in-memory Lua self.state entry (it was just dumped to durable JSONB above,
	// so the next login re-hydrates it) — otherwise a per-login entityScript would leak (7.5/7.6).
	if z.lua != nil && s.entity != nil {
		z.lua.dropEntityScript(s.entity.rid)
	}
	z.delPlayer(id)
	// Eager removal from the cross-shard `who` roster: a clean quit/leave drops the player immediately,
	// before the TTL (8.4). The roster's owner-guard means a handoff AWAY whose source-leave races the
	// destination's join can't evict the destination's fresh entry.
	z.presenceLeave(id)
	// Stop the player's durable-tell consumer: they no longer live here, so their tells accumulate in
	// the durable stream for their next host to drain (never delivered to a gone socket) (8.5).
	z.stopTellConsumer(id)
	z.log.Debug("player left", "player", id, "population", len(z.players))
	metrics.SetOccupancy(context.Background(), z.id, int64(len(z.players)))
}

// transferIn receives a player handed over from a sibling zone on the same shard (the
// destination side of an intra-shard cross-zone walk; the source side is Zone.move).
// It takes ownership of the existing player struct — same out channel, same appliedSeq,
// no snapshot, no epoch bump — registers it here, points its currentZone at this zone so
// the reader loop now routes input to us, announces the arrival, and shows the room.
func (z *Zone) transferIn(m transferInMsg) {
	s := m.s
	r := z.resolveRoom(m.room)
	if r == nil {
		// The destination zone hosts no rooms (empty-world boot): it cannot place the
		// transferred player. Disconnect cleanly rather than null-deref'ing in lookRoom. The
		// player keeps no presence here; the source already released it, so the session is
		// dropped (a real placement controller would re-route, Phase 10).
		z.log.Warn("intra-shard transfer rejected: destination has no rooms", "player", s.character, "zone", z.id)
		if s.currentZone != nil {
			s.currentZone.Store(z)
		}
		s.send(disconnectFrame("destination has no rooms"))
		return
	}
	// The entity now belongs to this zone: re-home it (rid allocator, zone owner) so a
	// future target reference resolves here, then place it in the destination room.
	s.entity.zone = z
	z.setPlayer(s.character, s)
	// Belt-and-suspenders combat clear: transferOut already disengaged the mover (and move() refuses
	// to walk while fighting), so this is normally a no-op. But it GUARANTEES the destination never
	// inherits a SOURCE-zone `fighting` *Entity or a posFighting state — combat is TRANSIENT and never
	// crosses a zone (P6-D8); the destination re-engages via a fresh `kill`. (No opponent-link to drop:
	// any opponent was in the SOURCE room, not reachable from here.)
	if s.entity.living != nil {
		// A player (prototype==nil) is a no-fork pass-through; routing through the COW
		// choke-point anyway keeps every Living mutation on one audited path.
		mutableLiving(s.entity).fighting = nil
		if position(s.entity) == posFighting {
			setPosition(s.entity, posStanding)
		}
	}
	// Re-arm the per-entity affect/regen tick on THIS zone (Phase 5.2). The source zone
	// registered the tick capturing the SOURCE pulse; the entity lives here now, so that handle
	// is stale and would block ensureTick (a.tick != nil) — affects/regen would silently freeze
	// on the destination. We clear + re-arm here, on our own goroutine. We do NOT clear it from
	// the source: that callback runs on the source goroutine and must never write this
	// now-destination-owned entity — it instead self-cancels on its next fire (it sees the player
	// absent in the source's z.players and returns false without touching the moved entity).
	if a, ok := Get[*Affected](s.entity); ok {
		a.tick = nil
		a.ensureTick(s.entity)
	}
	// Clear any stale forwarding entry from a previous departure from THIS zone: the
	// player is present here again, so handleInput will route to them directly.
	delete(z.forwarding, s.character)
	// From now on the player's input belongs to this zone. The source already removed
	// the player and set up forwarding for any line still in flight to it.
	if s.currentZone != nil {
		s.currentZone.Store(z)
	}
	Move(s.entity, r)
	z.actConceal("$n arrives.", s.entity, ToRoom) // #100: silent to those who can't see the arriver
	z.lookRoom(s)
	// [G13] room-scoped affects land on an entrant arriving via an intra-shard transfer too — the
	// destination room is THIS zone's, the entity is now ours (single-writer), so this is safe here.
	applyRoomAffectsTo(s.entity)
	z.aggroOnEntry(s.entity, r) // arrival-hook parity (distsys SC2): an aggressive mob engages a transferred-in player too
	z.sendPrompt(s)
	z.log.Debug("intra-shard transfer in", "player", s.character, "room", r.proto,
		"applied", s.appliedSeq, "population", len(z.players))
}

// attach binds a stream's out channel to a character. A new character is created and
// joined; an existing one (a re-dial or reconnect within the link-death window) is
// re-bound to the *same* session, preserving appliedSeq so replayed input dedups
// correctly. Either way an Attached frame goes out first; session.send stamps it with
// the resume point (appliedSeq) via ServerFrame.ack_input_seq.
func (z *Zone) attach(m attachMsg) {
	character, token, out, curZone, resumeEpoch := m.character, m.token, m.out, m.curZone, m.resumeEpoch
	s := z.players[character]
	// Eagerly reap a handed-off ORPHAN before the switch (the direct fix for a reconnect routed
	// back to the source after a successful cross-shard handoff). A frozen session with handedOff
	// is the leftover source copy of a handoff whose directory ownership CAS already COMMITTED — the
	// destination is the sole owner. A reconnect that lands here (the directory still pointed at the
	// home_zone) must REAP this copy (delete + dropToken, exactly like freezeExpire's handedOff
	// branch) and proceed AS A FRESH LOGIN — which then loads the player's durable record and routes
	// correctly — rather than rejecting "mid-transfer". This is NOT a both-own window: reaping a
	// handed-off orphan removes a copy the directory already disowned. A genuinely in-flight (frozen
	// but NOT handedOff) copy is a re-dial DURING the handoff and is still rejected below (the
	// both-own guard). Setting s=nil falls the switch through to the fresh-login default.
	if s != nil && s.frozen && s.handedOff {
		z.log.Debug("attach: reaping orphaned handed-off source copy; proceeding as fresh login", "player", character)
		z.delPlayer(character)
		if z.shard != nil && s.token != "" {
			z.shard.dropToken(s.token)
		}
		// The handoff to another shard committed: this source copy is an orphan. Drop our roster entry
		// (owner-guarded, so it can't evict the destination's fresh one if it already SET) (8.4).
		z.presenceLeave(character)
		z.stopTellConsumer(character) // the handed-off orphan no longer lives here; the destination drains its tells (8.5)
		s = nil
	}
	switch {
	case s != nil && s.pending:
		// Handoff bind: the gate re-dialed here after a Redirect. Activate the session
		// Prepare rehydrated — this is the destination self-commit.
		if token == "" || token != s.token {
			z.log.Warn("handoff bind rejected: token mismatch", "player", character)
			out <- disconnectFrame("handoff token invalid")
			return
		}
		if z.shard != nil {
			z.shard.dropToken(s.token)
		}
		s.out = out
		s.currentZone = curZone
		if curZone != nil {
			curZone.Store(z)
		}
		s.pending = false
		s.frozen = false
		s.attachGen++
		// Re-derive the reserved trust flags from the carried tier (#106), mirroring the fresh-login reconcile
		// (loginRoom). The flags were deliberately NOT carried across the seam (H-1); the SIGNED tier is, so an
		// admin/builder who walks between shards keeps holylight/builder/admin instead of arriving as a player.
		// Done BEFORE the arrival look so holylight governs what they see on entry. A baseline (empty) tier
		// clears every reserved flag — the correct fail-closed default. NOTE: wizinvis is a session concealment,
		// never tier-grantable (applyTierFlags clears it), so a staffer who was wizinvis on the source arrives
		// VISIBLE and triggers the "$n arrives." broadcast below — an intended, documented presence flicker (a
		// cross-shard hop is a session boundary for concealment), not a regression (pre-#106 they de-elevated
		// fully anyway).
		applyTierFlags(s.entity, s.tier)
		s.send(attachedFrame(z.id)) // resume ack = appliedSeq carried in the snapshot
		// prepare parked the entity's location at the destination room WITHOUT adding it
		// to the room contents (pending = invisible). Move now makes it visible. Guard the
		// location read: prepare now rejects an unplaceable room, but a defensive fallback
		// to the start room (resolveRoom("")) keeps this branch from ever null-deref'ing.
		r := z.resolveRoom("") // start-room fallback
		if s.entity.location != nil {
			r = z.resolveRoom(s.entity.location.proto)
		}
		if r != nil {
			Move(s.entity, r)                             // only now does the player become visible in the room
			z.actConceal("$n arrives.", s.entity, ToRoom) // #100: silent to those who can't see the arriver
			// Arrival-hook parity (distsys 6.4a SC1/SC2): a player arriving via a CROSS-SHARD handoff
			// must land in active room affects (a web/darkness field snares them on arrival, not only on
			// the next room tick) and trigger an aggressive mob — same as the local-move and intra-shard
			// paths. The destination room is THIS zone's and the entity is now ours (single-writer), safe.
			applyRoomAffectsTo(s.entity)
			z.aggroOnEntry(s.entity, r)
		}
		z.lookRoom(s)
		// Flush any notice queued while the session was pending (e.g. carried items the destination's
		// enabled-pack set could not spawn). Sent AFTER the room look so it reads as an arrival aside.
		if s.pendingNotice != "" {
			s.send(textFrame(s.pendingNotice))
			s.pendingNotice = ""
		}
		z.sendPrompt(s)
		// This shard is now the player's host (cross-shard handoff arrival): publish them to the `who`
		// roster. The roster's owner-guard lets this destination SET win over the source's stale entry,
		// and the source shard's own presenceLeave (handed-off orphan reap) drops its copy (8.4).
		z.presenceJoin(s)
		// This shard now hosts the player (cross-shard arrival): start their durable-tell consumer so
		// tells to them drain here (8.5). Idempotent.
		z.startTellConsumer(s)
		// Re-publish the effective hear-set + ignore list to the gate (Phase 8.6): recomputed against
		// THIS shard's channel_defs (which may differ) + the live entity + the carried comms state, so
		// the gate's receiver HEAR-filter is correct for the destination's content after the walk.
		z.publishCommsConfig(s)
		z.log.Debug("handoff committed: player activated", "player", character,
			"room", roomRef(s.entity.location), "applied", s.appliedSeq, "epoch", s.epoch)
		// Async-create window: the snapshot carried no PersistID (the source's CreateCharacter had
		// not yet returned when the handoff fired), so this destination has no id to flush against —
		// a quit here would be silently skipped (leave's pid==nil guard). Re-resolve the durable row
		// BY NAME off the zone goroutine (the row exists by now: the source's create committed it)
		// and adopt the PID + version, so the destination becomes able to persist. No-op when the PID
		// already crossed in the snapshot (the common case) or no store is configured.
		if s.entity.pid == nil {
			z.resolveHandoffPid(s)
		}

	case token != "":
		// A handoff token was presented but no pending player matches it
		// (expired/aborted/never-prepared/forged). Reject rather than re-bind something
		// else or spawn a fresh character — that would silently lose the migrated state.
		z.log.Warn("handoff bind rejected: no pending player for token", "player", character)
		out <- disconnectFrame("handoff token invalid")
		return

	case s != nil && s.frozen:
		// A re-dial to the SOURCE shard mid-handoff: the handoff owns the player.
		// Re-binding would resume a frozen player and risk a both-own window. Reject.
		z.log.Warn("attach rejected: character is mid-handoff (frozen)", "player", character)
		out <- disconnectFrame("character is mid-transfer")
		return

	case s != nil:
		// Re-attach. TWO shapes land here (token == "", not pending, not frozen):
		//
		//   - a genuine link-dead RESUME: the prior stream already dropped (s.detached) or its
		//     reader loop is on its way to detaching; the new connection picks up the same player.
		//   - a SECOND CONCURRENT login: the prior connection is STILL LIVE (!s.detached). The
		//     newest socket wins (single-session), but the old one must not be left connected-yet-
		//     MUTE — it gets a clean "logged in elsewhere" notice and its socket is torn down.
		//
		// We discriminate on s.detached: a still-attached prior connection is a live takeover to
		// KICK; a detached one is a resume (nothing to displace — the old socket is already gone).
		// Either way the new out is bound below; capture the OLD out FIRST so the kick reaches the
		// displaced connection's writer goroutine (which is still draining it) before we swap.
		if !s.detached && s.out != nil && s.out != out {
			old := s.out
			z.log.Debug("second login displaces live session; kicking old connection", "player", character)
			// Send on the OLD channel directly (s.out is about to be reassigned). The displaced
			// gate connection renders the notice + Disconnect and closes its socket — the same
			// teardown path "quit" uses (renderFrame -> Disconnect -> nc.Close). Non-blocking, like
			// session.send: if the old writer already stopped (a near-simultaneous drop) the frames
			// are harmlessly dropped — the old socket is gone anyway.
			displacedKick(old, s.appliedSeq)
		}
		// Reconcile the dedup fence with the gate's reported resume point. A fresh gate session
		// (reconnect or second login) restarts its input numbering at 1, so its first seqs sit AT
		// OR BELOW the carried appliedSeq and would be wrongly dropped as replays. When the gate
		// presents such a restarted numbering (inputSeq >= 1 and <= appliedSeq), clamp the fence to
		// inputSeq-1 so the new connection's first line is applied. inputSeq == 0 (test/internal,
		// and the redirect replay path) means "unspecified" — preserve appliedSeq for exactly-once
		// replay dedup (TestExactlyOnceAcrossRedial). The redirect replay path never reaches here
		// (it carries a handoff token and binds via the pending branch), so this only ever clamps
		// a fresh-session reconnect/takeover, never a legitimate replay.
		if m.inputSeq >= 1 && m.inputSeq <= s.appliedSeq {
			z.log.Debug("re-attach fence reset to fresh session resume point",
				"player", character, "old_applied", s.appliedSeq, "input_seq", m.inputSeq)
			s.appliedSeq = m.inputSeq - 1
		}
		s.out = out
		// Re-attach reuses the SAME session object, so the GMCP HUD change-detection buffers survive the
		// link-death window. The NEW gate connection has no HUD state, so clear them here to force
		// sendPrompt to re-prime Char.Vitals/Status on reconnect (Phase 9.2) — otherwise a reconnect with
		// unchanged vitals would leave the client's gauge blank/stale. The cross-shard handoff path gets
		// this for free (Prepare rehydrates a fresh session with nil buffers).
		s.lastVitals, s.lastStatus, s.lastStats, s.lastRoom = nil, nil, nil, nil
		s.lastInvItems, s.lastRoomItems, s.lastRoomPlayers = nil, nil, nil
		s.currentZone = curZone
		if curZone != nil {
			curZone.Store(z)
		}
		s.detached = false
		s.attachGen++
		s.send(attachedFrame(z.id))
		z.log.Debug("player re-attached", "player", character, "applied_seq", s.appliedSeq, "gen", s.attachGen)
		z.lookRoom(s)
		z.sendPrompt(s)

	default:
		// Fresh login. Seed the epoch from the directory's persisted placement (read off the
		// zone goroutine in server.go, threaded in as resumeEpoch) so it stays globally
		// monotonic per player: a relog after any prior cross-shard move resumes at the stored
		// epoch, and the NEXT move computes stored+1 — which the placement CAS accepts. Seed to
		// EXACTLY the stored value (not +1); brand-new characters (resumeEpoch 0) start at 1.
		epoch := resumeEpoch
		if epoch < 1 {
			epoch = 1
		}
		s = &session{character: character, out: out, epoch: epoch, currentZone: curZone, tier: m.tier}
		z.newPlayerEntity(s, character)
		z.log.Debug("fresh login epoch seeded", "player", character, "epoch", epoch, "resume", resumeEpoch, "tier", m.tier)
		if curZone != nil {
			curZone.Store(z)
		}
		s.attachGen++
		s.send(attachedFrame(z.id))
		// Persistence (docs/PHASE4-PLAN.md §4): rehydrate from the snapshot read off-goroutine,
		// or create a brand-new durable row, or (no store) stay ephemeral exactly as before.
		room := z.loginRoom(s, m)
		applyTierFlags(s.entity, s.tier) // #27: reconcile builder/admin/holylight to the verified tier (post-load)
		z.join(s, room)                  // registers, places, announces arrival, looks, prompts
		z.startSaveCadence()
	}
}

// loginRoom applies the persistence read path for a fresh login and returns the room ref the
// player should land in (empty => start room). It runs on the zone goroutine, so every entity
// mutation (loadCharacter, the create) is single-writer; the blocking I/O already happened
// off-goroutine (the load in server.go) or is the saver's job (the create's row insert is the one
// exception — see below). Three cases:
//
//   - a durable snapshot was loaded (m.loadedOK): rehydrate the entity (PersistID, version,
//     inventory/equipment) and land them in their SAVED room. This is also the crash-rehydrate-
//     by-name primitive (docs/PLACEMENT.md §6): the snapshot may have been written by a shard
//     this one never saw — load is keyed by name, so any shard can resume it.
//   - a store is configured but no row exists: a brand-new name — mint the row (PersistID) so the
//     next save can CAS it. The insert is a quick off-goroutine call posted back as the PID; the
//     character starts blank in the start room meanwhile.
//   - no store (ephemeral): return "" and stay blank, exactly today's behavior.
func (z *Zone) loginRoom(s *session, m attachMsg) ProtoRef {
	if m.loadedOK {
		loadCharacter(z, s, m.loaded)
		// First spawn of a content-built character (Phase 14.8): apply the recorded chargen result (point-buy
		// bases + the chosen bundles' grants) on the zone goroutine. The next save clears the chargen column.
		if m.loaded.PendingChargen != nil {
			applyPendingChargen(z, s, m.loaded.PendingChargen)
		}
		if m.loaded.RoomRef != "" {
			return ProtoRef(m.loaded.RoomRef)
		}
		return ""
	}
	if z.saver != nil && z.saver.store != nil {
		// Brand-new character: create the durable row off the zone goroutine and adopt the minted
		// PersistID when it returns (createDone). The blocking INSERT must not run here; spawn it
		// like beginHandoff. Until it returns the character is ephemeral-in-memory; the first
		// cadence flush after the PID arrives makes it durable. The start room is the create's
		// recorded location.
		z.createCharacter(s)
	}
	return ""
}

// createCharacter inserts a brand-new durable row for the session off the zone goroutine and
// posts the minted PersistID back as a createdMsg. The session is already live in the start room;
// the PID arriving later just makes future saves durable. Errors are logged (non-fatal): the
// character simply stays ephemeral until a later login retries the create. zoneRef/roomRef are
// captured now (the start room) so the row records the spawn location.
func (z *Zone) createCharacter(s *session) {
	store := z.saver.store
	name := s.character
	zoneRef := z.id
	roomRef := string(z.startRoom)
	z.log.Debug("creating new durable character", "player", name, "zone", zoneRef, "room", roomRef)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), saveIOTimeout)
		defer cancel()
		pid, err := store.CreateCharacter(ctx, name, zoneRef, roomRef)
		if err != nil {
			z.log.Debug("character create failed (staying ephemeral)", "player", name, "err", err)
			// Terminal failure: no createdMsg will ever come. Signal the zone so it can evict any
			// create-window logout stash this name parked (leave() while pid==nil). Without this the
			// orphaned entry would linger for the zone's lifetime (FOLLOW-UPS §2). Delete-only on the
			// zone goroutine — it cannot resurrect or mis-route a flush.
			z.post(createFailedMsg{id: name})
			return
		}
		z.post(createdMsg{id: name, pid: pid})
	}()
}

// prepare rehydrates a snapshot as a PENDING player in this zone (the destination
// side of Prepare). It is idempotent on the deterministic token and rejects an epoch
// at or below one already seen for the character. The pending player is in the zone's
// player map but not yet in its room's occupant set — invisible until the gate's
// re-dial activates it.
func (z *Zone) prepare(m prepareMsg) {
	character := m.snap.GetCharacterId()
	if existing := z.players[character]; existing != nil {
		switch {
		case existing.pending && existing.token == m.token:
			// Idempotent retry of the same Prepare.
			z.log.Debug("handoff prepare: idempotent retry", "player", character)
			m.reply <- nil
			return
		case m.epoch <= existing.epoch:
			m.reply <- status.Errorf(codes.FailedPrecondition, "stale epoch %d <= current %d", m.epoch, existing.epoch)
			return
		case existing.frozen:
			// A stale frozen copy left by a prior handoff AWAY from this shard, now
			// superseded by a newer handoff BACK (m.epoch > existing.epoch). Monotonic
			// epoch makes the return authoritative: discard the stale copy and rehydrate
			// fresh below. This is what makes A<->B round trips work; a never-returned
			// frozen copy is still GC'd later (freeze-timeout / discard signal, deferred).
			z.log.Debug("discarding stale frozen copy for return handoff",
				"player", character, "old_epoch", existing.epoch, "new_epoch", m.epoch)
			z.delPlayer(character)
		default:
			// A genuinely present (live) player with this id.
			m.reply <- status.Errorf(codes.AlreadyExists, "character %q already present", character)
			return
		}
	}
	r := z.resolveRoom(m.room)
	if r == nil {
		// This zone can't place the player anywhere — the target room is unknown AND the
		// start room hasn't spawned (e.g. a just-restarted destination mid-boot). Reject
		// cleanly rather than parking a pending entity with a nil location (a landmine that
		// later null-derefs on bind). The source thaws via handoffFailed.
		z.log.Warn("handoff prepare rejected: no placeable room", "player", character, "room", m.room)
		m.reply <- status.Errorf(codes.FailedPrecondition, "zone %q cannot place room %q", z.id, m.room)
		return
	}
	// Pack-set validation (security hardening): if the carried inventory/equipment names a prototype this
	// shard's enabled packs don't define, REJECT the handoff BEFORE rehydrating — the source then thaws the
	// player in place WITH their items intact, instead of accepting the move and silently DROPPING the items
	// post-commit (the old destination-mismatch data-loss window). A uniform-pack fleet never trips this; a
	// genuine mismatch (zones sharing an exit but not their packs) is a deployment misconfiguration surfaced
	// loudly rather than eaten. A malformed carry is NOT rejected here (it degrades to defaults below).
	if raw := m.snap.GetStateJson(); raw != "" {
		// Byte cap FIRST (cheap, before unmarshal): a forged/oversized carry — the handoff is unauthenticated
		// (§5) — can't force a huge allocation or a rehydrate-bomb. gRPC's message limit is far too loose to
		// bound the destination zone-goroutine work.
		if len(raw) > maxCarryStateBytes {
			z.log.Warn("handoff prepare rejected: carried state exceeds the byte cap",
				"player", character, "bytes", len(raw), "cap", maxCarryStateBytes, "zone", z.id)
			m.reply <- status.Errorf(codes.FailedPrecondition, "carried state too large (%d bytes)", len(raw))
			return
		}
		var st StateJSON
		if err := json.Unmarshal([]byte(raw), &st); err == nil {
			missing, nodes := z.carryItemAudit(st)
			switch {
			case len(missing) > 0:
				// Pack-set mismatch: reject before committing so the source keeps the player + items rather
				// than accepting the move and silently dropping the unknown items post-arrival.
				z.log.Warn("handoff prepare rejected: carried item prototypes unknown on this shard",
					"player", character, "missing", missing, "zone", z.id)
				m.reply <- status.Errorf(codes.FailedPrecondition,
					"destination lacks %d carried item prototype(s): %v", len(missing), missing)
				return
			case nodes > maxCarryItemNodes:
				// Width guard: a wide-but-shallow tree past the node cap would stall the zone on rehydrate.
				z.log.Warn("handoff prepare rejected: carried item count exceeds the node cap",
					"player", character, "nodes", nodes, "cap", maxCarryItemNodes, "zone", z.id)
				m.reply <- status.Errorf(codes.FailedPrecondition, "carried inventory too large (%d items)", nodes)
				return
			}
		}
	}
	s := &session{
		character:    character,
		appliedSeq:   m.snap.GetAppliedSeq(),
		stateVersion: m.snap.GetStateVersion(), // carry the CAS base across the handoff (§7)
		epoch:        m.epoch,
		pending:      true,
		token:        m.token,
		// Adopt the account trust tier carried on the SIGNED snapshot (#106). The reserved flags themselves are
		// NOT carried (applyStateComponents skips them — H-1: a flag restore bypasses the content op guard), so
		// elevation is re-DERIVED from this tier when the session activates (attach), mirroring the fresh-login
		// reconcile. Empty for a baseline player. Only ever trusted because it rode the verified payload — a
		// keyless (dev/test) shard that skips signature verification also skips carrying elevation into a trust
		// boundary, since those deployments are single-shard and never hand off.
		tier: m.snap.GetTier(),
	}
	e := z.newPlayerEntity(s, character)
	// Rehydrate the receiver-side comms-state subtree carried on the snapshot (Phase 8.6) so toggles/
	// ignore/AFK survive the cross-shard walk. Empty (all-default / pre-8.6 snapshot) installs nothing.
	// The effective hear-set is re-published to the gate when this pending session activates (attach).
	loadCommsStateJSON(s, m.snap.GetCommsState())
	// Rehydrate the player's REMAINING entity state carried on the snapshot (the full-state-carry fix):
	// inventory into contents + Wearer slots, attribute base overrides, resource currents clamped to the
	// DESTINATION-derived max, affects re-attached with their REMAINING durations (no on_apply re-fire),
	// flags, and cooldowns re-armed via rearmCooldown on THIS (destination) zone goroutine. It reuses the
	// SAME applier loadCharacter uses (applyStateComponents), so a fresh login and a handoff arrive at
	// byte-identical entity state — and the applier deliberately does NOT touch appliedSeq (seeded above
	// from the dedicated snapshot field, the linchpin). The pending entity is parked (location set below,
	// not yet in the room), so items land in contents now and become visible when attach Moves the player
	// in. Empty (a bare/contentless player or a pre-fix snapshot) installs nothing — defaults, exactly the
	// pre-carry behavior. dropped is the count of carried items whose prototype is unknown on THIS shard's
	// enabled-pack set (a destination-mismatch data-loss window save/load does not have).
	if raw := m.snap.GetStateJson(); raw != "" {
		var st StateJSON
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			// A malformed carry degrades to defaults (loud log), never a crash or a rejected handoff — the
			// player still arrives, just without the carried state (the pre-fix behavior).
			z.log.Error("handoff state carry unmarshal failed; arriving with defaults",
				"player", character, "err", err.Error())
		} else {
			dropped := applyStateComponents(z, s, st)
			if dropped > 0 {
				// After the pack-set pre-check above, an unknown PROTOTYPE can no longer reach here (that
				// path rejects the whole handoff). A residual drop is now only the maxItemNestDepth
				// TRUNCATION of a degenerately-deep carried container — still worth a loud log + the player
				// notice, but bounded and not a pack mismatch.
				z.log.Warn("handoff: carried container exceeded the nesting cap (deepest contents dropped)",
					"player", character, "dropped", dropped, "zone", z.id)
				s.pendingNotice = "Some of your items did not transfer to this area."
			}
		}
	}
	// Adopt the durable PersistID carried in the snapshot so a save on THIS (destination) shard
	// CASes against the SAME row the source created — the fix for "a handed-off character can't be
	// persisted on the destination". Paired with the stateVersion seeded above, the id + CAS base
	// cross the seam together, keeping the destination's first flush monotonic. Empty in the async-
	// create window (the row's PID had not returned at the source when the handoff fired); the bind
	// path (attach) then re-resolves it by name. INVARIANT: only SET here from a non-empty snapshot
	// PID — never clear a PID — so a later by-name resolve can't be clobbered by an empty carry.
	if pid := m.snap.GetPersistId(); pid != "" {
		p := PersistID(pid)
		e.pid = &p
	}
	// Defense-in-depth: the snapshot's Name is externally-sourced and the cross-shard
	// handoff is currently UNAUTHENTICATED (docs/PROTOCOL.md §5) — a forged/tampered
	// snapshot could carry a control-laden Name that would land here as a passively
	// rendered display name + targeting keyword, re-opening terminal injection through
	// a non-edge door. Sanitize it (strip control/non-graphic runes, cap length) the
	// same way the gate's validateName guards a login name. A legitimate name minted by
	// the edge passes through unchanged.
	name := textsan.CleanName(m.snap.GetName(), maxPlayerNameRunes)
	e.short = name
	e.keywords = []string{name}
	// Park the entity AT the destination room (location set) but NOT in its contents —
	// a pending player is invisible until the gate's re-dial activates it (attach Moves
	// it into the room then). location is how attach later recovers the destination room.
	e.location = r
	z.setPlayer(character, s)
	if z.shard != nil {
		// Index the token so a Play attach (the gate's re-dial) can route the bind to
		// THIS zone even on a multi-zone shard.
		z.shard.indexToken(m.token, z)
	}
	gen := s.attachGen
	time.AfterFunc(pendingTTL, func() { z.post(pendingExpireMsg{id: character, gen: gen}) })
	z.log.Debug("handoff prepared: pending player rehydrated", "player", character,
		"room", r.proto, "epoch", m.epoch, "applied", s.appliedSeq)
	m.reply <- nil
}

// abortPending discards a pending player by handoff token (the source cancelled).
func (z *Zone) abortPending(token string) {
	for id, s := range z.players {
		if s.pending && s.token == token {
			z.log.Debug("handoff aborted: discarding pending player", "player", id)
			z.delPlayer(id)
			if z.shard != nil {
				z.shard.dropToken(token)
			}
			return
		}
	}
}

// pendingExpire discards a pending player the gate never bound within the TTL. The
// generation check ignores a stale timer for a player that has since been activated.
// (A future refinement keeps it link-dead instead, since the directory still points
// here — see PROTOCOL.md §5.)
func (z *Zone) pendingExpire(id string, gen uint64) {
	if s := z.players[id]; s != nil && s.pending && s.attachGen == gen {
		z.log.Debug("pending player expired (gate never bound)", "player", id)
		z.delPlayer(id)
		if z.shard != nil {
			z.shard.dropToken(s.token)
		}
	}
}

// freezeTTL bounds how long a source-side frozen player (an in-flight cross-shard handoff)
// may linger before the backstop reaper fires. It MUST be >= pendingTTL and longer than
// handoffRPCTimeout so the normal resolutions win first (the >= holds today by CONSTRUCTION —
// it initializes TO pendingTTL and tracks it if pendingTTL changes; there is deliberately no
// runtime enforcement because tests shrink this var to exercise the reaper): the RPC timeout thaws a failed
// handoff (handoffFailMsg) and a successful one redirects, both well before this. This only
// catches the leftover: a frozen copy that never got cleaned up (e.g. a dead gate never
// re-dialed after a successful redirect, or a path that froze but neither posted result).
// It is a package var (not a const) so a test can shrink it to exercise the reaper quickly.
var freezeTTL = pendingTTL

// freezeExpire is the backstop for a frozen source-side player still frozen after freezeTTL.
// The gen check ignores a stale timer for a session that has since rebound/rebuilt (a return
// handoff, a re-attach). It then discriminates on s.handedOff:
//
//   - handedOff: the handoff SUCCEEDED — the directory's ownership CAS committed, so this
//     source copy is an ORPHAN. Remove it (and drop its token) so the character can reconnect
//     to the source without hitting the frozen "mid-transfer" reject. Thawing it would be a
//     both-own bug (two shards acting for one player), so we never thaw a handed-off copy.
//   - not handedOff: the handoff never committed — the directory never moved, so reclaiming
//     the source IS correct. THAW IN PLACE: restore via frozenFrom (like handoffFailed) and
//     tell the player the way is barred (timeout). The placement CAS stays the arbiter: we
//     only reclaim when the directory never recorded the move. Because the commit-marker
//     (handedOffMsg) sets handedOff at CAS-commit time and is enqueued ahead of redirectMsg,
//     this discriminator is correct regardless of where the freeze timer fires.
func (z *Zone) freezeExpire(id string, gen uint64) {
	s := z.players[id]
	if s == nil || !s.frozen || s.attachGen != gen {
		return // already resolved (thawed/handed-off-and-reaped) or rebound
	}
	if s.handedOff {
		// Successful handoff's orphaned source copy: remove it so reconnect to the source works.
		z.log.Debug("freeze timeout: reaping orphaned handed-off source copy", "player", id)
		z.reapHandedOffOrphan(id, s)
		return
	}
	// Handoff never completed: thaw in place and restore to the room they tried to leave.
	z.log.Debug("freeze timeout: thawing un-handed-off player in place", "player", id)
	s.frozen = false
	// The cross-shard freeze path disengaged the player before freezing, so they thaw NOT fighting and
	// no driver re-arm is needed. stopFight defensively in case a future path freezes a fighting player:
	// a thawed player left posFighting with no live target would soft-lock (the driver self-cancelled
	// when they froze and nothing re-arms it). This guarantees a clean standing state on thaw.
	z.stopFight(s.entity)
	if s.frozenFrom != nil {
		Move(s.entity, s.frozenFrom)
		z.actConceal("$n arrives.", s.entity, ToRoom) // #100: silent to those who can't see the arriver
		s.frozenFrom = nil
	}
	s.send(textFrame("The way is barred. (handoff timed out)"))
	z.sendPrompt(s)
}

// detach handles a player's stream dropping. out identifies which stream, so a stale
// detach from a stream already superseded by a re-attach is ignored. A clean quit
// removes the player at once; an unexpected drop marks the player link-dead and
// schedules a reap after the grace window (cancelled implicitly if it re-attaches and
// bumps attachGen).
func (z *Zone) detach(id string, out chan *playv1.ServerFrame) {
	s := z.players[id]
	if s == nil {
		// The player transferred out of this zone (intra-shard walk) while the separate
		// reader-loop goroutine still held the old currentZone, and the stream then
		// dropped. Forward the link-loss to the new owner so IT runs link-death; dropping
		// it here would strand the player alive in the destination. The transfer kept the
		// same out channel, so the destination's superseded-stream check still holds.
		if dest := z.forwarding[id]; dest != nil {
			dest.post(detachMsg{id: id, out: out})
		}
		return
	}
	if s.out != out {
		z.log.Debug("detach from superseded stream ignored", "player", id)
		return
	}
	if s.frozen {
		// Mid-handoff: the gate is re-dialing the destination shard. Do NOT remove the
		// player — the handoff owns its fate (commit -> discard, abort -> thaw).
		z.log.Debug("detach ignored: player frozen (handoff in progress)", "player", id)
		return
	}
	if s.quitting {
		z.log.Debug("clean quit, removing player", "player", id)
		z.leave(id)
		return
	}
	s.detached = true
	gen := s.attachGen
	// Flush durably NOW, before the 60s link-death grace — do NOT defer the player's current state to
	// the reap. A move is not itself a flush point (only cadence/logout/drain are), so without this a
	// player who walked and then dropped unexpectedly would not have their new room persisted until the
	// reap leave() 60s later — lost entirely if the shard crashes in that window. It also closes the
	// quit/detach ORDERING race: a fast quit whose detach is processed before `quitting` is observed
	// takes THIS path, and the immediate flush records the logout state regardless. The dump is on this
	// goroutine (race-free), the write is the saver's job (off-goroutine), and the entity is NOT removed
	// from the room here (reap/resume still decides its fate), so a re-attach within the grace resumes
	// normally — this only adds a durability checkpoint, it does not change the grace/resume semantics.
	z.enqueueSave(id, s, saveFlush)
	z.log.Debug("player link-dead", "player", id, "grace", linkDeadGrace)
	time.AfterFunc(linkDeadGrace, func() { z.post(reapMsg{id: id, gen: gen}) })
}

// reap removes a link-dead player that never re-attached within the grace window. The
// generation check ensures a player that has since re-attached (bumping attachGen) is
// not removed by a stale timer.
func (z *Zone) reap(id string, gen uint64) {
	if s := z.players[id]; s != nil && s.detached && s.attachGen == gen {
		z.log.Debug("reaping link-dead player", "player", id)
		z.leave(id)
	}
}

// markHandedOff flips the freeze-reaper's success discriminator at the directory CAS-commit
// point. Posted by the async handoff coordinator (Shard.beginHandoff) as handedOffMsg the
// instant SetPlayerShard succeeds, BEFORE redirectMsg — so it lands ahead of the Redirect in
// this zone's inbox. The both-own truth flips at the CAS commit; setting the flag here (not
// in redirect, one message later) means a freezeExpire firing in the old gap reaps the orphan
// instead of thawing a player whose handoff already succeeded. Clearing frozenFrom here makes
// the failure-restore (handoffFailed/thaw) a no-op from this point on — defense in depth
// against a both-own restore. Idempotent and single-writer: only POSTed to the inbox.
//
// Tolerated residual: the freeze timer runs on a separate goroutine and cannot observe the
// CAS return, so in a microsecond-scale sliver it may enqueue a freezeExpireMsg AHEAD of this
// marker (timer fires between SetPlayerShard returning ok and beginHandoff posting us). That
// freezeExpire thaws in place, and this marker then runs on an already-thawed session. The
// result is a transient, NON-authoritative source copy (the directory CAS already points at
// the destination); it self-heals when the player's gate re-dials on the trailing Redirect
// and the stale source stream goes link-dead and reaps. No dual ownership — the directory is
// the sole arbiter — and this sliver is bounded by goroutine scheduling, not by freezeTTL.
func (z *Zone) markHandedOff(id string) {
	s := z.players[id]
	if s == nil {
		return
	}
	s.handedOff = true
	s.frozenFrom = nil // committed to leaving: the failure-restore is no longer valid
	// The directory CAS committed: the DESTINATION shard is now the sole owner and starts its own
	// durable-tell consumer on arrival. Stop THIS (source) shard's consumer at the commit point so the
	// two shards never BOTH drain telos.comms.dtell.<player> — a double-drain would render a tell twice
	// (the two shards' delivered-cursors are independent sessions, so the per-session cursor can't
	// dedup across them). The destination drains everything from here (8.5).
	z.stopTellConsumer(id)
	z.log.Debug("handoff committed (directory CAS done)", "player", id)
}

// redirect tells a frozen player's client to re-dial the destination shard. Posted
// by the async handoff coordinator (Shard.beginHandoff) once the directory has
// recorded the new owner (always after the handedOffMsg commit-marker); the player stays
// frozen until the gate re-attaches there. The success discriminator is already set by
// markHandedOff — redirect only carries the epoch forward and sends the Redirect frame.
func (z *Zone) redirect(v redirectMsg) {
	s := z.players[v.id]
	if s == nil {
		return
	}
	s.epoch = v.epoch
	s.send(redirectFrame(v.targetAddr, v.token))
	z.log.Debug("redirect sent", "player", v.id, "target", v.targetAddr, "epoch", v.epoch)
	// Phase 16.4b: during a graceful drain the player is committed to the peer (handedOff is set — the
	// commit-marker handedOffMsg is enqueued ahead of this redirectMsg), so reap the frozen source orphan
	// NOW rather than waiting out freezeTTL. The zone then empties promptly and BeginDrain's wait completes.
	if s.handedOff && z.shard != nil && z.shard.isDraining() {
		z.reapHandedOffOrphan(v.id, s)
	}
}

// reapHandedOffOrphan removes a successfully-handed-off player's orphaned SOURCE copy — the destination owns
// them now, so this copy has no purpose. Shared by the freeze-timeout backstop and, during a graceful drain,
// the eager reap on redirect. Keeps the pop mirror accurate so BeginDrain's wait-until-empty sees the drop.
func (z *Zone) reapHandedOffOrphan(id string, s *session) {
	z.delPlayer(id)
	if z.shard != nil && s.token != "" {
		z.shard.dropToken(s.token) // the destination bound (or will bind) its own copy; drop the source token
	}
	z.presenceLeave(id)    // drop the orphaned source copy from the `who` roster (owner-guarded) (8.4)
	z.stopTellConsumer(id) // ensure the source consumer is gone (idempotent; normally stopped at markHandedOff) (8.5)
}

// setPlayer / delPlayer are the ONLY sanctioned ways to mutate z.players, so the pop mirror (read
// off-goroutine by BeginDrain's wait-until-empty) stays EXACTLY len(z.players) after every insert/delete —
// including the pending-add, transfer, and handoff-reap paths a scattered pop.Store would miss (Phase 16.4b
// review). Zone-goroutine only, like every z.players access.
func (z *Zone) setPlayer(id string, s *session) {
	z.players[id] = s
	z.pop.Store(int64(len(z.players)))
}

func (z *Zone) delPlayer(id string) {
	delete(z.players, id)
	z.pop.Store(int64(len(z.players)))
}

// handoffFailed thaws a player whose cross-shard move could not be initiated, so they
// are not left stuck frozen.
func (z *Zone) handoffFailed(v handoffFailMsg) {
	s := z.players[v.id]
	if s == nil {
		return
	}
	s.frozen = false
	// Disengaged before freeze (the cross-shard path), so this is normally a no-op; stopFight defends
	// against a future fighting-player freeze leaving the thawed player soft-locked posFighting with the
	// driver retired (distsys S1). Restore the entity to the room it tried to leave: move() detached it
	// (location=nil) for the in-flight handoff. Without this re-attach the location stays nil and the
	// next look/move null-derefs (commands.go lookRoom/move read s.entity.location).
	z.stopFight(s.entity)
	if s.frozenFrom != nil {
		Move(s.entity, s.frozenFrom)
		z.actConceal("$n arrives.", s.entity, ToRoom) // #100: silent to those who can't see the arriver
		s.frozenFrom = nil
	}
	z.log.Debug("handoff failed, thawing player", "player", v.id, "reason", v.reason)
	s.send(textFrame("The way is barred. (" + v.reason + ")"))
	z.sendPrompt(s)
}

// --- Durability ladder: cadence + reconcile (docs/PHASE4-PLAN.md §4) -------------------

// saveCheckpointPulses / saveFlushPulses set the two write cadences in pulse units (pulse.go's
// quarter-second heartbeat): a cheap Redis checkpoint every ~10s and a Postgres flush every ~60s.
// They are package vars (not consts) so a test can shrink them to exercise the ladder quickly.
var (
	saveCheckpointPulses uint64 = 40  // ~10s at 250ms/pulse
	saveFlushPulses      uint64 = 240 // ~60s
)

// startSaveCadence registers the per-zone save-cadence pulse callback the FIRST time a persisted
// player joins (idempotent: a no-op once savePulse is set). The callback fires ON the zone
// goroutine (pulse.go), so it has single-writer access: it dumps every persisted player and hands
// the snapshot to the async saver (which does the off-goroutine I/O). It emits a Redis checkpoint
// every saveCheckpointPulses and a Postgres flush every saveFlushPulses. With no saver configured
// (ephemeral) it is never registered, so a storeless zone pays nothing.
func (z *Zone) startSaveCadence() {
	if z.savePulse != nil || z.saver == nil || !z.saver.enabled() {
		return
	}
	z.savePulse = z.pulses.every(saveCheckpointPulses, func(pulse uint64) bool {
		reason := saveCheckpoint
		// A flush tick is a superset of a checkpoint tick: when the pulse aligns with the
		// (longer) flush stride, write both tiers. saveCheckpointPulses divides the pulse counter
		// the callback fires on, so test the flush stride against it.
		if pulse%saveFlushPulses < saveCheckpointPulses {
			reason = saveFlush
		}
		z.saveAll(reason)
		return true // keep ticking while the zone runs
	})
	z.log.Debug("save cadence started", "checkpoint_pulses", saveCheckpointPulses, "flush_pulses", saveFlushPulses)
}

// saveAll dumps every live, persisted, non-frozen player and enqueues a save at the given tier.
// Runs on the zone goroutine (the pulse callback). A pending/frozen player (mid-handoff) is
// skipped: its durable record belongs to the other shard until the handoff resolves, so saving it
// here would race the cross-shard owner's epoch/state_version.
func (z *Zone) saveAll(reason saveReason) {
	for id, s := range z.players {
		if s.pending || s.frozen || s.entity == nil || s.entity.pid == nil {
			continue
		}
		z.enqueueSave(id, s, reason)
	}
}

// enqueueSave dumps one session (on the zone goroutine) and hands the snapshot to the async saver.
// The dump is race-free (zone-owned reads only); the saver does the blocking write off-goroutine.
// A character with no PersistID (storeless/ephemeral, or not yet created) is skipped.
func (z *Zone) enqueueSave(id string, s *session, reason saveReason) {
	if z.saver == nil || !z.saver.enabled() || s.entity == nil || s.entity.pid == nil {
		return
	}
	z.saver.enqueue(saveRequest{snap: dumpCharacter(s), zone: z, id: id, reason: reason})
}

// saveOk advances the session's state_version to the value the Postgres CAS bumped it to, so the
// next save CASes on the current row version. Posted back by the saver after a successful flush;
// applied on the zone goroutine. A higher stored version is never lowered (a concurrent flush or
// reconcile may already have advanced it).
func (z *Zone) saveOk(id string, newVersion uint64) {
	s := z.players[id]
	if s == nil {
		return
	}
	if newVersion > s.stateVersion {
		s.stateVersion = newVersion
		z.log.Debug("save ok: state_version advanced", "player", id, "state_version", newVersion)
	}
}

// saveReconcile finishes a live player's CAS-loss reconcile (Zone.saveConflict's spawned re-read
// posts saveReconcileMsg here). It adopts the freshly re-read version AND re-dumps the player's
// CURRENT in-memory state at it, re-enqueuing the flush — so the data the losing flush carried is
// re-WRITTEN rather than dropped. Runs on the zone goroutine, so the re-dump is single-writer. If
// the player has since left (the reconcile read raced a logout), there is nothing to re-dump: the
// logout's own saveFinal flush already carried the authoritative final state, so we simply return.
func (z *Zone) saveReconcile(id string, newVersion uint64) {
	s := z.players[id]
	if s == nil {
		z.log.Debug("save reconcile: player gone; logout flush carried final state", "player", id)
		return
	}
	if newVersion > s.stateVersion {
		s.stateVersion = newVersion
	}
	z.log.Debug("save reconcile: re-dumping current state at fresh version", "player", id, "state_version", s.stateVersion)
	// Re-enqueue a cadence-tier flush (saveFlush, not saveFinal): the player is still present, so a
	// subsequent CAS miss should reconcile through this same live-session path again.
	z.enqueueSave(id, s, saveFlush)
}

// saveConflict reconciles a still-LIVE player's state_version CAS loss (a concurrent flush — its own
// cadence racing a checkpoint/drain, or a zombie/duplicated owner — saved first). It re-reads the
// current durable version off the zone goroutine — via the saver's store — and on return ADOPTS it
// AND re-dumps the player's CURRENT in-memory state, re-enqueuing the flush at the fresh version so
// the conflict re-WRITES rather than silently dropping the data it was carrying. The re-read is the
// ONLY blocking call, so it runs in a spawned goroutine (mirroring beginHandoff) that posts the
// refreshed version back as saveOkMsg; the zone goroutine then re-dumps + re-enqueues on its own
// goroutine (the saveOk handler), keeping the dump single-writer. If the store/character is gone we
// simply log — the next cadence flush re-CASes from whatever version the session then holds.
//
// This path is for a player STILL PRESENT in the zone. The logout/leave flush takes saveFinal
// instead (saver.go), which the saver reconciles itself — there is no live session here to re-dump
// once the player has been removed.
//
// Crash-failover note (docs/PLACEMENT.md §6): a conflict is exactly the fence that protects a
// genuinely-rehydrated player on a NEW shard from a zombie original — the original's save fails
// the CAS here. Reconciling by RE-READING (never force-writing) means this shard yields to the
// higher version before re-dumping, rather than blindly clobbering a legitimate newer owner: the
// re-dump still goes through a fresh CAS, so a truly-superseded shard simply loses again next time.
func (z *Zone) saveConflict(id string) {
	s := z.players[id]
	if s == nil || z.saver == nil || z.saver.store == nil || s.entity == nil || s.entity.pid == nil {
		z.log.Debug("save conflict: no session/store to reconcile", "player", id)
		return
	}
	name := s.character
	store := z.saver.store
	z.log.Debug("save conflict: reconciling (re-reading current version)", "player", id)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), saveIOTimeout)
		defer cancel()
		snap, found, err := store.LoadCharacter(ctx, name)
		if err != nil || !found {
			z.log.Debug("save conflict reconcile read failed", "player", name, "found", found, "err", err)
			return
		}
		// Adopt the current durable version AND re-dump current state at it. saveOk raises the
		// session's version (never lowers) then re-enqueues a flush, so the data this conflict was
		// carrying is re-written at the fresh version rather than lost.
		z.post(saveReconcileMsg{id: name, newVersion: snap.StateVersion})
	}()
}

// characterCreated adopts a freshly-minted PersistID onto the live player entity after the
// brand-new-character INSERT returned (createCharacter posted createdMsg). PersistID becomes REAL
// here (identity.go). Idempotent: a second create (a race between two logins of a new name — the
// CITEXT UNIQUE makes only one INSERT win) is guarded by checking pid==nil. The player may have
// logged out before the insert returned; then there is no session and we drop the PID (the row
// exists and the next login will LOAD it).
func (z *Zone) characterCreated(id string, pid PersistID) {
	s := z.players[id]
	if s == nil || s.entity == nil {
		// The player logged out DURING the create round-trip. If leave() stashed a final logout
		// snapshot (the create-window flush deferral), replay it now: stamp the freshly-minted PID on
		// and enqueue the saveFinal so the actions taken during the create window (e.g. the room they
		// walked to) are durable — logout stays a true flush point (docs/PERSISTENCE.md §6). The
		// CreateCharacter INSERT started the row at version 0 and the snapshot was dumped at version 0,
		// so its CAS lands cleanly; saveFinal's saver reconcile (finalizeFlush) covers any late cadence
		// race (there is none here — the session is gone). No stash => genuinely nothing to persist.
		if snap, ok := z.pendingFinalFlush[id]; ok {
			delete(z.pendingFinalFlush, id)
			snap.PID = pid
			z.log.Info("replaying deferred logout flush after create completion", "player", id, "pid", pid, "room", snap.RoomRef)
			z.saver.enqueue(saveRequest{snap: snap, zone: z, id: id, reason: saveFinal})
			return
		}
		z.log.Debug("character created but session gone; row will load next login", "player", id)
		return
	}
	// The player is still present: a stale stash (they quit then re-attached within the grace, or a
	// duplicate createdMsg) must not later clobber the live record — drop it; the live cadence/logout
	// flush is authoritative.
	delete(z.pendingFinalFlush, id)
	if s.entity.pid != nil {
		return // already has one (a prior create/load won the race)
	}
	p := pid
	s.entity.pid = &p
	z.log.Debug("new character durable id assigned", "player", id, "pid", pid)
	// Flush whatever the player did during the async-create round-trip: every save was skipped
	// while pid==nil (enqueueSave's guard), so persist current state now rather than waiting for
	// the next cadence tick — closes the new-character first-actions loss window for a player who
	// is still connected when the create returns.
	z.enqueueSave(id, s, saveFlush)
}

// characterCreateFailed evicts the create-window logout stash for a brand-new character whose async
// CreateCharacter failed permanently (createCharacter's goroutine posted createFailedMsg instead of
// createdMsg). It runs ON the zone goroutine (single-writer over the zone-owned map). If the player
// quit inside the create round-trip, leave() parked a final snapshot in pendingFinalFlush that
// characterCreated would normally replay; with the create dead there is no replay, so the entry would
// linger for the zone's lifetime (FOLLOW-UPS §2 — bounded, one cold entry per failed name, no
// amplification, but no active eviction). This reclaims it immediately. The op is DELETE-ONLY: it can
// neither resurrect a flush nor land one on the wrong row. A live session (the player re-attached, or
// a fast-create that never stashed) is left untouched — the live cadence/logout path owns the record;
// dropping a stash that does not exist is a no-op. This NEVER drops an entry a legitimate createdMsg
// will still replay: a permanent create failure is mutually exclusive with a (slow-but-successful)
// createdMsg — the goroutine posts exactly one of the two, so a slow success is never falsely evicted.
func (z *Zone) characterCreateFailed(id string) {
	if _, ok := z.pendingFinalFlush[id]; !ok {
		return // the common case: a fast create that returned an error but never stashed a logout
	}
	delete(z.pendingFinalFlush, id)
	z.log.Info("evicted create-window logout stash after permanent create failure (data was never persistable)", "player", id)
}

// resolveHandoffPid recovers a handed-off player's durable PersistID BY NAME when the handoff
// snapshot carried none (the async-create window, §C2). It reads the durable row off the zone
// goroutine — mirroring beginHandoff/saveConflict — and posts the PID + state_version back as
// adoptPidMsg. This is the engine-universal crash-rehydrate-by-name primitive (docs/PERSISTENCE.md
// §6): the row is keyed by name, so the destination can recover the id without the source having
// carried it. A graceful no-op when no store is configured (ephemeral) or the row is not found yet
// (the create still in flight) — the player simply stays unpersisted, the same observable as today's
// pre-create window, and a later cadence tick is NOT needed because the bind only fires once; if the
// row truly never appears the character is ephemeral, exactly the prior behavior.
func (z *Zone) resolveHandoffPid(s *session) {
	if z.saver == nil || z.saver.store == nil {
		return // ephemeral: nothing to resolve against
	}
	store := z.saver.store
	name := s.character
	z.log.Debug("handoff bind: PID absent, re-resolving by name", "player", name)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), saveIOTimeout)
		defer cancel()
		snap, found, err := store.LoadCharacter(ctx, name)
		if err != nil || !found {
			// The async create has not committed the row yet (or an infra hiccup). Non-fatal: the
			// character stays ephemeral on this shard, same as a pre-create login. We do NOT retry —
			// a brand-new char that races this hard is already an extreme edge; the row is durable
			// once the create lands and the NEXT login will load it.
			z.log.Debug("handoff PID re-resolve: row not found (staying ephemeral)", "player", name, "found", found, "err", err)
			return
		}
		z.post(adoptPidMsg{id: name, pid: snap.PID, version: snap.StateVersion})
	}()
}

// adoptHandoffPid installs a BY-NAME-resolved PersistID (+ the row's current state_version) onto a
// handed-off player's live entity, on the zone goroutine (single-writer). It is the destination twin
// of characterCreated for the async-create window: PersistID becomes REAL so cadence/logout flushes
// can CAS the row. Idempotent + guarded: it adopts only when the entity STILL has no PID (a carried
// snapshot PID or a racing createdMsg wins first) so it never clobbers an already-real identity. The
// version is adopted alongside the PID so the destination's first save CASes against the right base —
// never letting a stale snapshot version clobber a row the source may have advanced (monotonic CAS,
// docs/PERSISTENCE.md §7). After adopting, flush once so the player's post-handoff location (and any
// actions taken on this shard before the PID landed) reach the durable row promptly.
func (z *Zone) adoptHandoffPid(id string, pid PersistID, version uint64) {
	s := z.players[id]
	if s == nil || s.entity == nil {
		z.log.Debug("handoff PID adopt: session gone; row loads next login", "player", id)
		return
	}
	if s.entity.pid != nil {
		return // a carried PID or a racing create already made identity real
	}
	p := pid
	s.entity.pid = &p
	if version > s.stateVersion {
		s.stateVersion = version // adopt the row's CAS base so the first save is monotonic
	}
	z.log.Debug("handoff PID adopted by name", "player", id, "pid", pid, "state_version", s.stateVersion)
	z.enqueueSave(id, s, saveFlush)
}

// Arrival/departure/say lines that others should see now flow through Zone.act
// (act.go) — one perspective-aware call replaces the old broadcast helper. act walks
// the same uniform containment tree (room.contents, MUDLIB §4) and reaches each player
// through its PlayerControlled session sink, so the bystander text is unchanged.

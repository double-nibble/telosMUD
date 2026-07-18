// Package world is the simulation shard: the zone(s) this process owns, the
// actor event loop, the gRPC Play server, and the source side of the cross-shard
// handoff. Content loading and the full mudlib arrive in later phases
// (docs/ROADMAP.md).
//
// # The actor-per-zone model (docs/ARCHITECTURE.md §3)
//
// A Zone is owned by exactly one goroutine (Zone.Run). Rooms and players are plain
// data, not goroutines; only the zone goroutine reads or mutates them, so game logic
// needs no locks. Every interaction from outside that goroutine — a player's gRPC
// stream handler, and the async handoff coordinator — happens by posting a message to
// the zone inbox (Zone.post), never by touching zone state directly.
//
// # Following a command end to end
//
//	gRPC reader (server.go) -> zone inbox -> zone loop (zone.go/commands.go)
//	                                      -> player.out -> gRPC writer (server.go) -> wire
//
// Run with DEBUG=1 (see internal/obs) to watch every step narrated via slog.Debug.
package world

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
	roster "github.com/double-nibble/telosmud/internal/presence"
	"github.com/double-nibble/telosmud/internal/sessionlock"
)

// handoffRPCTimeout bounds the whole source-side handoff conversation (ShardForZone/
// EndpointForShard/Prepare/SetPlayerShard). Without a deadline a Prepare to a restarting
// or draining destination hangs forever and the coordinator posts NEITHER redirectMsg NOR
// handoffFailMsg — leaving the player permanently frozen and locked out of reconnect. On
// the deadline the fail(...) path posts handoffFailMsg and the source thaws + restores the
// player to the room they tried to leave. It is a package var (not a const) so a test can
// shrink it to exercise the timeout->thaw path quickly.
var handoffRPCTimeout = 5 * time.Second

// maxConcurrentHandoffs bounds the in-flight cross-shard handoff Prepares this shard runs at once, so a
// graceful drain's fan-out over a whole zone paces its Prepares against the target instead of stampeding it
// (16.4b review). A normal move is never throttled by a 32-deep pool.
const maxConcurrentHandoffs = 32

// HandoffDialer resolves a Handoff client for a peer shard's address. Injected so
// tests can dial in-process shards over bufconn.
type HandoffDialer func(addr string) (handoffv1.HandoffClient, error)

// Shard is one world process. It may host more than one zone: a player can walk
// between two zones THIS shard owns entirely in-process, with no cross-shard handoff
// (see Zone.transferIn / Zone.move). zones maps zone id -> zone; home is the zone a
// fresh login spawns in. addr is the shard's public address (what the gate and peer
// shards dial) and dir routes moves into zones OTHER shards own.
//
// # Cross-goroutine routing primitives (single-writer still holds)
//
// Each zone's state is mutated only by its own goroutine. Two deliberately small,
// concurrency-safe structures connect them:
//
//   - the per-connection currentZone (an atomic.Pointer[Zone] owned by the Play
//     stream, see server.go): which zone a player's input is posted to right now. A
//     zone Stores itself into it when it takes ownership of the player (attach /
//     transferIn); the reader loop Loads it for every line.
//   - tokenIndex (token -> zone): lets the Play attach and Handoff.Prepare route a
//     handoff bind to whichever hosted zone holds the matching pending player. Guarded
//     by mu; populated by Prepare, read on bind.
//
// No other shared mutable zone state exists. Intra-shard transfer of the player
// struct itself is still done by message-passing (transferInMsg), so only one zone
// goroutine ever owns a given player at a time.
type Shard struct {
	zones map[string]*Zone // zone id -> zone; all hosted on this process
	home  string           // zone a fresh login spawns in
	addr  string           // this shard's public address ("" in single-shard tests)
	dir   Locator          // directory for cross-shard routing; nil seals cross-shard exits
	peers HandoffDialer    // dials peer shards' Handoff service

	// verifyKey is account's Ed25519 PUBLIC key (Phase 14.3, ACCOUNT.md §9): the shard verifies the gate's
	// session assertion against it OFFLINE on a fresh-login Attach (no per-connect RPC to account). nil =>
	// assertions are NOT enforced (dev / pre-14.3 — the shard trusts the gate's asserted identity directly).
	verifyKey ed25519.PublicKey

	// handoffSignKey / handoffVerifyKey authenticate the cross-shard handoff (docs/REMAINING.md §1). All
	// shards in a cluster share the handoff keypair: the source SIGNS an outgoing Prepare with the private
	// key, the destination VERIFIES the incoming Prepare's snapshot_sig with the public key, so a forged
	// Prepare from a party without the key cannot inject an arbitrary state_json. Both nil => signing off
	// and enforcement off (dev/test, and the pre-signing behavior) — see handoffsig.go.
	handoffSignKey   ed25519.PrivateKey
	handoffVerifyKey ed25519.PublicKey

	// allowInsecureHandoff permits this shard to ACCEPT inbound cross-shard handoff RPCs (Prepare/AdoptZone)
	// while KEYLESS — i.e. without a snapshot signature to verify (#260). It defaults FALSE so a keyless world
	// REFUSES handoffs (fail-closed): a single-shard deployment never legitimately receives one, and an
	// unauthenticated Prepare on a reachable keyless world port is a known-prototype item-injection vector
	// (an econ dupe) independent of the peer/discoverability boot guard CheckHandoffAuth adds. It is set true
	// ONLY under the explicit TELOS_ALLOW_INSECURE opt-in (cmd wires cfg.AllowInsecure), mirroring
	// CheckHandoffAuth's posture — a trusted local/dev rig that deliberately runs keyless. A shard WITH a
	// verify key ignores this flag entirely: it always authenticates by signature.
	allowInsecureHandoff bool

	// sessionLock is the Phase-14.4 cross-shard single-session lock (sessionlock.Lock): on a fresh login the
	// stream goroutine ACQUIRES it (takeover) and a renewer heartbeats it; a session displaced by a newer
	// login (anywhere in the fleet) sees its renew fail and self-kicks. nil => not enforced (no Redis / dev).
	// lockTTL is the key's expiry (a crashed connection self-clears after it); lockRenew is the heartbeat
	// cadence (also how fast a takeover is noticed). Both default if zero (DefaultLockTTL/DefaultLockRenew).
	sessionLock sessionlock.Lock
	lockTTL     time.Duration
	lockRenew   time.Duration

	mu         sync.Mutex       // guards tokenIndex, zones, and runCtx/runWG/draining (16.4a runtime zone-add)
	tokenIndex map[string]*Zone // handoff token -> hosting zone (populated by Prepare)

	// residentZone maps a character -> the zone on THIS shard that currently HOLDS their session (including a
	// link-dead session still held through its grace). It is the live, in-memory answer to "which zone hosts
	// this player right now", maintained by setPlayer/delPlayer so it stays EXACTLY the union of every zone's
	// players map (the same choke points that mirror pop) (#321).
	//
	// A reconnect consults it BEFORE the durable zone_ref. The durable record lags an intra-shard zone walk by
	// an async flush (transferIn never enqueues a save; detach's save is drained off-goroutine), so a link-dead
	// resume routed by the stale durable zone lands in a zone that no longer holds the session, takes the
	// fresh-login branch, and DOUBLE-OWNS the character — a fresh copy while the detached copy still sits in
	// the zone they walked to. The in-memory index closes that window with no I/O.
	//
	// Its own mutex: setPlayer/delPlayer run on EVERY zone's goroutine and the reconnect read on a stream
	// goroutine, so keeping it off s.mu avoids contending with zone-hosting/drain. Guarded by residentMu.
	residentMu   sync.Mutex
	residentZone map[string]*Zone

	// content is the full loaded content — every pack's prototypes are already in protos (defineContent
	// fills from ALL loaded zones, not just the hosted set). Retained so HostZone (Phase 16.4a runtime
	// zone-add) can build a zone this shard did NOT host at boot: a standby re-claiming a draining peer's
	// zone. nil on a demo/test shard built without a LoadedContent (its zones are pre-built) — HostZone then
	// errors. Set by NewShardFromContent.
	content *content.LoadedContent

	// runCtx / runWG are captured by Run so HostZone can launch a runtime-added zone's actor on the shard's
	// lifetime ctx and have Run's Wait() cover it. closed flips true (under mu) once Run has observed ctx
	// cancel and is about to wg.Wait(): HostZone refuses after that, so its wg.Add can never race the Wait
	// (every successful Add is mu-ordered before closed=true, hence before Wait). Guarded by mu.
	// (BeginDrain adds a separate draining flag in 16.4b.)
	runCtx context.Context
	runWG  *sync.WaitGroup
	closed bool

	// Zone-lease ownership (Phase 16.4b): moved into the shard (from cmd/telos-world) so a graceful drain can
	// hand a zone's lease to a peer WITHOUT the source's own renewal fencing the whole shard. leaser is the
	// directory write-port (nil => no leasing, single-shard/dev); shardID is the directory write-authority
	// key; onFence cancels the shard's run ctx when we UNEXPECTEDLY lose a lease. handedOff records zones we
	// DELIBERATELY handed off (their renewal stops silently, no fence); leaseStop cancels a hosted zone's
	// renewal goroutine on handoff. handedOff + leaseStop are guarded by mu.
	leaser  ZoneLeaser
	shardID string
	// placement collects per-player directory placement writes from the zone goroutines for the
	// background writer (placement.go, #320). Coalescing by player: never blocks an actor loop, and
	// never discards a meaningful write.
	placement  *placementWriter
	leaseTTL   time.Duration
	leaseRenew time.Duration
	onFence    func()
	handedOff  map[string]bool
	leaseStop  map[string]context.CancelFunc

	// Per-zone ACTOR lifetime (#288). Every zone's Run goroutine gets its own ctx derived from runCtx, so a
	// single zone can be stopped without stopping the shard — the inverse of HostZone's runtime zone-add.
	// actorDone closes when that goroutine returns, which is what UnhostZone waits on before declaring the
	// zone gone (the Lua VM is torn down on the zone goroutine, in Run's defer). Both guarded by mu.
	actorStop map[string]context.CancelFunc
	actorDone map[string]chan struct{}

	// draining flips true on BeginDrain (16.4b): the Play attach path then REFUSES a fresh login (this shard
	// is going away) while still accepting a handoff BIND, so an in-flight cross-shard move completes. mu.
	draining bool

	// drainMarker publishes this shard's draining state to the directory during BeginDrain (#41), so a peer
	// draining at the same moment (a fleet rollout) does not pick US as its target. nil (no directory / dev)
	// disables it — the process-local `draining` flag still guards fresh logins.
	drainMarker DrainMarker
	// drainReleaser hands back this shard's drain-target reservations at drain completion (#284); nil
	// leaves them to expire on their own per-field TTL.
	drainReleaser DrainTargetReleaser

	// occPublisher heartbeats each hosted zone's live player count to the directory on the lease-renewal
	// cadence (#42), the load signal the placement coordinator weights the plan by. nil disables it (the
	// coordinator then falls back to zone-count balancing).
	occPublisher ZoneOccupancyPublisher

	// rebalancePort reads/refreshes/clears the coordinator's per-zone rebalance directive (#42 slice 3); the
	// owning shard polls it on the lease-renewal tick and executes a single-zone drain. nil disables it (no
	// coordinator-driven rebalancing — the shard still serves + is drained on SIGTERM as before).
	rebalancePort RebalancePort
	// rebalancing is the set of zones this shard is CURRENTLY rebalancing (a drain in flight), guarded by mu.
	// It is the primary in-flight-drain dedupe: a directive re-read on the next renewal tick refreshes the
	// directive rather than launching a second drain of the same zone.
	rebalancing map[string]bool
	// rebalanceBackoff[zone] is the earliest time a failed rebalance of that zone may be retried, guarded by
	// mu — so a persistently-unreachable target isn't re-dialed every renewal tick.
	rebalanceBackoff map[string]time.Time

	// localZones marks zones this shard hosts LOCALLY and UNLEASED (#212 embedded core bootstrap zone).
	// A local zone is (a) never lease-renewed — every shard hosts its OWN copy, so there is no single
	// owner to fence on, and (b) never handed off on a graceful drain — a draining peer's local zone is
	// not moved (the target shard already built its own). It IS still durably flushed on shutdown like
	// any zone. Read-only after construction (WithLocalZones, before Run); absent => a normal leased,
	// drainable zone.
	localZones map[string]bool

	// instances / mintRate / instanceLimits are the runtime-minted instanced-zone bookkeeping (#411,
	// instance.go): instance zone id -> the record its cap slot is charged against, and per-account mint-rate
	// buckets. Both guarded by mu — the SAME mutex that guards s.zones — so a mint's cap check and its
	// publish into s.zones are one atomic decision, and so UnhostZone can drop a zone and free its cap slot
	// in one hold. instanceLimits is set at construction and read-only after (WithInstanceLimits, before Run).
	//
	// An instance is hosted UNLEASED, like a local bootstrap zone, so it appears in s.zones but in none of the
	// lease/placement/handoff maps. It is also the only zone class this shard creates on its own initiative
	// rather than being told to, which is why its bound lives here rather than in the directory.
	instances      map[string]*instanceRecord
	mintRate       map[string]*mintBucket
	instanceLimits instanceLimits

	// handoffSem bounds the number of CONCURRENT in-flight cross-shard handoffs (Prepare RPCs) this shard
	// runs — so a graceful drain's fan-out over a whole zone doesn't fire N simultaneous Prepares at one
	// target and stampede it (16.4b review). A normal move (rare) is never throttled by a 32-deep slot pool.
	handoffSem chan struct{}

	// saver is the shard's async character writer (saver.go): one per shard, shared by every
	// hosted zone, drained by a single background goroutine started in Run. It does all the
	// off-zone-goroutine character I/O (Redis checkpoint + Postgres CAS). Always non-nil but
	// DISABLED (a no-op) unless a store/checkpointer was configured — so a storeless shard
	// keeps the pre-4.2 ephemeral behavior with zero extra goroutines or work.
	saver *saver

	// auditor is the shard's async audit-trail writer (auditor.go, #350): one per shard, shared by
	// every hosted zone, drained by a single background goroutine started in Run — the same lifecycle
	// as the saver. It does the off-zone-goroutine AppendAudit for the world-emitted events (death,
	// attribute grant, track step). Always non-nil but DISABLED (a no-op) unless WithAudit wired a
	// sink — so a storeless shard audits nothing and is byte-identical to a pre-#350 shard.
	auditor *auditor

	// protos is the per-shard prototype cache shared read-only by every hosted zone. It is held
	// here too (not only on each zone) so the hot-reload applier can swap entries into the one
	// shared cache (reload.go). nil only on a zero-value shard; newBareShard leaves it for the
	// constructor to set after building the cache.
	protos *protoCache

	// defs is the per-shard bundle of pack-global definition registries (attributes/resources/
	// damage-types — defs.go), shared read-only by every hosted zone exactly like protos. Built
	// once at construction, then only read; the 4.3-style hot-reload swap (a later slice) would
	// reload an entry into this one shared bundle. Set by the constructors after registration.
	defs *defRegistries

	// reloader, if non-nil, is the content hot-reload applier (reload.go): it subscribes to the
	// content bus and atomically swaps a rebuilt *Prototype into protos on an invalidation. nil
	// when no bus/source is configured — hot reload disabled, a busless shard is byte-identical
	// to a pre-4.3 shard. Set by WithHotReload, torn down at Run end.
	reloader *reloader

	// comms is the shard's Phase-8 comms SOURCE plumbing (comm.go): the RoleWorld commbus handle the
	// world publishes channel lines through, the server-held per-author sequence (P8-A3), and the
	// per-author rate-limit buckets (P8-A1). Always non-nil (a Disabled no-op bus until WithComms
	// wires a live one), shared read-only by every hosted zone exactly like protos/defs — a busless
	// shard publishes to nowhere and is byte-identical to a pre-Phase-8 shard.
	comms *commSource

	// chanHistory is the shard's #348 SHARD-LOCAL channel scrollback (channelhistory.go): a bounded
	// per-channel ring of recently-published lines, captured at the local publish path (cmdChannel) and
	// read back via `history <channel>` behind a FETCH-TIME canHear gate. Always non-nil on a constructed
	// shard; a bare/storeless test zone reaches nil via z.channelHistory() and simply captures/serves
	// nothing (the never-fatal degradation). PARTIAL BY CONSTRUCTION on a multi-shard fleet — see the file
	// header — because a shard only sees its OWN players' lines; cross-shard aggregation is a deferred slice.
	chanHistory *chanHistory

	// presence is the shard's Phase-8 cross-shard `who` plumbing (presence.go): the resident-player set
	// THIS shard hosts plus the background loop that publishes it to the shared presence roster (Redis in
	// prod). Always non-nil (DISABLED — a nil roster, every method a no-op — until WithPresence wires one),
	// shared read-only by every hosted zone exactly like comms/saver. A busless/rosterless shard does zero
	// presence work and `who` falls back to the zone-local list — byte-identical to a pre-8.4 shard.
	presence *presenceTracker

	// mail is the shard's Phase-8.7 durable mail inbox (mail.go), shared read-only by every hosted zone.
	// nil DISABLES mail (no Postgres / a storeless shard) — the mail commands degrade to "mail is
	// unavailable", never a crash, and a pre-8.7 / storeless shard is byte-identical. Wired by WithMail.
	// The store does its own blocking pool I/O off the zone goroutine (the mail command spawns a goroutine).
	mail MailStore

	// scopes is the shard's Phase-10.3b scoped-event-bus wiring (scope.go): it subscribes to the world
	// scope + each hosted region and routes a director's state broadcast DOWN to the affected zones'
	// read-replicas. nil DISABLES scope replication (no scoped bus / the single-shard tests) — such a
	// shard does zero scope work and is byte-identical to a pre-10.3 shard. Wired by WithScopeBus;
	// started in Run, torn down at Run end.
	scopes *scopeReplication
}

// NewDemoShard builds a single-shard midgaard world with no directory wiring — its
// cross-shard exits are sealed. Used by the single-shard tests and a bare run.
func NewDemoShard() *Shard {
	return newShard([]string{"midgaard"}, "midgaard", "", nil, nil)
}

// NewShard builds the named demo zone and wires it for cross-shard handoff: addr is
// this shard's public address, dir routes moves into zones other shards own, and
// peers dials those shards' Handoff service. Single-zone convenience wrapper around
// NewMultiShard.
func NewShard(zoneID, addr string, dir Locator, peers HandoffDialer) *Shard {
	return NewMultiShard([]string{zoneID}, zoneID, addr, dir, peers)
}

// NewMultiShard builds a shard hosting every zone in zoneIDs (home is the spawn zone
// for fresh logins) and wires each for cross-shard handoff. A move into a zone this
// shard hosts is handled in-process (Zone.move); a move into a zone another shard owns
// goes through beginHandoff as before.
func NewMultiShard(zoneIDs []string, home, addr string, dir Locator, peers HandoffDialer) *Shard {
	return newShard(zoneIDs, home, addr, dir, peers)
}

// newShard builds a shard whose zones are the EMBEDDED demo pack (test/bare-run path). Each
// zone is constructed via newDemoZone, which loads packs/demo.yaml into the shared cache and
// builds the named zone — so the demo world has no Postgres dependency and the Phase 1-3
// tests construct it exactly as before. Production uses NewShardFromContent instead, so the
// demo pack is never compiled into the engine's production boot path.
func newShard(zoneIDs []string, home, addr string, dir Locator, peers HandoffDialer) *Shard {
	s := newBareShard(home, addr, dir, peers)
	// Build the per-shard prototype cache ONCE here, before any zone goroutine runs
	// (prototype.go). It is shared read-only across every hosted zone, so the flyweight
	// pays off across the whole process and the cross-goroutine sharing needs no lock —
	// it is published immutable. After this loop nothing mutates the cache or a *Prototype.
	protos := newProtoCache()
	s.protos = protos
	// Register the demo pack's pack-global defs ONCE into the shared shard bundle, before any zone
	// runs (defineGlobals), so every hosted zone reads the SAME atomic-swap registries — the same
	// shared-read-only-after-publish discipline as protos. adopt points each zone at this bundle.
	if lc, err := content.LoadDemoPack(); err == nil {
		defineGlobals(s.defs, lc)
	}
	for _, id := range zoneIDs {
		z := newDemoZone(id, protos)
		s.adopt(id, z)
	}
	if s.zones[home] == nil && len(zoneIDs) > 0 {
		s.home = zoneIDs[0]
	}
	return s
}

// NewShardFromContent is the PRODUCTION constructor (cmd/telos-world buildShard). It fills
// the shared per-shard prototype cache from already-loaded content (Postgres or the embedded
// pack, chosen by the binary), then builds every hosted zone from that content via buildZone.
// With empty content (no DB / no enabled packs) every zone boots EMPTY — the bare-engine
// invariant — and a login is rejected cleanly rather than panicking (Zone.join guards).
//
// Unlike newShard it does NOT call newDemoZone, so no demo content is linked into the
// production path; the demo lives only in the YAML/DB.
func NewShardFromContent(lc *content.LoadedContent, zoneIDs []string, home, addr string, dir Locator, peers HandoffDialer) *Shard {
	s := newBareShard(home, addr, dir, peers)
	s.content = lc // retained for HostZone (16.4a): a standby builds a not-yet-hosted zone from this
	protos := newProtoCache()
	s.protos = protos
	defineContent(protos, lc) // fill the cache once from all loaded zones, before any zone runs
	defineGlobals(s.defs, lc) // register pack-global attribute/resource/damage-type defs (5.1)
	for _, id := range zoneIDs {
		z := newZone(id)
		z.protos = protos
		z.buildZone(lc) // spawn room singletons + run resets (empty if the zone wasn't loaded)
		s.adopt(id, z)
	}
	if s.zones[home] == nil && len(zoneIDs) > 0 {
		s.home = zoneIDs[0]
	}
	return s
}

// newBareShard allocates the Shard struct with its routing maps; callers then build and
// adopt the hosted zones. The saver starts DISABLED (nil store + checkpointer); WithPersistence
// swaps in a configured one before any zone is adopted.
func newBareShard(home, addr string, dir Locator, peers HandoffDialer) *Shard {
	return &Shard{
		zones:            map[string]*Zone{},
		home:             home,
		addr:             addr,
		dir:              dir,
		peers:            peers,
		tokenIndex:       map[string]*Zone{},
		residentZone:     map[string]*Zone{},
		handedOff:        map[string]bool{},
		leaseStop:        map[string]context.CancelFunc{},
		actorStop:        map[string]context.CancelFunc{},
		actorDone:        map[string]chan struct{}{},
		rebalancing:      map[string]bool{},
		rebalanceBackoff: map[string]time.Time{},
		// Instanced-zone bookkeeping (#411, instance.go). The limits default here rather than at first use so
		// a shard built by ANY constructor is capped — a zero-valued cap would mean "unbounded", which for the
		// per-shard bound is a resource-exhaustion hole rather than a permissive default.
		instances:      map[string]*instanceRecord{},
		mintRate:       map[string]*mintBucket{},
		instanceLimits: defaultInstanceLimits(),
		handoffSem:     make(chan struct{}, maxConcurrentHandoffs),
		placement:      newPlacementWriter(),
		saver:          newSaver(nil, nil), // disabled until WithPersistence configures it
		auditor:        newAuditor(nil),    // disabled until WithAudit configures it (#350)
		// Empty global-definition bundle (defs.go); the constructor registers content into it
		// before any zone runs and shares the SAME bundle pointer with every hosted zone.
		defs: newDefRegistries(),
		// Comms SOURCE plumbing (comm.go), DISABLED until WithComms wires a live RoleWorld bus — a
		// shard with no comms bus publishes channel lines to nowhere (the never-fatal degradation).
		comms: newCommSource(),
		// SHARD-LOCAL channel scrollback (channelhistory.go, #348): a bounded per-channel recent-lines ring
		// captured at the local publish path. Always non-nil (empty until a channel with history>0 is spoken
		// on); shared read-only-handle by every hosted zone via z.channelHistory(), exactly like comms.
		chanHistory: newChanHistory(),
		// Cross-shard presence tracker (presence.go), DISABLED until WithPresence wires a live roster — a
		// shard with no roster does no presence I/O and `who` reads the zone-local list (pre-8.4 behavior).
		presence: newPresenceTracker(),
	}
}

// WithPersistence configures the shard's durable character ladder: the Postgres-backed store and
// the optional Redis checkpointer. Either may be nil (a nil store = no durable record, a nil
// checkpointer = no Redis tier; both nil = ephemeral, today's behavior). It MUST be called before
// the shard's zones run (it is wired into the saver every zone shares via adopt). Returns the
// shard for chaining. The production constructor (cmd/telos-world buildShard) calls this; tests
// inject an in-memory store the same way.
func (s *Shard) WithPersistence(store CharacterStore, ckpt Checkpointer) *Shard {
	s.saver = newSaver(store, ckpt)
	// Re-point every already-adopted zone at the new saver (adopt copies the pointer). Callers
	// normally call this before adopting any zone, but re-pointing keeps the wiring order-free.
	for _, z := range s.zones {
		z.saver = s.saver
	}
	return s
}

// WithComms wires the shard's Phase-8 comms SOURCE bus (docs/PHASE8-PLAN.md slice 8.3, P8-D1): the
// world is the message SOURCE for channels, so it publishes channel lines through a RoleWorld commbus
// handle (commbus.OpenWorld from cmd/telos-world — NEVER OpenGate: a world handed a gate handle could
// not publish at all). bus MUST be a RoleWorld handle. It is OPTIONAL and never fatal: a nil bus (or
// the Disabled no-op a NATS-down OpenWorld returns) leaves the shard publishing to nowhere — channels
// degrade to "temporarily offline" for the speaker, the empty-boot/bare-zone tests stay green, and the
// shard is byte-identical to a pre-Phase-8 one. MUST be called before Run. Returns the shard for
// chaining; the production constructor wires it, tests inject a MemBus WORLD handle the same way.
func (s *Shard) WithComms(bus commbus.Bus) *Shard {
	if bus != nil {
		s.comms.bus = bus
	}
	return s
}

// WithVerifyKey wires account's Ed25519 public key so the shard ENFORCES session assertions on fresh logins
// (Phase 14.3). Without it the shard trusts the gate's asserted identity directly (dev / pre-14.3). Must be
// called before Run.
func (s *Shard) WithVerifyKey(pub ed25519.PublicKey) *Shard {
	s.verifyKey = pub
	return s
}

// WithHandoffKeys wires the shared cluster handoff keypair (docs/REMAINING.md §1). The shard signs its
// outgoing Prepare snapshots with priv and enforces the signature on incoming Prepares with pub. Either
// half may be nil independently (a shard that only sends, or only receives, in an asymmetric test), but a
// production cluster gives every shard BOTH so every handoff is signed and verified. Must be called before
// Run.
func (s *Shard) WithHandoffKeys(priv ed25519.PrivateKey, pub ed25519.PublicKey) *Shard {
	s.handoffSignKey = priv
	s.handoffVerifyKey = pub
	return s
}

// WithInsecureHandoff opts a KEYLESS shard into ACCEPTING unauthenticated inbound handoffs (#260). Without it
// a keyless shard REFUSES Prepare/AdoptZone outright (fail-closed) — the correct posture for a single-shard
// world, which never receives a handoff, and the block against a forged Prepare on a reachable keyless port.
// cmd/telos-world sets it from cfg.AllowInsecure (the same explicit opt-in that lets a keyless multi-shard
// rig boot). A shard WITH a handoff verify key ignores it: signatures are always enforced. Must be called
// before Run.
func (s *Shard) WithInsecureHandoff(on bool) *Shard {
	s.allowInsecureHandoff = on
	return s
}

// CheckHandoffAuth is the boot-time guard against a MULTI-SHARD deployment running with UNAUTHENTICATED
// handoffs (#251). A shard that is DISCOVERABLE in the directory (s.dir != nil) can be named as a handoff
// DESTINATION by a peer (ShardForZone/EndpointForShard resolve it), so its Handoff.Prepare will accept
// incoming snapshots. With no verify key, Prepare skips signature verification, so a reachable inter-shard
// port lets a forged Prepare inject arbitrary carried state — a KNOWN-prototype item dupe (econ break), and
// since #106 the destination strips an unsigned carried tier only as defense-in-depth. This is the PRIMARY
// control on the item-injection half: a cluster that can receive handoffs MUST verify them.
//
// The signal is s.dir (discoverability = the RECEIVE capability), not s.peers (the SEND dialer) — the distsys
// review's correction: a receive-only standby (dir set, no dialer) is a valid handoff destination the peers
// check would miss. s.peers is kept as belt-and-suspenders (a send-capable shard is also multi-shard). A
// single-shard deployment (no directory, cross-shard exits sealed) is neither discoverable nor a sender, so a
// missing key is fine there (dev/demo/tests). Returns an error; the caller (cmd) turns it into a fatal boot
// failure in production and a loud warning in dev (mirroring the caller-token posture).
//
// The keyless single-shard RESIDUAL this NOTE used to carry (Handoff.Prepare is registered on the world port
// in EVERY mode, so an attacker with direct reach to a keyless port could forge a Prepare) is now CLOSED by
// #260: a keyless shard REFUSES inbound Prepare/AdoptZone at request time unless allowInsecureHandoff is set
// (WithInsecureHandoff, from cfg.AllowInsecure). This boot guard still bounds the legitimate multi-shard peer
// path (a discoverable shard MUST be keyed); #260 bounds the raw port when keyless.
func (s *Shard) CheckHandoffAuth() error {
	if (s.dir != nil || s.peers != nil) && s.handoffVerifyKey == nil {
		return fmt.Errorf("world: multi-shard deployment (directory/peer configured) has no handoff verify key; " +
			"cross-shard Prepare would accept UNAUTHENTICATED snapshots (item-dupe / elevation vector) — " +
			"configure the shared handoff keypair (WithHandoffKeys) or run single-shard")
	}
	return nil
}

// Default single-session lock timing (Phase 14.4): the key lives DefaultLockTTL (a crash self-clears after
// it) and is heartbeated every DefaultLockRenew (also the takeover-detection latency).
const (
	DefaultLockTTL   = 30 * time.Second
	DefaultLockRenew = 10 * time.Second
)

// WithSessionLock wires the cross-shard single-session lock (Phase 14.4). ttl/renew default when zero; tests
// pass small values for a fast takeover. Without this the shard relies only on the within-shard takeover
// (zone.go). Must be called before Run.
func (s *Shard) WithSessionLock(lock sessionlock.Lock, ttl, renew time.Duration) *Shard {
	s.sessionLock = lock
	s.lockTTL = ttl
	if s.lockTTL <= 0 {
		s.lockTTL = DefaultLockTTL
	}
	s.lockRenew = renew
	if s.lockRenew <= 0 {
		s.lockRenew = DefaultLockRenew
	}
	return s
}

// WithCommsRate overrides this shard's per-author channel/tell token-bucket (P8-A1) — burst lines, one
// refilling every `refill`. Default is the production-tuned commRateBurst/commRateRefill. It is an
// operator/test seam: a multi-line journey test (or a high-throughput deployment) raises it so a rapid
// confirmation burst isn't throttled. burst<=0 leaves the default. MUST be called before Run.
func (s *Shard) WithCommsRate(burst int, refill time.Duration) *Shard {
	if burst > 0 {
		s.comms.rateBurst = burst
		s.comms.rateRefill = refill
	}
	return s
}

// WithTells wires the shard's Phase-8.5 DURABLE-tell transport (docs/PHASE8-PLAN.md slice 8.5, OQ-1):
// the JetStream handle the world PublishDurable's tells through and runs the per-resident durable
// consumer on. js MUST be a JetStream handle (commbus.OpenJetStream from cmd/telos-world). It is
// OPTIONAL and never fatal: a nil handle (or the DisabledJetStream a NATS-down OpenJetStream returns)
// leaves tells disabled — `tell` reports "temporarily offline" to the sender, the existing tests stay
// green, and the shard is byte-identical to a pre-8.5 one. MUST be called before Run. Returns the
// shard for chaining; the production constructor wires it, tests inject a MemJetStream the same way.
func (s *Shard) WithTells(js commbus.JetStream) *Shard {
	if js != nil {
		s.comms.js = js
	}
	return s
}

// WithPresence wires the shard's Phase-8 cross-shard `who` roster (docs/PHASE8-PLAN.md slice 8.4, P8-D4):
// the shared presence store (presence.NewRedis in prod, presence.NewMem in the cross-shard tests) and the
// stable shardID this shard writes under (the write-authority key, P8-A4 — a shard writes ONLY its own
// residents). It is OPTIONAL and never fatal: a nil roster (no Redis) leaves presence DISABLED, so `who`
// falls back to the zone-local list and the existing who tests stay green. MUST be called before Run (the
// background heartbeat loop is started there). Returns the shard for chaining; the production constructor
// wires it from the same Redis the directory uses, tests inject a Mem roster the same way.
func (s *Shard) WithPresence(rost roster.Roster, shardID string) *Shard {
	if rost != nil {
		s.presence.roster = rost
		s.presence.shardID = shardID
	}
	return s
}

// WithMail wires the shard's Phase-8.7 durable mail inbox (docs/PHASE8-PLAN.md slice 8.7, P8-D6): the
// store the mail commands send/list/read/delete through. store MUST be a MailStore (the same *store.Pool
// that backs CharacterStore in prod, or a MemStore in tests). It is OPTIONAL and never fatal: a nil store
// (no Postgres) leaves mail DISABLED — the mail commands report "mail is unavailable", the existing tests
// stay green, and the shard is byte-identical to a pre-8.7 one. MUST be called before Run. Returns the
// shard for chaining; the production constructor wires it from the same pool, tests inject a MemStore.
func (s *Shard) WithMail(store MailStore) *Shard {
	if store != nil {
		s.mail = store
	}
	return s
}

// WithAudit wires the shard's #350 durable audit trail: the sink the world-emitted events (death,
// attribute grant, track step) are appended through. sink MUST be an AuditSink (the same *store.Pool
// that backs CharacterStore/MailStore in prod, or a MemStore in tests — ONE shared character_audit
// table, also written by telos-account in-transaction). It is OPTIONAL and never fatal: a nil sink
// leaves auditing DISABLED — the emit sites and the `audit` command degrade cleanly, the existing tests
// stay green, and the shard is byte-identical to a pre-#350 one. MUST be called before Run (the drainer
// goroutine is started there, the saver's lifecycle). Returns the shard for chaining; the production
// constructor wires it from the same pool, tests inject a MemStore.
func (s *Shard) WithAudit(sink AuditSink) *Shard {
	if sink != nil {
		s.auditor = newAuditor(sink)
	}
	return s
}

// WithHotReload enables content hot reload (docs/PHASE4-PLAN.md §5): it subscribes the shard to
// the content bus so an invalidation re-reads and swaps the one affected prototype into the shared
// cache (reload.go). src is the single-ref re-read source (the Postgres store in prod, the embedded
// or in-memory source in tests); bus is the invalidation transport; enabledPacks scopes which
// edits this shard acts on. It is OPTIONAL: a nil bus or nil src leaves hot reload DISABLED and the
// shard byte-identical to a pre-4.3 shard. MUST be called before Run (the subscription is torn down
// at Run end). Returns the shard for chaining. The production constructor wires it; tests inject an
// in-memory bus + source the same way.
func (s *Shard) WithHotReload(src content.DefinitionSource, bus contentbus.Bus, enabledPacks []string, bootContentVersion uint64) *Shard {
	s.reloader = newReloader(src, s.protos, bus, enabledPacks, bootContentVersion, s)
	return s
}

// adopt registers a built zone on the shard and wires its cross-shard handoff hook + the shared
// async saver. Locks mu: safe at construction (single-threaded) and at runtime (HostZone), where
// Play/handoff/move routing may read s.zones concurrently.
func (s *Shard) adopt(id string, z *Zone) {
	s.mu.Lock()
	s.adoptLocked(id, z)
	s.mu.Unlock()
}

// adoptLocked is adopt's body assuming the caller holds mu — HostZone uses it so the map-write and the
// runWG.Add happen atomically under one lock hold (the closed-vs-Add ordering the shutdown guard needs).
func (s *Shard) adoptLocked(id string, z *Zone) {
	z.shard = s
	z.handoff = s.beginHandoff
	z.saver = s.saver
	// Share the ONE per-shard global-definition bundle with every hosted zone (defs.go), replacing
	// the private bundle newZone/newDemoZone gave it. Every zone goroutine then reads the same
	// atomic-swap registries lock-free, exactly as it shares the one protoCache.
	if s.defs != nil {
		z.defs = s.defs
	}
	s.zones[id] = z
}

// zoneByID returns the hosted zone with the given id (nil if not hosted), under mu so it is safe
// against a concurrent HostZone (16.4a). The routing hot paths (Play attach, Handoff.Prepare,
// intra-shard move) read through here.
func (s *Shard) zoneByID(id string) *Zone {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.zones[id]
}

// claimTransferTarget resolves a zone as the destination of an intra-shard transfer AND claims the arrival on
// it, in ONE hold of mu. It returns nil if this shard is not a legitimate destination for the zone, in which
// case NO claim was taken and the caller must fall through to the cross-shard path.
//
// The single hold is the whole point (#409). Resolving with zoneByID and claiming afterwards leaves a gap: a
// concurrent UnhostZone — which checks quiescence under this SAME mutex — can complete the entire teardown in
// between, and the claim then lands on a dead zone whose `post` abandons the send. The handover is dropped and
// the counter never comes back down, so the fix meant to prevent the orphan produces one plus a wedged
// counter. Under one hold the two orders are both safe: claim first and UnhostZone sees incoming > 0 and
// refuses; unhost first and this returns nil, before the source has mutated anything, so the move simply takes
// the cross-shard branch.
//
// Hosting the zone object is NOT sufficient to be its destination. A drain or a rebalance flips the zone's
// LEASE to a peer before the zone drains, marking it handedOff under this same mutex (lease.go) while we go on
// hosting the object until it empties. Admitting a walker into a zone we no longer own would keep it resident
// — and, now that quiescence blocks teardown on that resident, keep US hosting and mutating a zone whose lease
// and adopted copy live on another shard. That is two live writers on one zone scope, the invariant the whole
// single-writer design exists to protect. Refusing here instead sends the walker down the cross-shard branch,
// which routes them to the zone's NEW owner: they follow the zone rather than pinning a stale copy of it.
//
// `draining` covers the same hazard one step earlier, and additionally closes a gap in BeginDrain's wait set:
// that set skips local bootstrap zones, so a resident of one could otherwise walk into a leased zone after the
// drain had already flushed and reclaimed it, reproducing the original orphan on the drain path.
func (s *Shard) claimTransferTarget(zoneID string) *Zone {
	s.mu.Lock()
	defer s.mu.Unlock()
	z := s.zones[zoneID]
	if z == nil || s.handedOff[zoneID] || s.draining {
		return nil
	}
	z.claimInboundTransfer()
	return z
}

// zonesList snapshots the currently-hosted zones under mu, so an iterator (Run launch, Drain,
// BeginDrain) is safe against a concurrent HostZone and never ranges the live map.
func (s *Shard) zonesList() []*Zone {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Zone, 0, len(s.zones))
	for _, z := range s.zones {
		out = append(out, z)
	}
	return out
}

// HostZone builds and starts a zone this shard did NOT host at boot — the runtime zone-add primitive
// (Phase 16.4a) a standby uses to re-claim a draining peer's zone. It is idempotent (a re-host returns
// the existing zone) and in-process only: it builds the zone from the retained content, adopts it (so
// Play/handoff/move routing finds it), and launches its actor on the shard's run ctx. The DIRECTORY
// lease + placement flip that makes peers RESOLVE this shard as the zone's owner is the caller's job
// (the drain coordinator, 16.4b) — HostZone just makes the zone live locally so a Handoff.Prepare into
// it succeeds. Errors if the shard isn't running yet (no run ctx) or has no retained content.
func (s *Shard) HostZone(ctx context.Context, id string) (*Zone, error) {
	// First pass: cheap guards + the idempotent hit, without holding mu across the (slower) zone build.
	//
	// SECURITY (#262/#315). A signed AdoptZone used to be replayable inside a 60s clock-skew window, and this
	// early return was what kept that mostly harmless. It no longer has to be: the signature now binds the
	// zone's monotonic lease GENERATION, which the HandoverZone flip increments, so a captured AdoptZone stops
	// being honored the instant the handover it authorized lands. A replay is rejected at the door, not
	// tolerated here. That is what makes pruning s.zones (#288's teardown) safe to build.
	//
	// The early return is not a pure no-op: a zone that was handed AWAY and is now being handed BACK has its
	// renewal restarted below (#288). Without that, every rebalance-BACK leaves this shard hosting and serving
	// a zone whose lease it never renews, so the lease lapses and any shard may claim it — a second writer for
	// a zone we are still writing to. Single-writer is the invariant everything rests on.
	s.mu.Lock()
	if z := s.zones[id]; z != nil {
		handedOff := s.handedOff[id]
		runCtx := s.runCtx
		s.mu.Unlock()
		if handedOff && runCtx != nil {
			// RE-ADOPTION of a zone this shard previously gave away (#288). The object is still in s.zones —
			// nothing prunes it — so a re-adopt lands on the early return above. But a zone the coordinator
			// has handed BACK is not handed off any more, and until this ran, its renewal loop
			// returned on its first tick (zoneHandedOff) and its lease quietly lapsed while this shard kept
			// hosting and serving it. ShardForZone then resolves nobody and any shard may ClaimZone it: a
			// second host for a zone we are still writing to.
			//
			// SECURITY (#262/#315). A replayed AdoptZone used to reach here too, which made this call more than
			// a no-op: it planted a lease renewer for a zone the attacker never owned. AdoptZone's signature
			// now binds the zone's monotonic lease GENERATION, so a captured request is refused at the door
			// once its handover lands — the replay never gets this far. renewZoneLease still bounds the
			// pre-confirm adopting state (adoptConfirmDeadline), which covers the case no signature can:
			// an HONEST adoption whose flip never lands because the source died mid-drain (#327).
			s.clearZoneHandedOff(id)
			s.startZoneRenewal(runCtx, id)
			// Defensive (#269): a re-adopted zone's actor never stopped (a retained zone keeps serving), so a
			// coalescing flag armed before the handoff was already drained and disarmed — a no-op today. Reset
			// it anyway so the flag can never latch if zone-teardown ordering ever changes.
			z.commsRepublishArmed.Store(false)
			slog.Info("re-adopted a previously handed-off zone; lease renewal restarted", "zone", id)
		}
		return z, nil // already hosting it — idempotent
	}
	if s.runCtx == nil || s.runWG == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("HostZone %q: shard not running", id)
	}
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("HostZone %q: shard shutting down", id)
	}
	if s.content == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("HostZone %q: shard has no retained content to build from", id)
	}
	s.mu.Unlock()

	// Build OUTSIDE mu: buildZone spawns room singletons + runs resets, which shouldn't block routing reads.
	z := newZone(id)
	z.protos = s.protos
	z.buildZone(s.content)

	// Seed the zone's scope replica from the authoritative store BEFORE adoptLocked below publishes it into
	// s.zones (#280). Once it is in s.zones a world-scope delta can be posted to its inbox via zonesList, and
	// applyScopeSeed is a full-map REPLACE — so a seed arriving after a delta would clobber newer state with
	// the snapshot. Seeding here means the inbox itself enforces the order: seed first, deltas after.
	//
	// Without this, a drain-adopted zone starts with an EMPTY replica and only learns each world/region key
	// when it is next broadcast — reintroducing the #44 symptom (a sticky "war active" flag reading false)
	// at exactly the failover this subsystem exists to survive. A failed snapshot read degrades to "no seed",
	// the same as at boot. If we lose the adoption race below, this zone is discarded unrun and the seed with it.
	s.scopes.seedZone(ctx, z)

	// Register + launch atomically under mu, re-checking the guards a concurrent build/shutdown may have
	// tripped. Doing runWG.Add here under mu (gated on !closed) is what makes it impossible for the Add to
	// race Run's wg.Wait: Run sets closed=true under mu before Wait, so every surviving Add happens-before it.
	s.mu.Lock()
	if existing := s.zones[id]; existing != nil {
		s.mu.Unlock()
		return existing, nil // lost a concurrent HostZone race; discard our freshly-built (never-run) zone
	}
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("HostZone %q: shard shutting down", id)
	}
	s.adoptLocked(id, z) // registers in s.zones + wires handoff/saver/defs
	runCtx := s.runCtx
	s.runWG.Add(1)
	// Arm the actor's cancel + done in the SAME lock hold that published the zone, so no UnhostZone can ever
	// observe a zone in s.zones with no way to stop it (see armZoneActorLocked). registerZone below does a
	// blocking bus subscribe, which is exactly the window that would otherwise be wide open.
	zctx, zcancel, zdone := s.armZoneActorLocked(runCtx, id)
	s.mu.Unlock()

	// registerZone stamps z.scopes.regionID. It MUST stay above the `go z.Run` below: the region seed already
	// sitting in the inbox is DROPPED by applyScopeSeed if the stamp has not landed by the time the loop
	// consumes it, and that failure is silent (an empty region replica, no error).
	//
	// Bring the zone into scope replication (stamp its region + subscribe) BEFORE its actor starts, so a
	// region/world scope delta reaches a runtime-hosted zone and the regionID stamp isn't a race with a
	// region:get on the zone goroutine (16.4a defer). No-op when there's no scoped bus / the zone has no region.
	s.scopes.registerZone(z)

	go func() {
		defer s.runWG.Done()
		defer s.disarmZoneActor(id, zdone)
		defer zcancel()
		z.Run(zctx)
	}()
	// This is a RUNTIME adoption: it renews as an ADOPTING zone (adopted=true), so if the source's
	// HandoverZone flip never lands, its renewer un-adopts it after adoptConfirmDeadline (#327) instead of
	// leaving a permanent zombie. Boot zones never reach here, so they never un-adopt.
	s.startZoneRenewalAdopting(runCtx, id, true) // renew the adopted zone's lease (16.4b; no-op when leasing is off)
	slog.Info("hosting zone at runtime", "zone", id, "shard", s.addr)
	return z, nil
}

// indexToken records that zone z holds a pending player bound by token, so a Play
// attach or Handoff.Prepare on this shard can route the bind to the right zone.
func (s *Shard) indexToken(token string, z *Zone) {
	s.mu.Lock()
	s.tokenIndex[token] = z
	s.mu.Unlock()
}

// zoneForToken returns the zone holding the pending player for token, if any.
func (s *Shard) zoneForToken(token string) *Zone {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenIndex[token]
}

// dropToken removes a token index entry once its handoff resolves (bound/aborted/
// expired), so the map does not grow without bound.
func (s *Shard) dropToken(token string) {
	s.mu.Lock()
	delete(s.tokenIndex, token)
	s.mu.Unlock()
}

// indexResident records that zone z now holds character's session (#321). Called from setPlayer, so it
// mirrors z.players exactly. An unconditional set is correct: a character is resident in at most one zone on
// a shard at a time, and during an intra-shard transfer the destination's setPlayer is the newest truth.
func (s *Shard) indexResident(character string, z *Zone) {
	s.residentMu.Lock()
	s.residentZone[character] = z
	s.residentMu.Unlock()
}

// unindexResident drops character's residency, but ONLY if it still points to z (#321). On the plain A->B
// intra-shard walk the ordering is actually fixed — transferOut's delPlayer strictly precedes the posting of
// transferInMsg, so the source unindex happens-before the destination setPlayer — and a blind delete would
// be correct there. The only-if-mine guard is defense-in-depth: it makes the index robust to ANY future call
// site (or a re-ordering) where a source delete could otherwise erase a destination's fresh entry, matching
// the only-if-mine discipline the placement tombstone fence uses. Cheap, and it removes a latent footgun.
func (s *Shard) unindexResident(character string, z *Zone) {
	s.residentMu.Lock()
	if s.residentZone[character] == z {
		delete(s.residentZone, character)
	}
	s.residentMu.Unlock()
}

// zoneForResidentCharacter returns the zone on THIS shard currently holding character's session, or nil.
// Consulted by the Play attach BEFORE the durable zone_ref so a link-dead resume routes to the zone that
// actually holds the detached session, never the zone the durable record has gone stale naming (#321). Read
// on the stream goroutine; safe against the zone-goroutine writers via residentMu.
func (s *Shard) zoneForResidentCharacter(character string) *Zone {
	s.residentMu.Lock()
	defer s.residentMu.Unlock()
	return s.residentZone[character]
}

// GRPCDialer dials peer shards over plaintext gRPC, caching one connection per
// address. Used by the world binary; tests inject their own bufconn dialer.
func GRPCDialer() HandoffDialer {
	var mu sync.Mutex
	conns := map[string]*grpc.ClientConn{}
	return func(addr string) (handoffv1.HandoffClient, error) {
		mu.Lock()
		defer mu.Unlock()
		if conns[addr] == nil {
			cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return nil, err
			}
			conns[addr] = cc
		}
		return handoffv1.NewHandoffClient(conns[addr]), nil
	}
}

// Zone returns the shard's home zone. Convenience for single-zone callers/tests.
func (s *Shard) Zone() *Zone { return s.zoneByID(s.home) }

// ZoneByID returns the hosted zone with the given id, or nil.
func (s *Shard) ZoneByID(id string) *Zone { return s.zoneByID(id) }

// Run starts every hosted zone's actor loop on its own goroutine and the shard's single async
// saver drainer, then blocks until ctx is cancelled. One goroutine per zone preserves
// single-writer; the saver drainer is the ONE place character I/O runs, never on a zone
// goroutine. A disabled saver's run returns immediately (no goroutine cost for a storeless boot).
func (s *Shard) Run(ctx context.Context) {
	go s.saver.run(ctx)          // off-zone-goroutine character writer (no-op if disabled)
	go s.auditor.run(ctx)        // off-zone-goroutine audit-trail writer (#350; no-op if disabled)
	go s.runPlacementWriter(ctx) // off-zone-goroutine directory placement writer (#320; no-op without a directory)
	go s.presence.run(ctx)       // off-zone-goroutine cross-shard `who` heartbeat (no-op if disabled)
	go s.comms.publishLoop(ctx)  // off-zone-goroutine single-writer durable-tell publisher (8.5)
	// Idle-instance reaper (#411, instance.go). Its OWN shard-level ticker, deliberately NOT the lease-renewal
	// tick: instances take no lease, so no renewal goroutine exists for them to ride on. It only reads atomics
	// and calls UnhostZone, so it never touches zone state.
	go s.runInstanceReaper(ctx)
	// Hot reload runs on the bus's own subscription goroutine (no shard goroutine of its own);
	// just tear the subscription down when the shard stops so a restart doesn't double-subscribe.
	if s.reloader != nil {
		defer s.reloader.stop()
	}
	var wg sync.WaitGroup
	// Capture the run ctx + wg so HostZone (16.4a) can launch a runtime-added zone onto this same
	// lifetime and have the Wait below cover it. Snapshot the boot zones under the same lock so a
	// HostZone racing startup is either in the snapshot or adds itself after runWG is visible.
	type bootZone struct {
		z      *Zone
		ctx    context.Context
		cancel context.CancelFunc
		done   chan struct{}
	}
	s.mu.Lock()
	s.runCtx = ctx
	s.runWG = &wg
	boot := make([]bootZone, 0, len(s.zones))
	for _, z := range s.zones {
		// Arm each boot zone's actor under the SAME lock hold that publishes runCtx: the zones are already in
		// s.zones (they were built at construction), so an UnhostZone racing startup must never find one with
		// no cancel. Same invariant HostZone maintains.
		zctx, zcancel, zdone := s.armZoneActorLocked(ctx, z.id)
		boot = append(boot, bootZone{z: z, ctx: zctx, cancel: zcancel, done: zdone})
	}
	s.mu.Unlock()
	for _, b := range boot {
		wg.Add(1)
		go func(b bootZone) {
			defer wg.Done()
			defer s.disarmZoneActor(b.z.id, b.done)
			defer b.cancel()
			b.z.Run(b.ctx)
		}(b)
		s.startZoneRenewal(ctx, b.z.id) // shard-owned lease renewal (16.4b; no-op when leasing is off)
	}
	// Scope replication (10.3b + #44): started AFTER the zone actor loops launch above, so seedFromSnapshot's
	// BLOCKING seed posts always have a live drainer (seeding before launch could wedge boot if an inbox filled
	// — e.g. mass inbound handoff Prepare during a failover — before its loop drained it). Seed FIRST, then
	// subscribe: the seed is enqueued ahead of any live delta (the subscriptions don't exist until start()), so
	// replicas are seeded before deltas build on them. Runs on the bus's subscription goroutines; stop at exit.
	if s.scopes != nil {
		s.scopes.seedFromSnapshot()
		s.scopes.start()
		defer s.scopes.stop()
		go s.scopes.signalLoop(ctx) // off-zone-goroutine signal-up publisher (durable; 10.3c)
	}
	// Block on the shard's lifetime, THEN wait for every zone (incl. runtime-added ones) to finish.
	// A standby that won no zones has an empty wg; without the ctx wait it would exit Run immediately
	// and never be able to host a drained peer's zone. Each z.Run returns only on ctx cancel, so the
	// old wg.Wait()-only form is equivalent for a shard that booted with zones.
	<-ctx.Done()
	// Close the door under mu BEFORE Wait: a HostZone that already did its wg.Add under mu is ordered
	// ahead of us (its zone is in the wg and Wait covers it); any later HostZone sees closed and refuses,
	// so no Add can race this Wait even when the counter has just reached zero.
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	wg.Wait()
}

// Drain enqueues an immediate durable flush of every live, persisted player on every hosted zone
// — the shard-drain flush point (docs/PERSISTENCE.md §6, rolling redeploy). It posts a leaveMsg-
// free flush request to each zone goroutine (so the dump stays single-writer) and returns at once;
// the saver writes the snapshots off-goroutine. Phase 4 BUILDS this hook; the placement controller
// that TRIGGERS a drain (graceful handoff of every player before shutdown) is Phase 10. A no-op on
// a disabled (ephemeral) saver.
func (s *Shard) Drain(ctx context.Context) (dropped int) {
	if s.saver == nil || !s.saver.enabled() {
		return 0
	}
	zones := s.zonesList()
	type pending struct {
		z    *Zone
		done chan int
	}
	waits := make([]pending, 0, len(zones))
	for _, z := range zones {
		done := make(chan int, 1)
		select {
		case z.inbox <- drainFlushMsg{ctx: ctx, done: done}:
			waits = append(waits, pending{z: z, done: done})
		case <-z.dead:
			// Torn down between the snapshot above and now — the instance reaper keeps sweeping during a drain
			// (#411). The inbox is still buffered, so a raw send would SUCCEED against a stopped actor and the
			// wait below would then block until ctx expires (the whole shutdown deadline). z.post watches this
			// channel for exactly this reason; a raw send has to do it by hand. Nothing is owed: UnhostZone only
			// removes a quiescent zone.
		case <-ctx.Done():
			return dropped // a zone whose loop has stopped consuming must not block shutdown
		}
	}
	// WAIT for each zone to have DUMPED its players onto the saver queue (#282). Posting alone proves
	// nothing: a saver barrier taken immediately afterwards could drain an empty queue and report success
	// while the zones' saves were still sitting unposted in their inboxes.
	for _, w := range waits {
		select {
		case n := <-w.done:
			dropped += n
		case <-w.z.dead:
			// Torn down after we posted; nothing will ever answer. Same reasoning as the post above.
		case <-ctx.Done():
			return dropped
		}
	}
	return dropped
}

// FlushSaver blocks until every save enqueued so far has been written durably, or ctx expires, or the
// saver's drainer is already gone (#282).
//
// Call it between the drain and the shutdown that cancels the world context. The saver's drainer returns on
// ctx cancel WITHOUT draining its buffer, so without this barrier the reclaimed stragglers — enqueued last,
// microseconds before the cancel — lose their flush and come back up to a cadence-tick-stale state.
//
// It watches the shard's run context as well as the caller's: a lease fence can cancel worldCtx mid-drain,
// killing the drainer, and without that watch the barrier's blocking send would stall for the caller's whole
// timeout on a shutdown path that has nothing left to flush.
//
// A non-zero `dropped` from Drain means some saves never reached the queue at all; this barrier says nothing
// about those. The caller must report them.
func (s *Shard) FlushSaver(ctx context.Context) error {
	if s.saver == nil {
		return nil
	}
	s.mu.Lock()
	runCtx := s.runCtx
	s.mu.Unlock()
	var dead <-chan struct{}
	if runCtx != nil {
		dead = runCtx.Done()
	}
	return s.saver.flush(ctx, dead)
}

// Register installs the gRPC Play and Handoff services on the given server. The Play
// service routes a fresh login to the shard's home zone and a handoff bind to whichever
// hosted zone holds the matching pending player; Handoff routes Prepare by target zone.
func (s *Shard) Register(gs *grpc.Server) {
	registerPlay(gs, s)
	registerHandoff(gs, s)
}

// beginHandoff is the source side of a cross-shard move (PROTOCOL.md §3, §5). It runs
// asynchronously (never on the zone goroutine, so it may do blocking directory I/O):
// it resolves the destination shard, claims ownership in the directory with a bumped
// epoch (compare-and-set, so a stale handoff can't win), and posts a redirectMsg back
// to the zone to send the player a Redirect. On any failure it posts handoffFailMsg
// so the zone thaws the (otherwise stuck) frozen player.
//
// Step 4 adds the Handoff.Prepare RPC to the destination here, between the directory
// claim and the redirect; step 5 wires the gate to act on the Redirect.
func (s *Shard) beginHandoff(src *Zone, snap *handoffv1.PlayerSnapshot, destZone, destRoom string, epoch uint64) {
	go func() {
		// Bound concurrent in-flight handoffs so a drain's whole-zone fan-out doesn't stampede the target
		// (16.4b review). Acquired BEFORE the ctx below so the RPC timeout covers only the conversation, not
		// the queue wait; a long drain queue still drains well within the freeze backstop. nil (a raw
		// test-built shard) => unbounded, the pre-change behavior.
		if s.handoffSem != nil {
			s.handoffSem <- struct{}{}
			defer func() { <-s.handoffSem }()
		}
		// Bound the whole conversation: a hung Prepare to a restarting/draining destination
		// must not strand the frozen player forever. On deadline the fail(...) path below
		// thaws + restores them. Runs off the zone goroutine, so blocking here is safe.
		ctx, cancel := context.WithTimeout(context.Background(), handoffRPCTimeout)
		defer cancel()
		character := snap.GetCharacterId()
		newEpoch := epoch + 1
		log := slog.With("component", "handoff", "player", character, "dest_zone", destZone)

		fail := func(reason string) { src.post(handoffFailMsg{id: character, reason: reason}) }

		// Resolve the destination in two hops: which SHARD owns the zone, then where that
		// shard is reachable. The zone exit named only a logical zone ("darkwood"); the
		// directory turns that into a shard id and the shard id into a dial endpoint, so
		// the binding survives the owning shard moving hosts.
		destShardID, err := s.dir.ShardForZone(ctx, destZone)
		if err != nil {
			log.Warn("destination zone not in directory", "err", err)
			fail("destination unreachable")
			return
		}
		addr, err := s.dir.EndpointForShard(ctx, destShardID)
		if err != nil {
			log.Warn("owning shard has no live endpoint", "dest_shard", destShardID, "err", err)
			fail("destination unreachable")
			return
		}
		log.Debug("resolved destination", "dest_shard", destShardID, "endpoint", addr)
		client, err := s.peers(addr)
		if err != nil {
			log.Warn("cannot reach destination shard", "dest_shard", destShardID, "addr", addr, "err", err)
			fail("destination unreachable")
			return
		}

		// Prepare the destination: it rehydrates the player as a pending entity. Sign the request so a
		// key-enforcing destination accepts it (docs/REMAINING.md §1); an unconfigured source signs nil,
		// which only a keyless destination accepts.
		prepReq := &handoffv1.PrepareRequest{
			SessionId:    character, // session-id stand-in (deterministic token, §5)
			Snapshot:     snap,
			TargetZoneId: destZone,
			TargetRoomId: destRoom,
			Epoch:        newEpoch,
			FromShardId:  s.addr,
		}
		prepReq.SnapshotSig = signSnapshot(s.handoffSignKey, prepReq)
		resp, err := client.Prepare(ctx, prepReq)
		if err != nil {
			log.Warn("prepare rejected by destination", "err", err)
			fail("destination rejected the handoff")
			return
		}

		// Prepare succeeded: claim ownership in the directory (epoch CAS), recording the
		// destination SHARD ID (not its address) AND the destination ZONE (#320 — the zone is what a
		// later reconnect routes by, because a shard id goes stale the moment that zone is rebalanced).
		// On conflict, roll back the destination's pending entity.
		if ok, err := s.dir.SetPlayerShard(ctx, character, destShardID, destZone, newEpoch); err != nil || !ok {
			log.Warn("directory claim failed after prepare", "ok", ok, "err", err)
			// The rollback Abort needs a FRESH context: ctx may already be at/past its
			// deadline (e.g. SetPlayerShard was what timed out), which would cancel the
			// Abort before it could discard the destination's pending entity.
			abortCtx, ac := context.WithTimeout(context.Background(), 2*time.Second)
			// Sign the rollback so a key-enforcing destination accepts it (#314); an unconfigured source signs
			// nil, which only a keyless destination accepts — the same seam as the Prepare signature above.
			// Bind the rollback to the destination SHARD it is addressed to (#314) so a signature captured off
			// the plaintext wire cannot be replayed against a concurrent same-token pending at another shard.
			abortReq := &handoffv1.AbortRequest{
				HandoffToken: resp.GetHandoffToken(),
				Reason:       "directory conflict",
				ToShardId:    destShardID,
			}
			abortReq.Sig = signHandoffToken(s.handoffSignKey, handoffAbortDomain, abortReq.GetHandoffToken(), destShardID)
			if _, aerr := client.Abort(abortCtx, abortReq); aerr != nil {
				// Best-effort: the destination's pending self-heals via pendingTTL (60s) regardless. But log the
				// error so the rejection is not silent — in particular a PermissionDenied here means a version/key
				// skew (#314): a new keyed destination rejected an unsigned rollback from an old-code source
				// mid-rolling-upgrade. That is an actionable operator signal, not the usual network/deadline miss.
				if status.Code(aerr) == codes.PermissionDenied {
					log.Warn("rollback Abort rejected by destination (auth) — pending will self-heal via pendingTTL; "+
						"check handoff key/version skew during a rolling upgrade", "err", aerr)
				} else {
					log.Debug("rollback Abort did not reach destination; pending self-heals via pendingTTL", "err", aerr)
				}
			}
			ac()
			fail("ownership conflict")
			return
		}

		// Commit-marker FIRST: the directory CAS just committed, so the both-own truth has
		// flipped. Post handedOffMsg ahead of redirectMsg (and ahead of the log line below) so
		// the freeze-reaper's success discriminator is set at the CAS-commit point, not one
		// message later at Redirect-frame send. This closes the narrow both-own window where a
		// freezeExpire firing in the gap would thaw a player whose handoff already succeeded —
		// correctness no longer depends on freezeTTL >> handoffRPCTimeout.
		src.post(handedOffMsg{id: character})

		log.Debug("prepared + ownership claimed; redirecting", "dest_addr", resp.GetTargetShardAddr(), "epoch", newEpoch)
		src.post(redirectMsg{
			id:         character,
			targetAddr: resp.GetTargetShardAddr(),
			token:      resp.GetHandoffToken(),
			epoch:      newEpoch,
		})
	}()
}

// defineRoom authors one room PROTOTYPE (docs/MUDLIB.md §5) into the shard's cache: an
// immutable template carrying the room's display name (short), description (long), and a
// Room component template with an exits map. The exits map is part of the immutable
// template — it is populated HERE at authoring time and never mutated afterward (an
// instance that re-routes an exit COWs it via mutableRoom). Returns the prototype so the
// caller can wire its exits before any instance is spawned.
func defineRoom(c *protoCache, ref ProtoRef, name, desc string) *Prototype {
	comps := componentSet{}
	r := &Room{exits: map[string]ProtoRef{}}
	comps[reflect.TypeFor[*Room]()] = r
	return c.define(ref, nil, name, desc, comps)
}

// spawnRoom instantiates a room prototype into zone z and registers it in z.rooms (MUDLIB
// §4: a room is an Entity with a Room component and no location, its container the zone).
// Rooms are singletons — one instance per ref — so they share the prototype's immutable
// exits/name/desc by reference until something COWs them (nothing does in the demo).
func (z *Zone) spawnRoom(ref ProtoRef) *Entity {
	e := z.spawn(ref)
	z.rooms[ref] = e
	return e
}

// resyncRoom re-links a live singleton room entity to its freshly hot-reloaded prototype, IN PLACE, on
// the zone goroutine (#53/#191). It closes a gap the per-ref applier leaves: a room is spawned ONCE at
// boot and never re-spawned, so the applier's "next spawn uses the edit" semantics never reach it — a
// builder editing a room's description/exits/flags and running `reload` would see nothing. Here the
// singleton's short/long/keywords and its Room component (exits/sector/coord/flags) are re-pointed to the
// new prototype, so the edit takes effect on the live room the builder is standing in.
//
// Occupants are untouched — this only refreshes the room's OWN authored fields, never its containment —
// so no player is moved. Rooms are runtime-immutable (nothing COWs a room's component), so the live entity
// still aliases the prototype's component; re-pointing to the new prototype's is the same aliasing spawn
// establishes.
//
// Three cases by (live entity, new prototype):
//   - UPDATE (both present): re-point the singleton's fields, below — the common edit.
//   - ADD (no live entity, prototype present): a room NEW to this zone's graph. Spawn it live so exits INTO
//     it (resolved by ProtoRef at move time) resolve. Guarded to rooms THIS zone owns (ownsZoneRef over the
//     ref's zone prefix), since the invalidation fans out to every hosted zone. New rooms have no occupants,
//     so this
//     is safe; zone resets are NOT re-run (a bare room). CAVEAT: boot assigns a room to a zone by its
//     content `rooms:` LIST membership (build.go buildZone), not by ref prefix — the prefix==zone rule is
//     the same one cross-zone exit ROUTING already relies on (parseRef), so it is de-facto load-bearing, but
//     it is NOT enforced by the loader. A room authored into zone X with a ref prefixed for zone Y would
//     boot into X yet be skipped by this ADD (its cross-zone exits would already misroute too). A load-time
//     content-lint enforcing prefix==zone is the proper guard — tracked as a follow-up (#194).
//   - REMOVE (prototype deleted): NOT driven here. A deleted room's ref is absent from the content, so
//     PublishPack never emits a per-ref invalidation for it — this path never sees a deletion. Room teardown
//     is instead driven by the zone-SHAPE reconcile (reconcileZone), which diffs the whole zone's live
//     room set against the reloaded content. The p==nil case below is a defensive no-op.
func (z *Zone) resyncRoom(ref ProtoRef) {
	e := z.rooms[ref]
	p := z.protos.get(ref)
	if e == nil {
		// ADD a brand-new room this zone owns; ignore a new room that belongs to another hosted zone or a
		// deletion of a non-hosted ref (both p==nil and a foreign zone fall through to no-op). NOTE: when the
		// reconcile DESIRES this ref but p==nil, the room's per-ref prototype swap was lost upstream (a dropped
		// Lua-invalidation cannot cause it — the cache swap is a synchronous subscriber-goroutine write ordered
		// before the reconcile post — but a LoadDefinition/build failure for that ref would). The ADD then
		// silently no-ops and the desired room is simply not spawned until the next full reload re-converges.
		if p != nil {
			// ownsZoneRef so the gate keeps meaning "a room THIS zone owns" once ids and content refs can
			// differ (#72). Note this gate is not what decides whether an INSTANCE takes a hot reload —
			// reconciles are routed by actor id at reload.go's `z.id != inv.Ref` skip, so an instance never
			// receives its template's invalidation in the first place. That skip is the instance-freeze
			// mechanism; this is only the ownership test, and it stays correct either way.
			if zoneOf, _ := parseRef(ref); z.ownsZoneRef(zoneOf) {
				z.spawnRoom(ref)
				z.log.Debug("hot reload: new room added to live zone", "ref", ref)
			}
		}
		return
	}
	if p == nil {
		return // definition deleted — teardown is driven by reconcileZone, not the per-ref path
	}
	// Re-point the entity at the new prototype AND its components together, exactly as spawn establishes the
	// aliasing. Keeping e.prototype in lock-step with e.comps matters for COW: mutableComponent decides an
	// instance already owns a component by comparing it against e.prototype.comps[T] — if e.prototype lagged
	// the new component, a future live room mutation would be handed the SHARED prototype component to write
	// in place (cross-instance corruption). No room is COW'd today, but keeping the invariant costs nothing.
	e.prototype = p
	e.keywords = p.keywords
	e.short = p.short
	e.long = p.long
	if rc, ok := p.comps[reflect.TypeFor[*Room]()]; ok {
		e.comps[reflect.TypeFor[*Room]()] = rc
		e.room = rc.(*Room)
	}
	z.log.Debug("hot reload: live room re-synced from new prototype", "ref", ref)
}

// removeRoom tears a room that content DELETED out of the live zone (#191), the risky third case the zone-
// shape reconcile drives when a room ref that was live is no longer in the reloaded content. It runs on the
// zone goroutine (single-writer over z.rooms / z.players), so nothing races the teardown. It:
//   - RE-PLACES player occupants to the zone start room via z.relocate (they get the start room's look +
//     GMCP, never stranded looking at a gone room). A frozen session (mid cross-shard handoff) is skipped —
//     its entity is being transferred and the destination will place it.
//   - DESPAWNS ephemeral mobs and items in the room (never relocates them — that would dump every deleted
//     zone's population into the start room).
//   - drops the singleton from z.rooms, its Lua script state, and its prototype from the cache.
//
// It is IDEMPOTENT: a ref that is not (or no longer) live is a clean no-op, so a re-delivered reconcile or a
// removal racing an earlier one is harmless. It REFUSES to remove the live start room — deleting the login
// room out from under players is a content error; the reconcile applies a start_room change BEFORE removals,
// so repointing start_room first is what makes the old start room removable. Dangling exits from surviving
// rooms INTO the removed room are left as-is: they fail closed at move time (z.rooms lookup miss), and the
// exits map is the immutable shared prototype's — mutating a neighbour's exits to "clean up" would COW-
// corrupt a shared component. A load-time lint for that is a tracked follow-up.
func (z *Zone) removeRoom(ref ProtoRef) {
	e := z.rooms[ref]
	if e == nil {
		return // never hosted here, or already removed — idempotent no-op
	}
	if ref == z.startRoom {
		z.log.Warn("hot reload: refusing to remove the live zone start room; repoint start_room first",
			"zone", z.id, "room", ref)
		return
	}
	dest := z.resolveRoom(z.startRoom)
	if dest == nil || dest == e {
		// No safe fallback (start room unresolvable — e.g. the whole zone was deleted). Leave the room
		// rather than strand occupants nowhere; a re-run with a valid start room converges.
		z.log.Warn("hot reload: no safe fallback room; skipping room removal", "zone", z.id, "room", ref)
		return
	}
	// Snapshot occupants first: relocate/despawn Move()s them, which mutates e.contents underneath a live
	// range (the same trap the death.go corpse loop guards against).
	occupants := make([]*Entity, len(e.contents))
	copy(occupants, e.contents)
	for _, occ := range occupants {
		if occ == nil {
			continue
		}
		if s, ok := sessionOf(occ); ok {
			// Defensive: a frozen session is mid cross-shard handoff. In the normal handoff path its entity
			// is already Move()d to nil at freeze (drain.go), so it is NOT in this snapshot — but guard anyway
			// so a future frozen-but-still-placed state can never be yanked out from under an in-flight
			// transfer (that would race the destination's attach).
			if s.frozen {
				continue
			}
			z.relocate(s, dest)
			continue
		}
		z.despawnRoomContent(occ) // an ephemeral mob or item: destroy, don't relocate
	}
	delete(z.rooms, ref)
	if z.lua != nil {
		z.lua.dropEntityScript(e.rid) // the room's own trigger state, if it carried a script
	}
	// Drop the prototype (nil == remove). NOTE (writer convention): protoCache is the SHARD-wide cache and
	// its documented runtime writer is the reload-applier (subscriber) goroutine; here the ZONE goroutine
	// writes it because in the reconcile model the zone is the authoritative knower of a content-DELETION
	// (no invalidation names a deleted ref — PublishPack only emits present refs), and the applier cannot
	// know. protoCache.reload is a mutex-guarded copy-and-swap, so this concurrent writer is memory-safe. The
	// zone goroutine is the SOLE driver of a content-deletion (reconcileZone → removeRoom), so no
	// applier-goroutine writer races it for a deleted ref.
	z.protos.reload(ref, nil)
	z.log.Debug("hot reload: live room removed", "zone", z.id, "room", ref)
}

// reconcileZone is the single authoritative zone-SHAPE path (#191): given the reloaded content's DESIRED
// state (the zone's full room-ref set + start room + a monotonic version, carried on the KindZone
// invalidation), it converges the live room graph — spawn/resync every desired room (ADD + UPDATE off the
// already-swapped prototype cache) and tear down every live room the content no longer defines (removeRoom
// — re-place occupants, despawn ephemera). It replaces the per-ref room applier's resync (the KindRoom
// path no longer resyncs, reload.go); a deleted room's ref is absent from the content so no per-ref
// invalidation names it, and only a whole-zone diff can see the deletion. Runs on the zone goroutine
// (single-writer over z.rooms / z.startRoom / z.players), driven by a reconcileZoneMsg.
//
// VERSION GUARD: a reconcile whose version is ≤ the newest already applied is dropped — a racing reload's
// stale snapshot must not reorder ahead of a newer one (last-writer-wins by version, not by arrival). A
// zero version (a hand-published test invalidation) is never guarded.
//
// start_room is applied FIRST (before any removal) because removeRoom REFUSES to tear down the live start
// room — it is the evacuation fallback — so an edit that both moves the start room and deletes the old one
// only works if the repoint precedes the removals. The repoint is guarded to a room the content actually
// defines (desired[startRoom]) so a malformed edit can never point new logins at an undefined room; a start
// room not yet live (its ADD still in flight/dropped) leaves removeRoom without a fallback, which it handles
// by skipping and converging on a re-run.
func (z *Zone) reconcileZone(m reconcileZoneMsg) {
	if len(m.rooms) == 0 {
		// An empty desired set would make EVERY live room a straggler — a mass teardown from an almost-
		// certainly malformed or degenerate payload. PublishPack always carries a real zone's full room set,
		// and a zone genuinely emptied of content emits NO KindZone at all (retiring a whole zone is a
		// rolling-reboot op, not a hot reload — removeRoom's start-room refusal would leave a stranded husk
		// anyway). So treat an empty payload as a no-op: skip without even advancing the version cursor,
		// rather than wipe the zone down to its start room.
		z.log.Warn("hot reload: zone-shape reconcile with empty room set; skipping (won't mass-teardown a live zone)",
			"zone", z.id, "version", m.version)
		return
	}
	if m.version != 0 {
		if m.version <= z.lastReconciledPackVer {
			z.log.Debug("hot reload: dropping stale zone-shape reconcile", "zone", z.id,
				"version", m.version, "applied", z.lastReconciledPackVer)
			return
		}
		z.lastReconciledPackVer = m.version
	}
	desired := make(map[ProtoRef]bool, len(m.rooms))
	for _, r := range m.rooms {
		desired[ProtoRef(r)] = true
	}
	if m.startRoom != "" && desired[m.startRoom] {
		z.startRoom = m.startRoom
	}
	// ADD/UPDATE: converge every desired room off the already-swapped cache. resyncRoom is 3-way — it
	// spawns a room NEW to the graph (guarded internally to refs this zone owns by prefix), re-points a live
	// room's authored fields from the new prototype (the common UPDATE), and no-ops a ref whose prototype is
	// absent. Ranging `desired` (a different map) while resyncRoom's ADD writes z.rooms is safe.
	for ref := range desired {
		z.resyncRoom(ref)
	}
	// REMOVE: any live room no longer desired (z.rooms holds only this zone's rooms). Snapshot the stragglers
	// before removing — removeRoom deletes from z.rooms, which would mutate the map underneath a live range.
	var stragglers []ProtoRef
	for ref := range z.rooms {
		if !desired[ref] {
			stragglers = append(stragglers, ref)
		}
	}
	for _, ref := range stragglers {
		z.removeRoom(ref)
	}
	z.log.Debug("hot reload: zone-shape reconciled", "zone", z.id,
		"desired", len(desired), "removed", len(stragglers), "version", m.version)
}

// relocate forcibly evacuates a live player to dest (#191 room removal) — a directionless move mirroring the
// ARRIVAL tail of move(): disengage any combat, announce the departure/arrival, Move, then z.lookRoom (the
// single chokepoint that re-emits the room text AND the change-detected GMCP Room.Info, so the player is not
// left staring at a room that is gone), room affects, aggro, and a prompt. It deliberately does NOT fire the
// OnLeaveRoom checkpoint (a forced evacuation of a deleted room grants no opportunity attacks) nor the FROM
// room's `leave` trigger (that room is being destroyed). Zone goroutine only.
func (z *Zone) relocate(s *session, dest *Entity) {
	if s == nil || s.entity == nil || dest == nil {
		return
	}
	z.disengage(s.entity) // sever any combat link before the yank — no fighting pointer survives the move
	z.actConceal("$n vanishes as the area shifts.", s.entity, ToRoom)
	s.send(textFrame("The ground shifts and you are swept somewhere safe."))
	Move(s.entity, dest)
	z.actConceal("$n arrives.", s.entity, ToRoom)
	z.lookRoom(s)
	applyRoomAffectsTo(s.entity) // room-scoped affects land on the arrival, as a normal move does
	z.aggroOnEntry(s.entity, dest)
	z.sendPrompt(s)
}

// despawnRoomContent destroys an ephemeral mob or item that was in a removed room (#191). It severs combat
// and threat links (a mob fighting a relocated player, a threat-table entry) so no stale pointer survives,
// detaches from the world tree (Move to nil, which also unequips a worn item), and drops any Lua trigger /
// spawn-census state so a repop-on-timer zone does not leak an entityScript per destroyed mob. This is a
// content-deletion despawn, NOT a death — no corpse, loot, or on-kill hook fires. Zone goroutine only.
func (z *Zone) despawnRoomContent(e *Entity) {
	if e == nil {
		return
	}
	// Recurse into held/contained entities FIRST (a mob's carried inventory, a container's contents) so
	// their Lua spawn-census / entityScript state is dropped too — otherwise a destroyed mob's carried
	// scripted item leaks those map entries. Snapshot first: Move mutates e.contents underneath the range.
	// Containment is a tree (an entity has one location), so the recursion terminates.
	held := make([]*Entity, len(e.contents))
	copy(held, e.contents)
	for _, c := range held {
		z.despawnRoomContent(c)
	}
	if e.living != nil {
		z.disengage(e)
		z.scrubThreat(e)
	}
	Move(e, nil)
	if z.lua != nil {
		z.lua.dropEntityScript(e.rid)
		z.lua.dropLuaSpawn(e.rid)
	}
}

// newDemoZone has moved to build.go: the hand-authored body is GONE (Phase 4.1). It is now a
// thin wrapper that loads the EMBEDDED demo content pack into the shared per-shard cache and
// builds the named zone via the content loader, producing byte-identical prototypes. The
// authoring helpers below (defineRoom/spawnRoom/defineChest) remain as general prototype-
// construction utilities the prototype/container tests use directly.

// defineChest authors the CONTAINER prototype — the COW-arming object (Finding 6). It starts
// CLOSED; open/close flip Container.closed, which is shared with this prototype, so the verbs
// must COW via mutableComponent (cmdOpen/cmdClose). capacity caps how many items it holds.
// Retained as a test authoring helper (container_test's concurrent-COW race builds a bare
// cache from it); the demo world itself is authored in the demo pack (packs/demo.yaml).
func defineChest(c *protoCache) *Prototype {
	comps := componentSet{}
	comps[reflect.TypeFor[*Physical]()] = &Physical{weight: 40, material: "oak"}
	comps[reflect.TypeFor[*Container]()] = &Container{capacity: 10, closed: true}
	return c.define("midgaard:obj:chest",
		[]string{"chest", "oak", "wooden"},
		"a wooden chest",
		"A heavy wooden chest sits here.",
		comps)
}

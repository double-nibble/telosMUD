package world

import (
	"context"
	"log/slog"
	"sync"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/commbus"
)

// comm.go holds the shard-level comms SOURCE plumbing for Phase-8 channels (docs/PHASE8-PLAN.md slice
// 8.3): the RoleWorld commbus handle the world publishes through, a server-held monotonic per-author
// sequence (P8-A3), and a per-author token-bucket rate limiter (P8-A1). They live on the Shard (one
// per process, shared by every hosted zone) because comms is player-scoped and cross-zone — a player
// may speak from any zone this shard hosts, and their author sequence + rate budget must be one stream
// regardless of which zone goroutine handles the line.
//
// # Concurrency
//
// Unlike zone state, these structures ARE reached from MULTIPLE zone goroutines (each hosted zone's
// loop), so — like the Shard's tokenIndex and the saver — they are explicitly mutex-guarded. They hold
// NO zone state (only counters keyed by author id), so guarding them does not weaken the single-writer
// invariant: a zone goroutine asking "what's my next comm seq / am I rate-limited" is a pure function
// of comms-source state, never a read or write of another zone's world data. The commbus handle itself
// is concurrency-safe (it is the same Bus a gate uses); Publish is called off the zone goroutine path
// only in the sense that it does not touch zone state — it runs ON the zone goroutine but only hands a
// value to the bus, which owns its own delivery goroutines.

// commSource is the shard's comms SOURCE state. Always non-nil on a constructed shard; its bus is a
// Disabled RoleWorld no-op until WithComms wires a real one (so a shard with no comms bus publishes to
// nowhere — the never-fatal degradation, byte-identical to a pre-Phase-8 shard).
type commSource struct {
	bus commbus.Bus // RoleWorld handle (the world is the publisher); never nil (Disabled when off)

	// js is the DURABLE-tell transport (Phase 8.5, jetstream.go): the world PublishDurable's a tell here
	// and runs a per-resident durable consumer that drains it. nil until WithTells wires one — a shard
	// with no JetStream handle has tells disabled (the never-fatal degradation), byte-identical to a
	// pre-8.5 shard. Shared by every hosted zone (player-scoped, cross-zone) exactly like bus.
	js commbus.JetStream

	mu  sync.Mutex
	seq map[string]uint64       // author id -> last issued monotonic sequence (P8-A3)
	rl  map[string]*tokenBucket // author id -> per-author rate-limit bucket (P8-A1)

	// rateBurst / rateRefill are this source's per-author token-bucket parameters (P8-A1), seeded from
	// the package defaults at construction. Per-source FIELDS (not the package vars) so a test sets a
	// shard's budget WITHOUT racing the package var against a lingering zone goroutine creating a bucket
	// in another test. Read under mu (rateOK) when a bucket is lazily created.
	rateBurst  int
	rateRefill time.Duration

	// consumers holds the live per-resident durable-tell consumer this shard runs, keyed by player id
	// (Phase 8.5). One per hosted player, started on join/handoff-arrival and stopped on leave/orphan-
	// reap, so a player walking zone->zone WITHIN this shard keeps ONE consumer. Guarded by mu (the
	// consumer goroutines + the zone goroutines both touch it). It holds NO zone state — only a Consumer
	// handle keyed by player id — so guarding it does not weaken single-writer.
	consumers map[string]commbus.Consumer

	// drainPace caps the login-backlog drain rate (P8-A5, set from loginDrainPace at construction). A
	// per-source FIELD (not the package var) so a test sets it WITHOUT racing the package var against a
	// concurrently-starting consumer in another test. Read once per consumer at start.
	drainPace time.Duration

	// pub is the shard's single-writer durable-tell PUBLISHER queue (Phase 8.5): zone goroutines
	// ENQUEUE a tellJob here (FIFO) and ONE background goroutine (publishLoop, started in Shard.Run)
	// resolves + assigns the per-author seq + PublishDurable's IN ORDER. Routing every tell through one
	// ordered worker is what makes per-sender order hold (P8-A3): the seq-assign and the stream append
	// happen on one goroutine, so two quick tells from one sender are appended in send order. A full
	// queue drops the tell with a notice to the sender (a flood is the sender's problem; the bus is not
	// stalled) — the saver/presence backpressure discipline. Buffered so a brief publish-I/O stall does
	// not block a zone goroutine.
	pub chan tellJob
}

// tellJob is one queued durable tell handed from a zone goroutine to the shard publisher (comm.go).
// Everything in it was captured ON the zone goroutine (the engine-set author from the live entity, the
// sanitized body); the publisher only does the off-goroutine resolve + seq-assign + publish.
type tellJob struct {
	authorID   string
	authorName string
	target     string
	body       string
	out        chan *playv1.ServerFrame
	dir        Locator
	log        *slog.Logger
}

// newCommSource builds a disabled comms source (a no-op RoleWorld bus). WithComms swaps in a live bus.
func newCommSource() *commSource {
	return &commSource{
		bus:        commbus.Disabled(commbus.RoleWorld),
		js:         commbus.DisabledJetStream(),
		seq:        map[string]uint64{},
		rl:         map[string]*tokenBucket{},
		rateBurst:  commRateBurst,
		rateRefill: commRateRefill,
		consumers:  map[string]commbus.Consumer{},
		drainPace:  loginDrainPace,
		pub:        make(chan tellJob, tellPublishQueue),
	}
}

// nextSeq returns the next monotonic sequence for author id (P8-A3). A single subject is one ordered
// stream (the broker imposes one publish order), and this per-author counter gives every receiver the
// same per-author order with gap detection. Concurrency-safe (multiple zone goroutines).
func (c *commSource) nextSeq(author string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq[author]++
	return c.seq[author]
}

// rateOK reports whether author id may emit one comms line now, consuming a token (P8-A1). A flood
// drains the author's bucket and rateOK returns false — throttling the SENDER ONLY; the dropped line
// never reaches the bus, so no other player's delivery is affected. Concurrency-safe.
func (c *commSource) rateOK(author string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.rl[author]
	if b == nil {
		b = newTokenBucket(c.rateBurst, c.rateRefill)
		c.rl[author] = b
	}
	return b.take(time.Now())
}

// commRateBurst / commRateRefill tune the per-author comms token bucket (P8-A1): a burst of
// commRateBurst lines, refilling one token every commRateRefill. Generous enough that normal chat never
// trips it, tight enough that a flood is throttled within a line or two. Package vars (not consts) so a
// test can shrink/grow them deterministically.
var (
	commRateBurst  = 5
	commRateRefill = 2 * time.Second
)

// tokenBucket is a minimal monotonic-clock token bucket: capacity tokens, one refilled every `refill`.
// take(now) consumes a token if available (refilling lazily based on elapsed time) and reports success.
// It is NOT goroutine-safe on its own — commSource.rateOK holds c.mu around every take, so the bucket
// is only ever touched under that lock.
type tokenBucket struct {
	capacity int
	refill   time.Duration
	tokens   int
	last     time.Time
}

func newTokenBucket(capacity int, refill time.Duration) *tokenBucket {
	return &tokenBucket{capacity: capacity, refill: refill, tokens: capacity, last: time.Now()}
}

// --- Zone-goroutine accessors into the shard's comms source ----------------------------------

// commsBus returns the RoleWorld commbus handle the channel publish path uses, or nil on a bare zone
// (no shard) — the caller treats nil as comms-unavailable. A shard always has a non-nil bus (Disabled
// when comms are off), so a real run never sees nil here. Read-only; safe from any zone goroutine.
func (z *Zone) commsBus() commbus.Bus {
	if z.shard == nil || z.shard.comms == nil {
		return nil
	}
	return z.shard.comms.bus
}

// commNextSeq returns the server-held monotonic next sequence for author (P8-A3). A bare zone (no
// shard) falls back to a private counter so a standalone unit test still gets monotonic seqs.
func (z *Zone) commNextSeq(author string) uint64 {
	if z.shard == nil || z.shard.comms == nil {
		if z.bareComm == nil {
			z.bareComm = newCommSource()
		}
		return z.bareComm.nextSeq(author)
	}
	return z.shard.comms.nextSeq(author)
}

// commRateOK reports whether author may emit a comms line now, consuming a token (P8-A1). A bare zone
// (no shard) falls back to a private bucket set.
func (z *Zone) commRateOK(author string) bool {
	if z.shard == nil || z.shard.comms == nil {
		if z.bareComm == nil {
			z.bareComm = newCommSource()
		}
		return z.bareComm.rateOK(author)
	}
	return z.shard.comms.rateOK(author)
}

// take refills tokens for the elapsed time (capped at capacity), then consumes one if available.
func (b *tokenBucket) take(now time.Time) bool {
	if b.refill > 0 && now.After(b.last) {
		gained := int(now.Sub(b.last) / b.refill)
		if gained > 0 {
			b.tokens += gained
			if b.tokens > b.capacity {
				b.tokens = b.capacity
			}
			// Advance `last` by the consumed whole intervals so fractional time is not lost.
			b.last = b.last.Add(time.Duration(gained) * b.refill)
		}
	}
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

// startConsumer starts (or no-ops if already running) the per-resident durable-tell consumer for
// playerID (Phase 8.5). route returns the zone CURRENTLY hosting the player (it Loads the session's
// currentZone atomic pointer, which every move/handoff updates), so the drained tell is always posted
// to the live zone even as the player walks zone->zone within the shard — without the consumer
// touching zone-owned state from its bus goroutine (the only cross-goroutine read is the atomic
// pointer). Each message is posted via tellDeliverMsg and the JetStream ack is gated on the zone's
// render-or-suppress reply. Concurrency-safe; called from a zone goroutine on join/arrival.
func (c *commSource) startConsumer(playerID string, route func() *Zone) {
	if c.js == nil || route == nil {
		return
	}
	c.mu.Lock()
	if _, running := c.consumers[playerID]; running {
		c.mu.Unlock()
		return
	}
	// Reserve the slot before the (possibly blocking) Consume so a concurrent start can't double-run.
	c.consumers[playerID] = nil
	c.mu.Unlock()

	subj := commbus.DtellSubject(playerID)
	pace := c.drainPace // the per-source pace (set at construction), captured once for the goroutine
	// The durable consumer id is the player id (stable, so a restart RESUMES from the last ack, not
	// the start — the at-least-once delivery + the cursor suppression give steady-state exactly-once,
	// never-lost rendering). The handler posts to the
	// player's current zone and blocks for the ack so the JetStream ack reflects the actual render.
	cons, err := c.js.Consume(subj, playerID, func(msg commbus.Message, backlog bool) commbus.AckDecision {
		decision := routeTellDeliver(route, playerID, msg, backlog)
		if decision == commbus.AckDelivered && pace > 0 {
			// PACE the drain (P8-A5, backlog pacing): cap the rate at which tells reach a freshly-joined
			// gate so a long OFFLINE backlog drains as a steady "while you were away…" stream, not a
			// single flood. The cap is small (loginDrainPace) so a live tell to an online player is
			// effectively unpaced; it only matters for a deep backlog. A NAK is not paced (it redelivers
			// on its own backoff). Runs on the consumer goroutine — never a zone goroutine.
			time.Sleep(pace)
		}
		return decision
	})
	if err != nil {
		// Never fatal: tells degrade to undelivered (they remain durable in the stream for a later
		// consumer). Release the reserved slot so a retry can start one.
		c.mu.Lock()
		delete(c.consumers, playerID)
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	// A stopConsumer may have raced in while we were starting; honor it.
	if _, stillWanted := c.consumers[playerID]; !stillWanted {
		c.mu.Unlock()
		_ = cons.Stop()
		return
	}
	c.consumers[playerID] = cons
	c.mu.Unlock()
}

// stopConsumer tears down playerID's durable consumer if running (clean leave / handed-off orphan).
// Concurrency-safe; idempotent.
func (c *commSource) stopConsumer(playerID string) {
	c.mu.Lock()
	cons, running := c.consumers[playerID]
	delete(c.consumers, playerID)
	c.mu.Unlock()
	if running && cons != nil {
		_ = cons.Stop()
	}
}

// --- Zone-goroutine accessors into the shard's comms source (tell side) -----------------------

// commSourceOrNil returns the shard's comms source, or nil on a bare zone (no shard). The tell
// consumer lifecycle uses it; nil means "tells unavailable here" (a standalone unit zone).
func (z *Zone) commSourceOrNil() *commSource {
	if z.shard == nil {
		return nil
	}
	return z.shard.comms
}

// channelHistory returns the shard's SHARD-LOCAL channel-scrollback store (#348, channelhistory.go), or
// nil on a bare zone (no shard). The publish path uses it to CAPTURE a rendered line and the `history`
// command uses it to serve buffered lines; nil means "no scrollback here" — a storeless unit zone
// captures/serves nothing and never panics (the never-fatal degradation, mirroring commSourceOrNil).
func (z *Zone) channelHistory() *chanHistory {
	if z.shard == nil {
		return nil
	}
	return z.shard.chanHistory
}

// tellJS returns the shard's durable-tell JetStream handle, or nil on a bare zone. A shard always has
// a non-nil handle (DisabledJetStream when off), so a real run never sees nil here.
func (z *Zone) tellJS() commbus.JetStream {
	if z.shard == nil || z.shard.comms == nil {
		return nil
	}
	return z.shard.comms.js
}

// dir returns the shard's directory Locator for tell-target resolution (P8-D5), or nil on a bare zone
// or a single-shard run with no directory (in which case sendTell accepts-and-publishes without a
// resolve check — the durable-always posture).
func (z *Zone) dir() Locator {
	if z.shard == nil {
		return nil
	}
	return z.shard.dir
}

// routeTellDeliver posts one drained durable tell to the resident's CURRENT zone (resolved via the
// route func, which Loads the session's currentZone atomic pointer) and blocks for the ack result. It
// runs on a bus consumer goroutine (NOT a zone goroutine), so it reaches the zone only via the inbox —
// single-writer preserved. The authoritative resident re-check is on the zone goroutine
// (deliverDrainedTell reports failure if the player is not actually in z.players right now).
//
// Every failure the world can produce here is TRANSIENT (#266) — an unknown current zone (mid cross-shard
// handoff, or a bare test session), a not-our-resident re-check, an emit blip on a momentarily closed bus,
// or a zone too busy/shutting-down to reply within tellAckTimeout. All of them clear on their own, so each
// maps to RetryTransient and redelivers on the backoff schedule rather than being retried instantly and
// parked. There is NO world-side poison today: a malformed message never reaches here (the commbus
// unmarshal drops it), so DropPoison is deliberately unused on this path.
func routeTellDeliver(route func() *Zone, playerID string, msg commbus.Message, backlog bool) commbus.AckDecision {
	z := route()
	if z == nil {
		return commbus.RetryTransient // unknown current zone: redeliver on the schedule; the message stays durable
	}
	ack := make(chan bool, 1)
	z.post(tellDeliverMsg{target: playerID, msg: msg, backlog: backlog, ack: ack})
	select {
	case ok := <-ack:
		if ok {
			return commbus.AckDelivered
		}
		return commbus.RetryTransient // not our resident right now, or the gate emit blipped
	case <-time.After(tellAckTimeout):
		// The zone did not reply in time (shutting down / overloaded): redeliver rather than lose it.
		return commbus.RetryTransient
	}
}

// tellAckTimeout bounds how long the consumer waits for the zone's render-or-suppress reply before
// NAKing (so a stopped/overloaded zone redelivers rather than losing the tell). A package var so a
// test can shrink it.
var tellAckTimeout = 5 * time.Second

// tellPublishQueue bounds the shard's durable-tell publisher queue. A full queue drops the tell (a
// flood throttles the SENDER, never stalls the bus or another sender) — the saver/presence
// backpressure discipline. Generous: tells are point-to-point and low-rate.
const tellPublishQueue = 256

// tellPublishIOTimeout bounds one resolve+publish so a hung directory/JetStream can't wedge the FIFO
// publisher (which would freeze every subsequent sender's tells). On timeout the tell is abandoned
// with a notice to the sender. Mirrors saveIOTimeout / presenceIOTimeout.
const tellPublishIOTimeout = 5 * time.Second

// enqueueTell hands a tellJob to the shard publisher WITHOUT blocking the zone goroutine. A full queue
// drops it with a "too busy" notice to the sender (back to the out channel, ack 0 like a comms frame).
// Concurrency-safe; called from a zone goroutine.
func (c *commSource) enqueueTell(j tellJob) {
	select {
	case c.pub <- j:
	default:
		writeFrameTo(j.out, textFrame("The tell system is busy; try again."))
	}
}

// publishLoop is the shard's SINGLE durable-tell publisher (started once by Shard.Run). It drains the
// FIFO queue and, for each tell IN ORDER, resolves the target via the directory (P8-D5 / P8-A4, NEVER
// presence), assigns the per-author seq (so the stream append order matches seq order — per-sender
// order, P8-A3), and PublishDurable's with an ALWAYS-set idempotency key (P8-A5). One ordered worker is
// what guarantees two quick tells from one sender are appended in send order. A nil/disabled JetStream
// makes every job a "temporarily offline" notice. Runs off every zone goroutine.
func (c *commSource) publishLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-c.pub:
			c.publishOne(ctx, j)
		}
	}
}

func (c *commSource) publishOne(ctx context.Context, j tellJob) {
	cctx, cancel := context.WithTimeout(ctx, tellPublishIOTimeout)
	defer cancel()

	// ROUTE-VIA-DIRECTORY (P8-D5 / P8-A4): resolve the target via the epoch-authoritative directory,
	// NEVER presence. A resolve MISS (a never-seen name) refuses the tell to the sender. With no
	// directory (a single-shard test/bare run) we cannot validate existence, so we accept-and-publish —
	// the target's own world drains it if/when they log in (the durable-always posture).
	if j.dir != nil {
		if _, found, err := j.dir.PlayerShard(cctx, j.target); err == nil && !found {
			writeFrameTo(j.out, textFrame("There is no player by that name."))
			return
		}
		// A directory error degrades to accept-and-publish (never lose a tell on a transient Redis blip).
	}

	seq := c.nextSeq(j.authorID) // monotonic per-author, assigned in publish order (the FIFO worker)
	msg := commbus.Message{
		AuthorID:       j.authorID,
		AuthorName:     j.authorName,
		Seq:            seq,
		IdempotencyKey: commbus.NewIdempotencyKey(j.authorID, seq), // ALWAYS set (P8-A5)
		Body:           j.body,
	}
	if err := c.js.PublishDurable(cctx, commbus.DtellSubject(j.target), msg); err != nil {
		if j.log != nil {
			j.log.Debug("durable tell publish failed", "from", j.authorID, "to", j.target, "err", err)
		}
		writeFrameTo(j.out, textFrame("Tells are temporarily offline."))
		return
	}
	writeFrameTo(j.out, textFrame("You tell "+j.target+", '"+j.body+"'"))
	if j.log != nil {
		j.log.Debug("durable tell published", "from", j.authorID, "to", j.target, "seq", seq)
	}
}

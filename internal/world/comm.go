package world

import (
	"sync"
	"time"

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

	mu  sync.Mutex
	seq map[string]uint64       // author id -> last issued monotonic sequence (P8-A3)
	rl  map[string]*tokenBucket // author id -> per-author rate-limit bucket (P8-A1)
}

// newCommSource builds a disabled comms source (a no-op RoleWorld bus). WithComms swaps in a live bus.
func newCommSource() *commSource {
	return &commSource{
		bus: commbus.Disabled(commbus.RoleWorld),
		seq: map[string]uint64{},
		rl:  map[string]*tokenBucket{},
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
		b = newTokenBucket(commRateBurst, commRateRefill)
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

package world

import "sync"

// channelhistory.go is the SHARD-LOCAL channel scrollback store for #348 (member-gated channel
// history) — slice 1 of the retrieval work the P8-D3 channel_def deliberately deferred. A channel_def
// already carries a per-channel `history` buffer size from content (channel.go), but retrieval was
// parked with one load-bearing in-code invariant, which THIS file exists to honor: when retrieval
// lands it MUST gate fetches on canHear (the split hear predicate) AT FETCH TIME — a player who LOST
// hear access must not replay lines from when they had it. The ring here only STORES lines; the
// fetch-time gate lives in cmdHistory (commscmds.go), which evaluates def.canHear against the LIVE
// entity before it ever reads this store. This store keeps NO access state of its own — it is a dumb
// bounded buffer, so it cannot leak a line the command's gate hasn't cleared.
//
// # PARTIAL BY CONSTRUCTION — the "no silent caps" caveat, made LOUD
//
// This ring captures lines at the LOCAL shard's channel-publish path (cmdChannel, after a successful
// bus.Publish). On a single-shard deployment that is the whole channel stream, so `history` is
// COMPLETE. On a multi-shard fleet it is PARTIAL: each shard only ever sees the lines its OWN hosted
// players authored, because a channel line reaches other shards over the bus as a delivery, not back
// through their publish path. So `history gossip` on shard B shows only shard-B-authored gossip, never
// shard A's. This is intentional for slice 1 and is surfaced LOUDLY — here in this comment AND in a
// z.log.Debug at every capture ("shard-local; partial on a multi-shard fleet") — rather than silently
// pretending to be the full record.
//
// # Deferred follow-ups (NOT in this slice)
//
//   - Slice 2 (cross-shard aggregation): make `history` the FULL channel record on a fleet — e.g. a
//     bus-fed shared recent-lines store every shard captures into, or a scatter-gather fetch. Until
//     then the partial nature is documented, not hidden.
//   - Slice 3 (durability across restart): the ring is in-memory, so a shard restart drops its
//     scrollback. A durable tier (mirroring the durable-tell JetStream posture) would survive it.
//
// # Concurrency
//
// A channelDef is immutable and shared read-only across zone goroutines (channel.go), so it CANNOT
// hold this mutable ring. The store lives on the Shard (one per process, reached via z.channelHistory())
// and is touched from MULTIPLE zone goroutines — every hosted zone's publish path appends, and the
// history command snapshots — exactly like commSource, so it carries its own mutex. It holds NO zone
// state (only per-channel rendered lines keyed by ref), so guarding it does not weaken the single-writer
// invariant, mirroring commSource.mu.

// chanHistoryEntry is one captured channel line: the pre-RENDERED body (format + color + sanitized $t,
// exactly what the gate would write) plus the author id, retained ONLY so the fetching player's ignore
// set can be re-applied at read time (the live-gate-funnel parity — an ignored author's line is dropped
// before the reader sees it, in history as in the live stream).
type chanHistoryEntry struct {
	authorID string
	body     string // the fully-rendered line (def.renderLine output) — retrieval reuses the format for free
}

// chanHistory is the shard's per-channel recent-lines ring set, keyed by channel ref. Each ref maps to
// a bounded slice holding the last N rendered lines (N = the channel_def's `history`, a content opt-in).
// Guarded by mu (concurrent zone goroutines append; the history command snapshots) — it holds no zone
// state, so the lock does not weaken single-writer (mirrors commSource).
type chanHistory struct {
	mu    sync.Mutex
	rings map[string][]chanHistoryEntry
}

// newChanHistory builds an empty history store. Always safe to call; a shard with no channels simply
// never appends (history==0 channels are never captured — see cmdChannel).
func newChanHistory() *chanHistory {
	return &chanHistory{rings: map[string][]chanHistoryEntry{}}
}

// append records one rendered line on channel ref, keeping at most `limit` lines (the channel_def's
// `history`). When the ring would exceed limit the OLDEST line is dropped (a FIFO scrollback). A limit<=0
// is a no-op (history==0 => the channel opted out of scrollback — preserve that: capture nothing).
// Under the lock; concurrency-safe from any zone goroutine.
func (h *chanHistory) append(ref string, limit int, e chanHistoryEntry) {
	if h == nil || limit <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ring := append(h.rings[ref], e)
	if len(ring) > limit {
		// Keep the last `limit`, dropping the oldest. Copy into a fresh slice so the dropped head is not
		// pinned by the underlying array (and the retained window never aliases evicted entries).
		trimmed := make([]chanHistoryEntry, limit)
		copy(trimmed, ring[len(ring)-limit:])
		ring = trimmed
	}
	h.rings[ref] = ring
}

// snapshot returns a COPY of channel ref's current ring (oldest-first), or nil when the channel has no
// buffered lines. A copy so the caller (the history command, on a zone goroutine) can filter/render
// without holding the lock or racing a concurrent append. Concurrency-safe.
func (h *chanHistory) snapshot(ref string) []chanHistoryEntry {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ring := h.rings[ref]
	if len(ring) == 0 {
		return nil
	}
	out := make([]chanHistoryEntry, len(ring))
	copy(out, ring)
	return out
}

package gate

import "sync"

// session is the per-telnet-connection state that survives gRPC re-dials. A
// player logs in once; the session_id and the input sequence are minted ONCE at
// that login and held here for the whole connection, even as the gate re-targets
// the Play stream across a cross-shard redirect (docs/PROTOCOL.md §5: "the gate
// owns the input buffer", session_id and seq are session-scoped, not per-stream).
//
// session is the gate's only durable per-player state. It owns:
//
//   - id: the stable session_id presented on every Attach.
//   - an ordered, gap-free input buffer keyed by seq (the un-acked window). Each
//     typed line is assigned the next seq, appended, and sent; frames carry the
//     world's input high-water (ack_input_seq) and the buffer is pruned up to it.
//   - on a redirect, the buffer is the replay source: everything still un-acked
//     after the new shard reports its resume point is re-sent, in order.
//
// It is guarded by a mutex because the reader loop (assigning/buffering input)
// and the writer loop (pruning on ack) touch it concurrently.
type session struct {
	id string

	mu      sync.Mutex
	nextSeq uint64       // seq to assign to the NEXT line (1-based)
	buf     []bufferedIn // un-acked input, ascending by seq
	frozen  bool         // true while a redirect is in flight: live input queues, is not "live"
}

// bufferedIn is one un-acked input line held for possible replay.
type bufferedIn struct {
	seq  uint64
	text string
}

// newSession mints a session with a stable id. seq starts at 0; the first line
// assigned gets seq 1.
func newSession(id string) *session {
	return &session{id: id}
}

// add assigns the next seq to a typed line, appends it to the un-acked buffer, and
// returns the seq plus whether the session is currently frozen (a redirect in
// flight). Reporting `frozen` under the SAME lock that appended keeps the decision
// consistent. The guarantee is "never LOST", not "never both": a line added in the
// narrow window after the writer flags a redirect but before freeze takes effect can
// be both buffered (for replay to the destination) AND live-sent to the old shard —
// but the source froze the player synchronously and DROPS such a late line, so it
// still applies exactly once, on the destination. The line is retained for replay
// until the world acks it.
func (s *session) add(text string) (seq uint64, frozen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSeq++
	seq = s.nextSeq
	s.buf = append(s.buf, bufferedIn{seq: seq, text: text})
	return seq, s.frozen
}

// prune drops every buffered line with seq <= ack (the world's input high-water).
// It returns how many entries were removed so callers can trace buffer churn.
// Called as each ServerFrame arrives, whatever the payload (ack rides on all).
func (s *session) prune(ack uint64) (removed int) {
	if ack == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := 0
	for i < len(s.buf) && s.buf[i].seq <= ack {
		i++
	}
	if i == 0 {
		return 0
	}
	// Reslice off the acked prefix; copy down so the backing array can shrink.
	s.buf = append(s.buf[:0], s.buf[i:]...)
	return i
}

// nextSeqValue is the seq the gate WILL send next (resume point on re-dial). It is
// reported in Attach.input_seq so the destination knows the gate's position.
func (s *session) nextSeqValue() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextSeq + 1
}

// freeze marks the session as redirecting. While frozen, the reader loop still
// assigns seqs and buffers lines (so nothing is lost and order is preserved) but
// must NOT forward them live — they wait behind the replay (see add's frozen return).
func (s *session) freeze() {
	s.mu.Lock()
	s.frozen = true
	s.mu.Unlock()
}

// replayFrom drops anything the new shard already applied (seq <= ack, deduped there)
// and returns a snapshot of the remaining un-acked buffer, in order: exactly what the
// gate re-sends after the destination reports its resume point. It does NOT clear the
// frozen flag — the caller keeps replaying (and the forwarder keeps buffering) until
// drainReplay confirms the buffer is fully sent, so a live line can never overtake the
// replay on the wire. (Lines typed DURING the freeze were appended to buf and so are
// included here if still un-acked.)
func (s *session) replayFrom(ack uint64) []bufferedIn {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := 0
	for i < len(s.buf) && s.buf[i].seq <= ack {
		i++
	}
	if i > 0 {
		s.buf = append(s.buf[:0], s.buf[i:]...)
	}
	out := make([]bufferedIn, len(s.buf))
	copy(out, s.buf)
	return out
}

// tailAfter returns buffered lines with seq > sentThrough, in order: the lines that
// arrived (during the freeze) after a replay batch was snapshotted, so the caller can
// send them too before thawing. Empty means the buffer is drained up to sentThrough.
func (s *session) tailAfter(sentThrough uint64) []bufferedIn {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []bufferedIn
	for _, b := range s.buf {
		if b.seq > sentThrough {
			out = append(out, b)
		}
	}
	return out
}

// thawIfDrained clears the frozen flag iff no buffered line has seq > sentThrough
// (everything sent so far covers the buffer tail). It reports whether it thawed. The
// check-and-clear is atomic under the lock, so the instant the freeze lifts there is
// no un-sent buffered line a live forward could race ahead of. Returns false when a
// line slipped in during the final replay batch — the caller sends it, then retries.
func (s *session) thawIfDrained(sentThrough uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.buf {
		if b.seq > sentThrough {
			return false
		}
	}
	s.frozen = false
	return true
}

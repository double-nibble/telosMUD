package gate

import (
	"reflect"
	"testing"
)

// seqs extracts the seqs from a replay snapshot for terse assertions.
func seqs(in []bufferedIn) []uint64 {
	out := make([]uint64, len(in))
	for i, b := range in {
		out[i] = b.seq
	}
	return out
}

// TestSessionAssignsMonotonicSeq: each added line gets the next seq, 1-based, and
// nextSeqValue reports the seq the gate WILL send next (the Attach resume point).
func TestSessionAssignsMonotonicSeq(t *testing.T) {
	s := newSession("sess-1")
	if got := s.nextSeqValue(); got != 1 {
		t.Fatalf("nextSeqValue before any input = %d, want 1", got)
	}
	for want := uint64(1); want <= 3; want++ {
		seq, frozen := s.add("line")
		if seq != want {
			t.Fatalf("add #%d returned seq %d, want %d", want, seq, want)
		}
		if frozen {
			t.Fatalf("add #%d reported frozen on an un-frozen session", want)
		}
	}
	if got := s.nextSeqValue(); got != 4 {
		t.Fatalf("nextSeqValue after 3 inputs = %d, want 4", got)
	}
}

// TestSessionPruneOnAck: the buffer is the un-acked window; prune drops everything
// at or below the world's high-water and reports how many it removed.
func TestSessionPruneOnAck(t *testing.T) {
	s := newSession("sess-2")
	for i := 0; i < 5; i++ {
		s.add("line")
	}

	if removed := s.prune(0); removed != 0 {
		t.Fatalf("prune(0) removed %d, want 0 (ack 0 means nothing acked)", removed)
	}
	if got := seqs(s.buf); !reflect.DeepEqual(got, []uint64{1, 2, 3, 4, 5}) {
		t.Fatalf("buffer after prune(0) = %v, want [1 2 3 4 5]", got)
	}

	if removed := s.prune(2); removed != 2 {
		t.Fatalf("prune(2) removed %d, want 2", removed)
	}
	if got := seqs(s.buf); !reflect.DeepEqual(got, []uint64{3, 4, 5}) {
		t.Fatalf("buffer after prune(2) = %v, want [3 4 5]", got)
	}

	// A stale/lower ack is a no-op (acks are monotonic but defensively idempotent).
	if removed := s.prune(1); removed != 0 {
		t.Fatalf("prune(1) after prune(2) removed %d, want 0", removed)
	}

	if removed := s.prune(5); removed != 3 {
		t.Fatalf("prune(5) removed %d, want 3", removed)
	}
	if got := s.buf; len(got) != 0 {
		t.Fatalf("buffer after prune(5) = %v, want empty", seqs(got))
	}
}

// TestSessionReplayFromCursor: replayFrom returns only the un-acked tail (seq > ack),
// in order, which is exactly what the gate re-sends to the destination shard.
func TestSessionReplayFromCursor(t *testing.T) {
	s := newSession("sess-3")
	for i := 0; i < 4; i++ {
		s.add("line")
	}
	s.freeze()

	// Destination reports it already applied through seq 2 (the resume point): replay
	// only 3 and 4.
	got := seqs(s.replayFrom(2))
	if !reflect.DeepEqual(got, []uint64{3, 4}) {
		t.Fatalf("replayFrom(2) = %v, want [3 4]", got)
	}
	// replayFrom drops the acked prefix but leaves the freeze in place (the caller thaws
	// only after the batch is sent).
	if !s.frozen {
		t.Fatal("replayFrom must not clear the frozen flag")
	}
	if left := seqs(s.buf); !reflect.DeepEqual(left, []uint64{3, 4}) {
		t.Fatalf("buffer after replayFrom(2) = %v, want [3 4]", left)
	}
	// After "sending" through seq 4 the buffer is drained: thaw succeeds.
	if !s.thawIfDrained(4) {
		t.Fatal("thawIfDrained(4) = false, want true (buffer fully sent)")
	}
	if s.frozen {
		t.Fatal("thawIfDrained(4) did not clear the frozen flag")
	}

	// A destination that already has everything (ack >= top) replays nothing.
	s.freeze()
	if got := s.replayFrom(4); len(got) != 0 {
		t.Fatalf("replayFrom(4) = %v, want empty", seqs(got))
	}
	if !s.thawIfDrained(4) {
		t.Fatal("thawIfDrained after empty replay = false, want true")
	}
}

// TestSessionBufferingDuringRedirect: while frozen, add() reports frozen so the reader
// buffers (does not forward) — and those lines are picked up by the subsequent replay,
// in order, behind the already-buffered window.
func TestSessionBufferingDuringRedirect(t *testing.T) {
	s := newSession("sess-4")
	s.add("a") // seq 1
	s.add("b") // seq 2

	// Redirect begins: freeze. Lines typed now must be buffered, not forwarded.
	s.freeze()
	if seq, frozen := s.add("c"); seq != 3 || !frozen {
		t.Fatalf("add during freeze = (%d,%v), want (3,true)", seq, frozen)
	}
	if seq, frozen := s.add("d"); seq != 4 || !frozen {
		t.Fatalf("add during freeze = (%d,%v), want (4,true)", seq, frozen)
	}

	// Destination resumes from 1 (it had only the first line): replay 2,3,4 in order —
	// the lines typed during the freeze queue AFTER the earlier ones, never before.
	got := seqs(s.replayFrom(1))
	if !reflect.DeepEqual(got, []uint64{2, 3, 4}) {
		t.Fatalf("replayFrom(1) = %v, want [2 3 4]", got)
	}

	// Simulate a line arriving mid-batch: after sending through seq 3, a new line 5 is
	// buffered. thawIfDrained(3) must refuse (5 is un-sent); tailAfter(3) surfaces it.
	if seq, frozen := s.add("mid"); seq != 5 || !frozen {
		t.Fatalf("add mid-replay = (%d,%v), want (5,true)", seq, frozen)
	}
	if s.thawIfDrained(3) {
		t.Fatal("thawIfDrained(3) = true, want false (seq 5 un-sent)")
	}
	if tail := seqs(s.tailAfter(3)); !reflect.DeepEqual(tail, []uint64{4, 5}) {
		t.Fatalf("tailAfter(3) = %v, want [4 5]", tail)
	}
	// Once the tail is sent through 5, thaw succeeds and live forwarding resumes.
	if !s.thawIfDrained(5) {
		t.Fatal("thawIfDrained(5) = false, want true")
	}
	if seq, frozen := s.add("e"); seq != 6 || frozen {
		t.Fatalf("add after replay = (%d,%v), want (6,false)", seq, frozen)
	}
}

// TestSessionStableID: the id is minted once and never changes (Attach carries it on
// every re-dial).
func TestSessionStableID(t *testing.T) {
	s := newSession("stable-id")
	if s.id != "stable-id" {
		t.Fatalf("session id = %q, want stable-id", s.id)
	}
}

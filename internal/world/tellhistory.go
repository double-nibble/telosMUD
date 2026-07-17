package world

import (
	"strings"
	"time"
)

// tellhistory.go is the IN-SESSION tell-history ring (#349, slice 1): a small per-session buffer of the
// recent tells a player SENT and RECEIVED, surfaced via the `tells` command (alias `replay`). It is a pure
// capture — a read-only mirror bolted onto the existing tell paths — and changes NOTHING about tell
// delivery semantics (tell.go). The two obligations it satisfies:
//
//   - "what did I just say / what was just said to me" — a player who scrolled past a tell (combat spam,
//     a screen clear) can re-read the last handful without a client-side scrollback.
//   - pair-privacy BY CONSTRUCTION — the ring lives on the receiving/sending session, so A<->B's tells are
//     only ever in A's ring and B's ring; a third player C's ring can never contain them (there is no
//     shared store to leak from). No access check is needed because there is no cross-player read path.
//
// # Session-scoped, transient (the deliberate slice-1 limitation)
//
// The ring lives on the *session and is zone-goroutine-owned (single-writer, no lock — the same discipline
// as lastTellFrom / tellCursor). It is NOT persisted and does NOT ride the cross-shard handoff snapshot, so
// it RESETS on relog and on a shard walk — exactly like lastTellFrom (the `reply` target), whose doc note
// this mirrors. That is acceptable and intended for slice 1: it is a recent-activity convenience, not a
// durable ledger.
//
// # Slice 2 (durable across sessions) is DEFERRED
//
// A durable, cross-session tell history (survives relog / shard walk) is a separate slice with real
// write-amplification concerns: every tell would fan out an extra persisted write to BOTH participants'
// history subtrees (on top of the durable-stream write the tell already costs), and the buffer would need a
// bounded, size-guarded at-rest form + a load/dump path (like TellCursorJSON). That trade-off is out of
// scope here; slice 1 ships the transient ring and defers durability to a follow-up.

// tellLogMax bounds the ring: only the last tellLogMax tells (sent + received, interleaved chronologically)
// are retained; older entries drop off the front. A package var (not a const) so a test can shrink it to
// exercise the trim without driving twenty-plus real tells.
var tellLogMax = 20

// tellLogEntry is one captured tell, either sent BY (outbound) or received BY (inbound) this session. The
// body is the SANITIZED form actually put on the wire (outbound) / delivered (inbound), never a raw client
// string — the capture reuses the already-cleaned body from the live path, so the history can never surface
// content the tell pipeline would have stripped.
type tellLogEntry struct {
	outbound  bool   // true => this player SENT it ("You told X"); false => RECEIVED it ("X tells you")
	other     string // the other party's player id (the tell target for outbound, the author for inbound)
	otherName string // the other party's DISPLAY name for the render (the sender lacks the target's live
	// entity, so an outbound entry falls back to the id; an inbound entry has the author's engine-set name)
	body string    // the sanitized tell body (no prefix — the render helper adds the "You told / tells you" frame)
	at   time.Time // capture time; retained for a future "(3m ago)" adornment / slice-2 ordering, unused today
}

// recordTell appends one captured tell to the session ring and trims it to the last tellLogMax entries
// (oldest dropped off the front). Zone-goroutine-only, no lock — the same single-writer discipline as
// lastTellFrom / tellCursor (every caller is a zone-goroutine handler: sendTell for outbound,
// deliverDrainedTell for inbound). A nil/short ring is fine; append grows it lazily.
func (s *session) recordTell(e tellLogEntry) {
	s.tellLog = append(s.tellLog, e)
	if len(s.tellLog) > tellLogMax {
		// Drop the oldest overflow. Re-slice off the front (a bounded copy on the rare over-cap append) so the
		// backing array does not grow without bound as tells accumulate over a long session.
		drop := len(s.tellLog) - tellLogMax
		s.tellLog = append(s.tellLog[:0:0], s.tellLog[drop:]...)
	}
}

// renderTellLog renders the session's ring chronologically (oldest -> newest) into one multi-line string
// (the established multi-line Send pattern — see renderInbox). Each line MIRRORS the live tell wording from
// tell.go so the history reads identically to what the player saw:
//
//   - outbound => "You told <otherName>, '<body>'"   (past tense — this is a record of what you said)
//   - inbound  => "<otherName> tells you, '<body>'"  (matching deliverDrainedTell's live render frame)
//
// An empty ring returns the friendly notice. Pure read of zone-owned state; zone goroutine.
func renderTellLog(s *session) string {
	if s == nil || len(s.tellLog) == 0 {
		return "You have no recent tells."
	}
	var b strings.Builder
	b.WriteString("Recent tells:")
	for _, e := range s.tellLog {
		b.WriteByte('\n')
		b.WriteString("  ")
		if e.outbound {
			b.WriteString("You told " + e.otherName + ", '" + e.body + "'")
		} else {
			b.WriteString(e.otherName + " tells you, '" + e.body + "'")
		}
	}
	return b.String()
}

// cmdTells renders the player's recent-tells ring (`tells`, alias `replay`). Available to every player (no
// MinRank) — a mortal convenience, not a staff verb. Pure read of the session's own ring; it mutates
// nothing and, like the other comms commands, never releases ownership (dispatch prompts on return).
func cmdTells(c *Context) error {
	c.Send(renderTellLog(c.s))
	return nil
}

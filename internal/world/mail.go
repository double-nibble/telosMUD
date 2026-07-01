package world

import (
	"context"
	"errors"
	"time"
)

// MailInboxCap bounds a recipient's TOTAL inbox (security hardening): once the recipient holds this many,
// SendMail evicts the oldest READ message to make room and, only if the inbox is entirely UNREAD, refuses
// (ErrMailboxFull). So N senders — or one attacker's several characters — can't grow a victim's inbox (or
// the DB) without bound, and a full-of-read inbox never wedges legitimate new mail on spam. The per-sender
// rate limit throttles the fill; this caps the ceiling. A candidate for content-config later; a const is
// the minimal guard.
const MailInboxCap = 100

// ErrMailboxFull is returned by SendMail when the recipient is at MailInboxCap. It is a POLICY rejection,
// DISTINCT from an infrastructure error, so the caller renders "their mailbox is full" (retryable by the
// recipient clearing space) rather than the generic "mail is unavailable".
var ErrMailboxFull = errors.New("recipient mailbox is full")

// mail.go is the WORLD-side contract + DTO for Phase-8 slice 8.7 durable mail (docs/PHASE8-PLAN.md
// 8.7, P8-D6). Mail is a persistent, queryable read/send inbox (Postgres, not a log) and is ENGINE
// mechanism — it exists with zero content; a storeless shard simply has mail DISABLED (never a crash).
//
// The store is OPTIONAL exactly like CharacterStore: a nil MailStore means "mail unavailable" and the
// mail commands degrade cleanly (a one-line notice, no panic). Two implementations satisfy it — the pgx
// store against the `mail` table (internal/store/mail.go) and the in-memory MemStore (memstore.go), so
// the whole mail journey is hermetically testable with no live Postgres, and a gated real-PG round-trip
// pins the SQL (the CharacterStore discipline).
//
// SECURITY (the access-control contract every implementation MUST honor):
//   - from_player is ENGINE-SET by the caller (the source world from the live *Entity, s.character) —
//     the store NEVER derives the sender from a client field (P8-A2, no impersonated mail).
//   - ListMail / ReadMail / DeleteMail are PLAYER-SCOPED at the QUERY: every one takes the authenticated
//     `player` (the inbox owner) and the implementation MUST scope by it (WHERE ... AND to_player =
//     player), so a player cannot read or delete another player's mail by guessing an id. The scoping is
//     in the store, not just the command — a misbehaving command can never widen it.
//
// All methods do blocking pool I/O and run OFF the zone goroutine (a short-lived goroutine the mail
// command spawns, the cmdWho/login-read discipline) — never on the actor loop.

// MailEntry is one stored mail message in its world-facing form. It is the DTO the store maps the `mail`
// row to/from, keeping the on-disk columns independent of any wire/proto shape. ID is the server-minted
// message id (a UUID string); a player references a message by its 1-based INBOX POSITION in the
// command UX (mail read 2), never by this opaque id, so the id is never surfaced to or accepted from a
// client.
type MailEntry struct {
	ID      string    // server-minted message id (opaque to the player)
	To      string    // recipient (the inbox owner)
	From    string    // ENGINE-SET sender identity
	Subject string    // sanitized on send
	Body    string    // sanitized on send
	SentAt  time.Time // server clock at send
	Read    bool      // read_at IS NOT NULL — the unread/read state
}

// MailStore is the durable mail inbox (docs/PERSISTENCE.md durable Postgres tier). It is deliberately
// small: send (INSERT), list an inbox (newest-first), read one item by inbox position (fetch + mark
// read), delete one item. Every read/delete is player-scoped (the `player` argument is the inbox owner
// the implementation MUST scope the query by). nil disables mail entirely (the never-fatal degradation).
type MailStore interface {
	// SendMail inserts one message. `from` is the ENGINE-SET sender (the caller's resolved identity,
	// never a client field); `to` is the recipient. subject/body are already sanitized by the caller.
	// It mints and returns the message id. An error is an infrastructure failure (the caller degrades to
	// "mail is unavailable", never crashes).
	SendMail(ctx context.Context, to, from, subject, body string) (id string, err error)

	// ListMail returns `player`'s inbox newest-first (the unread/read state on each entry). The query is
	// scoped to to_player = player — it can only ever return that player's own mail. An empty inbox is a
	// nil slice (not an error).
	ListMail(ctx context.Context, player string) ([]MailEntry, error)

	// ReadMail fetches the message at 1-based inbox position `pos` for `player` (newest-first, the same
	// order ListMail returns), marks it read (sets read_at if unread), and returns it. found=false (nil
	// error) when pos is out of range. SCOPED to to_player = player at the query — a player cannot read
	// another player's mail by position or id.
	ReadMail(ctx context.Context, player string, pos int) (entry MailEntry, found bool, err error)

	// DeleteMail removes the message at 1-based inbox position `pos` for `player` (newest-first).
	// deleted=false (nil error) when pos is out of range. SCOPED to to_player = player — a player cannot
	// delete another player's mail.
	DeleteMail(ctx context.Context, player string, pos int) (deleted bool, err error)
}

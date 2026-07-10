package world

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/textsan"
)

// mailcmds.go holds the Phase-8 slice 8.7 player MAIL commands (docs/PHASE8-PLAN.md 8.7, P8-D6):
// `mail` (list the inbox), `mail read <n>`, `mail delete <n>`, and `mail send <name> <subject> | <body>`.
// Mail is a WORLD command (registered beside say/who/tell) because the SENDER identity is ENGINE-SET from
// the live session (s.character / the *Entity), never a client field (P8-A2 — no impersonated mail), and
// the recipient is resolved against the authoritative directory. Mail is ENGINE mechanism (no content
// needed); a storeless shard simply has it disabled.
//
// # The off-zone discipline
//
// Every mail store call is blocking pool I/O, so it MUST NOT run on the zone goroutine. The command
// captures the engine-set sender + the sanitized subject/body ON the zone goroutine (single-writer reads
// of the live entity), then spawns a short-lived goroutine for the store I/O + the directory resolve +
// the new-mail notify, writing the result straight to the session out channel (ack 0, like a comms
// frame) — the same off-goroutine discipline cmdWho/sendTell use. The session pointer's `character`/`out`
// are immutable for the connection, so capturing them is race-free; we never touch zone-owned state off
// the loop.
//
// # SECURITY (where each obligation is enforced)
//
//   - FROM IS ENGINE-SET (P8-A2): SendMail's `from` is s.character captured on the zone goroutine, never
//     parsed from input. A player cannot forge another sender.
//   - READ/DELETE SCOPING: ListMail/ReadMail/DeleteMail are called with the AUTHENTICATED player
//     (s.character) and the store scopes every query by it (WHERE ... AND to_player = player). A player
//     references a message only by its 1-based INBOX POSITION (never an opaque id), and the position is
//     resolved against THEIR OWN inbox — so there is no id a player could guess to reach another's mail.
//   - SANITIZE (P8-A7): subject + body are CleanLine'd on send (control/ANSI/IAC stripped, length-capped),
//     so a mail render cannot inject terminal control bytes or forge markup.
//   - RECIPIENT RESOLUTION: a `mail send` to a never-seen name is REFUSED to the sender (resolved via the
//     directory PlayerShard, the same epoch-authoritative existence check tells use) — a typo'd recipient
//     is refused, not silently lost. With no directory (a single-shard/bare run) we accept-and-store (the
//     target reads it on login), the durable-always posture tells take.
//   - SIZE / FLOOD LIMITS: the per-author comms token bucket (commRateOK, P8-A1) throttles `mail send`
//     just like channels/tells, so a SINGLE sender's rate is bounded; subject/body are length-capped by
//     the sanitizer. NOTE: this is per-SENDER only — there is no per-RECIPIENT inbox cap or retention
//     yet, so many senders can still grow a victim's inbox over time (the integrity boundary holds; this
//     is a griefing/storage gap tracked in FOLLOW-UPS — add an inbox cap + a ListMail LIMIT).

// mailSubjectMaxRunes caps the subject so the inbox list stays readable and a single mail can't carry an
// unbounded subject. The body is capped by textsan.CleanLine (MaxLineBytes). A package var for tests.
var mailSubjectMaxRunes = 60

// mailCommands returns the 8.7 mail command set, appended to the base table (registerCommands). Lower
// priority than movement/look/say (registered with the other comms commands) so abbreviation precedence
// is unchanged.
func mailCommands() []*Command {
	return []*Command{
		{Name: "mail", Run: cmdMail},
	}
}

// cmdMail dispatches the mail sub-commands. Bare `mail` lists the inbox; `mail read <n>` / `mail delete
// <n>` act on the nth inbox message; `mail send <name> <subject> | <body>` sends. A storeless shard
// reports "mail is unavailable" for every form (the never-fatal degradation). Never releases ownership,
// so dispatch prompts on return.
func cmdMail(c *Context) error {
	z, s := c.z, c.s
	if z.mailStore() == nil {
		c.Send("Mail is unavailable.")
		return nil
	}
	args := strings.Fields(strings.TrimSpace(c.Rest()))
	if len(args) == 0 {
		z.mailList(s)
		return nil
	}
	switch strings.ToLower(args[0]) {
	case "read":
		z.mailReadCmd(s, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c.Rest()), args[0])))
	case "delete", "del":
		z.mailDeleteCmd(s, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c.Rest()), args[0])))
	case "send":
		// The send tail is everything after "send": "<name> <subject> | <body>".
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c.Rest()), args[0]))
		z.mailSendCmd(s, rest)
	default:
		c.Send("Usage: mail | mail read <n> | mail delete <n> | mail send <name> <subject> | <body>")
	}
	return nil
}

// mailStore returns the shard's mail store, or nil on a bare zone / a storeless shard (mail disabled).
// Read-only; safe from any zone goroutine.
func (z *Zone) mailStore() MailStore {
	if z.shard == nil {
		return nil
	}
	return z.shard.mail
}

// mailList lists the player's inbox newest-first (off the zone goroutine). The store scopes the query to
// the authenticated player, so it can only ever return that player's own mail.
func (z *Zone) mailList(s *session) {
	// RATE-LIMIT (P8-A1): mail list spawns a goroutine + a Postgres query, so bound it on the same
	// per-author comms bucket mail send / channels / tells use — otherwise it is the cheapest unbounded
	// per-session async-I/O path (worse than the cached who roster: it hits PG). Enforced on the zone
	// goroutine BEFORE the goroutine spawns, so a throttled invocation never touches the store.
	if !z.commRateOK(s.character) {
		s.send(textFrame("You are checking your mail too fast."))
		return
	}
	store := z.mailStore()
	player := s.character
	out := s.out
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), mailIOTimeout)
		defer cancel()
		entries, err := store.ListMail(ctx, player)
		if err != nil {
			writeFrameTo(out, textFrame("Mail is unavailable."))
			return
		}
		writeFrameTo(out, textFrame(renderInbox(entries)))
	}()
}

// renderInbox formats an inbox listing: a header + one line per message, numbered by 1-based position
// (newest-first), each marked unread/read with sender + subject. The position is what `mail read <n>` /
// `mail delete <n>` reference. An empty inbox is a friendly notice.
func renderInbox(entries []MailEntry) string {
	if len(entries) == 0 {
		return "Your mailbox is empty."
	}
	var b strings.Builder
	b.WriteString("Your mailbox:")
	for i, e := range entries {
		b.WriteByte('\n')
		b.WriteString("  ")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		if e.Read {
			b.WriteString("[read]   ")
		} else {
			b.WriteString("[UNREAD] ")
		}
		b.WriteString(e.From)
		b.WriteString(": ")
		if e.Subject == "" {
			b.WriteString("(no subject)")
		} else {
			b.WriteString(e.Subject)
		}
	}
	return b.String()
}

// mailReadCmd reads the nth inbox message (1-based), marking it read (off the zone goroutine). A
// non-numeric / out-of-range position is a friendly notice. SCOPED to the authenticated player by the
// store (it can only read that player's own mail).
func (z *Zone) mailReadCmd(s *session, arg string) {
	pos, ok := parseMailPos(arg)
	if !ok {
		writeFrameTo(s.out, textFrame("Read which message? (mail read <n>)"))
		return
	}
	// RATE-LIMIT the async-PG read (see mailList) — AFTER the cheap arg parse, so a malformed `mail read`
	// that never hits the store also never spends a token.
	if !z.commRateOK(s.character) {
		s.send(textFrame("You are reading your mail too fast."))
		return
	}
	store := z.mailStore()
	player := s.character
	out := s.out
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), mailIOTimeout)
		defer cancel()
		entry, found, err := store.ReadMail(ctx, player, pos)
		if err != nil {
			writeFrameTo(out, textFrame("Mail is unavailable."))
			return
		}
		if !found {
			writeFrameTo(out, textFrame("You have no message "+strconv.Itoa(pos)+"."))
			return
		}
		writeFrameTo(out, textFrame(renderMail(entry)))
	}()
}

// renderMail formats a single read message: sender, sent time, subject, body. Subject/body are already
// sanitized (cleaned on send), so this is plain assembly.
func renderMail(e MailEntry) string {
	var b strings.Builder
	b.WriteString("From: ")
	b.WriteString(e.From)
	b.WriteByte('\n')
	b.WriteString("Date: ")
	b.WriteString(e.SentAt.UTC().Format("2006-01-02 15:04 UTC"))
	b.WriteByte('\n')
	b.WriteString("Subject: ")
	if e.Subject == "" {
		b.WriteString("(no subject)")
	} else {
		b.WriteString(e.Subject)
	}
	b.WriteByte('\n')
	b.WriteByte('\n')
	if e.Body == "" {
		b.WriteString("(no body)")
	} else {
		b.WriteString(e.Body)
	}
	return b.String()
}

// mailDeleteCmd deletes the nth inbox message (1-based, off the zone goroutine). SCOPED to the
// authenticated player by the store — a player cannot delete another player's mail.
func (z *Zone) mailDeleteCmd(s *session, arg string) {
	pos, ok := parseMailPos(arg)
	if !ok {
		writeFrameTo(s.out, textFrame("Delete which message? (mail delete <n>)"))
		return
	}
	// RATE-LIMIT the async-PG delete (see mailList) — same bucket, after the arg parse.
	if !z.commRateOK(s.character) {
		s.send(textFrame("You are deleting mail too fast."))
		return
	}
	store := z.mailStore()
	player := s.character
	out := s.out
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), mailIOTimeout)
		defer cancel()
		deleted, err := store.DeleteMail(ctx, player, pos)
		if err != nil {
			writeFrameTo(out, textFrame("Mail is unavailable."))
			return
		}
		if !deleted {
			writeFrameTo(out, textFrame("You have no message "+strconv.Itoa(pos)+"."))
			return
		}
		writeFrameTo(out, textFrame("Message "+strconv.Itoa(pos)+" deleted."))
	}()
}

// mailSendCmd sends a mail. The on-zone half captures the ENGINE-SET sender + sanitizes the recipient,
// subject, and body (P8-A2 / P8-A7), then the off-zone half resolves the recipient against the directory
// (refusing a never-seen name — P8-A8/recipient resolution), stores it, and — if the recipient is ONLINE
// — publishes a "you have new mail" notify to their gate (the comms-bus sink). The tail is
// "<name> <subject> | <body>"; without a '|' the whole subject tail is the subject and the body is empty.
func (z *Zone) mailSendCmd(s *session, rest string) {
	name, tail := split(rest)
	if name == "" {
		s.send(textFrame("Mail whom? (mail send <name> <subject> | <body>)"))
		return
	}
	// RECIPIENT TOKEN SANITIZE (P8-A8 / the tell-target discipline): the name becomes a directory key and
	// a NATS subject token (the new-mail notify rides commbus.TellSubject), so reject a control/subject
	// metacharacter token rather than inject it.
	target := safeTellTarget(name)
	if target == "" {
		s.send(textFrame("There is no player by that name."))
		return
	}
	if target == s.character {
		s.send(textFrame("You cannot mail yourself."))
		return
	}
	// RATE-LIMIT (P8-A1): mail send shares the per-author comms token bucket with channels/tells, so a
	// single SENDER's rate is bounded (no per-RECIPIENT inbox cap yet — FOLLOW-UPS). Enforced HERE, on the
	// zone goroutine, BEFORE the store write — a dropped send never reaches the inbox.
	if !z.commRateOK(s.character) {
		s.send(textFrame("You are sending mail too fast."))
		return
	}
	// Parse subject | body. SANITIZE both (P8-A7): CleanLine strips control/ANSI/IAC and length-caps; the
	// subject is additionally rune-capped for a readable inbox list.
	subject, body := splitMailBody(tail)
	subject = capRunes(textsan.CleanLine(subject), mailSubjectMaxRunes)
	body = textsan.CleanLine(body)

	from := s.character // ENGINE-SET sender (P8-A2): the live session identity, never a client field
	fromName := from
	if s.entity != nil {
		fromName = s.entity.Name()
	}
	store := z.mailStore()
	bus := z.commsBus()
	dir := z.dir()
	out := s.out
	log := z.log
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), mailIOTimeout)
		defer cancel()

		// RECIPIENT RESOLUTION (P8-A8 / never-silently-lost): refuse a never-seen name. The directory is
		// the epoch-authoritative existence check (a character that has ever logged in has a placement). A
		// directory MISS refuses; a directory ERROR degrades to accept-and-store (never lose mail on a
		// transient Redis blip); NO directory (single-shard/bare) accepts-and-stores (durable-always).
		online := false
		if dir != nil {
			shardID, found, derr := dir.PlayerShard(ctx, target)
			if derr == nil && !found {
				writeFrameTo(out, textFrame("There is no player by that name."))
				return
			}
			online = derr == nil && found && shardID != ""
		}

		id, err := store.SendMail(ctx, target, from, subject, body)
		if errors.Is(err, ErrMailboxFull) {
			writeFrameTo(out, textFrame(target+"'s mailbox is full."))
			return
		}
		if err != nil {
			writeFrameTo(out, textFrame("Mail is unavailable."))
			return
		}
		if log != nil {
			log.Debug("mail sent", "from", from, "to", target, "id", id)
		}
		writeFrameTo(out, textFrame("Mail sent to "+target+"."))

		// NEW-MAIL NOTIFY: ping the recipient's gate over the comms-bus tell subject (the SAME sink the gate
		// already renders, 8.2). The world is the source; the gate stays a sink. Never-fatal: a nil/disabled
		// bus is a clean no-op. The notify carries NO body — just a ping — so it can't leak the mail text to
		// a path that skips the recipient's own inbox scoping.
		//
		// `online` IS A MISNOMER: it means "has a placement", i.e. HAS EVER LOGGED IN — not "is currently
		// connected". The placement persists across logout, so this was already true of anyone who had been
		// handed off across shards; #320 widened it to every player, because the world now writes a placement
		// on login rather than only on a handoff. An offline recipient therefore now also gets a ping, which
		// the durable tell subject delivers on their next login. Harmless (they were going to see the mail
		// anyway) and redundant — but it is not what the name says. The right oracle for "currently
		// connected" is the presence roster; this path deliberately never consults presence for ROUTING
		// (P8-A4), but a notification gate is not routing. Tracked in #325.
		if online && bus != nil {
			_ = bus.Publish(ctx, commbus.TellSubject(target), commbus.Message{
				AuthorID:   from,
				AuthorName: fromName,
				Body:       "You have new mail from " + fromName + ".",
			})
		}
	}()
}

// splitMailBody splits a "<subject> | <body>" tail on the first '|'. With no '|', the whole tail is the
// subject and the body is empty (a clean one-line subject-only mail). Both halves are trimmed.
func splitMailBody(tail string) (subject, body string) {
	if i := strings.IndexByte(tail, '|'); i >= 0 {
		return strings.TrimSpace(tail[:i]), strings.TrimSpace(tail[i+1:])
	}
	return strings.TrimSpace(tail), ""
}

// parseMailPos parses a 1-based inbox position from a command arg. ok=false for a non-numeric or < 1
// value (the caller sends a usage notice).
func parseMailPos(arg string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// capRunes truncates s to at most n runes (rune-safe, never splitting a multi-byte rune). The body is
// already byte-capped by CleanLine; this is the additional subject readability cap.
func capRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// mailIOTimeout bounds one mail store call so a hung Postgres can't wedge the spawned goroutine. Mirrors
// presenceIOTimeout / saveIOTimeout.
const mailIOTimeout = 5 * time.Second

// sortMailNewestFirst is a stable newest-first sort by SentAt (a tie broken by id) used by the MemStore
// mail impl so its ListMail order matches the Postgres `ORDER BY sent_at DESC` — keeping the hermetic and
// gated tests asserting the same inbox order. The pgx store sorts in SQL; this mirrors it for the mem path.
func sortMailNewestFirst(entries []MailEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].SentAt.Equal(entries[j].SentAt) {
			return entries[i].ID > entries[j].ID
		}
		return entries[i].SentAt.After(entries[j].SentAt)
	})
}

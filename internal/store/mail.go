package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/double-nibble/telosmud/internal/world"
)

// mail.go is the pgx implementation of world.MailStore against the `mail` table (00007_mail.sql;
// docs/PHASE8-PLAN.md slice 8.7, P8-D6). Mail is durable, queryable player state — send is an INSERT,
// the inbox is a newest-first SELECT, read is a fetch+mark, delete removes one row. All four run OFF the
// zone goroutine (the mail command spawns a goroutine), so the blocking pool calls are fine.
//
// SECURITY (the access-control contract, enforced in the SQL — not just the command):
//   - from_player is whatever the caller passes; the WORLD passes the ENGINE-SET sender (s.character),
//     never a client field (P8-A2). The store does not derive or trust a sender from anywhere else.
//   - Every read/delete is PLAYER-SCOPED at the query: the recipient `player` is bound into a
//     `WHERE to_player = $player` predicate, and a player references a message by its 1-based INBOX
//     POSITION (resolved against THEIR OWN newest-first inbox via OFFSET), never by a guessable id. So a
//     player can only ever read/delete their own mail — the scoping lives in the predicate, and a
//     misbehaving command can never widen it.
//
// The newest-first order is `ORDER BY sent_at DESC, id DESC` (the index `mail (to_player, sent_at DESC)`
// serves the recipient scope + the sort; `id DESC` breaks a same-instant tie deterministically, matching
// the MemStore's tie-break so the hermetic and gated tests assert the same position->message mapping).

// SendMail inserts one message, minting the message id (a v4 UUID). `from` is the engine-set sender the
// caller resolved; subject/body are already sanitized. sent_at/read_at default in SQL (now() / NULL).
func (p *Pool) SendMail(ctx context.Context, to, from, subject, body string) (string, error) {
	id := uuid.New()
	// Cap the recipient's inbox: the count subquery + insert are one statement, which bounds the UNBOUNDED
	// growth vector. Under READ COMMITTED a concurrency burst can still over-shoot by (writers-1) rows (the
	// classic phantom), so the ceiling is "≈cap, may briefly reach cap+N" — fine for a resource guard, and
	// the per-sender rate limit throttles the fill. A HARD ceiling would need a counter row + ON CONFLICT or
	// SERIALIZABLE. RowsAffected()==0 means the inbox was full at statement time.
	tag, err := p.pool.Exec(ctx,
		`INSERT INTO mail (id, to_player, from_player, subject, body)
		 SELECT $1, $2, $3, $4, $5
		  WHERE (SELECT count(*) FROM mail WHERE to_player = $2) < $6`,
		id, to, from, subject, body, world.MailInboxCap)
	if err != nil {
		return "", fmt.Errorf("store: send mail to %q: %w", to, err)
	}
	if tag.RowsAffected() == 0 {
		// Inbox at cap. Retention sweep (docs/REMAINING.md §1): evict the OLDEST READ message to make room,
		// so a full inbox of already-read mail can't wedge new delivery on spam. Only READ mail is evicted —
		// an unread message is never silently dropped — so an inbox full of UNREAD mail still refuses (the
		// sender is told it's full). The insert re-runs its own count-guard, so the two statements can't
		// overshoot the cap even under concurrency.
		if evicted, derr := p.evictOldestRead(ctx, to); derr != nil {
			return "", derr
		} else if !evicted {
			return "", world.ErrMailboxFull // full of unread mail: nothing to reclaim
		}
		tag, err = p.pool.Exec(ctx,
			`INSERT INTO mail (id, to_player, from_player, subject, body)
			 SELECT $1, $2, $3, $4, $5
			  WHERE (SELECT count(*) FROM mail WHERE to_player = $2) < $6`,
			id, to, from, subject, body, world.MailInboxCap)
		if err != nil {
			return "", fmt.Errorf("store: send mail to %q (after sweep): %w", to, err)
		}
		if tag.RowsAffected() == 0 {
			return "", world.ErrMailboxFull // a concurrent sender re-filled the freed slot; caller may retry
		}
	}
	return id.String(), nil
}

// evictOldestRead deletes the single oldest READ message in `player`'s inbox, returning whether a row was
// removed. It is the retention sweep's reclaim step: it never touches UNREAD mail, so it can only reclaim
// slots the recipient has already seen. Oldest-first (`sent_at ASC, id ASC`) mirrors the newest-first inbox
// order so the message evicted is the one at the BOTTOM of the reader's list.
func (p *Pool) evictOldestRead(ctx context.Context, player string) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM mail
		  WHERE id = (
		          SELECT id FROM mail
		           WHERE to_player = $1 AND read_at IS NOT NULL
		           ORDER BY sent_at ASC, id ASC
		           LIMIT 1
		        )`, player)
	if err != nil {
		return false, fmt.Errorf("store: evict oldest read mail for %q: %w", player, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListMail returns `player`'s inbox newest-first, scoped to to_player = player (CITEXT, case-insensitive).
// An empty inbox is a nil slice (not an error). read_at IS NOT NULL maps to MailEntry.Read.
func (p *Pool) ListMail(ctx context.Context, player string) ([]world.MailEntry, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, to_player, from_player, subject, body, sent_at, read_at IS NOT NULL
		   FROM mail
		  WHERE to_player = $1
		  ORDER BY sent_at DESC, id DESC
		  LIMIT $2`, player, world.MailInboxCap)
	if err != nil {
		return nil, fmt.Errorf("store: list mail for %q: %w", player, err)
	}
	defer rows.Close()
	var out []world.MailEntry
	for rows.Next() {
		var (
			id   uuid.UUID
			e    world.MailEntry
			read bool
		)
		if err := rows.Scan(&id, &e.To, &e.From, &e.Subject, &e.Body, &e.SentAt, &read); err != nil {
			return nil, fmt.Errorf("store: scan mail for %q: %w", player, err)
		}
		e.ID = id.String()
		e.Read = read
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list mail for %q: %w", player, err)
	}
	return out, nil
}

// ReadMail fetches the message at 1-based inbox position `pos` for `player` (newest-first), marks it read
// (sets read_at if NULL), and returns it. found=false (nil error) when pos is out of range. The position
// is resolved against the player's OWN scoped inbox (OFFSET on a `WHERE to_player = $player` query), and
// the UPDATE re-asserts `to_player = $player` — double-scoped, so no id another player owns is reachable.
func (p *Pool) ReadMail(ctx context.Context, player string, pos int) (world.MailEntry, bool, error) {
	if pos < 1 {
		return world.MailEntry{}, false, nil
	}
	// Resolve the position to a concrete id WITHIN the player's scoped, newest-first inbox, then mark it
	// read and return it in one round-trip. The UPDATE re-scopes by to_player so the access control is on
	// the mutating statement too, not only the position lookup.
	var (
		id   uuid.UUID
		e    world.MailEntry
		read bool
	)
	err := p.pool.QueryRow(ctx,
		`UPDATE mail
		    SET read_at = COALESCE(read_at, now())
		  WHERE id = (
		          SELECT id FROM mail
		           WHERE to_player = $1
		           ORDER BY sent_at DESC, id DESC
		           OFFSET $2 LIMIT 1
		        )
		    AND to_player = $1
		 RETURNING id, to_player, from_player, subject, body, sent_at, read_at IS NOT NULL`,
		player, pos-1).
		Scan(&id, &e.To, &e.From, &e.Subject, &e.Body, &e.SentAt, &read)
	if errors.Is(err, pgx.ErrNoRows) {
		return world.MailEntry{}, false, nil // pos out of range for this player's inbox
	}
	if err != nil {
		return world.MailEntry{}, false, fmt.Errorf("store: read mail %d for %q: %w", pos, player, err)
	}
	e.ID = id.String()
	e.Read = read
	return e, true, nil
}

// DeleteMail removes the message at 1-based inbox position `pos` for `player` (newest-first). deleted=false
// (nil error) when pos is out of range. The position is resolved within the player's scoped inbox and the
// DELETE re-asserts `to_player = $player` — double-scoped, so a player can only delete their own mail.
func (p *Pool) DeleteMail(ctx context.Context, player string, pos int) (bool, error) {
	if pos < 1 {
		return false, nil
	}
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM mail
		  WHERE id = (
		          SELECT id FROM mail
		           WHERE to_player = $1
		           ORDER BY sent_at DESC, id DESC
		           OFFSET $2 LIMIT 1
		        )
		    AND to_player = $1`,
		player, pos-1)
	if err != nil {
		return false, fmt.Errorf("store: delete mail %d for %q: %w", pos, player, err)
	}
	return tag.RowsAffected() > 0, nil
}

// Compile-time assertion that *Pool satisfies world.MailStore.
var _ world.MailStore = (*Pool)(nil)

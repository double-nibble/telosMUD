package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/world"
)

// TestMailInboxCapPersists pins the ATOMIC pgx inbox cap (the production path the hermetic MemStore test
// can't): SendMail refuses once the recipient holds world.MailInboxCap rows (the count subquery + insert are
// one statement, so no TOCTOU), returning ErrMailboxFull, and ListMail bounds its render at the cap.
func TestMailInboxCapPersists(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	stamp := time.Now().Format("150405.000000")
	victim := "MailCap-" + stamp
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM mail WHERE to_player = $1`, victim)
	})

	for i := 0; i < world.MailInboxCap; i++ {
		if _, err := p.SendMail(ctx, victim, "Spammer", "s", "b"); err != nil {
			t.Fatalf("send %d (under cap): %v", i, err)
		}
	}
	if _, err := p.SendMail(ctx, victim, "Spammer", "s", "b"); !errors.Is(err, world.ErrMailboxFull) {
		t.Fatalf("send past cap: err = %v, want ErrMailboxFull", err)
	}
	inbox, err := p.ListMail(ctx, victim)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != world.MailInboxCap {
		t.Fatalf("ListMail returned %d rows, want the cap %d", len(inbox), world.MailInboxCap)
	}
}

// mail_test.go holds the GATED Postgres integration test for the pgx MailStore (00007_mail.sql;
// docs/PHASE8-PLAN.md slice 8.7). Like TestCharacterCRUD it requires TELOS_TEST_DSN and t.Skip's when
// unset (a hermetic `go test ./...` passes; CI / a dev with the DSN runs it). It pins the SQL the
// hermetic MemStore mail tests can't: the real INSERT/SELECT/UPDATE/DELETE, the newest-first order, the
// read-marks-read, and — the security obligation — the PLAYER-SCOPED read/delete (a second player cannot
// reach the first's mail by position or id, enforced by the `WHERE to_player = $player` predicate).

// TestMailCRUD exercises the pgx MailStore against a real database: send inserts; list returns
// newest-first with the unread state; read fetches + marks read; delete removes one; and the access
// control holds (player B's read/delete cannot reach player A's mail).
func TestMailCRUD(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	stamp := time.Now().Format("150405.000000")
	alice := "MailAlice-" + stamp
	bob := "MailBob-" + stamp
	mallory := "MailMallory-" + stamp
	t.Cleanup(func() {
		// Hard-delete every row addressed to the test recipients so re-runs start clean.
		_, _ = p.pool.Exec(context.Background(),
			`DELETE FROM mail WHERE to_player IN ($1,$2,$3)`, alice, bob, mallory)
	})

	// Bob sends Alice two messages; the second is newer, so it lists first.
	if _, err := p.SendMail(ctx, alice, bob, "first", "body one"); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // ensure a distinct sent_at so the newest-first order is unambiguous
	if _, err := p.SendMail(ctx, alice, bob, "second", "body two"); err != nil {
		t.Fatalf("send 2: %v", err)
	}

	// List: newest-first, both UNREAD, from Bob.
	inbox, err := p.ListMail(ctx, alice)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(inbox) != 2 {
		t.Fatalf("inbox len = %d, want 2", len(inbox))
	}
	if inbox[0].Subject != "second" || inbox[1].Subject != "first" {
		t.Fatalf("inbox order = [%q,%q], want newest-first [second,first]", inbox[0].Subject, inbox[1].Subject)
	}
	if inbox[0].Read || inbox[1].Read || inbox[0].From != bob {
		t.Fatalf("inbox state wrong: %+v", inbox)
	}

	// Read position 1 (the newest, "second"): returns it and marks it read.
	entry, found, err := p.ReadMail(ctx, alice, 1)
	if err != nil || !found {
		t.Fatalf("read 1: found=%v err=%v", found, err)
	}
	if entry.Subject != "second" || entry.Body != "body two" || !entry.Read {
		t.Fatalf("read entry = %+v, want subject=second body='body two' read=true", entry)
	}
	// Re-list: position 1 is now read; position 2 still unread.
	inbox, _ = p.ListMail(ctx, alice)
	if !inbox[0].Read || inbox[1].Read {
		t.Fatalf("after read, read-state = [%v,%v], want [true,false]", inbox[0].Read, inbox[1].Read)
	}

	// Read out of range: found=false, no error.
	if _, found, err := p.ReadMail(ctx, alice, 99); err != nil || found {
		t.Fatalf("read out-of-range: found=%v err=%v, want found=false", found, err)
	}

	// ACCESS CONTROL (the security obligation): Mallory's read/delete at position 1 cannot reach Alice's
	// mail — Mallory's inbox is empty, so found=false / deleted=false, and Alice's mail is untouched.
	if _, found, err := p.ReadMail(ctx, mallory, 1); err != nil || found {
		t.Fatalf("Mallory read of Alice's mail: found=%v err=%v, want found=false (scoping breach)", found, err)
	}
	if deleted, err := p.DeleteMail(ctx, mallory, 1); err != nil || deleted {
		t.Fatalf("Mallory delete of Alice's mail: deleted=%v err=%v, want deleted=false (scoping breach)", deleted, err)
	}
	if inbox, _ := p.ListMail(ctx, alice); len(inbox) != 2 {
		t.Fatalf("Alice's inbox len = %d after Mallory's attempts, want 2 intact", len(inbox))
	}

	// Delete Alice's position 2 ("first"): one left.
	deleted, err := p.DeleteMail(ctx, alice, 2)
	if err != nil || !deleted {
		t.Fatalf("delete 2: deleted=%v err=%v", deleted, err)
	}
	inbox, _ = p.ListMail(ctx, alice)
	if len(inbox) != 1 || inbox[0].Subject != "second" {
		t.Fatalf("after delete, inbox = %+v, want only 'second'", inbox)
	}

	// Delete out of range: deleted=false, no error.
	if deleted, err := p.DeleteMail(ctx, alice, 99); err != nil || deleted {
		t.Fatalf("delete out-of-range: deleted=%v err=%v, want deleted=false", deleted, err)
	}
}

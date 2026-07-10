package world

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
)

// TestMailInboxCap pins the anti-griefing/storage cap: SendMail refuses once the recipient is at
// MailInboxCap (per-recipient), returning the distinct ErrMailboxFull so the caller renders "mailbox full"
// (not "unavailable"); a different recipient is unaffected, and freeing a slot re-opens delivery.
func TestMailInboxCap(t *testing.T) {
	ms := NewMemStore()
	ctx := context.Background()
	for i := 0; i < MailInboxCap; i++ {
		if _, err := ms.SendMail(ctx, "Victim", "Sender", "s", "b"); err != nil {
			t.Fatalf("send %d (under cap) failed: %v", i, err)
		}
	}
	if _, err := ms.SendMail(ctx, "Victim", "Sender", "s", "b"); !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("send past cap: err = %v, want ErrMailboxFull", err)
	}
	if _, err := ms.SendMail(ctx, "Other", "Sender", "s", "b"); err != nil {
		t.Fatalf("cap is per-recipient: send to Other failed: %v", err)
	}
	if _, err := ms.DeleteMail(ctx, "Victim", 1); err != nil {
		t.Fatalf("delete to free a slot: %v", err)
	}
	if _, err := ms.SendMail(ctx, "Victim", "Sender", "s", "b"); err != nil {
		t.Fatalf("send after freeing a slot failed: %v", err)
	}
}

// TestMailInboxCapEvictsOldestRead pins the retention sweep (docs/REMAINING.md §1): a FULL inbox that holds
// at least one READ message accepts new mail by evicting the oldest read one, so spam can't wedge delivery;
// but an inbox full of UNREAD mail still refuses (no unread message is ever silently dropped).
func TestMailInboxCapEvictsOldestRead(t *testing.T) {
	ms := NewMemStore()
	ctx := context.Background()
	for i := 0; i < MailInboxCap; i++ {
		if _, err := ms.SendMail(ctx, "Victim", "Sender", "s", "b"); err != nil {
			t.Fatalf("fill %d failed: %v", i, err)
		}
	}
	// All unread => the sweep finds nothing to reclaim => still refuses.
	if _, err := ms.SendMail(ctx, "Victim", "Sender", "s", "b"); !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("full-of-unread send: err = %v, want ErrMailboxFull", err)
	}
	// Read the OLDEST message (bottom of the newest-first inbox), then a new send must succeed by evicting it.
	if _, ok, err := ms.ReadMail(ctx, "Victim", MailInboxCap); err != nil || !ok {
		t.Fatalf("mark oldest read: ok=%v err=%v", ok, err)
	}
	if _, err := ms.SendMail(ctx, "Victim", "Sender", "fresh", "b"); err != nil {
		t.Fatalf("send after a read message exists should evict + succeed: %v", err)
	}
	inbox, err := ms.ListMail(ctx, "Victim")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(inbox) != MailInboxCap {
		t.Fatalf("inbox size after evict+send = %d, want %d (stayed at cap)", len(inbox), MailInboxCap)
	}
	if inbox[0].Subject != "fresh" {
		t.Fatalf("newest message subject = %q, want the freshly delivered one", inbox[0].Subject)
	}
}

// mail_test.go is the hermetic Phase-8.7 mail journey (docs/PHASE8-PLAN.md 8.7): send/list/read/delete
// round-trip, offline-mail-on-login, the new-mail notify to an online recipient, the read/delete
// access-control scoping (security), from-player engine-set (security), and the storeless degrade — all
// against the MemStore mail impl (no live Postgres). The gated real-PG round-trip + scoping is in
// internal/store/mail_test.go (like TestCharacterCRUD).

// mailShard builds a single-shard world with the demo pack and a MemStore-backed mail inbox, returning the
// shard, its zone, and the MemStore so a test can both drive commands and inspect the durable rows. No
// directory => `mail send` accepts-and-stores any target (the durable-always posture), which is what the
// hermetic offline/round-trip tests want; the recipient-refusal path is exercised with a directory below.
func mailShard(t *testing.T) (*Shard, *Zone, *MemStore) {
	t.Helper()
	ms := NewMemStore()
	sh := NewDemoShard().WithMail(ms)
	return sh, sh.Zone(), ms
}

// waitMailLine waits for an output frame containing substr, draining prompt/other frames. The mail
// commands answer on a spawned goroutine, so the assertion must poll s.out (not read synchronously).
func waitMailLine(t *testing.T, s *session, substr string) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-s.out:
			if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), substr) {
				return o.GetMarkup()
			}
		case <-deadline:
			t.Fatalf("player %s: timed out waiting for %q", s.character, substr)
			return ""
		}
	}
}

// TestMailSendListReadDeleteRoundTrip is the core 8.7 round-trip: a player sends mail to another, who
// lists it (UNREAD), reads it (marked read, body shown), and deletes it (gone from the inbox).
//
// NOTE: since #65 rate-limited list/read/delete, Bob's FIVE mail actions below sit at EXACTLY the
// default burst (commRateBurst=5). A sixth Bob mail action (or a Bob say/tell interleaved) would spend
// past the burst and hit the throttle — if you add a step here, raise the budget with
// `mailShard`+`.WithCommsRate(...)`. The throttle behavior itself is pinned in TestMailReadRateLimited.
func TestMailSendListReadDeleteRoundTrip(t *testing.T) {
	_, z, _ := mailShard(t)
	alice := newTestPlayerEntity(z, "Alice")
	bob := newTestPlayerEntity(z, "Bob")

	z.dispatch(alice, "mail send Bob Hello there | how are you")
	waitMailLine(t, alice, "Mail sent to Bob.")

	// Bob lists: one UNREAD message from Alice with the subject.
	z.dispatch(bob, "mail")
	list := waitMailLine(t, bob, "Your mailbox:")
	if !strings.Contains(list, "[UNREAD]") || !strings.Contains(list, "Alice") || !strings.Contains(list, "Hello there") {
		t.Fatalf("inbox listing wrong: %q", list)
	}

	// Bob reads message 1: the body shows, and it is now marked read.
	z.dispatch(bob, "mail read 1")
	read := waitMailLine(t, bob, "how are you")
	if !strings.Contains(read, "From: Alice") || !strings.Contains(read, "Subject: Hello there") {
		t.Fatalf("read render wrong: %q", read)
	}
	z.dispatch(bob, "mail")
	relist := waitMailLine(t, bob, "Your mailbox:")
	if strings.Contains(relist, "[UNREAD]") {
		t.Fatalf("message still UNREAD after read: %q", relist)
	}

	// Bob deletes message 1: the inbox is empty.
	z.dispatch(bob, "mail delete 1")
	waitMailLine(t, bob, "Message 1 deleted.")
	z.dispatch(bob, "mail")
	waitMailLine(t, bob, "Your mailbox is empty.")
}

// TestMailReadRateLimited pins #65: mail list/read/delete each spawn a goroutine + a Postgres query, so
// they share the per-author comms token bucket (mail send already did). Once the burst is spent, the next
// mail read/list is throttled SYNCHRONOUSLY — the store is never touched — instead of being an unbounded
// per-session async-PG spam path.
func TestMailReadRateLimited(t *testing.T) {
	ms := NewMemStore()
	// Burst 2, effectively no refill for the test window: Bob gets exactly two mail actions.
	sh := NewDemoShard().WithMail(ms).WithCommsRate(2, time.Minute)
	z := sh.Zone()
	alice := newTestPlayerEntity(z, "Alice")
	bob := newTestPlayerEntity(z, "Bob")

	// Alice mails Bob so he has something to read (spends ALICE's bucket, not Bob's — buckets are per-author).
	z.dispatch(alice, "mail send Bob Subj | body one")
	waitMailLine(t, alice, "Mail sent to Bob.")

	// Bob's two mail actions spend his 2-token burst.
	z.dispatch(bob, "mail")
	waitMailLine(t, bob, "Your mailbox:")
	z.dispatch(bob, "mail read 1")
	waitMailLine(t, bob, "body one")

	// The THIRD is throttled — the guard runs on the zone goroutine before any goroutine/PG.
	z.dispatch(bob, "mail")
	waitMailLine(t, bob, "checking your mail too fast")
}

// TestMailOfflineThenReadOnLogin proves an OFFLINE recipient (never joined / no live session) receives
// mail, stored durably, and reads it when they next come online via `mail`. The mail store is independent
// of session presence — the message lands in the inbox at send and waits.
func TestMailOfflineThenReadOnLogin(t *testing.T) {
	_, z, ms := mailShard(t)
	alice := newTestPlayerEntity(z, "Alice")

	// Carol is OFFLINE (no session in this zone). Alice mails her.
	z.dispatch(alice, "mail send Carol While you slept | a secret")
	waitMailLine(t, alice, "Mail sent to Carol.")

	// The durable inbox has it even though Carol was never online.
	inbox, err := ms.ListMail(context.Background(), "Carol")
	if err != nil || len(inbox) != 1 {
		t.Fatalf("offline inbox = %v (err %v), want 1 message", inbox, err)
	}

	// Carol logs in (a fresh session joins) and reads it.
	carol := newTestPlayerEntity(z, "Carol")
	z.dispatch(carol, "mail")
	list := waitMailLine(t, carol, "Your mailbox:")
	if !strings.Contains(list, "While you slept") {
		t.Fatalf("Carol's on-login inbox missing the offline mail: %q", list)
	}
}

// TestMailNewMailNotifyToOnlineRecipient proves a "you have new mail" ping reaches an ONLINE recipient's
// gate over the comms bus tell subject (the same sink the gate renders). The world is the source; we
// observe the gate-side subscriber. A directory marks the recipient online so the notify fires.
func TestMailNewMailNotifyToOnlineRecipient(t *testing.T) {
	ms := NewMemStore()
	wbus, gate := commbus.NewWorldBus()
	t.Cleanup(func() { _ = wbus.Close() })
	dir := newMailDir(map[string]string{"Bob": "shard-1"}) // Bob is online on shard-1

	sh := newShard([]string{"midgaard"}, "midgaard", "addr", dir, nil).
		WithComms(wbus).
		WithMail(ms)
	z := sh.Zone()

	// Bob's gate subscribes its concrete tell subject (the 8.2 sink).
	notifies := make(chan commbus.Message, 4)
	sub, err := gate.Subscribe(commbus.TellSubject("Bob"), func(m commbus.Message) { notifies <- m })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	alice := newTestPlayerEntity(z, "Alice")
	z.dispatch(alice, "mail send Bob Ping | check this")
	waitMailLine(t, alice, "Mail sent to Bob.")

	select {
	case m := <-notifies:
		if !strings.Contains(m.Body, "new mail") || !strings.Contains(m.Body, "Alice") {
			t.Fatalf("new-mail notify body wrong: %q", m.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("online recipient never received the new-mail notify")
	}
}

// TestMailReadDeleteAccessControl is the SECURITY scoping test (read/delete): player B cannot read or
// delete player A's mail. Each player's read/delete is scoped to THEIR OWN inbox by position — B's
// `mail read 1` reaches B's (empty) inbox, never A's message. The store-level scoping is what enforces
// this (the command only ever passes the authenticated player); the gated PG test pins the SQL WHERE.
func TestMailReadDeleteAccessControl(t *testing.T) {
	_, z, ms := mailShard(t)
	attacker := newTestPlayerEntity(z, "Mallory")

	// Alice has one message (sent by Bob, directly into the store — Alice need not be online).
	if _, err := ms.SendMail(context.Background(), "Alice", "Bob", "private", "for alice only"); err != nil {
		t.Fatal(err)
	}

	// Mallory tries to read/delete "message 1" — but it resolves against MALLORY's inbox, which is empty.
	z.dispatch(attacker, "mail read 1")
	waitMailLine(t, attacker, "You have no message 1.")
	z.dispatch(attacker, "mail delete 1")
	waitMailLine(t, attacker, "You have no message 1.")

	// Alice's message is untouched (not read, not deleted).
	inbox, err := ms.ListMail(context.Background(), "Alice")
	if err != nil || len(inbox) != 1 {
		t.Fatalf("Alice inbox = %v (err %v), want her message intact", inbox, err)
	}
	if inbox[0].Read {
		t.Fatal("attacker's `mail read 1` marked Alice's mail read (scoping bypassed)")
	}
}

// TestMailFromPlayerIsEngineSet is the SECURITY impersonation test (P8-A2): the stored sender is the
// authenticated session identity, NOT anything in the input. Even when the input subject/body try to
// spoof a "From:", the durable row's from_player is the real sender.
func TestMailFromPlayerIsEngineSet(t *testing.T) {
	_, z, ms := mailShard(t)
	alice := newTestPlayerEntity(z, "Alice")

	// Alice sends with a forged-looking subject; the engine-set sender is still Alice.
	z.dispatch(alice, "mail send Bob From: Admin | trust me")
	waitMailLine(t, alice, "Mail sent to Bob.")

	inbox, err := ms.ListMail(context.Background(), "Bob")
	if err != nil || len(inbox) != 1 {
		t.Fatalf("Bob inbox = %v (err %v), want 1", inbox, err)
	}
	if inbox[0].From != "Alice" {
		t.Fatalf("stored from_player = %q, want the engine-set sender Alice (impersonation gate failed)", inbox[0].From)
	}
}

// TestMailDisabledWithoutStore proves a storeless shard degrades cleanly: every mail form reports "mail is
// unavailable", never a crash.
func TestMailDisabledWithoutStore(t *testing.T) {
	z := NewDemoShard().Zone() // no WithMail => mail disabled
	s := newTestPlayerEntity(z, "Alice")

	for _, form := range []string{"mail", "mail read 1", "mail delete 1", "mail send Bob hi"} {
		z.dispatch(s, form)
		waitMailLine(t, s, "Mail is unavailable.")
	}
}

// TestMailRefusesUnknownRecipient proves a `mail send` to a never-seen name is REFUSED to the sender (the
// directory existence check), not silently lost. A directory that knows only Bob refuses a send to Nobody.
func TestMailRefusesUnknownRecipient(t *testing.T) {
	ms := NewMemStore()
	dir := newMailDir(map[string]string{"Bob": "shard-1"}) // only Bob has ever logged in
	sh := newShard([]string{"midgaard"}, "midgaard", "addr", dir, nil).WithMail(ms)
	z := sh.Zone()
	alice := newTestPlayerEntity(z, "Alice")

	z.dispatch(alice, "mail send Nobody hi there")
	waitMailLine(t, alice, "There is no player by that name.")

	// Nothing was stored for the bogus recipient.
	inbox, _ := ms.ListMail(context.Background(), "Nobody")
	if len(inbox) != 0 {
		t.Fatalf("a refused send still stored mail: %v", inbox)
	}

	// A send to a KNOWN recipient is accepted.
	z.dispatch(alice, "mail send Bob hi there")
	waitMailLine(t, alice, "Mail sent to Bob.")
}

// TestMailCannotMailSelf proves you cannot mail yourself (a trivial guard).
func TestMailCannotMailSelf(t *testing.T) {
	_, z, _ := mailShard(t)
	alice := newTestPlayerEntity(z, "Alice")
	z.dispatch(alice, "mail send Alice note to self")
	waitMailLine(t, alice, "You cannot mail yourself.")
}

// --- a minimal directory stub for mail recipient resolution + online-notify ------------------

// mailDir is a tiny Locator that answers PlayerShard from a fixed name->shard map (a player NOT in the
// map is found=false => the recipient is refused). The other Locator methods are unused by the mail path
// and return zero values. It exists so the recipient-resolution + online-notify paths are testable without
// a real Redis directory.
type mailDir struct {
	placements map[string]string // player id -> shard id (presence == "ever logged in")
}

func newMailDir(placements map[string]string) *mailDir {
	return &mailDir{placements: placements}
}

func (d *mailDir) PlayerShard(_ context.Context, playerID string) (string, bool, error) {
	shardID, ok := d.placements[playerID]
	return shardID, ok, nil
}

func (d *mailDir) ShardForZone(context.Context, string) (string, error)     { return "", nil }
func (d *mailDir) EndpointForShard(context.Context, string) (string, error) { return "", nil }
func (d *mailDir) RegisterPlacement(context.Context, string, string, string, uint64) (bool, error) {
	return true, nil
}

func (d *mailDir) SetPlayerShard(context.Context, string, string, string, uint64) (bool, error) {
	return true, nil
}
func (d *mailDir) PlayerEpoch(context.Context, string) (uint64, bool, error) { return 0, false, nil }

var _ Locator = (*mailDir)(nil)

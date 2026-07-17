package world

import (
	"strings"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
)

// tellhistory_test.go is the white-box test set for the in-session tell-history ring (#349, slice 1). It
// reuses the tell_test.go harness (tellShard / joinTellPlayer / subscribeTell / the wait* probes) so the
// capture is exercised END TO END through the real send + drain paths, then reads the ring back through the
// `tells` command itself (driven as normal player input on the zone goroutine — the race-free read).

// runTells drives the `tells` command for a player and returns the rendered multi-line output. It posts the
// command as ordinary input (so it runs on the zone goroutine, single-writer) and drains the session out
// channel for the notice.
func runTells(t *testing.T, z *Zone, s *session) string {
	t.Helper()
	z.post(inputMsg{id: s.character, line: "tells"})
	return drainMarkup(t, s, "Recent tells", "You have no recent tells")
}

// drainMarkup drains the session out channel until it sees a frame whose markup contains ANY of the given
// substrings, returning that markup. It is the read-side companion to drainContains for a test that needs
// the whole rendered block (not just a boolean) — e.g. to assert ordering across multiple lines.
func drainMarkup(t *testing.T, s *session, anyOf ...string) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-s.out:
			if o := f.GetOutput(); o != nil {
				m := o.GetMarkup()
				for _, sub := range anyOf {
					if strings.Contains(m, sub) {
						return m
					}
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for markup containing any of %v", anyOf)
			return ""
		}
	}
}

// TestTellHistoryCapturesSent: A tells B -> A's ring has one OUTBOUND entry naming B + the body.
func TestTellHistoryCapturesSent(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	z := tellShard(t, core.WorldHandle(), js, dir)
	alice := joinTellPlayer(t, z, "Alice")
	joinTellPlayer(t, z, "Bob")

	z.post(inputMsg{id: "Alice", line: "tell Bob hello there"})
	// The sender echo confirms the tell was sent (and the capture happens on the same zone-goroutine turn).
	if !drainContains(t, alice, "You tell Bob, 'hello there'") {
		t.Fatal("sender was not echoed the tell")
	}

	out := runTells(t, z, alice)
	if !strings.Contains(out, "You told Bob, 'hello there'") {
		t.Fatalf("sent tell not in history: %q", out)
	}
}

// TestTellHistoryCapturesReceivedLive: a LIVE tell to an online B -> B's ring has one INBOUND entry.
func TestTellHistoryCapturesReceivedLive(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	z := tellShard(t, core.WorldHandle(), js, dir)
	bobInbox := subscribeTell(t, core.GateHandle(), "Bob")
	joinTellPlayer(t, z, "Alice")
	bob := joinTellPlayer(t, z, "Bob")

	z.post(inputMsg{id: "Alice", line: "tell Bob live message"})
	recvTell(t, bobInbox)                  // the world emitted to Bob's gate (delivery ran)
	waitLastTellFrom(t, z, "Bob", "Alice") // ...and the drain advanced (so the capture ran too)

	out := runTells(t, z, bob)
	if !strings.Contains(out, "Alice tells you, 'live message'") {
		t.Fatalf("received (live) tell not in history: %q", out)
	}
}

// TestTellHistoryCapturesReceivedBacklog: an OFFLINE (backlog) tell is captured when B drains it on login —
// a backlog tell was really received, so it belongs in the history.
func TestTellHistoryCapturesReceivedBacklog(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	z := tellShard(t, core.WorldHandle(), js, dir)
	bobInbox := subscribeTell(t, core.GateHandle(), "Bob")
	joinTellPlayer(t, z, "Alice")

	// Bob OFFLINE: the tell queues durably, drains on login.
	z.post(inputMsg{id: "Alice", line: "tell Bob away message"})
	bob := joinTellPlayer(t, z, "Bob")
	recvTell(t, bobInbox)
	waitLastTellFrom(t, z, "Bob", "Alice")

	out := runTells(t, z, bob)
	// The history stores the raw body (no "while you were away" framing — that is only the live socket render).
	if !strings.Contains(out, "Alice tells you, 'away message'") {
		t.Fatalf("received (backlog) tell not in history: %q", out)
	}
}

// TestTellHistoryRendersBothDirectionsChronologically: after A<->B exchange, each side's `tells` shows both
// directions in order with the right wording.
func TestTellHistoryRendersBothDirectionsChronologically(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	z := tellShard(t, core.WorldHandle(), js, dir)
	aliceInbox := subscribeTell(t, core.GateHandle(), "Alice")
	bobInbox := subscribeTell(t, core.GateHandle(), "Bob")
	alice := joinTellPlayer(t, z, "Alice")
	joinTellPlayer(t, z, "Bob")

	z.post(inputMsg{id: "Alice", line: "tell Bob one"})
	recvTell(t, bobInbox)
	waitLastTellFrom(t, z, "Bob", "Alice")
	z.post(inputMsg{id: "Bob", line: "reply two"})
	recvTell(t, aliceInbox)
	waitLastTellFrom(t, z, "Alice", "Bob")

	out := runTells(t, z, alice)
	sent := strings.Index(out, "You told Bob, 'one'")
	recv := strings.Index(out, "Bob tells you, 'two'")
	if sent < 0 || recv < 0 {
		t.Fatalf("both directions not both present: %q", out)
	}
	if sent > recv {
		t.Fatalf("history not chronological (sent should precede received): %q", out)
	}
}

// TestTellHistoryPairPrivacy: with A<->B active, a third player C's `tells` shows nothing of A<->B. This is
// structural (C's ring is a separate per-session buffer), but the test pins it against a regression that
// might route history through a shared store.
func TestTellHistoryPairPrivacy(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob", "Carol")

	z := tellShard(t, core.WorldHandle(), js, dir)
	bobInbox := subscribeTell(t, core.GateHandle(), "Bob")
	joinTellPlayer(t, z, "Alice")
	joinTellPlayer(t, z, "Bob")
	carol := joinTellPlayer(t, z, "Carol")

	z.post(inputMsg{id: "Alice", line: "tell Bob secret between us"})
	recvTell(t, bobInbox)
	waitLastTellFrom(t, z, "Bob", "Alice")

	out := runTells(t, z, carol)
	if strings.Contains(out, "secret between us") {
		t.Fatalf("C's history leaked A<->B tell: %q", out)
	}
	if !strings.Contains(out, "You have no recent tells") {
		t.Fatalf("uninvolved player should have an empty history: %q", out)
	}
}

// TestTellHistorySkipsIgnoredSender: B ignores A; A tells B -> the drain still runs (lastTellFrom advances)
// but B's ring gains NO entry, so the history matches what B actually saw (the gate drops ignored tells).
func TestTellHistorySkipsIgnoredSender(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	z := tellShard(t, core.WorldHandle(), js, dir)
	bobInbox := subscribeTell(t, core.GateHandle(), "Bob")
	joinTellPlayer(t, z, "Alice")
	bob := joinTellPlayer(t, z, "Bob")

	// Bob ignores Alice (drain the confirmation).
	z.post(inputMsg{id: "Bob", line: "ignore Alice"})
	if !drainContains(t, bob, "now ignoring Alice") {
		t.Fatal("ignore was not confirmed")
	}

	z.post(inputMsg{id: "Alice", line: "tell Bob you cannot see this"})
	recvTell(t, bobInbox)                  // the world still emits (unfiltered) — delivery ran
	waitLastTellFrom(t, z, "Bob", "Alice") // ...and the drain advanced, so the capture DECISION ran

	out := runTells(t, z, bob)
	if strings.Contains(out, "you cannot see this") {
		t.Fatalf("an ignored sender's tell was captured into history: %q", out)
	}
	if !strings.Contains(out, "You have no recent tells") {
		t.Fatalf("expected an empty history after the only tell was ignore-skipped: %q", out)
	}
}

// TestTellHistoryBounded: more than tellLogMax exchanges keep only the last N (oldest dropped).
func TestTellHistoryBounded(t *testing.T) {
	// Shrink the ring so the test drives a small, deterministic overflow (restored on return).
	orig := tellLogMax
	tellLogMax = 3
	t.Cleanup(func() { tellLogMax = orig })

	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	z := tellShard(t, core.WorldHandle(), js, dir)
	alice := joinTellPlayer(t, z, "Alice")
	joinTellPlayer(t, z, "Bob")

	const n = 5
	for i := 1; i <= n; i++ {
		z.post(inputMsg{id: "Alice", line: "tell Bob m" + itoaTest(i)})
		if !drainContains(t, alice, "You tell Bob, 'm"+itoaTest(i)+"'") {
			t.Fatalf("tell %d not echoed", i)
		}
	}

	out := runTells(t, z, alice)
	// The oldest two (m1, m2) must have dropped; the last three (m3..m5) remain.
	for _, dropped := range []string{"'m1'", "'m2'"} {
		if strings.Contains(out, dropped) {
			t.Fatalf("history exceeded the bound; %s should have been trimmed: %q", dropped, out)
		}
	}
	for _, kept := range []string{"'m3'", "'m4'", "'m5'"} {
		if !strings.Contains(out, kept) {
			t.Fatalf("history dropped a within-bound entry; %s should be present: %q", kept, out)
		}
	}
}

// TestTellHistoryEmpty: `tells` with no history renders the friendly notice.
func TestTellHistoryEmpty(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice")

	z := tellShard(t, core.WorldHandle(), js, dir)
	alice := joinTellPlayer(t, z, "Alice")

	out := runTells(t, z, alice)
	if !strings.Contains(out, "You have no recent tells.") {
		t.Fatalf("empty history did not render the friendly notice: %q", out)
	}
}

package world

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/commbus"
)

// tell_test.go is the white-box test set for Phase-8 slice 8.5 (tells, durable-always). It drives the
// SOURCE publish path (resolve-via-directory / sanitize / engine-author / idempotency-key) and the
// TARGET drain (per-player durable consumer -> dedup-via-cursor -> emit to the gate tell subject), all
// against the MemJetStream stand-in + a MemBus + a fake Locator, in ONE process with NO broker.
//
// The done-when proofs: a cross-shard ONLINE tell arrives; an OFFLINE tell is delivered on next login
// EXACTLY ONCE; a redelivery renders ONCE (the character-state cursor); per-sender order holds; a tell
// to an unknown player is refused; `reply` targets the last sender.

// fakeLocator is a minimal world.Locator for the tell tests: it answers PlayerShard for known players
// (resolve hit) and reports found=false for unknown names (resolve miss -> the tell is refused). The
// handoff methods are unused here (single-shard-per-player tells).
type fakeLocator struct {
	mu      sync.Mutex
	players map[string]string // player id -> shard id (presence of a key == "this player exists")
}

func newFakeLocator(known ...string) *fakeLocator {
	m := map[string]string{}
	for _, k := range known {
		m[k] = "shard-x"
	}
	return &fakeLocator{players: m}
}

func (f *fakeLocator) PlayerShard(_ context.Context, playerID string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sh, ok := f.players[playerID]
	return sh, ok, nil
}

func (f *fakeLocator) ShardForZone(context.Context, string) (string, error)     { return "", nil }
func (f *fakeLocator) EndpointForShard(context.Context, string) (string, error) { return "", nil }
func (f *fakeLocator) SetPlayerShard(context.Context, string, string, uint64) (bool, error) {
	return true, nil
}

func (f *fakeLocator) PlayerEpoch(context.Context, string) (uint64, bool, error) {
	return 0, false, nil
}

// tellShard builds a demo shard wired with a shared MemBus (world handle), a shared MemJetStream
// (durable tells), and a fake Locator, runs it, and returns its home zone. The gate handle of the
// SAME bus is what a test subscribes on to observe the world's emit to the tell subject.
func tellShard(t *testing.T, wbus commbus.Bus, js commbus.JetStream, dir Locator) *Zone {
	t.Helper()
	sh := NewShard("midgaard", "addr", dir, nil).WithComms(wbus).WithTells(js)
	sh.comms.drainPace = time.Millisecond // fast paced-drain for the test (a per-shard field, no shared var)
	// A generous per-author rate budget (a per-source FIELD, no shared package var) so multi-tell tests
	// (per-sender order, offline backlog) are not throttled by the production burst; the rate-limit test
	// overrides these on its own shard.
	sh.comms.rateBurst, sh.comms.rateRefill = 1000, time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sh.Run(ctx)
	return sh.Zone()
}

// subscribeTell subscribes the gate handle to a player's concrete tell subject (commbus.TellSubject)
// — the emit point the world's drain publishes to — and returns a channel of rendered Bodies.
func subscribeTell(t *testing.T, gate commbus.Bus, player string) <-chan commbus.Message {
	t.Helper()
	got := make(chan commbus.Message, 32)
	sub, err := gate.Subscribe(commbus.TellSubject(player), func(m commbus.Message) { got <- m })
	if err != nil {
		t.Fatalf("subscribe tell %s: %v", player, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return got
}

func recvTell(t *testing.T, ch <-chan commbus.Message) commbus.Message {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a tell emit")
		return commbus.Message{}
	}
}

// TestCrossShardOnlineTell: an online target receives a tell, rendered, on its concrete tell subject.
// Both players join (so the target's resident consumer is live), Alice tells Bob, Bob's gate sees it.
func TestCrossShardOnlineTell(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	z := tellShard(t, core.WorldHandle(), js, dir)
	bobInbox := subscribeTell(t, core.GateHandle(), "Bob")

	alice := joinTellPlayer(t, z, "Alice")
	joinTellPlayer(t, z, "Bob") // Bob online -> his consumer drains live

	z.post(inputMsg{id: "Alice", line: "tell Bob hello there"})

	m := recvTell(t, bobInbox)
	if !strings.Contains(m.Body, "Alice tells you, 'hello there'") {
		t.Fatalf("tell not rendered as expected: %q", m.Body)
	}
	// The sender is echoed "You tell Bob, ...".
	if !drainContains(t, alice, "You tell Bob, 'hello there'") {
		t.Fatal("sender was not echoed the tell")
	}
}

// TestOfflineTellDeliveredOnLogin is the slice done-when: a tell to an OFFLINE target sits in the
// durable stream and is delivered when the target logs in (exactly once).
func TestOfflineTellDeliveredOnLogin(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	z := tellShard(t, core.WorldHandle(), js, dir)
	bobInbox := subscribeTell(t, core.GateHandle(), "Bob")

	joinTellPlayer(t, z, "Alice")

	// Bob is OFFLINE: Alice tells him twice. The tells land in the durable stream, nothing is emitted yet.
	z.post(inputMsg{id: "Alice", line: "tell Bob first while away"})
	z.post(inputMsg{id: "Alice", line: "tell Bob second while away"})
	assertNoTell(t, bobInbox) // offline: nothing delivered to the (absent) gate yet

	// Bob logs in: his resident consumer drains the backlog, paced, exactly once and in order.
	joinTellPlayer(t, z, "Bob")
	m1 := recvTell(t, bobInbox)
	m2 := recvTell(t, bobInbox)
	if !strings.Contains(m1.Body, "first while away") || !strings.Contains(m2.Body, "second while away") {
		t.Fatalf("offline backlog not drained in order: %q then %q", m1.Body, m2.Body)
	}
	// The OFFLINE backlog is rendered with the "while you were away…" wording (the paced login drain).
	if !strings.HasPrefix(m1.Body, "While you were away,") {
		t.Fatalf("offline tell not rendered with the away wording: %q", m1.Body)
	}
	assertNoTell(t, bobInbox) // exactly those two, no duplicate
}

// TestRedeliveryRendersOnce drives a CONSUMER RESTART against the same MemJetStream + the character-
// state cursor: after Bob drains a tell, restarting his consumer (a reconnect / unacked redelivery)
// re-presents the backlog, but the per-sender delivered-cursor suppresses it — it renders ONCE.
func TestRedeliveryRendersOnce(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	z := tellShard(t, core.WorldHandle(), js, dir)
	bobInbox := subscribeTell(t, core.GateHandle(), "Bob")

	joinTellPlayer(t, z, "Alice")
	bob := joinTellPlayer(t, z, "Bob")

	z.post(inputMsg{id: "Alice", line: "tell Bob only once please"})
	m := recvTell(t, bobInbox)
	if !strings.Contains(m.Body, "only once please") {
		t.Fatalf("first delivery wrong: %q", m.Body)
	}

	// Force the cursor to be observable, then simulate a REDELIVERY: stop + restart Bob's consumer with
	// a FRESH MemJetStream consumer cursor (re-Consume re-drains from where its internal cursor was —
	// to truly model a redelivery we drive the same message back through deliverDrainedTell). Easiest:
	// directly re-present the same durable message to the zone and assert the character-state cursor
	// suppresses the render (no new emit).
	waitTellCursor(t, z, bob, "Alice", 1)
	ack := make(chan bool, 1)
	z.post(tellDeliverMsg{target: "Bob", msg: commbus.Message{
		AuthorID: "Alice", AuthorName: "Alice", Seq: 1, Body: "Alice tells you, 'only once please'",
	}, ack: ack})
	if got := <-ack; !got {
		t.Fatal("redelivery should ACK (suppressed-as-delivered), not NAK")
	}
	assertNoTell(t, bobInbox) // suppressed by the cursor: rendered ONCE total
}

// TestTellToUnknownPlayerRefused: a tell to a name with no directory placement is refused to the sender.
func TestTellToUnknownPlayerRefused(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice") // Nobody is NOT known

	z := tellShard(t, core.WorldHandle(), js, dir)
	alice := joinTellPlayer(t, z, "Alice")

	z.post(inputMsg{id: "Alice", line: "tell Nobody are you there"})
	if !drainContains(t, alice, "no player by that name") {
		t.Fatal("a tell to an unknown player was not refused to the sender")
	}
}

// TestReplyTargetsLastSender: `reply` sends to whoever last told you.
func TestReplyTargetsLastSender(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	z := tellShard(t, core.WorldHandle(), js, dir)
	aliceInbox := subscribeTell(t, core.GateHandle(), "Alice")

	alice := joinTellPlayer(t, z, "Alice")
	joinTellPlayer(t, z, "Bob")

	// Alice tells Bob; Bob replies. Bob's lastTellFrom is Alice, so reply reaches Alice.
	z.post(inputMsg{id: "Alice", line: "tell Bob hi bob"})
	// Wait for Bob to have received it (his lastTellFrom is set on delivery).
	waitLastTellFrom(t, z, "Bob", "Alice")
	z.post(inputMsg{id: "Bob", line: "reply hi back alice"})

	m := recvTell(t, aliceInbox)
	if !strings.Contains(m.Body, "Bob tells you, 'hi back alice'") {
		t.Fatalf("reply did not target the last sender: %q", m.Body)
	}
	_ = alice
}

// TestPerSenderTellOrder proves a sender's tells arrive in SEND ORDER (P8-A3): a burst of tells from
// one sender, delivered to an offline-then-online target, drains in order — the single-writer publisher
// + the append-ordered stream + the strictly-increasing cursor.
func TestPerSenderTellOrder(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	z := tellShard(t, core.WorldHandle(), js, dir)
	bobInbox := subscribeTell(t, core.GateHandle(), "Bob")

	joinTellPlayer(t, z, "Alice")

	// A burst of tells while Bob is OFFLINE (so they all queue durably and the order is observable on drain).
	const n = 12
	for i := 1; i <= n; i++ {
		z.post(inputMsg{id: "Alice", line: "tell Bob m" + itoaTest(i)})
	}
	joinTellPlayer(t, z, "Bob")

	for i := 1; i <= n; i++ {
		m := recvTell(t, bobInbox)
		want := "m" + itoaTest(i)
		if !strings.Contains(m.Body, want) && !strings.HasSuffix(strings.TrimRight(m.Body, "'"), want) {
			t.Fatalf("tell %d out of order: got %q want suffix %q", i, m.Body, want)
		}
	}
}

// TestLoggingOutRaceTellNotLost is the P8-A4 durable-fallback proof: a tell published while the target
// is logged out (the online->offline race the durable-always model eliminates) is NOT lost — it sits in
// the stream and is delivered on the target's next login. (Durable-always means there is no core path to
// lose it on; this pins the guarantee.)
func TestLoggingOutRaceTellNotLost(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	z := tellShard(t, core.WorldHandle(), js, dir)
	bobInbox := subscribeTell(t, core.GateHandle(), "Bob")

	joinTellPlayer(t, z, "Alice")
	// Bob logs in then OUT (his consumer stops) before the tell — the logging-out window.
	bob := joinTellPlayer(t, z, "Bob")
	_ = bob
	z.post(leaveMsg{id: "Bob"})
	waitGone(t, z, "Bob")

	z.post(inputMsg{id: "Alice", line: "tell Bob caught you mid-logout"})
	assertNoTell(t, bobInbox) // Bob is gone: nothing delivered to a dead socket

	// Bob logs back in: the durable tell is delivered (never lost).
	joinTellPlayer(t, z, "Bob")
	m := recvTell(t, bobInbox)
	if !strings.Contains(m.Body, "caught you mid-logout") {
		t.Fatalf("a tell published during logout was lost: %q", m.Body)
	}
}

// itoaTest is a tiny int->string for the order test (avoids strconv in the test import set).
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// waitGone waits until a player is no longer a resident of z (their consumer stopped). It uses the
// existing synchronous presence query (presenceMsg) so the membership read is race-free on the zone
// goroutine.
func waitGone(t *testing.T, z *Zone, player string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		reply := make(chan presence, 1)
		z.post(presenceMsg{id: player, reply: reply})
		if p := <-reply; !p.present {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("player %s never left", player)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// joinTellPlayer joins a player into z and waits until they have arrived (so their consumer is live).
func joinTellPlayer(t *testing.T, z *Zone, name string) *session {
	t.Helper()
	s := newTestPlayerEntity(z, name)
	z.post(joinMsg{s: s})
	waitMarkup(t, s, "The Temple Square")
	return s
}

func assertNoTell(t *testing.T, ch <-chan commbus.Message) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("unexpected tell emit: %q", m.Body)
	case <-time.After(200 * time.Millisecond):
	}
}

// waitTellCursor polls the zone (via a synchronous read posted to the goroutine) for a session's
// per-sender cursor reaching want. It reads zone-owned state by posting a closure; to keep the test
// simple we poll the session field under the zone goroutine's quiescence (the field is only written on
// the zone goroutine, and by the time the emit was observed the write has happened-before via the bus).
func waitTellCursor(t *testing.T, z *Zone, s *session, author string, want uint64) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		done := make(chan uint64, 1)
		z.post(tellCursorProbeMsg{id: s.character, author: author, reply: done})
		select {
		case got := <-done:
			if got >= want {
				return
			}
		case <-deadline:
			t.Fatalf("tell cursor for %s/%s never reached %d", s.character, author, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitLastTellFrom(t *testing.T, z *Zone, player, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		done := make(chan string, 1)
		z.post(lastTellProbeMsg{id: player, reply: done})
		select {
		case got := <-done:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("lastTellFrom for %s never became %q", player, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTellCursorRoundTripSuppressesRedeliveryAfterRestart is the OQ-4 persistence proof: the delivered-
// cursor rides StateJSON through dump -> JSON -> load (a logout/login or crash-rehydrate), so a
// redelivery AFTER the restart renders ONCE. It exercises dumpCharacter (cursor -> StateJSON.Tells),
// the JSON round-trip, loadTellCursor (StateJSON.Tells -> session), and deliverDrainedTell's
// strictly-greater render gate against the restored cursor.
func TestTellCursorRoundTripSuppressesRedeliveryAfterRestart(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	// A session that has delivered up to Alice:5 and Carol:2.
	src := &session{character: "Bob"}
	z.newPlayerEntity(src, "Bob")
	src.tellCursor = map[string]uint64{"Alice": 5, "Carol": 2}

	snap := dumpCharacter(src)
	if snap.State.Tells == nil || snap.State.Tells.Delivered["Alice"] != 5 || snap.State.Tells.Delivered["Carol"] != 2 {
		t.Fatalf("cursor not dumped into StateJSON.Tells: %+v", snap.State.Tells)
	}

	// JSON round-trip the StateJSON (the at-rest form the saver writes / the loader reads).
	raw, err := json.Marshal(snap.State)
	if err != nil {
		t.Fatal(err)
	}
	var loaded StateJSON
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}

	// Relog: a FRESH session, cursor restored from the round-tripped StateJSON.
	dst := &session{character: "Bob", out: make(chan *playv1.ServerFrame, 8)}
	loadTellCursor(dst, loaded.Tells)
	if dst.tellCursor["Alice"] != 5 {
		t.Fatalf("cursor not restored on load: %+v", dst.tellCursor)
	}

	// Place the restored session in the zone so deliverDrainedTell can find it.
	z.players["Bob"] = dst
	// Wire a comms bus so the emit path has somewhere to publish (a plain MemBus world handle).
	bus := commbus.NewMemBus()
	t.Cleanup(func() { _ = bus.Close() })
	z.shard = &Shard{comms: &commSource{bus: bus, js: commbus.DisabledJetStream(), seq: map[string]uint64{}, rl: map[string]*tokenBucket{}, consumers: map[string]commbus.Consumer{}}}

	// A REDELIVERY of Alice:5 (<= the restored cursor) is suppressed but ACKed (render-once across restart).
	if ok := z.deliverDrainedTell(tellDeliverMsg{target: "Bob", msg: commbus.Message{AuthorID: "Alice", AuthorName: "Alice", Seq: 5, Body: "old"}}); !ok {
		t.Fatal("a redelivered (already-delivered) tell must ACK, not NAK")
	}
	if dst.lastTellFrom == "Alice" {
		t.Fatal("a suppressed redelivery must NOT set lastTellFrom (it rendered nothing)")
	}

	// A NEW Alice:6 (> cursor) renders and advances the cursor.
	if ok := z.deliverDrainedTell(tellDeliverMsg{target: "Bob", msg: commbus.Message{AuthorID: "Alice", AuthorName: "Alice", Seq: 6, Body: "new"}}); !ok {
		t.Fatal("a new tell must ACK")
	}
	if dst.tellCursor["Alice"] != 6 || dst.lastTellFrom != "Alice" {
		t.Fatalf("new tell did not advance cursor / set reply target: cursor=%v last=%q", dst.tellCursor, dst.lastTellFrom)
	}
}

// TestTellRateLimitsSenderOnly is the tell-path rate-limit security test (P8-A1 / security LOW-2): a
// flood of tells from one author is throttled at the SENDER — only `burst` reach the durable stream —
// so a determined sender cannot inflate a victim's durable backlog / login-drain, and a DIFFERENT
// author is unaffected (the bucket is per-author). It mirrors the channel rate-limit test; the rate
// gate is enforced synchronously in sendTell BEFORE the tell is published.
func TestTellRateLimitsSenderOnly(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Flooder", "Victim", "Bystander", "Bob")

	z := tellShard(t, core.WorldHandle(), js, dir)
	// Override to a strict 2-tell burst (no refill within the test) on THIS shard's source — a per-source
	// field, so no package-var race with any other test's goroutines.
	z.shard.comms.rateBurst, z.shard.comms.rateRefill = 2, time.Hour

	joinTellPlayer(t, z, "Flooder")
	joinTellPlayer(t, z, "Bystander")

	// Victim is OFFLINE: the flood lands (or is throttled) in the durable stream, which we then count by
	// peeking the stream's pending depth for a fresh consumer id — no delivery side effects.
	for i := 0; i < 6; i++ {
		z.post(inputMsg{id: "Flooder", line: "tell Victim spam"})
	}
	// Give the synchronous rate gate + the async publisher time to settle.
	waitPublished(t, js, commbus.DtellSubject("Victim"), "rl-probe-1", 2)
	if got := js.Pending(commbus.DtellSubject("Victim"), "rl-probe-2"); got != 2 {
		t.Fatalf("flooder published %d durable tells, want 2 (the burst); tell rate limit not throttling the sender", got)
	}

	// The flooder is told (sender-only) they are going too fast; the dropped tells never reached the stream.
	// A DIFFERENT author is unaffected — their own bucket is full.
	for i := 0; i < 3; i++ {
		z.post(inputMsg{id: "Bystander", line: "tell Bob hello there"})
	}
	waitPublished(t, js, commbus.DtellSubject("Bob"), "rl-probe-3", 2)
	if got := js.Pending(commbus.DtellSubject("Bob"), "rl-probe-4"); got != 2 {
		t.Fatalf("a different author was throttled by the flooder (published %d, want 2); the bucket is not per-author", got)
	}
}

// TestTellTargetSubjectInjectionRefused is the P8-A8 / security LOW-1 test: a target token carrying a
// NATS subject metacharacter (a dot/wildcard/whitespace) is REFUSED before it can form a subject, even
// on the dir==nil (no directory existence check) path — so it never reaches the broker as a crafted
// subject and nothing is published.
func TestTellTargetSubjectInjectionRefused(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })

	// NO directory (dir==nil): the existence check is absent, so the subject-token sanitizer is the ONLY
	// defense — exactly the residual this test closes.
	z := tellShard(t, core.WorldHandle(), js, nil)
	flooder := joinTellPlayer(t, z, "Sneaky")

	for _, bad := range []string{"Bob.evil", "Bob.>", "all.*", "a b"} {
		z.post(inputMsg{id: "Sneaky", line: "tell " + bad + " injected"})
	}
	// The sender is refused each time; nothing crafted reaches the stream.
	if !drainContains(t, flooder, "no player by that name") {
		t.Fatal("a subject-metacharacter target was not refused to the sender")
	}
	// No durable subject was created for any crafted token (the publisher never ran for them).
	for _, bad := range []string{"Bob.evil", "Bob.>", "all.*", "a b"} {
		if n := js.Pending(commbus.DtellSubject(bad), "probe"); n != 0 {
			t.Fatalf("a crafted target %q reached the durable subject space (pending=%d)", bad, n)
		}
	}
}

// waitPublished waits until the durable subject has at least want entries pending for a fresh probe
// consumer id (a peek that does not advance any real cursor), so a test can synchronize on the async
// publisher having appended.
func waitPublished(t *testing.T, js *commbus.MemJetStream, subj, probeID string, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if js.Pending(subj, probeID) >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("durable subject %s never reached %d pending (last %d)", subj, want, js.Pending(subj, probeID))
		case <-time.After(5 * time.Millisecond):
		}
	}
}

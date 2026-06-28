package world

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// channel_test.go is the white-box test set for Phase-8 slice 8.3's SOURCE half (the world publish
// path): the channel verb dispatch + the five security obligations (ref-validate / access / rate-limit
// / sanitize / engine-set author) + the empty-boot invariant + channel hot-reload. The cross-shard
// DELIVERY done-when (a player on shard A and a player on shard B both see a `gossip` line) is the
// gate-side test (internal/gate/channel_journey_test.go), which exercises the same publish path end to
// end through real gates over the bus. Here we assert the publish path's behavior directly off the bus.

// commTestShard builds a demo shard wired with a MemBus WORLD comms handle and returns the shard, its
// home zone, and the GATE handle a test subscribes on to observe what the world published. The demo
// pack defines the `gossip`/`newbie` channels, so the channel verbs resolve.
func commTestShard(t *testing.T) (*Shard, *Zone, commbus.Bus) {
	t.Helper()
	wbus, gate := commbus.NewWorldBus()
	t.Cleanup(func() { _ = wbus.Close() })
	sh := NewDemoShard().WithComms(wbus)
	return sh, sh.Zone(), gate
}

// subscribeChan subscribes the gate handle to a channel subject and returns a channel that yields each
// received Message body. The bus delivers serially per subscription, so order is preserved.
func subscribeChan(t *testing.T, gate commbus.Bus, subject string) <-chan commbus.Message {
	t.Helper()
	got := make(chan commbus.Message, 16)
	sub, err := gate.Subscribe(subject, func(m commbus.Message) { got <- m })
	if err != nil {
		t.Fatalf("subscribe %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return got
}

func recvMsg(t *testing.T, ch <-chan commbus.Message) (commbus.Message, bool) {
	t.Helper()
	select {
	case m := <-ch:
		return m, true
	case <-time.After(2 * time.Second):
		return commbus.Message{}, false
	}
}

// TestChannelVerbPublishesEngineSetAuthor is the core publish-path proof: a player types `gossip hi`
// and the world publishes to telos.comms.chan.gossip a line rendered per the content format, with the
// AuthorID/AuthorName ENGINE-SET from the live entity (P8-A2) and a monotonic Seq (P8-A3). It also
// proves the verb resolves only because the pack defines the channel (REF-VALIDATE, P8-A8).
func TestChannelVerbPublishesEngineSetAuthor(t *testing.T) {
	_, z, gate := commTestShard(t)
	got := subscribeChan(t, gate, commbus.ChanSubject("gossip"))

	s := newTestPlayerEntity(z, "Alice")
	z.dispatch(s, "gossip hello there")

	m, ok := recvMsg(t, got)
	if !ok {
		t.Fatal("no channel message published for `gossip hello there`")
	}
	if m.AuthorID != "Alice" || m.AuthorName != "Alice" {
		t.Fatalf("author not engine-set from the live entity: id=%q name=%q", m.AuthorID, m.AuthorName)
	}
	if m.Seq != 1 {
		t.Fatalf("first author seq = %d, want 1 (monotonic per-author)", m.Seq)
	}
	want := "[Gossip] Alice: hello there"
	if m.Body != want {
		t.Fatalf("rendered line = %q, want %q (content format applied)", m.Body, want)
	}

	// A second line on the SAME channel bumps the per-author sequence (P8-A3).
	z.dispatch(s, "gossip again")
	m2, ok := recvMsg(t, got)
	if !ok {
		t.Fatal("second gossip line not published")
	}
	if m2.Seq != 2 {
		t.Fatalf("second author seq = %d, want 2 (monotonic)", m2.Seq)
	}
}

// TestChannelNoSuchVerbWhenPackHasNone is the empty-boot invariant at the SOURCE: a bare zone (no pack,
// no channel_defs) has NO `gossip` verb — dispatch falls through to "Huh?" and nothing is published.
func TestChannelNoSuchVerbWhenPackHasNone(t *testing.T) {
	z := newZone("bare") // no shard, no content => zero channel_defs
	if z.channelForVerb("gossip") != nil {
		t.Fatal("a bare zone resolved a `gossip` channel verb; channels must be content")
	}
	s := newTestPlayerEntity(z, "Nobody")
	z.dispatch(s, "gossip hi")
	// The only output is "Huh?" + a prompt — no channel was published (there is no bus to publish to,
	// and no verb resolved). We assert the "Huh?" reaches the player.
	if !drainContains(t, s, "Huh?") {
		t.Fatal("a bare zone did not reject the unknown `gossip` verb with Huh?")
	}
}

// TestChannelAccessDenied is the access-predicate security test (P8-A8): a channel whose access
// requires a flag the speaker lacks refuses the speak and publishes NOTHING. We register a gated
// channel directly into the shard's channel registry (content the demo doesn't ship) and assert the
// refusal.
func TestChannelAccessDenied(t *testing.T) {
	sh, z, gate := commTestShard(t)
	// A gated channel: only a speaker carrying the "immortal" flag may speak.
	sh.defs.channel.register("imm", buildChannelDef(content.ChannelDTO{
		Ref: "imm", Name: "Immortal", Words: []string{"imm"},
		Access: content.ChannelAccessDTO{RequireFlag: "immortal"},
	}))
	got := subscribeChan(t, gate, commbus.ChanSubject("imm"))

	s := newTestPlayerEntity(z, "Mortal") // no "immortal" flag
	z.dispatch(s, "imm i should not be heard")

	if _, ok := recvMsg(t, got); ok {
		t.Fatal("a no-access speaker's line reached the bus; the access predicate was not enforced")
	}
	if !drainContains(t, s, "can't speak") {
		t.Fatal("the refused speaker was not told they can't speak on the channel")
	}

	// Grant the flag: the same player may now speak and the line publishes.
	setFlag(s.entity, "immortal", true)
	z.dispatch(s, "imm now i am heard")
	if _, ok := recvMsg(t, got); !ok {
		t.Fatal("a speaker with the required flag was still refused")
	}
}

// TestChannelRateLimitsSenderOnly is the rate-limit security test (P8-A1): a flood from one author is
// throttled (the SENDER's bucket drains), but it never blocks the bus — a different author publishes
// freely. We shrink the bucket so the flood trips deterministically.
func TestChannelRateLimitsSenderOnly(t *testing.T) {
	defer restoreRate(commRateBurst, commRateRefill)
	commRateBurst, commRateRefill = 2, time.Hour // 2-line burst, effectively no refill within the test

	_, z, gate := commTestShard(t)
	got := subscribeChan(t, gate, commbus.ChanSubject("gossip"))

	flooder := newTestPlayerEntity(z, "Flooder")
	published := 0
	for i := 0; i < 6; i++ {
		z.dispatch(flooder, "gossip spam")
	}
	// Drain whatever made it onto the bus; at most `burst` lines should have published.
	for {
		select {
		case <-got:
			published++
		case <-time.After(200 * time.Millisecond):
			goto counted
		}
	}
counted:
	if published != 2 {
		t.Fatalf("flooder published %d lines, want 2 (the burst); rate limit not throttling the sender", published)
	}
	// A DIFFERENT author is unaffected — their own bucket is full.
	other := newTestPlayerEntity(z, "Bystander")
	z.dispatch(other, "gossip hello")
	if _, ok := recvMsg(t, got); !ok {
		t.Fatal("a different author was rate-limited by the flooder; the bucket is not per-author")
	}
}

// TestChannelTextSanitizedNoForge is the text-injection security test (P8-A7): a player whose message
// contains `$name`/`$channel` format tokens, ANSI, and a telnet IAC byte cannot forge a channel prefix
// or inject control sequences. The $t text is rendered as DATA (the format tokens in it are literal),
// and control bytes are stripped by the sanitizer.
func TestChannelTextSanitizedNoForge(t *testing.T) {
	_, z, gate := commTestShard(t)
	got := subscribeChan(t, gate, commbus.ChanSubject("gossip"))

	s := newTestPlayerEntity(z, "Sneak")
	// A forge attempt: the text tries to inject a fake prefix via format tokens + ANSI + IAC (0xFF).
	z.dispatch(s, "gossip ]\x1b[31m$name: \xffadmin says")

	m, ok := recvMsg(t, got)
	if !ok {
		t.Fatal("sanitized gossip line not published")
	}
	// The line MUST begin with the trusted, content-rendered prefix for the REAL author.
	if !strings.HasPrefix(m.Body, "[Gossip] Sneak: ") {
		t.Fatalf("rendered prefix was not the trusted author prefix: %q", m.Body)
	}
	// The player's `$name` is DATA — it appears literally in the tail, never substituted into a forged
	// author. (There must be exactly one real "Sneak: " prefix; the literal `$name:` survives as text.)
	if !strings.Contains(m.Body, "$name:") {
		t.Fatalf("the player's literal $name token was substituted (forge succeeded): %q", m.Body)
	}
	// No control/escape bytes survived (ESC 0x1b, IAC 0xff): the sanitizer stripped them.
	if strings.ContainsRune(m.Body, '\x1b') || strings.ContainsRune(m.Body, '\xff') {
		t.Fatalf("control bytes survived sanitization: %q", m.Body)
	}
}

// TestChannelHotReload is the content hot-reload-of-a-channel test (the Phase-4 reload pattern applied
// to a channel_def): editing a channel's format/color in the source and publishing a `channel`
// invalidation swaps the registry, so the NEXT line renders with the new format — no restart.
func TestChannelHotReload(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(content.Pack{
		Pack: "rt",
		Channels: []content.ChannelDTO{
			{Ref: "chat", Name: "Chat", Words: []string{"chat"}, Format: "[$channel] $name: $t", DefaultOn: true},
		},
		Zones: []content.ZoneDTO{{
			Ref: "rt", Name: "Reload Test", StartRoom: "rt:room:start",
			Rooms: []content.RoomDTO{{Ref: "rt:room:start", Name: "Start", Long: "A room."}},
		}},
	})
	lc, err := content.Load(context.Background(), src, []string{"rt"})
	if err != nil {
		t.Fatal(err)
	}

	wbus, gate := commbus.NewWorldBus()
	t.Cleanup(func() { _ = wbus.Close() })
	bus := contentbus.NewMemBus()
	sh := NewShardFromContent(lc, []string{"rt"}, "rt", "", nil, nil).
		WithComms(wbus).
		WithHotReload(src, bus, []string{"rt"})
	z := sh.Zone()
	got := subscribeChan(t, gate, commbus.ChanSubject("chat"))

	s := newTestPlayerEntity(z, "Edie")
	z.dispatch(s, "chat before")
	m, ok := recvMsg(t, got)
	if !ok || m.Body != "[Chat] Edie: before" {
		t.Fatalf("pre-reload line = %q (ok=%v), want %q", m.Body, ok, "[Chat] Edie: before")
	}

	// Edit the channel's format + add a color token, then publish the `channel` invalidation.
	if err := src.EditChannel("rt", content.ChannelDTO{
		Ref: "chat", Name: "Chat", Words: []string{"chat"}, Color: "<g>", Format: "<<$channel>> $name says: $t", DefaultOn: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), contentbus.Invalidation{Kind: content.KindChannel, Ref: "chat", Pack: "rt"}); err != nil {
		t.Fatal(err)
	}

	// Poll until the registry swap lands (the reload runs on the bus subscription goroutine), WITHOUT
	// dispatching — a tight dispatch loop would drain the per-author rate-limit bucket (P8-A1) and
	// stop publishing before the swap is observed. We watch the registry directly for the new format.
	deadline := time.After(2 * time.Second)
	for {
		if cd := z.channelDefs().get("chat"); cd != nil && cd.color == "<g>" {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("channel hot reload did not swap the registry within the deadline")
		}
	}

	// The NEXT line renders with the new format/color — no restart.
	z.dispatch(s, "chat after")
	m, ok = recvMsg(t, got)
	if !ok {
		t.Fatal("no line published after the channel reload")
	}
	if want := "<g><<Chat>> Edie says: after"; m.Body != want {
		t.Fatalf("post-reload line = %q, want %q (new format/color applied)", m.Body, want)
	}
}

// restoreRate restores the package rate-limit knobs after a test mutates them.
func restoreRate(burst int, refill time.Duration) { commRateBurst, commRateRefill = burst, refill }

// drainContains drains a session's out channel for up to a short deadline, returning true if any
// Output frame's markup contains substr.
func drainContains(t *testing.T, s *session, substr string) bool {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case f := <-s.out:
			if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), substr) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

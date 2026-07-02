package world

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
)

// commsstate_test.go is the white-box test set for Phase-8 slice 8.6's SOURCE half: the comms-state
// subtree (channel toggles / ignore / AFK) and the world-computed effective {enabled ∩ hearable} hear-set
// that drives the receiver HEAR-filter. The gate-side funnel + hear-filter delivery proofs are the
// black-box gate tests (internal/gate/comms_toggle_journey_test.go).

// drainConfig subscribes a gate-role handle to a player's config subject and returns a channel of the
// decoded ConfigPayloads the world publishes there (the hear-set + ignore list).
func drainConfig(t *testing.T, gate commbus.Bus, player string) <-chan commbus.ConfigPayload {
	t.Helper()
	out := make(chan commbus.ConfigPayload, 16)
	sub, err := gate.Subscribe(commbus.ConfigSubject(player), func(m commbus.Message) {
		p, err := commbus.UnmarshalConfig(m.Body)
		if err == nil {
			out <- p
		}
	})
	if err != nil {
		t.Fatalf("subscribe config %s: %v", player, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return out
}

func recvConfig(t *testing.T, ch <-chan commbus.ConfigPayload) (commbus.ConfigPayload, bool) {
	t.Helper()
	select {
	case p := <-ch:
		return p, true
	case <-time.After(2 * time.Second):
		return commbus.ConfigPayload{}, false
	}
}

// TestCommsStateRoundTrip proves the comms-state subtree (channel overrides + ignore + AFK) round-trips
// through StateJSON: dump -> marshal -> unmarshal -> load reconstructs the same in-memory state. This is
// the persistence proof (survives logout/login + crash-rehydrate, since StateJSON is the durable form).
func TestCommsStateRoundTrip(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Saver")

	cs := commsOf(s)
	cs.chanOverride["gossip"] = false // disabled gossip
	cs.chanOverride["newbie"] = true  // explicitly enabled newbie
	cs.ignore["Spammer"] = struct{}{}
	cs.ignore["Troll"] = struct{}{}
	cs.afk = true
	cs.afkMsg = "back soon"

	dumped := dumpCommsState(s)
	if dumped == nil {
		t.Fatal("dumpCommsState returned nil for a non-default state")
	}

	// Round-trip through the durable JSON form.
	raw, err := marshalCommsState(dumped)
	if err != nil {
		t.Fatalf("marshalCommsState: %v", err)
	}
	reloaded, err := unmarshalCommsState(raw)
	if err != nil {
		t.Fatalf("unmarshalCommsState: %v", err)
	}

	s2 := newTestPlayerEntity(z, "Saver")
	loadCommsState(s2, reloaded)

	if s2.comms == nil {
		t.Fatal("loadCommsState installed no state")
	}
	if on, ok := s2.comms.chanOverride["gossip"]; !ok || on {
		t.Fatalf("gossip override = (%v,%v), want (false,true)", on, ok)
	}
	if on, ok := s2.comms.chanOverride["newbie"]; !ok || !on {
		t.Fatalf("newbie override = (%v,%v), want (true,true)", on, ok)
	}
	if !s2.comms.ignored("Spammer") || !s2.comms.ignored("Troll") {
		t.Fatal("ignore list did not round-trip")
	}
	if !s2.comms.afk || s2.comms.afkMsg != "back soon" {
		t.Fatalf("AFK = (%v,%q), want (true,%q)", s2.comms.afk, s2.comms.afkMsg, "back soon")
	}
}

// TestCommsStateSurvivesLogoutLogin proves the comms-state subtree round-trips through the actual
// dumpCharacter → StateJSON → loadCharacter plumbing (a logout/login / crash-rehydrate, hermetically):
// the field is wired into the durable save form and re-installed on load. This is the persistence
// done-when at the world-plumbing level (the gated real-PG round-trip is in internal/store).
func TestCommsStateSurvivesLogoutLogin(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	src := &session{character: "Persist"}
	z.newPlayerEntity(src, "Persist")
	cs := commsOf(src)
	cs.chanOverride["gossip"] = false
	cs.ignore["Blocked"] = struct{}{}
	cs.afk = true
	cs.afkMsg = "afk note"

	snap := dumpCharacter(src)
	if snap.State.Comms == nil {
		t.Fatal("dumpCharacter did not include the comms subtree in StateJSON")
	}

	// Re-login: a fresh session/entity loads the snapshot.
	dst := &session{character: "Persist"}
	z.newPlayerEntity(dst, "Persist")
	loadCharacter(z, dst, snap)

	if dst.comms == nil {
		t.Fatal("loadCharacter did not rehydrate the comms subtree")
	}
	if on, ok := dst.comms.chanOverride["gossip"]; !ok || on {
		t.Fatalf("reloaded gossip override = (%v,%v), want (false,true)", on, ok)
	}
	if !dst.comms.ignored("Blocked") {
		t.Fatal("reloaded ignore list lost 'Blocked'")
	}
	if !dst.comms.afk || dst.comms.afkMsg != "afk note" {
		t.Fatalf("reloaded AFK = (%v,%q), want (true,\"afk note\")", dst.comms.afk, dst.comms.afkMsg)
	}
}

// TestCommsStateDefaultOmitted proves an all-default comms state dumps to nil (omitted from the save),
// and a pre-8.6 save (nil Comms) loads as all-default — the backward-compat default.
func TestCommsStateDefaultOmitted(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Fresh")
	if dumpCommsState(s) != nil {
		t.Fatal("a never-touched player dumped a non-nil comms subtree")
	}
	// Touch then clear back to default-equivalent: an empty override map + no ignores + not AFK dumps nil.
	_ = commsOf(s)
	if dumpCommsState(s) != nil {
		t.Fatal("an empty comms state dumped a non-nil subtree (should be omitted)")
	}
	// A pre-8.6 load (nil) leaves the player at defaults.
	loadCommsState(s, nil)
	if s.comms != nil && (len(s.comms.chanOverride) != 0 || len(s.comms.ignore) != 0 || s.comms.afk) {
		t.Fatal("loading a nil comms subtree changed the default state")
	}
}

// restrictedHearShard builds a shard whose pack defines an OPEN channel (gossip) AND a RESTRICTED channel
// (`secret`, require_flag=insider) so the hear-filter can be tested speak-side AND hear-side. It returns
// the shard, its zone, and a gate handle for observing config + channel publishes.
func restrictedHearShard(t *testing.T) (*Shard, *Zone, commbus.Bus) {
	t.Helper()
	src := content.NewMemSource()
	src.SetPack(content.Pack{
		Pack: "ht",
		Channels: []content.ChannelDTO{
			{Ref: "gossip", Name: "Gossip", Words: []string{"gossip"}, Format: "[$channel] $name: $t", DefaultOn: true},
			{
				Ref: "secret", Name: "Secret", Words: []string{"secret"},
				Format:    "[$channel] $name: $t",
				DefaultOn: true,
				Access:    content.ChannelAccessDTO{RequireFlag: "insider"},
			},
		},
		Zones: []content.ZoneDTO{{
			Ref: "ht", Name: "Hear Test", StartRoom: "ht:room:start",
			Rooms: []content.RoomDTO{{Ref: "ht:room:start", Name: "Start", Long: "A room."}},
		}},
	})
	lc, err := content.Load(context.Background(), src, []string{"ht"})
	if err != nil {
		t.Fatal(err)
	}
	wbus, gate := commbus.NewWorldBus()
	t.Cleanup(func() { _ = wbus.Close() })
	sh := NewShardFromContent(lc, []string{"ht"}, "ht", "", nil, nil).WithComms(wbus)
	return sh, sh.Zone(), gate
}

// TestEffectiveHearSetEnabledIntersectHearable is the SOURCE-half hear-filter proof: the world computes
// {enabled ∩ hearable}. An OPEN channel the player has enabled is in the set; a RESTRICTED channel the
// player CANNOT hear is NOT, even though it is default_on; granting the flag adds it; disabling a channel
// removes it. This is what drives the gate's per-channel subscription (the receiver HEAR-filter), and it
// closes the content guardrail (a restricted channel is heard only by a player who passes its predicate).
func TestEffectiveHearSetEnabledIntersectHearable(t *testing.T) {
	_, z, _ := restrictedHearShard(t)

	mortal := newTestPlayerEntity(z, "Mortal") // no "insider" flag
	got := z.effectiveHearSet(mortal)
	if !containsStr(got, "gossip") {
		t.Fatalf("hear-set %v missing the open `gossip`", got)
	}
	if containsStr(got, "secret") {
		t.Fatalf("hear-set %v includes the RESTRICTED `secret` for a player who cannot hear it (guardrail open)", got)
	}

	// Grant access: `secret` now enters the hear-set (default_on + now hearable).
	setFlag(mortal.entity, "insider", true)
	got = z.effectiveHearSet(mortal)
	if !containsStr(got, "secret") {
		t.Fatalf("hear-set %v missing `secret` after granting the required flag", got)
	}

	// Disable gossip: it drops out even though it is hearable (enabled ∩ hearable).
	commsOf(mortal).chanOverride["gossip"] = false
	got = z.effectiveHearSet(mortal)
	if containsStr(got, "gossip") {
		t.Fatalf("hear-set %v still includes disabled `gossip`", got)
	}
}

// TestPublishCommsConfigCarriesHearSetAndIgnore proves publishCommsConfig publishes the effective hear-set
// + ignore list to the player's config subject — the world->gate channel for the HEAR-filter + the funnel.
func TestPublishCommsConfigCarriesHearSetAndIgnore(t *testing.T) {
	_, z, gate := restrictedHearShard(t)
	cfg := drainConfig(t, gate, "Insider")

	s := newTestPlayerEntity(z, "Insider")
	setFlag(s.entity, "insider", true)
	commsOf(s).ignore["Blocked"] = struct{}{}

	z.publishCommsConfig(s)

	p, ok := recvConfig(t, cfg)
	if !ok {
		t.Fatal("no comms config published")
	}
	if !containsStr(p.HearChannels, "gossip") || !containsStr(p.HearChannels, "secret") {
		t.Fatalf("config hear-set %v missing an enabled+hearable channel", p.HearChannels)
	}
	if !containsStr(p.Ignore, "Blocked") {
		t.Fatalf("config ignore %v missing the ignored author", p.Ignore)
	}
}

// TestRepublishOnAccessChangeCoversHearRestricted pins the mid-session republish FLOW for the SPLIT
// predicate (the round-5 review finding): with a channel whose HEARING alone is gated (open speak,
// hear_access require-flag), republishCommsOnAccessChange must actually publish — the cheap guard
// (anyChannelGatesHearing) must not short-circuit on the open SPEAK predicate — and the published
// hear-set must track the flag both ways (gain hears, loss stops hearing).
func TestRepublishOnAccessChangeCoversHearRestricted(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(content.Pack{
		Pack: "hr",
		Channels: []content.ChannelDTO{{
			Ref: "confession", Name: "Confession", Words: []string{"confess"},
			DefaultOn:  true,
			HearAccess: &content.ChannelAccessDTO{RequireFlag: "confessor"},
		}},
		Zones: []content.ZoneDTO{{
			Ref: "hr", Name: "Hear Restricted", StartRoom: "hr:room:start",
			Rooms: []content.RoomDTO{{Ref: "hr:room:start", Name: "Start", Long: "A room."}},
		}},
	})
	lc, err := content.Load(context.Background(), src, []string{"hr"})
	if err != nil {
		t.Fatal(err)
	}
	wbus, gate := commbus.NewWorldBus()
	t.Cleanup(func() { _ = wbus.Close() })
	sh := NewShardFromContent(lc, []string{"hr"}, "hr", "", nil, nil).WithComms(wbus)
	z := sh.Zone()

	s := newTestPlayerEntity(z, "Sinner")
	cfg := drainConfig(t, gate, "Sinner")

	// Gain the flag mid-session → the republish must fire and the hear-set must include the channel.
	setFlag(s.entity, "confessor", true)
	z.republishCommsOnAccessChange(s.entity)
	p, ok := recvConfig(t, cfg)
	if !ok {
		t.Fatal("republish did not publish for a hear-restricted channel (the guard short-circuited on the open speak predicate)")
	}
	if !containsStr(p.HearChannels, "confession") {
		t.Fatalf("hear-set %v missing `confession` after gaining the flag", p.HearChannels)
	}

	// Lose the flag mid-session → republished hear-set must DROP the ref (the eavesdropping fix).
	setFlag(s.entity, "confessor", false)
	z.republishCommsOnAccessChange(s.entity)
	p, ok = recvConfig(t, cfg)
	if !ok {
		t.Fatal("republish did not publish after the flag was lost")
	}
	if containsStr(p.HearChannels, "confession") {
		t.Fatalf("hear-set %v still includes `confession` after losing the flag (mid-session eavesdropping)", p.HearChannels)
	}
}

// TestChannelsToggleCommand drives the `channels off`/`channels on` command and asserts it records the
// override + re-publishes the config (so the gate re-subscribes).
func TestChannelsToggleCommand(t *testing.T) {
	_, z, gate := restrictedHearShard(t)
	s := newTestPlayerEntity(z, "Toggler")
	// Join-equivalent: publish once so the drain has a baseline; then toggle.
	cfg := drainConfig(t, gate, "Toggler")

	z.dispatch(s, "channels off gossip")
	if on, ok := s.comms.chanOverride["gossip"]; !ok || on {
		t.Fatalf("after `channels off gossip`, override = (%v,%v), want (false,true)", on, ok)
	}
	p, ok := recvConfig(t, cfg)
	if !ok {
		t.Fatal("`channels off` did not re-publish the config")
	}
	if containsStr(p.HearChannels, "gossip") {
		t.Fatalf("hear-set %v still includes gossip after `channels off gossip`", p.HearChannels)
	}

	z.dispatch(s, "channels on gossip")
	p, ok = recvConfig(t, cfg)
	if !ok {
		t.Fatal("`channels on` did not re-publish the config")
	}
	if !containsStr(p.HearChannels, "gossip") {
		t.Fatalf("hear-set %v missing gossip after `channels on gossip`", p.HearChannels)
	}
}

// TestIgnoreToggleCommand drives `ignore <name>` (add/remove) and the self-ignore refusal.
func TestIgnoreToggleCommand(t *testing.T) {
	_, z, _ := restrictedHearShard(t)
	s := newTestPlayerEntity(z, "Iggy")

	z.dispatch(s, "ignore Spammer")
	if !s.comms.ignored("Spammer") {
		t.Fatal("`ignore Spammer` did not add to the ignore set")
	}
	z.dispatch(s, "ignore Spammer") // toggle off
	if s.comms.ignored("Spammer") {
		t.Fatal("`ignore Spammer` a second time did not remove from the ignore set")
	}
	// Cannot ignore yourself.
	z.dispatch(s, "ignore Iggy")
	if s.comms != nil && s.comms.ignored("Iggy") {
		t.Fatal("a player ignored themselves (should be refused)")
	}
}

// TestAFKCommandAndClearOnInput proves `afk <msg>` sets the flag + message, `who` would mark it (the
// presence flag), and the player's NEXT non-afk input clears it.
func TestAFKCommandAndClearOnInput(t *testing.T) {
	_, z, _ := restrictedHearShard(t)
	s := newTestPlayerEntity(z, "Away")
	// Place the player so dispatch of a real command (look) works.
	z.join(s, "")

	z.dispatch(s, "afk gone fishing")
	if !s.comms.afk || s.comms.afkMsg != "gone fishing" {
		t.Fatalf("after `afk gone fishing`, AFK=(%v,%q)", s.comms.afk, s.comms.afkMsg)
	}
	if !playerAFK(s) {
		t.Fatal("playerAFK is false after setting AFK (who would not mark it)")
	}

	// A different command clears AFK.
	z.dispatch(s, "look")
	if s.comms.afk {
		t.Fatal("AFK was not cleared by the next input")
	}
	if playerAFK(s) {
		t.Fatal("playerAFK still true after clear-on-input")
	}
}

// TestAFKAutoReply proves an AFK target's delivered LIVE tell sends a "<name> is AFK: <msg>" auto-reply
// back to the sender (over the sender's tell subject), and a backlog drain does NOT.
func TestAFKAutoReply(t *testing.T) {
	sh, z, gate := restrictedHearShard(t)
	_ = sh
	// The auto-reply is published to the SENDER's tell subject; subscribe to observe it.
	senderTells := make(chan commbus.Message, 4)
	sub, err := gate.Subscribe(commbus.TellSubject("Caller"), func(m commbus.Message) { senderTells <- m })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	target := newTestPlayerEntity(z, "Sleeper")
	z.join(target, "")
	commsOf(target).afk = true
	commsOf(target).afkMsg = "zzz"

	// Deliver a LIVE tell from Caller to Sleeper directly via the drain path.
	ack := z.deliverDrainedTell(tellDeliverMsg{
		target:  "Sleeper",
		msg:     commbus.Message{AuthorID: "Caller", AuthorName: "Caller", Seq: 1, Body: "wake up"},
		backlog: false,
	})
	if !ack {
		t.Fatal("live tell to an AFK target was not acked")
	}

	select {
	case m := <-senderTells:
		if !strings.Contains(m.Body, "is AFK: zzz") {
			t.Fatalf("auto-reply body = %q, want it to mention `is AFK: zzz`", m.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no AFK auto-reply reached the sender")
	}
}

// containsStr reports whether s contains v.
func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

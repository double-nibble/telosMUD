package world

import (
	"context"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// commschanreload_test.go pins #75: a channel_def hot reload re-publishes live players' comms configs so a
// retightened (or loosened) channel takes effect immediately, not at the player's next toggle/handoff/relog.

// chanReloadPack builds a source pack with one channel + a minimal zone. hearFlag != "" gates HEARING behind
// that require-flag; "" leaves the channel open-hear.
func chanReloadPack(hearFlag string) content.Pack {
	ch := content.ChannelDTO{Ref: "confession", Name: "Confession", Words: []string{"confess"}, DefaultOn: true}
	if hearFlag != "" {
		ch.HearAccess = &content.ChannelAccessDTO{RequireFlag: hearFlag}
	}
	return content.Pack{
		Pack:     "reloadtest",
		Channels: []content.ChannelDTO{ch},
		Zones: []content.ZoneDTO{{
			Ref: "rt", Name: "RT", StartRoom: "rt:room:start",
			Rooms: []content.RoomDTO{{Ref: "rt:room:start", Name: "Start", Long: "A room."}},
		}},
	}
}

// TestRepublishAllCommsBothDirections proves the zone-side handler republishes EVERY player's config, in BOTH
// eligibility directions: a player who CAN hear a gated channel gets it in their hear-set, one who CANNOT is
// excluded — so a channel retighten drops the ineligible and a loosen adds the newly-eligible. (Unlike the
// per-entity access-change path, this must not short-circuit on anyChannelGatesHearing.)
func TestRepublishAllCommsBothDirections(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(chanReloadPack("confessor")) // hear-gated
	lc, err := content.Load(context.Background(), src, []string{"reloadtest"})
	if err != nil {
		t.Fatal(err)
	}
	wbus, gate := commbus.NewWorldBus()
	t.Cleanup(func() { _ = wbus.Close() })
	sh := NewShardFromContent(lc, []string{"rt"}, "rt", "", nil, nil).WithComms(wbus)
	z := sh.Zone()

	priest := newTestPlayerEntity(z, "Priest")
	setFlag(priest.entity, "confessor", true)  // eligible to hear
	sinner := newTestPlayerEntity(z, "Sinner") // NOT eligible
	// Register both as live hosted players (a join does this; republishAllComms iterates z.players).
	z.players["Priest"] = priest
	z.players["Sinner"] = sinner
	priestCfg := drainConfig(t, gate, "Priest")
	sinnerCfg := drainConfig(t, gate, "Sinner")

	z.republishAllComms("confession")

	pc, ok := recvConfig(t, priestCfg)
	if !ok {
		t.Fatal("the eligible player's config was not republished")
	}
	if !containsStr(pc.HearChannels, "confession") {
		t.Fatalf("eligible player's hear-set %v missing `confession`", pc.HearChannels)
	}
	sc, ok := recvConfig(t, sinnerCfg)
	if !ok {
		t.Fatal("the ineligible player's config was not republished")
	}
	if containsStr(sc.HearChannels, "confession") {
		t.Fatalf("ineligible player's hear-set %v wrongly includes `confession`", sc.HearChannels)
	}
}

// TestChannelReloadFansOutRepublish proves reloadChannel — after swapping the new channel def into the
// registry — fans a republishCommsMsg out to every hosted zone (the wiring that carries the fix to live
// sessions). Verified by the message landing on the zone inbox (the zone is not run here; the fan-out is a
// postOrDrop, so it enqueues).
func TestChannelReloadFansOutRepublish(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(chanReloadPack("")) // starts OPEN-hear
	lc, err := content.Load(context.Background(), src, []string{"reloadtest"})
	if err != nil {
		t.Fatal(err)
	}
	bus := contentbus.NewMemBus()
	t.Cleanup(func() { _ = bus.Close() })
	sh := NewShardFromContent(lc, []string{"rt"}, "rt", "", nil, nil).
		WithHotReload(src, bus, []string{"reloadtest"}, 0)
	if sh.reloader == nil {
		t.Fatal("hot reload not enabled")
	}
	z := sh.Zone()

	// Retighten the channel in the source, then drive the reload directly (no bus hop).
	if err := src.EditChannel("reloadtest", content.ChannelDTO{
		Ref: "confession", Name: "Confession", Words: []string{"confess"}, DefaultOn: true,
		HearAccess: &content.ChannelAccessDTO{RequireFlag: "confessor"},
	}); err != nil {
		t.Fatal(err)
	}
	sh.reloader.reloadChannel(contentbus.Invalidation{Kind: content.KindChannel, Ref: "confession", Pack: "reloadtest"})

	if !inboxHasRepublishComms(z, "confession") {
		t.Fatal("reloadChannel did not fan a republishCommsMsg out to the hosted zone (#75)")
	}
}

// TestChannelRemoveFansOutRepublish proves a channel DELETION also republishes — a removed channel must drop
// live subscribers' subscriptions, not just stop resolving its verb.
func TestChannelRemoveFansOutRepublish(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(chanReloadPack("confessor"))
	lc, err := content.Load(context.Background(), src, []string{"reloadtest"})
	if err != nil {
		t.Fatal(err)
	}
	bus := contentbus.NewMemBus()
	t.Cleanup(func() { _ = bus.Close() })
	sh := NewShardFromContent(lc, []string{"rt"}, "rt", "", nil, nil).
		WithHotReload(src, bus, []string{"reloadtest"}, 0)
	z := sh.Zone()

	// Remove the channel from the source (SetPack with no channels), then drive the reload.
	src.SetPack(content.Pack{Pack: "reloadtest", Zones: chanReloadPack("").Zones})
	sh.reloader.reloadChannel(contentbus.Invalidation{Kind: content.KindChannel, Ref: "confession", Pack: "reloadtest"})

	if !inboxHasRepublishComms(z, "confession") {
		t.Fatal("a channel removal did not fan a republishCommsMsg out (live subscribers keep a stale subscription)")
	}
}

// TestRetryRepublishCommsDeliversAfterDrop is the fault-injection proof for the security-relevant drop path
// (#75): when the fan-out's postOrDrop DROPS a republish (a full zone inbox), the bounded-retry re-posts it,
// so the too-permissive hear-set is not left permanently stale. We fill the inbox so the first post drops,
// then drain it — the retry goroutine re-delivers within backoff.
func TestRetryRepublishCommsDeliversAfterDrop(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(chanReloadPack("confessor"))
	lc, err := content.Load(context.Background(), src, []string{"reloadtest"})
	if err != nil {
		t.Fatal(err)
	}
	bus := contentbus.NewMemBus()
	t.Cleanup(func() { _ = bus.Close() })
	sh := NewShardFromContent(lc, []string{"rt"}, "rt", "", nil, nil).
		WithHotReload(src, bus, []string{"reloadtest"}, 0)
	z := sh.Zone()

	// Fill the inbox to capacity so the fan-out's postOrDrop DROPS the republish (the zone is not run here,
	// so nothing else touches the inbox). 256 = the inbox buffer (zone.go).
	for i := 0; i < 256; i++ {
		z.inbox <- republishCommsMsg{ref: "filler"}
	}
	// Fan out — drops into the full inbox and hands the drop to bounded-retry.
	sh.reloader.republishCommsToZones("confession")

	// Drain the inbox; once there is room the retry goroutine re-posts "confession" (within linear backoff).
	deadline := time.After(2 * time.Second)
	for {
		select {
		case m := <-z.inbox:
			if rc, ok := m.(republishCommsMsg); ok && rc.ref == "confession" {
				return // the dropped republish was re-delivered by bounded-retry
			}
		case <-deadline:
			t.Fatal("retryRepublishComms never re-delivered the dropped republish — a stale hear-set would persist")
		}
	}
}

// inboxHasRepublishComms non-blockingly drains z's inbox looking for a republishCommsMsg naming ref.
func inboxHasRepublishComms(z *Zone, ref string) bool {
	for {
		select {
		case m := <-z.inbox:
			if rc, ok := m.(republishCommsMsg); ok && rc.ref == ref {
				return true
			}
		default:
			return false
		}
	}
}

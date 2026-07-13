package world

import (
	"context"
	"sync"
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

// TestCommsRepublishRetryNotStarvedByReconcileBudget is the #345 guard (comms direction): comms republish
// retries draw from their OWN budget (maxCommsRepublishRetryGoroutines), so a fully-exhausted RECONCILE
// budget must not starve a comms republish retry. We pin the reconcile budget to zero and prove a dropped
// comms republish is still re-delivered. Before #345 both drew from maxReconcileRetryGoroutines, so this
// would have been abandoned (a stale, too-permissive hear-set — the #75 security-relevant miss).
func TestCommsRepublishRetryNotStarvedByReconcileBudget(t *testing.T) {
	sh := coalesceShard(t)
	z := sh.Zone()

	// Reconcile budget exhausted to zero. If comms still shared it, the republish drop below would be abandoned.
	oldRec := maxReconcileRetryGoroutines
	maxReconcileRetryGoroutines = 0
	t.Cleanup(func() { maxReconcileRetryGoroutines = oldRec })

	// Fill the inbox so the fan-out's postOrDrop DROPS and hands the drop to bounded-retry.
	for i := 0; i < cap(z.inbox); i++ {
		z.inbox <- republishCommsMsg{ref: "filler"}
	}
	sh.reloader.republishCommsToZones("confession")

	// The comms retry (its own budget) re-delivers once there is room, despite the zeroed reconcile budget.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case m := <-z.inbox:
			if rc, ok := m.(republishCommsMsg); ok && rc.ref == "confession" {
				return // comms retry ran independently of the exhausted reconcile budget (#345)
			}
		case <-deadline:
			t.Fatal("comms republish was starved by the exhausted RECONCILE budget — the budgets are not independent (#345)")
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

// countRepublishComms non-blockingly drains z's inbox, returning how many republishCommsMsg it held. Used to
// assert coalescing collapsed a burst to one message.
func countRepublishComms(z *Zone) int {
	n := 0
	for {
		select {
		case m := <-z.inbox:
			if _, ok := m.(republishCommsMsg); ok {
				n++
			}
		default:
			return n
		}
	}
}

// coalesceShard builds a single-zone shard with hot reload wired, for the #269 coalescing tests.
func coalesceShard(t *testing.T) *Shard {
	t.Helper()
	src := content.NewMemSource()
	src.SetPack(chanReloadPack(""))
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
	return sh
}

// TestChannelReloadBurstCoalescesToOneRepublishPerZone is the #269 headline. A pack edit touching K channels
// emits K serial KindChannel invalidations, each calling republishCommsToZones. Because republishAllComms is
// ref-independent, the FIRST already produces the correct final state — the other K-1 are redundant shard-wide
// republish storms. The coalescing flag must collapse the burst to ONE queued republish per zone.
func TestChannelReloadBurstCoalescesToOneRepublishPerZone(t *testing.T) {
	sh := coalesceShard(t)
	z := sh.Zone()

	// Simulate a 20-channel pack edit: 20 KindChannel fan-outs arrive back-to-back on the subscriber
	// goroutine, before the zone (not run here) drains any of them.
	const k = 20
	for i := 0; i < k; i++ {
		sh.reloader.republishCommsToZones("confession")
	}

	if n := countRepublishComms(z); n != 1 {
		t.Fatalf("a %d-channel reload burst queued %d republishes; coalescing must collapse it to exactly 1 "+
			"per zone (#269)", k, n)
	}
}

// TestCoalescedRepublishConvergesAfterProcessing proves the flag DISARMS when the zone processes the message,
// so a genuinely later channel edit still gets its own republish — coalescing must not permanently suppress.
func TestCoalescedRepublishConvergesAfterProcessing(t *testing.T) {
	sh := coalesceShard(t)
	z := sh.Zone()

	sh.reloader.republishCommsToZones("confession")
	if n := countRepublishComms(z); n != 1 {
		t.Fatalf("first fan-out queued %d, want 1", n)
	}
	// countRepublishComms only drained the inbox; the zone handler is what disarms the flag. Process the
	// message the way the zone loop would.
	z.handle(republishCommsMsg{ref: "confession"})

	// A later edit must now get its own republish — the flag was disarmed.
	sh.reloader.republishCommsToZones("confession")
	if n := countRepublishComms(z); n != 1 {
		t.Fatalf("a later edit after the zone processed the first queued %d republishes, want 1 — the "+
			"coalescing flag did not disarm (all future republishes suppressed)", n)
	}
}

// TestCoalesceFlagReleasedOnRetryExhaustion is the correctness guard on the flag lifecycle. The flag stays
// armed across a retry (so a concurrent edit coalesces onto the in-flight one), but if the retry is abandoned
// — here by shrinking the budget to zero so the drop is dropped outright — it MUST be released, or the flag
// latches and suppresses every future republish to that zone (a stale hear-set that never clears).
func TestCoalesceFlagReleasedOnRetryExhaustion(t *testing.T) {
	sh := coalesceShard(t)
	z := sh.Zone()

	// Force the COMMS republish budget to zero so the dropped fan-out is abandoned immediately (no retry
	// goroutine). #345: comms republish draws from its own budget, distinct from the reconcile budget.
	old := maxCommsRepublishRetryGoroutines
	maxCommsRepublishRetryGoroutines = 0
	t.Cleanup(func() { maxCommsRepublishRetryGoroutines = old })

	// Fill the inbox so the fan-out's postOrDrop drops.
	for i := 0; i < cap(z.inbox); i++ {
		z.inbox <- republishCommsMsg{ref: "filler"}
	}
	sh.reloader.republishCommsToZones("confession") // arms, drops, budget-exhausted -> must release the flag
	if z.commsRepublishArmed.Load() {
		t.Fatal("the coalescing flag stayed armed after the retry was abandoned — every future republish to " +
			"this zone would be suppressed, leaving a permanently stale hear-set (#269)")
	}

	// Prove it: drain the inbox and a fresh fan-out must queue a new republish.
	_ = countRepublishComms(z)
	sh.reloader.republishCommsToZones("confession")
	if n := countRepublishComms(z); n != 1 {
		t.Fatalf("after the flag was released a fresh fan-out queued %d republishes, want 1", n)
	}
}

// threeChannelPack builds a source pack with three OPEN-hear channels + a minimal zone, so a multi-channel
// reload burst has distinct refs to edit.
func threeChannelPack() content.Pack {
	ch := func(ref, word string) content.ChannelDTO {
		return content.ChannelDTO{Ref: ref, Name: ref, Words: []string{word}, DefaultOn: true}
	}
	return content.Pack{
		Pack:     "reloadtest",
		Channels: []content.ChannelDTO{ch("alpha", "a"), ch("beta", "b"), ch("gamma", "g")},
		Zones: []content.ZoneDTO{{
			Ref: "rt", Name: "RT", StartRoom: "rt:room:start",
			Rooms: []content.RoomDTO{{Ref: "rt:room:start", Name: "Start", Long: "A room."}},
		}},
	}
}

// TestMultiChannelReloadConvergesToCorrectHearSet is the convergence proof both reviews asked for: coalescing
// must drop redundant MESSAGES without dropping UPDATES. A burst of THREE distinct channel edits — two
// retightened out of the player's reach, one left open — must, after the single coalesced republish runs,
// leave the player hearing exactly the still-open channel. If coalescing dropped a needed update, the final
// hear-set would be wrong (a stale, too-permissive set — the #75 security-relevant miss).
func TestMultiChannelReloadConvergesToCorrectHearSet(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(threeChannelPack())
	lc, err := content.Load(context.Background(), src, []string{"reloadtest"})
	if err != nil {
		t.Fatal(err)
	}
	wbus, gate := commbus.NewWorldBus()
	t.Cleanup(func() { _ = wbus.Close() })
	bus := contentbus.NewMemBus()
	t.Cleanup(func() { _ = bus.Close() })
	sh := NewShardFromContent(lc, []string{"rt"}, "rt", "", nil, nil).
		WithComms(wbus).
		WithHotReload(src, bus, []string{"reloadtest"}, 0)
	z := sh.Zone()

	// A live player who carries no flags — so a channel gated behind any require_flag drops out of their
	// hear-set, while an open channel stays.
	player := newTestPlayerEntity(z, "Listener")
	z.players["Listener"] = player
	cfg := drainConfig(t, gate, "Listener")

	// The burst: retighten alpha and gamma behind a flag the player lacks; leave beta open. Edit the source,
	// then drive each reload directly (K serial KindChannel invalidations, as the subscriber would).
	retighten := func(ref string) {
		if err := src.EditChannel("reloadtest", content.ChannelDTO{
			Ref: ref, Name: ref, Words: []string{ref[:1]}, DefaultOn: true,
			HearAccess: &content.ChannelAccessDTO{RequireFlag: "privileged"},
		}); err != nil {
			t.Fatal(err)
		}
		sh.reloader.reloadChannel(contentbus.Invalidation{Kind: content.KindChannel, Ref: ref, Pack: "reloadtest"})
	}
	retighten("alpha")
	retighten("gamma") // beta is deliberately left untouched (still open)

	// Coalesced: the three swaps produced ONE queued republish, not three.
	if n := countRepublishComms(z); n != 1 {
		t.Fatalf("a 2-edit burst queued %d republishes; coalescing must collapse to 1 (#269)", n)
	}
	// Run the single coalesced republish on the zone goroutine, as the loop would.
	z.handle(republishCommsMsg{ref: "alpha"})

	// The single coalesced republish pushes exactly ONE config for the player; block for it (gate delivery is
	// async). Its hear-set must reflect BOTH retightens AND the untouched channel: alpha/gamma gone, beta kept.
	last, ok := recvConfig(t, cfg)
	if !ok {
		t.Fatal("the coalesced republish pushed no config for the live player")
	}
	if containsStr(last.HearChannels, "alpha") || containsStr(last.HearChannels, "gamma") {
		t.Fatalf("final hear-set %v still includes a retightened channel — coalescing dropped a needed "+
			"update, leaving a stale too-permissive set (#269 must converge, not just dedupe)", last.HearChannels)
	}
	if !containsStr(last.HearChannels, "beta") {
		t.Fatalf("final hear-set %v dropped the still-open channel beta", last.HearChannels)
	}
}

// TestCoalesceFlagReleasedOnRetryExhaustion_Attempts covers the flag release on the RETRIES-exhausted path
// (distinct from the budget-exhausted path): the retry goroutine re-posts reconcileRetryAttempts times into a
// still-full inbox, all drop, and it must release the flag on the way out — or the flag latches and suppresses
// every future republish to this zone.
func TestCoalesceFlagReleasedOnRetryExhaustion_Attempts(t *testing.T) {
	sh := coalesceShard(t)
	z := sh.Zone()

	// Fill the inbox and keep it full (the zone is never run), so every retry attempt's postOrDrop drops.
	for i := 0; i < cap(z.inbox); i++ {
		z.inbox <- republishCommsMsg{ref: "filler"}
	}
	sh.reloader.republishCommsToZones("confession") // arms, drops, hands to a retry goroutine that will exhaust

	// After all attempts exhaust (linear backoff over reconcileRetryAttempts), the flag must be released.
	deadline := time.After(5 * time.Second)
	for z.commsRepublishArmed.Load() {
		select {
		case <-deadline:
			t.Fatal("the coalescing flag stayed armed after the retry EXHAUSTED its attempts — future " +
				"republishes to this zone would be suppressed (#269)")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestCoalesceFlagReleasedOnReloaderStop covers the flag release on the reloader-STOP path: a retry in flight
// when the reloader stops (retryDone closed) must release the flag rather than leave it latched on a zone that
// may outlive the reloader.
func TestCoalesceFlagReleasedOnReloaderStop(t *testing.T) {
	sh := coalesceShard(t)
	z := sh.Zone()

	for i := 0; i < cap(z.inbox); i++ {
		z.inbox <- republishCommsMsg{ref: "filler"}
	}
	sh.reloader.republishCommsToZones("confession") // arms, drops, retry goroutine now backing off
	if !z.commsRepublishArmed.Load() {
		t.Fatal("precondition: the flag should be armed while the retry is in flight")
	}
	sh.reloader.stop() // closes retryDone; the retry's select must release the flag and return

	deadline := time.After(5 * time.Second)
	for z.commsRepublishArmed.Load() {
		select {
		case <-deadline:
			t.Fatal("the coalescing flag stayed armed after the reloader stopped mid-retry (#269)")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestConcurrentReloadBurstConvergesUnderRace is the net for the disarm-ORDERING decision. The lost-edit
// failure mode (disarm AFTER republishAllComms) is only reachable when a channel edit's registry swap lands
// between the zone's registry read and its disarm — an inherently concurrent window. We run the zone loop and,
// over many rounds, fire a small rapid burst that ENDS in a known gate state, then require the player's
// hear-set to converge to it. A disarm-after regression drops the burst's final coalesced edit, so at least
// one round's convergence check fails. Many rounds push the cumulative detection toward certainty while the
// whole test stays well under a second.
func TestConcurrentReloadBurstConvergesUnderRace(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(chanReloadPack("")) // starts open-hear
	lc, err := content.Load(context.Background(), src, []string{"reloadtest"})
	if err != nil {
		t.Fatal(err)
	}
	wbus, gate := commbus.NewWorldBus()
	t.Cleanup(func() { _ = wbus.Close() })
	bus := contentbus.NewMemBus()
	t.Cleanup(func() { _ = bus.Close() })
	sh := NewShardFromContent(lc, []string{"rt"}, "rt", "", nil, nil).
		WithComms(wbus).
		WithHotReload(src, bus, []string{"reloadtest"}, 0)
	z := sh.Zone()

	// Register the player BEFORE the loop starts — z.players is zone-goroutine-owned.
	player := newTestPlayerEntity(z, "Listener")
	z.players["Listener"] = player
	cfg := drainConfig(t, gate, "Listener")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	// Drain the config stream CONTINUOUSLY into a shared "latest hear-set", so the gate's bounded delivery
	// buffer never fills mid-burst (which would block the zone goroutine on publish and stall the test).
	var mu sync.Mutex
	var latest []string
	seen := false
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case p := <-cfg:
				mu.Lock()
				latest, seen = p.HearChannels, true
				mu.Unlock()
			}
		}
	}()

	gate2 := func(on bool) {
		ha := &content.ChannelAccessDTO{}
		if on {
			ha.RequireFlag = "privileged" // player lacks it -> not hearable
		}
		_ = src.EditChannel("reloadtest", content.ChannelDTO{
			Ref: "confession", Name: "Confession", Words: []string{"confess"}, DefaultOn: true, HearAccess: ha,
		})
		sh.reloader.reloadChannel(contentbus.Invalidation{Kind: content.KindChannel, Ref: "confession", Pack: "reloadtest"})
	}
	// hearsExpected reports whether the latest config matches the desired open/closed state.
	hearsExpected := func(open bool) bool {
		mu.Lock()
		defer mu.Unlock()
		return seen && containsStr(latest, "confession") == open
	}

	for round := 0; round < 30; round++ {
		open := round%2 == 0
		// A rapid burst that creates the coalescing window, ending in the round's target state. gate2(true)
		// GATES the channel (un-hearable); gate2(false) opens it. So to END hearable (open==true) the final
		// call must be gate2(false) == gate2(!open).
		for i := 0; i < 6; i++ {
			gate2(open)  // churn to the opposite...
			gate2(!open) // ...then back to the target (open -> gate2(false) -> hearable)
		}
		deadline := time.After(3 * time.Second)
		for !hearsExpected(open) {
			select {
			case <-deadline:
				t.Fatalf("round %d: the player's hear-set never converged to open=%v — a coalesced edit was "+
					"lost (disarm ordering regression, #269)", round, open)
			case <-time.After(2 * time.Millisecond):
			}
		}
	}
}

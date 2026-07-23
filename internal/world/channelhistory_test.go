package world

import (
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// channelhistory_test.go is the white-box test set for #348 (member-gated channel scrollback, slice 1):
// the shard-local capture ring + the `history <channel>` command + the load-bearing FETCH-TIME canHear
// gate (P8-D3). Capture happens at the SOURCE publish path (cmdChannel), so these tests drive real
// dispatch through commTestShard (the same harness channel_test.go uses) and assert the buffered lines.

// registerHistChannel installs a channel with a `history` buffer directly into the shard's registry
// (content the demo doesn't ship) and returns its def. openHear leaves hearing open; a non-empty
// hearFlag makes a split hear_access that requires that flag (the fetch-time-gate shape).
func registerHistChannel(t *testing.T, sh *Shard, ref, name string, history int, hearFlag string) *channelDef {
	t.Helper()
	dto := content.ChannelDTO{
		Ref: ref, Name: name, Words: []string{ref}, History: history, DefaultOn: true,
	}
	if hearFlag != "" {
		dto.HearAccess = &content.ChannelAccessDTO{RequireFlag: hearFlag}
	}
	def := buildChannelDef(dto)
	sh.defs.channel.register(ref, def)
	return def
}

// TestChannelHistoryCaptureEvicts proves the ring keeps only the last N lines (oldest evicted) when more
// than N are published, in order.
func TestChannelHistoryCaptureEvicts(t *testing.T) {
	sh, z, _ := commTestShard(t)
	const n = 3
	registerHistChannel(t, sh, "log", "Log", n, "")

	s := newTestPlayerEntity(z, "Amy")
	// Publish n+2 lines; only the last n survive.
	for _, msg := range []string{"one", "two", "three", "four", "five"} {
		z.dispatch(s, "log "+msg)
	}

	ring := z.channelHistory().snapshot("log")
	if len(ring) != n {
		t.Fatalf("ring holds %d lines, want %d (oldest evicted)", len(ring), n)
	}
	wantTails := []string{"three", "four", "five"}
	for i, want := range wantTails {
		if !strings.HasSuffix(ring[i].body, want) {
			t.Fatalf("ring[%d] body = %q, want a line ending in %q (order/eviction wrong)", i, ring[i].body, want)
		}
	}
}

// TestChannelHistoryRetrieval proves `history <ref>` prints the buffered lines to the fetching player.
func TestChannelHistoryRetrieval(t *testing.T) {
	sh, z, _ := commTestShard(t)
	registerHistChannel(t, sh, "log", "Log", 5, "")

	s := newTestPlayerEntity(z, "Bea")
	z.dispatch(s, "log alpha")
	z.dispatch(s, "log beta")

	drain(s) // discard the say echoes/prompts so nextOutput sees only the history render
	z.dispatch(s, "history log")
	out := nextOutput(t, s)
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("`history log` did not replay the buffered lines: %q", out)
	}
}

// TestChannelHistoryFetchTimeGate is THE critical negative test (the P8-D3 invariant): a channel with a
// split hear predicate buffers lines while a reader qualifies; once the reader LOSES hear access the
// `history` fetch is refused and emits ZERO privileged lines — canHear is evaluated at FETCH TIME, not
// "was a member when the line was said".
func TestChannelHistoryFetchTimeGate(t *testing.T) {
	sh, z, _ := commTestShard(t)
	// Open speak, hear gated on the "vip" flag (a `confess`-shape channel), with a scrollback buffer.
	registerHistChannel(t, sh, "vault", "Vault", 5, "vip")

	reader := newTestPlayerEntity(z, "Rita")
	setFlag(reader.entity, "vip", true) // qualifies to hear

	// Speak (open) two lines that land in the ring. The distinctive token proves no leak after demotion.
	z.dispatch(reader, "vault secret-one")
	z.dispatch(reader, "vault secret-two")

	// While qualified, history returns the lines.
	z.dispatch(reader, "history vault")
	if !drainContains(t, reader, "secret-one") {
		t.Fatal("a qualified reader could not read the channel history")
	}

	// DEMOTE: strip the flag. The reader can no longer hear the channel.
	setFlag(reader.entity, "vip", false)
	z.dispatch(reader, "history vault")
	if !drainContains(t, reader, "can't hear") {
		t.Fatal("the demoted reader was not refused at fetch time (the P8-D3 invariant)")
	}
	// And CRITICALLY: not one privileged line leaked.
	if drainContains(t, reader, "secret") {
		t.Fatal("a demoted reader replayed privileged history lines — the fetch-time gate leaked")
	}
}

// TestChannelHistoryIgnoreFilter proves the fetching player's ignore set is re-applied at read time, so
// an ignored author's lines never appear in history output (parity with the live receiver funnel).
func TestChannelHistoryIgnoreFilter(t *testing.T) {
	sh, z, _ := commTestShard(t)
	registerHistChannel(t, sh, "log", "Log", 10, "")

	loud := newTestPlayerEntity(z, "Loud")
	reader := newTestPlayerEntity(z, "Quiet")

	z.dispatch(loud, "log annoying-noise")
	z.dispatch(reader, "log my-own-line")

	// The reader ignores Loud.
	commsOf(reader).ignore["Loud"] = struct{}{}

	z.dispatch(reader, "history log")
	if !drainContains(t, reader, "my-own-line") {
		t.Fatal("the reader's own line was dropped from history")
	}
	if drainContains(t, reader, "annoying-noise") {
		t.Fatal("an ignored author's line survived in history (the ignore funnel was not re-applied)")
	}
}

// TestChannelHistoryZeroBufferNotTracked proves a channel with history==0 captures nothing (a content
// opt-out) and `history` reports no recent history.
func TestChannelHistoryZeroBufferNotTracked(t *testing.T) {
	sh, z, _ := commTestShard(t)
	registerHistChannel(t, sh, "chatter", "Chatter", 0, "") // history == 0 => no ring

	s := newTestPlayerEntity(z, "Cam")
	z.dispatch(s, "chatter hello")
	z.dispatch(s, "chatter world")

	if ring := z.channelHistory().snapshot("chatter"); len(ring) != 0 {
		t.Fatalf("a history==0 channel captured %d lines, want 0 (content opt-out)", len(ring))
	}
	z.dispatch(s, "history chatter")
	if !drainContains(t, s, "No recent history") {
		t.Fatal("a history==0 channel did not report empty scrollback")
	}
}

// TestChannelHistoryMultiShardFooter is #401: the history ring is SHARD-LOCAL, so on a multi-shard fleet the
// view is potentially partial — `history <channel>` appends an in-band "may be incomplete" footer, but ONLY
// on a multi-shard deployment. A single-shard run (the common case and every other test here) shows no
// footer, so the caveat never noises up the normal case.
func TestChannelHistoryMultiShardFooter(t *testing.T) {
	sh, z, _ := commTestShard(t)
	registerHistChannel(t, sh, "log", "Log", 5, "")
	s := newTestPlayerEntity(z, "Cid")
	z.dispatch(s, "log hello")

	// Single-shard (sh.dir and sh.peers both nil): no footer.
	drain(s)
	z.dispatch(s, "history log")
	out := nextOutput(t, s)
	if !strings.Contains(out, "hello") {
		t.Fatalf("history did not replay the buffered line: %q", out)
	}
	if strings.Contains(out, "shard-local") {
		t.Fatalf("single-shard history must NOT carry the multi-shard footer: %q", out)
	}

	// Flip to a multi-shard fleet (a directory = another shard can route to us) and re-fetch.
	sh.dir = newFakeLocator("Cid")
	drain(s)
	z.dispatch(s, "history log")
	out = nextOutput(t, s)
	if !strings.Contains(out, "hello") {
		t.Fatalf("multi-shard history lost its line: %q", out)
	}
	if !strings.Contains(out, "shard-local") {
		t.Fatalf("multi-shard history must carry the partial-view footer: %q", out)
	}
}

// TestChannelHistoryBareZoneNoPanic proves a bare zone (no shard, nil chanHistory) degrades to "No
// recent history" and never panics. We register the channel def directly on the bare zone's registry.
func TestChannelHistoryBareZoneNoPanic(t *testing.T) {
	z := newZone("bare") // no shard => z.channelHistory() is nil
	z.channelDefs().register("log", buildChannelDef(content.ChannelDTO{
		Ref: "log", Name: "Log", Words: []string{"log"}, History: 5, DefaultOn: true,
	}))
	s := newTestPlayerEntity(z, "Solo")

	z.dispatch(s, "history log") // must not panic
	if !drainContains(t, s, "No recent history") {
		t.Fatal("a bare zone did not degrade `history` cleanly to No recent history")
	}
}

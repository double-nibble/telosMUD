package world

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	roster "github.com/double-nibble/telosmud/internal/presence"
)

// luadisplay_who_test.go — #24 part (a): the COLLECTION-bound display surface. `who` is the first surface whose
// subject is a LIST rather than the viewer, and whose data is fetched by blocking I/O OFF the zone goroutine.
//
// Two invariants are under test:
//
//  1. SINGLE-WRITER. The Lua VM is one-per-zone and zone-goroutine-owned. The async roster fetch must post its
//     result back to the zone inbox (whoRenderMsg) and the render must happen in that handler — never in the
//     fetch goroutine. TestWhoPostsRenderBackToZoneGoroutine asserts the message; the -race suite proves the
//     absence of a VM race.
//  2. CONCEALMENT. Rows are filtered in Go BEFORE binding, so a concealed player is never a record a template
//     could disclose.

// --- the collection binding ---------------------------------------------------------------------

// TestRenderDisplayListBindsSelfAndList pins the two binds of the collection renderer: `self` is the viewer's
// entity handle (live, re-resolved) and `list` is a plain-data array of records — never handles, because a
// roster row may describe a player hosted on ANOTHER shard (T7: no handle names a foreign-zone entity).
func TestRenderDisplayListBindsSelfAndList(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	z.defBundle().displayDefs["who"] = `
		local out = {"viewer=" .. self:name(), "n=" .. #list}
		for _, p in ipairs(list) do
			out[#out+1] = p.name .. "/" .. p.shard .. "/afk=" .. tostring(p.afk)
		end
		return table.concat(out, " ")`

	entries := []roster.Entry{
		{PlayerID: "bob", Name: "Bob", ShardID: "shard-b", AFK: true},
		{PlayerID: "amy", Name: "Amy", ShardID: "shard-a"},
	}
	got, ok := z.renderWhoSheet(viewer, entries, false)
	if !ok {
		t.Fatal("the who template should have rendered")
	}
	// `self` is the viewer's handle; the roster rows are sorted by name (Amy before Bob) and carry their fields.
	for _, want := range []string{"viewer=Viewer", "n=2", "Amy/shard-a/afk=false", "Bob/shard-b/afk=true"} {
		if !strings.Contains(got, want) {
			t.Fatalf("who template binding missing %q; got %q", want, got)
		}
	}
	if strings.Index(got, "Amy") > strings.Index(got, "Bob") {
		t.Fatalf("roster rows must be name-sorted (the built-in renderWho order): %q", got)
	}
}

// TestWhoTemplateNeverSeesConcealedRows is the collection-surface counterpart of the occupants() leak test: the
// concealment filter runs in GO, before binding, so a concealed row is not merely un-rendered — it is ABSENT
// from `list` and the template cannot disclose it however it chooses to render. A holylight viewer sees it.
func TestWhoTemplateNeverSeesConcealedRows(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	// A deliberately LEAKY template: it dumps every row it is handed, concealment bit and all.
	z.defBundle().displayDefs["who"] = `
		local out = {}
		for _, p in ipairs(list) do out[#out+1] = p.name end
		return "WHO[" .. table.concat(out, ",") .. "]"`

	entries := []roster.Entry{
		{PlayerID: "amy", Name: "Amy"},
		{PlayerID: "sneak", Name: "Sneak", Concealed: true},
	}

	ordinary, ok := z.renderWhoSheet(viewer, entries, false)
	if !ok {
		t.Fatal("the who template should have rendered")
	}
	if strings.Contains(ordinary, "Sneak") {
		t.Fatalf("LEAK: a concealed roster row was bound into a content template: %q", ordinary)
	}
	if !strings.Contains(ordinary, "WHO[Amy]") {
		t.Fatalf("the visible row must be bound: %q", ordinary)
	}

	seeAll, _ := z.renderWhoSheet(viewer, entries, true)
	if !strings.Contains(seeAll, "Sneak") {
		t.Fatalf("a holylight viewer must be handed the concealed rows: %q", seeAll)
	}
}

// TestWhoTemplateFallsBack: no template / a broken template both degrade to the built-in renderWho output.
func TestWhoTemplateFallsBack(t *testing.T) {
	entries := []roster.Entry{{PlayerID: "amy", Name: "Amy"}}
	for _, tc := range []struct{ name, body string }{
		{"no template", ""},
		{"non-string return", `return 42`},
		{"runtime error", `error("boom")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z, _, room := harmZone(t)
			viewer := harmPlayer(z, room, "Viewer")
			if tc.body != "" {
				z.defBundle().displayDefs["who"] = tc.body
			}
			if _, ok := z.renderWhoSheet(viewer, entries, false); ok {
				t.Fatal("renderWhoSheet must report !ok so the caller uses the built-in renderWho")
			}
			// The built-in output is unchanged.
			if got := renderWho(entries, false); !strings.Contains(got, "Players online:") ||
				!strings.Contains(got, "Amy") {
				t.Fatalf("the built-in who fallback changed: %q", got)
			}
		})
	}
}

// TestRenderWhoUnchanged pins the exact pre-#24 built-in output (header, one indented name per line, AFK mark,
// name-sorted) — the fallback every pack without a `who` template still gets.
func TestRenderWhoUnchanged(t *testing.T) {
	entries := []roster.Entry{
		{PlayerID: "bob", Name: "Bob", AFK: true},
		{PlayerID: "amy", Name: "Amy"},
		{PlayerID: "nameless"}, // no display name => the player id
	}
	want := "Players online:\n Amy\n Bob (AFK)\n nameless"
	if got := renderWho(entries, false); got != want {
		t.Fatalf("built-in renderWho output drifted:\n got %q\nwant %q", got, want)
	}
}

// --- single-writer: the async fetch must bounce back onto the zone goroutine --------------------

// whoTestShard builds a presence-enabled demo shard WITHOUT running its zone loop, so a test can inspect what
// cmdWho posts to the inbox rather than racing the handler. The roster is seeded directly (the tracker's eager
// write loop only runs under sh.Run).
func whoTestShard(t *testing.T, rost roster.Roster) (*Zone, *session) {
	t.Helper()
	sh := NewDemoShard().WithPresence(rost, "shard-a")
	z := sh.Zone()
	z.whoCooldown = 0
	s := newTestPlayerEntity(z, "Alice")
	s.entity.short = "Alice"
	for _, r := range z.rooms { // any room: cmdWho does not read the location, but dispatch prompts need one
		Move(s.entity, r)
		break
	}
	z.players["Alice"] = s
	return z, s
}

// TestWhoPostsRenderBackToZoneGoroutine is THE single-writer gate. cmdWho spawns a goroutine to do the blocking
// roster read; that goroutine must NOT render (the Lua VM is zone-goroutine-owned). It must post the raw entries
// back as a whoRenderMsg, and the ZONE goroutine's handler renders them. We assert on the message itself, so the
// discipline is pinned structurally rather than inferred from output.
func TestWhoPostsRenderBackToZoneGoroutine(t *testing.T) {
	mem := roster.NewMem()
	if err := mem.Set(context.Background(), "shard-a",
		[]roster.Entry{{PlayerID: "Alice", Name: "Alice", ShardID: "shard-a"}}, roster.DefaultTTL); err != nil {
		t.Fatal(err)
	}
	z, s := whoTestShard(t, mem)

	z.dispatch(s, "who") // runs cmdWho inline (as the zone goroutine would); it spawns the fetch goroutine

	// No output frame may be produced by the fetch goroutine — the render has not happened yet.
	m := recvInbox(t, z, 2*time.Second)
	wr, ok := m.(whoRenderMsg)
	if !ok {
		t.Fatalf("the async roster read must post whoRenderMsg back to the zone inbox for an ON-GOROUTINE "+
			"render; got %T (rendering in the fetch goroutine would race the zone's Lua VM)", m)
	}
	if wr.viewer != s.entity || wr.out != s.out {
		t.Fatal("whoRenderMsg must carry the viewer + out channel captured on the zone goroutine")
	}
	if len(wr.entries) == 0 {
		t.Fatal("whoRenderMsg must carry the roster snapshot the fetch goroutine read")
	}

	// Now run the handler as the zone goroutine would: THIS is where the (Lua) render happens.
	z.defBundle().displayDefs["who"] = `return "TEMPLATED:" .. #list`
	z.handle(wr)
	if out := drainAllText(s.out); !strings.Contains(out, "TEMPLATED:1") {
		t.Fatalf("the whoRenderMsg handler must render the `who` template on the zone goroutine: %q", out)
	}
}

// TestWhoRenderMsgFallsBackToBuiltIn: the same handler, with no template, writes the built-in renderWho list.
func TestWhoRenderMsgFallsBackToBuiltIn(t *testing.T) {
	z, s := whoTestShard(t, roster.NewMem())
	delete(z.defBundle().displayDefs, "who") // the demo pack ships one; this test is the no-template pack
	z.handle(whoRenderMsg{
		out:     s.out,
		viewer:  s.entity,
		entries: []roster.Entry{{PlayerID: "amy", Name: "Amy"}},
	})
	out := drainAllText(s.out)
	if !strings.Contains(out, "Players online:") || !strings.Contains(out, "Amy") {
		t.Fatalf("a no-template who must render the built-in list: %q", out)
	}
}

// TestWhoRosterFailurePostsFallbackMsg: an errored roster read still bounces to the zone goroutine (the
// pre-existing whoFallbackMsg), never renders off it.
func TestWhoRosterFailurePostsFallbackMsg(t *testing.T) {
	fr := &failingRoster{inner: roster.NewMem()}
	fr.failList.Store(true)
	z, s := whoTestShard(t, fr)

	z.dispatch(s, "who")
	if _, ok := recvInbox(t, z, 2*time.Second).(whoFallbackMsg); !ok {
		t.Fatal("a failed roster read must post whoFallbackMsg back to the zone goroutine")
	}
}

// recvInbox pulls one message off the zone inbox (the zone loop is NOT running in these tests).
func recvInbox(t *testing.T, z *Zone, d time.Duration) msg {
	t.Helper()
	select {
	case m := <-z.inbox:
		return m
	case <-time.After(d):
		t.Fatal("nothing was posted back to the zone inbox")
		return nil
	}
}

// --- the zone-local (presence-disabled / degraded) path -----------------------------------------

// TestWhoLocalUsesTemplateAndFilters: the zone-local list renders through the SAME `who` template (so a
// player's who looks identical whether the roster is readable), and still omits players the viewer can't see.
func TestWhoLocalUsesTemplateAndFilters(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	sneak := harmPlayer(z, room, "Sneak")
	z.defBundle().displayDefs["who"] = `
		local out = {}
		for _, p in ipairs(list) do out[#out+1] = p.name end
		return "WHO[" .. table.concat(out, ",") .. "]"`

	if got := z.whoLocalSheet(viewer); !strings.Contains(got, "WHO[Sneak,Viewer]") {
		t.Fatalf("the zone-local who must render the template with name-sorted rows: %q", got)
	}
	setFlag(sneak, flagHidden, true)
	got := z.whoLocalSheet(viewer)
	if strings.Contains(got, "Sneak") {
		t.Fatalf("LEAK: a hidden player reached the zone-local who template: %q", got)
	}
	if !strings.Contains(got, "WHO[Viewer]") {
		t.Fatalf("the viewer must always see themselves in the local who: %q", got)
	}
	// A holylight viewer sees the hidden player (the same canSee chokepoint).
	setFlag(viewer, flagHolylight, true)
	if got := z.whoLocalSheet(viewer); !strings.Contains(got, "Sneak") {
		t.Fatalf("holylight must see the hidden player in the local who: %q", got)
	}
}

// TestWhoLocalFallsBackToBuiltIn: with no `who` template the zone-local list is the unchanged pre-#24 output.
func TestWhoLocalFallsBackToBuiltIn(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	got := z.whoLocalSheet(viewer)
	if !strings.Contains(got, "Players online:") || !strings.Contains(got, "Viewer") {
		t.Fatalf("no-template zone-local who must be the built-in list: %q", got)
	}
	if got != z.whoLocal(viewer) {
		t.Fatalf("the fallback must be byte-identical to whoLocal: %q vs %q", got, z.whoLocal(viewer))
	}
}

// --- race: many concurrent `who` renders, one Lua VM --------------------------------------------

// TestWhoTemplateConcurrentRendersAreRaceFree is the -race regression guard for the single-writer rule. Many
// sessions hammer `who` (each spawning an async roster read) while the zone loop also renders `score` templates
// and runs its heartbeat. Every Lua render must land on the ONE zone goroutine; if the who render were done in
// the fetch goroutine, -race would report a data race on the shared LState (and on the entity state the
// templates read). It is a no-op assertion under `go test` without -race, and the real gate under `-race`.
func TestWhoTemplateConcurrentRendersAreRaceFree(t *testing.T) {
	shared := roster.NewMem()
	sh := NewDemoShard().WithPresence(shared, "shard-a")
	sh.presence.heartbeat = 10 * time.Millisecond
	z := sh.Zone()
	z.whoCooldown = 0
	// Both templates touch the zone-owned Lua VM AND live entity state (self:name(), self:room():occupants()).
	z.defBundle().displayDefs["who"] = `
		local n = 0
		for _, p in ipairs(list) do n = n + #p.name end
		return "who:" .. self:name() .. ":" .. n`
	z.defBundle().displayDefs["score"] = `
		local occ = #self:room():occupants()
		return "score:" .. self:name() .. ":" .. occ`

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)

	sessions := make([]*session, 0, 4)
	for _, name := range []string{"Ann", "Ben", "Cas", "Dee"} {
		sessions = append(sessions, joinPlayer(t, z, name))
	}

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *session) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				z.post(inputMsg{id: s.character, line: "who"})
				z.post(inputMsg{id: s.character, line: "score"})
			}
		}(s)
	}
	// Drain concurrently so the out channels never fill and wedge the zone loop.
	done := make(chan struct{})
	var drainWG sync.WaitGroup
	for _, s := range sessions {
		drainWG.Add(1)
		go func(out chan *playv1.ServerFrame) {
			defer drainWG.Done()
			for {
				select {
				case <-out:
				case <-done:
					return
				}
			}
		}(s.out)
	}
	wg.Wait()
	time.Sleep(200 * time.Millisecond) // let the async roster reads post back and render
	close(done)
	drainWG.Wait()
}

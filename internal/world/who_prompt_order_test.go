package world

import (
	"context"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	roster "github.com/double-nibble/telosmud/internal/presence"
)

// who_prompt_order_test.go — #371: on the async (presence-enabled) `who` path the trailing prompt must land
// AFTER the roster sheet, never before it. cmdWho returns immediately (the roster read is off-goroutine) and
// the render happens later in the whoRenderMsg/whoFallbackMsg inbox handler; dispatch must therefore SKIP its
// on-return prompt (ctx.deferPrompt) and let the handler emit the prompt itself, last.

// frameKind classifies a server frame for order assertions: "output" (a text sheet), "prompt", or "other"
// (GMCP HUD deltas etc., which sendPrompt may interleave and which we ignore for ordering).
func frameKind(f *playv1.ServerFrame) string {
	switch {
	case f.GetPrompt() != nil:
		return "prompt"
	case f.GetOutput() != nil:
		return "output"
	default:
		return "other"
	}
}

// drainKinds returns the ordered kinds of every currently-queued frame (non-blocking).
func drainKinds(out chan *playv1.ServerFrame) []string {
	var kinds []string
	for {
		select {
		case f := <-out:
			kinds = append(kinds, frameKind(f))
		default:
			return kinds
		}
	}
}

// seedRoster puts one entry on the shard so the async read succeeds and posts whoRenderMsg.
func seedRoster(t *testing.T, mem *roster.Mem) {
	t.Helper()
	if err := mem.Set(context.Background(), "shard-a",
		[]roster.Entry{{PlayerID: "Alice", Name: "Alice", ShardID: "shard-a"}}, roster.DefaultTTL); err != nil {
		t.Fatal(err)
	}
}

// TestWhoDefersPromptUntilAfterRender is the core #371 gate: dispatch("who") on the async path must NOT emit a
// prompt on return, and the whoRenderMsg handler must emit exactly one prompt AFTER writing the roster sheet.
func TestWhoDefersPromptUntilAfterRender(t *testing.T) {
	mem := roster.NewMem()
	seedRoster(t, mem)
	z, s := whoTestShard(t, mem)

	// Drain any login/setup frames so we observe only the `who` interaction.
	drainKinds(s.out)

	z.dispatch(s, "who") // async: spawns the fetch goroutine, sets deferPrompt, returns without prompting

	// Nothing the dispatch itself produced may be a prompt — the async output has not been written yet, so a
	// prompt here would precede it (the exact #371 bug: "> = Players online: =").
	for _, k := range drainKinds(s.out) {
		if k == "prompt" {
			t.Fatal("dispatch(who) emitted a prompt BEFORE the async roster output (#371): the prompt must be deferred")
		}
	}

	// Deliver the async result on the zone goroutine, as the runtime would.
	m := recvInbox(t, z, 2*time.Second)
	wr, ok := m.(whoRenderMsg)
	if !ok {
		t.Fatalf("async who must post whoRenderMsg; got %T", m)
	}
	if wr.s != s {
		t.Fatal("whoRenderMsg must carry the requesting session so the handler can resolve it for the trailing prompt (#371)")
	}
	z.handle(wr)

	// The handler must have written the sheet and THEN a single trailing prompt — the last frame is the prompt,
	// and at least one output frame precedes it.
	kinds := drainKinds(s.out)
	if len(kinds) == 0 {
		t.Fatal("the whoRenderMsg handler produced no frames")
	}
	if last := kinds[len(kinds)-1]; last != "prompt" {
		t.Fatalf("the trailing frame must be the prompt, got %q (full order: %v)", last, kinds)
	}
	sawOutputBeforePrompt := false
	prompts := 0
	for _, k := range kinds {
		switch k {
		case "output":
			if prompts == 0 {
				sawOutputBeforePrompt = true
			}
		case "prompt":
			prompts++
		}
	}
	if !sawOutputBeforePrompt {
		t.Fatalf("the roster sheet must be written before the prompt, got order %v", kinds)
	}
	if prompts != 1 {
		t.Fatalf("the async who must emit exactly one trailing prompt, got %d (order %v)", prompts, kinds)
	}
}

// TestWhoFallbackDefersPrompt: the roster-read-error path (whoFallbackMsg) owns its trailing prompt too, so a
// degraded shard still prints sheet-then-prompt rather than prompt-then-sheet.
func TestWhoFallbackDefersPrompt(t *testing.T) {
	fr := &failingRoster{inner: roster.NewMem()}
	fr.failList.Store(true)
	z, s := whoTestShard(t, fr)
	drainKinds(s.out)

	z.dispatch(s, "who")
	for _, k := range drainKinds(s.out) {
		if k == "prompt" {
			t.Fatal("dispatch(who) on the fallback path must also defer the prompt (#371)")
		}
	}

	m := recvInbox(t, z, 2*time.Second)
	fb, ok := m.(whoFallbackMsg)
	if !ok {
		t.Fatalf("a failed roster read must post whoFallbackMsg; got %T", m)
	}
	if fb.s != s {
		t.Fatal("whoFallbackMsg must carry the requesting session for the trailing prompt (#371)")
	}
	z.handle(fb)

	kinds := drainKinds(s.out)
	if len(kinds) == 0 || kinds[len(kinds)-1] != "prompt" {
		t.Fatalf("the fallback handler must end with a prompt, got %v", kinds)
	}
}

// TestWhoLatePromptGuardedOnDepartedSession: if the player leaves this zone during the async window (logged
// out / walked to a sibling zone / handed off cross-shard — modeled as removal from z.players), the late
// prompt must NO-OP. The sheet is still written (harmless), but a prompt must not be sent to a stale session.
func TestWhoLatePromptGuardedOnDepartedSession(t *testing.T) {
	mem := roster.NewMem()
	seedRoster(t, mem)
	z, s := whoTestShard(t, mem)
	drainKinds(s.out)

	z.dispatch(s, "who")
	m := recvInbox(t, z, 2*time.Second)
	wr := m.(whoRenderMsg)

	// The session departs this zone before the async result is handled.
	delete(z.players, s.character)

	z.handle(wr)

	for _, k := range drainKinds(s.out) {
		if k == "prompt" {
			t.Fatal("a departed session must not receive the late who prompt (#371 guard)")
		}
	}
}

// TestWhoLatePromptGuardedOnFrozenSession: during a cross-shard handoff the source session is FROZEN but
// stays in z.players until it is reaped — a bounded window where the map-identity guard alone would pass.
// The late who prompt must NO-OP for a frozen session (its frames belong to the handoff machinery, and its
// entity may already be detached from the room). Mirrors the flushHUD frozen/pending guard.
func TestWhoLatePromptGuardedOnFrozenSession(t *testing.T) {
	mem := roster.NewMem()
	seedRoster(t, mem)
	z, s := whoTestShard(t, mem)
	drainKinds(s.out)

	z.dispatch(s, "who")
	m := recvInbox(t, z, 2*time.Second)
	wr := m.(whoRenderMsg)

	// The session enters a cross-shard handoff: frozen, but still mapped in z.players.
	s.frozen = true

	z.handle(wr)

	for _, k := range drainKinds(s.out) {
		if k == "prompt" {
			t.Fatal("a frozen (mid-handoff) session must not receive the late who prompt (#371 guard)")
		}
	}
}

// TestWhoLatePromptGuardedOnReconnect: a NEW session for the same character (reconnect during the async
// window) must not receive the OLD request's trailing prompt — the guard compares session identity, not just
// the character key.
func TestWhoLatePromptGuardedOnReconnect(t *testing.T) {
	mem := roster.NewMem()
	seedRoster(t, mem)
	z, s := whoTestShard(t, mem)
	drainKinds(s.out)

	z.dispatch(s, "who")
	m := recvInbox(t, z, 2*time.Second)
	wr := m.(whoRenderMsg)

	// A fresh login replaces the character's session in z.players (a different *session, different out).
	s2 := newTestPlayerEntity(z, "Alice")
	for _, r := range z.rooms {
		Move(s2.entity, r)
		break
	}
	z.players["Alice"] = s2

	z.handle(wr)

	// The NEW session must not get the old request's prompt.
	for _, k := range drainKinds(s2.out) {
		if k == "prompt" {
			t.Fatal("a reconnected session must not receive the previous session's late who prompt (#371 guard)")
		}
	}
}

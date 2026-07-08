package world

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// act_utf8_test.go — the docs/REMAINING.md Track-0 (#21) render-path UTF-8 guarantee for act() NAME
// substitution. $n/$N resolve through nameFor to an entity's display name and the assembled line is
// capitalizeFirst'd; a multibyte name (accented Latin, fully-RTL Arabic) must reach a viewer BYTE-INTACT —
// never split, reordered, or corrupted — and a lowercase multibyte initial must capitalize correctly. The
// capitalizeFirst unit (incl. caseless Arabic/CJK) is pinned by act_cap_test.go; this exercises the full
// act() -> nameFor -> capitalizeFirst -> session pipeline end to end.
func TestActRendersMultibyteNames(t *testing.T) {
	z, _, room := harmZone(t)
	actor := harmPlayer(z, room, "Élise") // "Élise" — accented Latin initial (É = U+00C9)
	victim := harmPlayer(z, room, "بلال") // a fully-RTL Arabic name
	harmPlayer(z, room, "Observer")
	obs := z.players["Observer"]

	drainOutputs(obs)
	z.act("$n bows to $N.", actor, nil, victim, "", "", ToRoom)
	got := strings.Join(drainOutputs(obs), "")

	if !strings.Contains(got, "Élise") {
		t.Errorf("act() dropped/mangled the multibyte $n actor name; got %q", got)
	}
	if !strings.Contains(got, "بلال") {
		t.Errorf("act() dropped/mangled the multibyte $N victim name; got %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("act() produced invalid UTF-8 (a rune was split/reordered): %q", got)
	}
}

// TestActCapitalizesMultibyteInitial: a lowercase multibyte name at line start is capitalized through the
// full act() pipeline (é -> É), not left lowercase and not byte-corrupted — the classic Diku initial-cap must
// be rune-correct, not byte-naive.
func TestActCapitalizesMultibyteInitial(t *testing.T) {
	z, _, room := harmZone(t)
	actor := harmPlayer(z, room, "élise") // "élise" — lowercase é (U+00E9) initial
	harmPlayer(z, room, "Observer")
	obs := z.players["Observer"]

	drainOutputs(obs)
	z.act("$n waves.", actor, nil, nil, "", "", ToRoom)
	got := strings.Join(drainOutputs(obs), "")
	if !strings.Contains(got, "Élise waves") { // "Élise waves"
		t.Errorf("act() should capitalize the multibyte initial (é->É) end-to-end; got %q", got)
	}
	if strings.Contains(got, "élise") {
		t.Errorf("act() left the multibyte initial lowercase; got %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("act() produced invalid UTF-8: %q", got)
	}
}

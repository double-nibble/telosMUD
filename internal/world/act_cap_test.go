package world

import "testing"

// act_cap_test.go — the presentation initial-cap (Track 1, act.go capitalizeFirst): a line/message beginning
// with a lowercase-authored short renders capitalized, TOKEN-AWARE (caps the text inside a leading {{color}}
// token, not the marker), and inert on caseless scripts (Arabic/CJK) so it composes with the i18n render path.
func TestCapitalizeFirst(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"lowercase article", "a torch lies here.", "A torch lies here."},
		{"already capital", "You say hi.", "You say hi."},
		{"proper name", "Kurt waves.", "Kurt waves."},
		{"leading color token — cap the text not the brace", "{{FG_CYAN}}north, east", "{{FG_CYAN}}North, east"},
		{"multiple leading tokens", "{{FG_RED}}{{BOLD}}warning", "{{FG_RED}}{{BOLD}}Warning"},
		{"leading whitespace", "  a goblin arrives.", "  A goblin arrives."},
		{"digit first — unchanged", "5 gold coins.", "5 gold coins."},
		{"empty", "", ""},
		{"only a token, no text — unchanged", "{{FG_RED}}", "{{FG_RED}}"},
		{"unclosed token — unchanged", "{{FG_RED danger", "{{FG_RED danger"},
		{"caseless RTL Arabic — unchanged", "مرحبا يا عالم", "مرحبا يا عالم"},
		{"caseless CJK — unchanged", "世界 is here.", "世界 is here."},
	}
	// A multibyte first letter that HAS a case is capped correctly (built from an explicit code point so the
	// source encoding can't make it precomposed-vs-decomposed): é (U+00E9) -> É (U+00C9).
	if got, want := capitalizeFirst(string(rune(0x00E9))+"clair"), string(rune(0x00C9))+"clair"; got != want {
		t.Fatalf("capitalizeFirst(é...) = %q, want %q", got, want)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := capitalizeFirst(tc.in); got != tc.want {
				t.Fatalf("capitalizeFirst(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

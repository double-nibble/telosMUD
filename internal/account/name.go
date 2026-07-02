package account

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// name.go — character-name validation. telos-account owns name reservation (Phase 14), so the rules live
// here (the gate's login-time check mirrors them). A name becomes an in-world entity keyword + the unique
// routing key, so the rules keep it printable, dot-free (Diku target syntax), and not digit/dot-leading.

// MaxNameLen caps a character name's rune length.
const MaxNameLen = 20

// ValidateCharacterName checks a candidate name. It returns ("", true) when valid, else a short reason code
// + false. The reason codes ("required"/"too_long"/"leading_dot"/"leading_digit"/"contains_dot"/
// "contains_brace"/"invalid_char") are stable enough for a UI to localize.
func ValidateCharacterName(name string) (string, bool) {
	if name == "" {
		return "required", false
	}
	if utf8.RuneCountInString(name) > MaxNameLen {
		return "too_long", false
	}
	if name[0] == '.' {
		return "leading_dot", false
	}
	if r, _ := utf8.DecodeRuneInString(name); unicode.IsDigit(r) {
		return "leading_digit", false
	}
	if strings.ContainsRune(name, '.') {
		return "contains_dot", false
	}
	// Braces are reserved for the {{TOKEN}} color markup: a name embedding a known token (e.g.
	// "{{FG_RED}}Bob") renders as a colored "Bob" on telnet and STRIPS to "Bob" in GMCP payloads —
	// an impersonation vector. Reject the characters outright (not just names the strip mutates) so
	// a future vocabulary addition can't retroactively turn an accepted name into a spoof.
	if strings.ContainsAny(name, "{}") {
		return "contains_brace", false
	}
	for _, r := range name {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			return "invalid_char", false
		}
	}
	return "", true
}

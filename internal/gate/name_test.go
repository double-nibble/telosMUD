package gate

import (
	"strings"
	"testing"
)

// TestValidateNameAccepts: ordinary names pass.
func TestValidateNameAccepts(t *testing.T) {
	for _, name := range []string{
		"Aragorn",
		"gandalf",
		"Jean-Luc",
		"O'Brien",
		"café", // multibyte, printable
		"a2",   // digit allowed after the first rune
	} {
		if reason, ok := validateName(name); !ok {
			t.Errorf("validateName(%q) rejected: %s; want accepted", name, reason)
		}
	}
}

// TestValidateNameRejects: empty, over-length, control-char, leading-dot,
// leading-digit, and dotted names are all rejected.
func TestValidateNameRejects(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"over-length":   strings.Repeat("a", maxNameLen+1),
		"control char":  "Ar\x1bagorn",
		"bel":           "ding\x07",
		"leading dot":   ".hidden",
		"leading digit": "2nd",
		"dotted":        "all.orc",
		"trailing dot":  "bob.",
		"color token":   "{{FG_RED}}Bob",
		"lone brace":    "Bo}b",
	}
	for label, name := range cases {
		if reason, ok := validateName(name); ok {
			t.Errorf("validateName(%q) [%s] accepted; want rejected", name, label)
		} else if reason == "" {
			t.Errorf("validateName(%q) [%s] rejected with empty reason", name, label)
		}
	}
}

// TestValidateNameMaxLenBoundary: exactly maxNameLen runes is accepted; the count
// is in runes, not bytes, so a multibyte name at the rune limit still passes.
func TestValidateNameMaxLenBoundary(t *testing.T) {
	if _, ok := validateName(strings.Repeat("a", maxNameLen)); !ok {
		t.Errorf("name of exactly maxNameLen runes was rejected")
	}
	if _, ok := validateName(strings.Repeat("é", maxNameLen)); !ok {
		t.Errorf("multibyte name of exactly maxNameLen runes was rejected (byte vs rune count?)")
	}
}

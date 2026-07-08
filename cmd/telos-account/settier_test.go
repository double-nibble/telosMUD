package main

import (
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// TestValidateTier covers the CLI's ladder-validation DECISION (the branch --force skips): a defined tier
// passes; an unknown one returns a message naming the known tiers and pointing at --force. Pure (no DB).
func TestValidateTier(t *testing.T) {
	ladder := content.NewTrustLadder(nil) // built-in default: player/builder/admin

	if msg := validateTier(ladder, "admin"); msg != "" {
		t.Errorf("a defined tier must validate clean, got %q", msg)
	}
	if msg := validateTier(ladder, "player"); msg != "" {
		t.Errorf("the baseline must validate clean, got %q", msg)
	}
	msg := validateTier(ladder, "sorcerer")
	if msg == "" {
		t.Fatal("an unknown tier must be rejected")
	}
	if !strings.Contains(msg, "sorcerer") || !strings.Contains(msg, "admin") || !strings.Contains(msg, "--force") {
		t.Errorf("the rejection should name the bad tier, list known tiers, and mention --force; got %q", msg)
	}
}

// settier_test.go — the break-glass CLI arg contract (#108). The DB write path (SetAccountTierSystem) is
// covered by the gated store test; here we pin the flag validation that must fail BEFORE any config load or DB
// connection, so an operator running it wrong gets a clear usage error rather than a confusing DB failure.
func TestRunSetTierCLIArgValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no args", nil},
		{"missing tier", []string{"--character", "Gandalf"}},
		{"missing character", []string{"--tier", "admin"}},
		{"unknown flag", []string{"--nope", "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code := runSetTierCLI(tc.args); code == 0 {
				t.Fatalf("runSetTierCLI(%v) = 0, want a non-zero exit for invalid args", tc.args)
			}
		})
	}
}

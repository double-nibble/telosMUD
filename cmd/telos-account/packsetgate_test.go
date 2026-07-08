package main

import (
	"errors"
	"testing"
)

// TestPackSetGate (#259) pins telos-account's FAIL-CLOSED boot decision for the pack-set divergence check,
// mirroring cmd/telos-world: a divergence (non-nil error) refuses to start unless TELOS_ALLOW_INSECURE was
// explicitly set (then it starts with a warning). A consistent pack set (nil error) starts silently.
func TestPackSetGate(t *testing.T) {
	diverge := errors.New("pack-set divergence")
	tests := []struct {
		name          string
		divergErr     error
		allowInsecure bool
		wantFatal     bool
		wantWarn      bool
	}{
		{"divergence, not allowed => REFUSE (fail closed)", diverge, false, true, false},
		{"divergence, explicitly allowed => start with warning", diverge, true, false, true},
		{"consistent (no error) => start silently", nil, false, false, false},
		{"consistent, allowInsecure set => still silent", nil, true, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			warn, fatal := packSetGate(tc.divergErr, tc.allowInsecure)
			if (fatal != nil) != tc.wantFatal {
				t.Fatalf("fatal = %v, want fatal=%v", fatal, tc.wantFatal)
			}
			if (warn != "") != tc.wantWarn {
				t.Fatalf("warn = %q, want warn=%v", warn, tc.wantWarn)
			}
		})
	}
}

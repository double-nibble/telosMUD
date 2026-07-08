package main

import (
	"errors"
	"testing"
)

// TestHandoffAuthGate (#251) pins the FAIL-CLOSED boot decision: when CheckHandoffAuth reports a keyless
// discoverable shard (non-nil error) and insecure mode was NOT explicitly opted into, the world must refuse to
// start (fatal). A single-shard/keyed shard (nil error) always starts.
func TestHandoffAuthGate(t *testing.T) {
	guardErr := errors.New("keyless multi-shard")
	tests := []struct {
		name          string
		shardErr      error
		allowInsecure bool
		wantFatal     bool
		wantWarn      bool
	}{
		{"keyless discoverable, not allowed => REFUSE (fail closed)", guardErr, false, true, false},
		{"keyless discoverable, explicitly allowed => start with warning", guardErr, true, false, true},
		{"single-shard/keyed (no guard error) => start silently", nil, false, false, false},
		{"single-shard/keyed, allowInsecure set => still silent", nil, true, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fatal, warn := handoffAuthGate(tc.shardErr, tc.allowInsecure)
			if (fatal != nil) != tc.wantFatal {
				t.Fatalf("fatal = %v, want fatal=%v", fatal, tc.wantFatal)
			}
			if (warn != "") != tc.wantWarn {
				t.Fatalf("warn = %q, want warn=%v", warn, tc.wantWarn)
			}
		})
	}
}

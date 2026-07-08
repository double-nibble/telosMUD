package main

import "testing"

// TestCallerAuthGate (#247) pins the FAIL-CLOSED boot decision: with NO caller token and NO explicit insecure
// opt-in, the account server must refuse to start (fatal). This is the security invariant — a production deploy
// that forgot TELOS_ACCOUNT_CALLER_TOKEN must not silently serve an open, unauthenticated privileged API.
func TestCallerAuthGate(t *testing.T) {
	tests := []struct {
		name          string
		token         string
		allowInsecure bool
		wantFatal     bool
		wantWarn      bool
	}{
		{"no token, not allowed => REFUSE (fail closed)", "", false, true, false},
		{"no token, explicitly allowed => open with warning", "", true, false, true},
		{"token set => start silently (allowInsecure irrelevant)", "s3cr3t", false, false, false},
		{"token set, allowInsecure set => still silent (token wins)", "s3cr3t", true, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			warn, fatal := callerAuthGate(tc.token, tc.allowInsecure)
			if (fatal != nil) != tc.wantFatal {
				t.Fatalf("fatal = %v, want fatal=%v", fatal, tc.wantFatal)
			}
			if (warn != "") != tc.wantWarn {
				t.Fatalf("warn = %q, want warn=%v", warn, tc.wantWarn)
			}
		})
	}
}

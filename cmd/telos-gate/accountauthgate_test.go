package main

import "testing"

// TestAccountAuthGate (#96, security-review finding 1) pins the FAIL-CLOSED boot decision for OAuth
// enforcement: a gate with no account service (empty TELOS_ACCOUNT_TARGET) would accept the passwordless
// bare-name login, so it must refuse to start unless TELOS_ALLOW_INSECURE explicitly opts in. An
// account-configured gate always starts silently.
func TestAccountAuthGate(t *testing.T) {
	tests := []struct {
		name          string
		accountTarget string
		allowInsecure bool
		wantFatal     bool
		wantWarn      bool
	}{
		{"no account, not allowed => REFUSE (fail closed)", "", false, true, false},
		{"no account, explicitly allowed => start with warning", "", true, false, true},
		{"account configured => start silently", "account:9100", false, false, false},
		{"account configured, allowInsecure set => still silent", "account:9100", true, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			warn, fatal := accountAuthGate(tc.accountTarget, tc.allowInsecure)
			if (fatal != nil) != tc.wantFatal {
				t.Fatalf("fatal = %v, want fatal=%v", fatal, tc.wantFatal)
			}
			if (warn != "") != tc.wantWarn {
				t.Fatalf("warn = %q, want warn=%v", warn, tc.wantWarn)
			}
		})
	}
}

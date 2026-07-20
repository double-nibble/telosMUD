package obs

import "testing"

// TestLogRawInput pins the TELOS_LOG_RAW_INPUT opt-in that gates verbatim player-input logging
// (#454). It is a SEPARATE flag from DEBUG on purpose: turning on debug logging must never
// silently start recording player tells/chat/link-codes into a durable log store. This asserts
// the two are decoupled — DEBUG alone does not enable raw input, and raw input alone does not
// enable DEBUG.
func TestLogRawInput(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"off", false},
		{"no", false},
		{"1", true},
		{"true", true},
		{"TRUE", true}, // case-insensitive
		{"yes", true},
		{"on", true},
		{" 1 ", true}, // trimmed
	}
	for _, tc := range cases {
		t.Setenv("TELOS_LOG_RAW_INPUT", tc.val)
		if got := LogRawInput(); got != tc.want {
			t.Errorf("LogRawInput() with TELOS_LOG_RAW_INPUT=%q = %v, want %v", tc.val, got, tc.want)
		}
	}
}

// TestRawInputDecoupledFromDebug asserts the two flags do not bleed into each other: enabling one
// must never enable the other. This is the crux of #454 — "turn on debug" can never mean "record
// player chat".
func TestRawInputDecoupledFromDebug(t *testing.T) {
	t.Setenv("DEBUG", "1")
	t.Setenv("TELOS_LOG_RAW_INPUT", "")
	if LogRawInput() {
		t.Error("DEBUG=1 must NOT enable raw-input logging")
	}
	if !DebugEnabled() {
		t.Error("DEBUG=1 should enable debug")
	}

	t.Setenv("DEBUG", "")
	t.Setenv("TELOS_LOG_RAW_INPUT", "1")
	if DebugEnabled() {
		t.Error("TELOS_LOG_RAW_INPUT=1 must NOT enable debug")
	}
	if !LogRawInput() {
		t.Error("TELOS_LOG_RAW_INPUT=1 should enable raw-input logging")
	}
}

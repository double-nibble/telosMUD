package config

import (
	"os"
	"testing"
)

// tunables_test.go — #368: the operator-tunable engine limits.

// TestTunablesFromEnv pins the documented precedence (defaults < YAML < TELOS_*) for the new knobs, and the
// deliberate handling of a malformed value.
//
// A malformed value is IGNORED rather than coerced. Atoi("abc") yields 0, and 0 means "use the default" —
// so coercing would hand back the default while the operator believed their setting had taken, which is the
// silent-misconfiguration shape this whole issue is about.
func TestTunablesFromEnv(t *testing.T) {
	t.Setenv("TELOS_LUA_INSTR_BUDGET", "250000")
	t.Setenv("TELOS_LUA_CALL_DEADLINE_MS", "25")
	c := Default()
	c.applyEnv()
	if c.Tunables.LuaInstrBudget != 250_000 {
		t.Fatalf("LuaInstrBudget = %d, want 250000", c.Tunables.LuaInstrBudget)
	}
	if c.Tunables.LuaCallDeadlineMS != 25 {
		t.Fatalf("LuaCallDeadlineMS = %d, want 25", c.Tunables.LuaCallDeadlineMS)
	}

	t.Setenv("TELOS_LUA_INSTR_BUDGET", "not-a-number")
	c2 := Default()
	c2.applyEnv()
	if c2.Tunables.LuaInstrBudget != 0 {
		t.Fatalf("a malformed budget produced %d; Atoi yields 0 and 0 means DEFAULT, so coercing would hand "+
			"back the default while the operator believed their setting had taken", c2.Tunables.LuaInstrBudget)
	}
	if c2.Tunables.Err() == nil {
		t.Fatal("a malformed tunable was silently ignored. Warning about it and running the default is the " +
			"exact silent misconfiguration this feature exists to end — the host must be able to refuse the boot")
	}
}

// TestTunablesRoundTripYAML pins the struct tags. A typo'd `yaml:` tag parses to zero, which means "use the
// default" — so the operator's file would be read successfully and have no effect whatsoever.
func TestTunablesRoundTripYAML(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	if err := os.WriteFile(path, []byte("tunables:\n  lua_instr_budget: 250000\n  lua_call_deadline_ms: 25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tunables.LuaInstrBudget != 250_000 || c.Tunables.LuaCallDeadlineMS != 25 {
		t.Fatalf("YAML round trip gave %+v, want budget 250000 / deadline 25 — check the yaml struct tags",
			c.Tunables)
	}
}

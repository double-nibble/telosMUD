package config

import (
	"os"
	"strings"
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
//
// Every knob is in the one document deliberately: the yaml tag is the ONLY thing this exercises, and a tag is
// exactly the sort of detail that is right for the field it was written for and wrong for the one it was
// pasted onto.
func TestTunablesRoundTripYAML(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	const doc = "tunables:\n" +
		"  lua_instr_budget: 250000\n" +
		"  lua_call_deadline_ms: 25\n" +
		"  instances_per_account: 4\n" +
		"  instances_per_shard: 300\n" +
		"  instance_mint_burst: 7\n" +
		"  instance_mint_window_sec: 90\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := TunablesConfig{
		LuaInstrBudget: 250_000, LuaCallDeadlineMS: 25,
		InstancesPerAccount: 4, InstancesPerShard: 300,
		InstanceMintBurst: 7, InstanceMintWindowSec: 90,
	}
	if c.Tunables != want {
		t.Fatalf("YAML round trip gave %+v, want %+v — check the yaml struct tags", c.Tunables, want)
	}
}

// TestTunablesEnvNamesMapToTheirOwnField is a copy-paste net (#436).
//
// Six near-identical env blocks differ only in a name and a field pointer, and a paste that reads
// TELOS_INSTANCE_MINT_BURST into InstancesPerShard passes any test that sets one variable and checks one
// field. So each case sets ONLY its own variable and asserts every OTHER field is still zero.
func TestTunablesEnvNamesMapToTheirOwnField(t *testing.T) {
	cases := []struct {
		env string
		get func(TunablesConfig) int
	}{
		{"TELOS_LUA_INSTR_BUDGET", func(c TunablesConfig) int { return c.LuaInstrBudget }},
		{"TELOS_LUA_CALL_DEADLINE_MS", func(c TunablesConfig) int { return c.LuaCallDeadlineMS }},
		{"TELOS_INSTANCES_PER_ACCOUNT", func(c TunablesConfig) int { return c.InstancesPerAccount }},
		{"TELOS_INSTANCES_PER_SHARD", func(c TunablesConfig) int { return c.InstancesPerShard }},
		{"TELOS_INSTANCE_MINT_BURST", func(c TunablesConfig) int { return c.InstanceMintBurst }},
		{"TELOS_INSTANCE_MINT_WINDOW_SEC", func(c TunablesConfig) int { return c.InstanceMintWindowSec }},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			// A value no other field would plausibly be checked against, so a cross-wired read is visible.
			t.Setenv(tc.env, "4242")
			c := Default()
			c.applyEnv()
			if got := tc.get(c.Tunables); got != 4242 {
				t.Fatalf("%s did not reach its own field: got %d, want 4242", tc.env, got)
			}
			// The other five must be untouched. This is the half that catches the paste.
			for _, other := range cases {
				if other.env == tc.env {
					continue
				}
				if got := other.get(c.Tunables); got != 0 {
					t.Fatalf("setting only %s also moved the field behind %s to %d; two env names are wired "+
						"to one struct field", tc.env, other.env, got)
				}
			}
		})
	}
}

// TestTunablesReportsEveryMalformedValue. Last-write-wins error reporting hands an operator one typo at a
// time, so a ConfigMap with three mistakes costs three deploy cycles to boot — and with six knobs on one
// surface, more than one typo is the ordinary case rather than the exotic one.
func TestTunablesReportsEveryMalformedValue(t *testing.T) {
	t.Setenv("TELOS_INSTANCES_PER_SHARD", "lots")
	t.Setenv("TELOS_INSTANCE_MINT_BURST", "six")
	c := Default()
	c.applyEnv()

	err := c.Tunables.Err()
	if err == nil {
		t.Fatal("two malformed tunables were silently ignored")
	}
	for _, want := range []string{"TELOS_INSTANCES_PER_SHARD", "TELOS_INSTANCE_MINT_BURST"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error names only some of the malformed values (%q missing): %v", want, err)
		}
	}
	// And the malformed values did NOT land as zeroes that would read as "use the default".
	if c.Tunables.InstancesPerShard != 0 || c.Tunables.InstanceMintBurst != 0 {
		t.Fatalf("a malformed value was coerced rather than left alone: %+v", c.Tunables)
	}
}

// TestDirectoryAddressResolution — #429. The bool this returns decides whether an evicting policy on the
// coordination Redis REFUSES THE BOOT, so getting it wrong is either a fleet that will not start or a
// silently un-enforced gate.
func TestDirectoryAddressResolution(t *testing.T) {
	cases := []struct {
		name          string
		cfg           RedisConfig
		wantAddr      string
		wantDedicated bool
		why           string
	}{
		{
			name:     "unset falls back to the shared addr",
			cfg:      RedisConfig{Addr: "cache:6379"},
			wantAddr: "cache:6379", wantDedicated: false,
			why: "an untouched deployment must be unchanged, and must stay on the WARN-only side of the gate",
		},
		{
			name:     "a distinct directory addr is dedicated",
			cfg:      RedisConfig{Addr: "cache:6379", DirectoryAddr: "coord:6379"},
			wantAddr: "coord:6379", wantDedicated: true,
		},
		{
			name:     "the SAME address spelled twice is NOT dedicated",
			cfg:      RedisConfig{Addr: "cache:6379", DirectoryAddr: "cache:6379"},
			wantAddr: "cache:6379", wantDedicated: false,
			why: "this is one instance however it is spelled. Reporting it as dedicated would make the boot " +
				"gate fatal on a SHARED Redis — refusing to start a fleet whose operator changed nothing but " +
				"a config line, and ordering them into noeviction on a cache-sized instance",
		},
		{
			name:     "a directory addr with no cache addr still counts",
			cfg:      RedisConfig{DirectoryAddr: "coord:6379"},
			wantAddr: "coord:6379", wantDedicated: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, dedicated := tc.cfg.DirectoryAddress()
			if addr != tc.wantAddr {
				t.Fatalf("addr = %q, want %q", addr, tc.wantAddr)
			}
			if dedicated != tc.wantDedicated {
				t.Fatalf("dedicated = %v, want %v. %s", dedicated, tc.wantDedicated, tc.why)
			}
		})
	}
}

// TestDirectoryAddrFromEnv pins the env name alongside the yaml tag.
func TestDirectoryAddrFromEnv(t *testing.T) {
	t.Setenv("TELOS_REDIS_DIRECTORY_ADDR", "coord:6379")
	c := Default()
	c.applyEnv()
	if c.Redis.DirectoryAddr != "coord:6379" {
		t.Fatalf("DirectoryAddr = %q; TELOS_REDIS_DIRECTORY_ADDR did not reach it", c.Redis.DirectoryAddr)
	}
	if c.Redis.Addr == "coord:6379" {
		t.Fatal("the directory address also overwrote the CACHE address, which would put the checkpoint " +
			"tier on the coordination instance — the opposite of the split")
	}
}

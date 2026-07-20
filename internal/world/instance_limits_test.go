package world

import (
	"context"
	"strings"
	"testing"
	"time"
)

// instance_limits_test.go — #436: the operator-facing, validated instance caps.
//
// The whole risk of this change is that a cap becomes CONFIGURABLE and, in becoming so, becomes INERT — a
// value that parses, logs plausibly and never reaches the mint path. So these tests are split deliberately:
// the validator tests pin what is refused and that a refusal changes nothing, and TestSetInstanceLimitsBinds
// pins that an accepted value actually refuses a real mint on a real running shard.

// TestSetInstanceLimitsZeroMeansDefault. Zero is the "I did not set this" sentinel across the whole tunables
// surface, and for these four the alternative reading — unlimited — is resource exhaustion for the counts and
// a silent disarm for the window (now.Sub(windowStart) >= 0 is true on every mint, so the bucket resets each
// time and the burst check never fires).
func TestSetInstanceLimitsZeroMeansDefault(t *testing.T) {
	sh := NewShard("midgaard", "", nil, nil)
	if err := sh.SetInstanceLimits(0, 0, 0, 0); err != nil {
		t.Fatalf("the all-defaults set was refused: %v", err)
	}
	got, want := sh.instanceLimits, defaultInstanceLimits()
	if got != want {
		t.Fatalf("zeroes gave %+v, want the compiled defaults %+v", got, want)
	}
	// Spelled out because a regression here is invisible: a mintWindow of 0 disarms the ONLY bound on
	// mint-abandon-mint churn while every concurrent-cap test in this package stays green.
	if got.mintWindow == 0 {
		t.Fatal("a zero mint window was taken literally; the rate limit would then reset its bucket on every " +
			"mint and refuse nothing, with the boot log showing a plausible configuration")
	}
}

// TestSetInstanceLimitsValidatesEffectiveNotRawValues is the highest-value case in this file, and both cases
// are FAIL-OPEN if it regresses.
//
// Every cross-field rule here compares two values, and an operator characteristically sets ONE of them. A
// validator that checks the RAW input sees the other as 0, concludes there is nothing to compare, and accepts
// a pair that the effective values would have refused. The unset field is not absent — it is the default, and
// the default is what will actually be enforced at runtime.
func TestSetInstanceLimitsValidatesEffectiveNotRawValues(t *testing.T) {
	t.Run("a lone window shrink is a 360-per-minute mint rate", func(t *testing.T) {
		// The whole rule lives in the ratio, and the operator supplied one side of it. Raw: burst 0, so the
		// rate computes as 0 and passes. Effective: the default burst of 6 over a one-second window is 360
		// full zone builds a minute, six times the ceiling — and this is the shape of an operator trying to
		// make the limiter STRICTER.
		sh := NewShard("midgaard", "", nil, nil)
		err := sh.SetInstanceLimits(0, 0, 0, 1)
		if err == nil {
			t.Fatal("a one-second window against an UNSET burst was accepted. Raw, the burst reads 0 and the " +
				"rate computes to 0; effectively it is the default 6 per second — 360 zone builds a minute")
		}
		if !strings.Contains(err.Error(), "mints per minute") {
			t.Fatalf("refused for the wrong reason: %v", err)
		}
	})

	t.Run("a lone shard-cap shrink puts the per-account cap above it", func(t *testing.T) {
		// Raw: perAccount 0, nothing to compare. Effective: the default 3 against a shard cap of 2, so the
		// per-account bound can never fire and one account can fill the shard.
		sh := NewShard("midgaard", "", nil, nil)
		err := sh.SetInstanceLimits(0, 2, 0, 0)
		if err == nil {
			t.Fatal("a shard cap of 2 against an UNSET per-account cap was accepted; the effective per-account " +
				"cap is the default 3, which can then never fire")
		}
		if !strings.Contains(err.Error(), "instances_per_account=3") {
			t.Fatalf("the refusal did not name the EFFECTIVE per-account value it compared against, so an "+
				"operator cannot see why a shard cap alone was refused: %v", err)
		}
	})
}

// TestSetInstanceLimitsRefusesAndChangesNothing. A setter that validates field-by-field can write two of the
// four values and then refuse, leaving the shard running a configuration nobody chose — while the operator
// has been told the set was rejected. Same property luacaps_test.go pins for SetLuaCaps.
func TestSetInstanceLimitsRefusesAndChangesNothing(t *testing.T) {
	cases := []struct {
		name                                           string
		perAccount, perShard, mintBurst, mintWindowSec int
		wantIn                                         string
	}{
		{"negative is not unlimited", -1, 0, 0, 0, "no unlimited"},
		{"negative window", 0, 0, 0, -1, "no unlimited"},
		{"per-account above its ceiling", maxInstancesPerAccount + 1, 0, 0, 0, "instances_per_account"},
		{"per-shard above its ceiling", 0, maxInstancesPerShard + 1, 0, 0, "instances_per_shard"},
		{"per-account above per-shard", 8, 4, 0, 0, "can never fire"},
		{"burst above its ceiling", 0, 0, maxInstanceMintBurst + 1, 0, "instance_mint_burst"},
		{"window above its ceiling", 0, 0, 0, int(maxInstanceMintWindow.Seconds()) + 1, "not stricter"},
		// The int64 wrap. time.Duration(n) * time.Second scales by 1e9, so a large seconds value wraps — and
		// 2^55+60 wraps to exactly 1m0s, which is INSIDE the accepted range. Validating only the product
		// therefore accepts this and silently installs a one-minute window, reporting a configuration the
		// operator never wrote. The refusal must name the number they actually typed.
		{"a seconds value that wraps into the legal range", 0, 0, 0, 1<<55 + 60, "instance_mint_window_sec=36028797018964028"},
		// The pair rule. Each half is individually legal — burst 60 is exactly the ceiling, a 5s window is well
		// inside its range — and together they are 720 mints a minute, i.e. 720 full zone builds.
		{"a legal burst over a legal window is an illegal RATE", 0, 0, 60, 5, "mints per minute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sh := NewShard("midgaard", "", nil, nil)
			// A non-default starting point, so "unchanged" is a real assertion rather than a coincidence with
			// the defaults the failed call would have substituted anyway.
			sh.WithInstanceLimits(2, 9, 4, 30*time.Second)
			before := sh.instanceLimits

			err := sh.SetInstanceLimits(tc.perAccount, tc.perShard, tc.mintBurst, tc.mintWindowSec)
			if err == nil {
				t.Fatalf("SetInstanceLimits(%d,%d,%d,%d) was accepted", tc.perAccount, tc.perShard,
					tc.mintBurst, tc.mintWindowSec)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("refusal %q does not explain the constraint (want it to mention %q)", err, tc.wantIn)
			}
			if sh.instanceLimits != before {
				t.Fatalf("a REFUSED SetInstanceLimits moved the limits from %+v to %+v; a rejected "+
					"configuration must change nothing, or the shard runs values nobody chose",
					before, sh.instanceLimits)
			}
		})
	}
}

// TestSetInstanceLimitsWindowFloor is the floor case, separated because expressing it in the table above
// needs a sub-second window and the setter's unit is seconds — the floor IS one second, so the only value
// below it is zero, which means "default". The floor therefore guards a value the seconds-based surface
// cannot express, and that is worth stating rather than silently leaving untested.
func TestSetInstanceLimitsWindowFloor(t *testing.T) {
	if minInstanceMintWindow != time.Second {
		t.Fatalf("minInstanceMintWindow is %v; the seconds-valued operator surface can express sub-floor "+
			"values now, so the table above needs a real floor case", minInstanceMintWindow)
	}
	sh := NewShard("midgaard", "", nil, nil)
	if err := sh.SetInstanceLimits(0, 0, 1, 1); err != nil {
		t.Fatalf("a one-second window (exactly the floor) was refused: %v", err)
	}
}

// TestSetInstanceLimitsAcceptsTheBoundaries. A ceiling that is off by one refuses a configuration the docs
// promise, and the config.example.yaml ranges are what operators will copy.
func TestSetInstanceLimitsAcceptsTheBoundaries(t *testing.T) {
	sh := NewShard("midgaard", "", nil, nil)
	// maxInstancesPerAccount against maxInstancesPerShard, and a burst/window pair at exactly the rate ceiling
	// (60 mints per 30s == 120/min).
	if err := sh.SetInstanceLimits(maxInstancesPerAccount, maxInstancesPerShard, 60, 30); err != nil {
		t.Fatalf("the documented maxima were refused, so config.example.yaml's stated ranges are a lie: %v", err)
	}
	if sh.instanceLimits.perShard != maxInstancesPerShard || sh.instanceLimits.mintWindow != 30*time.Second {
		t.Fatalf("accepted set did not apply: %+v", sh.instanceLimits)
	}
}

// TestSetInstanceLimitsBinds is the WIRING half, and the one that would catch a setter that validates
// beautifully and applies nothing.
//
// It runs a real shard and mints real instances, because reserveInstanceSlot refuses with "shard not running"
// whenever runCtx is nil — so a cap test against an unstarted shard is green for EVERY value of the cap,
// including a setter that is a no-op. It asserts both directions: the mint below the cap must SUCCEED (or the
// refusal proves nothing) and the one at the cap must fail FOR THE CAP'S OWN REASON.
func TestSetInstanceLimitsBinds(t *testing.T) {
	sh, cancel := runningShardWith(t, []string{"midgaard"}, "midgaard", func(sh *Shard) {
		if err := sh.SetInstanceLimits(1, 8, 6, 60); err != nil {
			t.Fatalf("SetInstanceLimits: %v", err)
		}
	})
	defer cancel()

	if _, err := sh.MintInstance(context.Background(), "darkwood", "acct-a"); err != nil {
		t.Fatalf("the FIRST mint failed under a per-account cap of 1, so the refusal below would prove "+
			"nothing about the cap: %v", err)
	}
	_, err := sh.MintInstance(context.Background(), "darkwood", "acct-a")
	if err == nil {
		t.Fatal("a second mint succeeded under a configured per-account cap of 1: the value passed " +
			"validation and never reached reserveInstanceSlot")
	}
	if !strings.Contains(err.Error(), "account already holds") {
		t.Fatalf("the second mint failed for the wrong reason — this test only pins the CAP if the refusal "+
			"is the cap's: %v", err)
	}
	// A different account is unaffected: the bound that fired is per-account, not the global one.
	if _, err := sh.MintInstance(context.Background(), "darkwood", "acct-b"); err != nil {
		t.Fatalf("a second ACCOUNT was refused, so what fired was not the per-account cap: %v", err)
	}
}

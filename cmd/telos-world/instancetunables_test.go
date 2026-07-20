package main

import (
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/world"
)

// instancetunables_test.go — #436: the boot wiring that carries the configured instance caps to the shard.
//
// This exists because deleting the call site left the ENTIRE suite green. internal/world tests the validator
// exhaustively, and internal/config tests the parse — but nothing tested the four-argument hop between them,
// which is the one place a config field can land in the wrong parameter. A shard with a swapped mapping boots
// happily and enforces limits the operator never wrote.
//
// It pins the mapping WITHOUT an exported accessor for the private limits, by giving each field in turn a
// value just past its own ceiling and requiring the refusal to name that knob AND that number. A field read
// into the wrong parameter then either refuses under a different knob's name or does not refuse at all.

func testShard(t *testing.T) *world.Shard {
	t.Helper()
	return world.NewShard("midgaard", "", nil, nil)
}

// TestApplyInstanceTunablesMapsEachFieldToItsOwnParameter is the anti-swap net.
func TestApplyInstanceTunablesMapsEachFieldToItsOwnParameter(t *testing.T) {
	cases := []struct {
		name   string
		tune   config.TunablesConfig
		wantIn string
	}{
		{"per-account", config.TunablesConfig{InstancesPerAccount: 65}, "instances_per_account=65"},
		{"per-shard", config.TunablesConfig{InstancesPerShard: 1025}, "instances_per_shard=1025"},
		{"mint burst", config.TunablesConfig{InstanceMintBurst: 61}, "instance_mint_burst=61"},
		{"mint window", config.TunablesConfig{InstanceMintWindowSec: 3601}, "instance_mint_window_sec=3601"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := applyInstanceTunables(testShard(t), tc.tune)
			if err == nil {
				t.Fatalf("%+v was accepted; the field is over its own ceiling, so either it never reached "+
					"SetInstanceLimits or it reached a different parameter", tc.tune)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("the refusal does not name %q, so this config field is wired to the wrong "+
					"SetInstanceLimits parameter: %v", tc.wantIn, err)
			}
		})
	}
}

// TestApplyInstanceTunablesAcceptsAnUnsetConfig. The overwhelmingly common deployment sets none of these, and
// it must boot: every field is 0, which means "use the compiled default" rather than "unlimited" or "zero".
// A regression here does not misconfigure a shard, it refuses to start every shard in the fleet.
func TestApplyInstanceTunablesAcceptsAnUnsetConfig(t *testing.T) {
	if err := applyInstanceTunables(testShard(t), config.TunablesConfig{}); err != nil {
		t.Fatalf("an untouched config was refused, so a deployment that sets none of these knobs would not "+
			"boot: %v", err)
	}
}

// TestApplyInstanceTunablesAcceptsAPlausibleOperatorConfig — the whole point of the feature is that a
// reasonable non-default configuration works end to end, not merely that bad ones are refused.
func TestApplyInstanceTunablesAcceptsAPlausibleOperatorConfig(t *testing.T) {
	tune := config.TunablesConfig{
		InstancesPerAccount:   2,
		InstancesPerShard:     64,
		InstanceMintBurst:     4,
		InstanceMintWindowSec: 120,
	}
	if err := applyInstanceTunables(testShard(t), tune); err != nil {
		t.Fatalf("a plausible operator config was refused: %v", err)
	}
}

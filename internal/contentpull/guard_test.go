package contentpull

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeLister is a test ZoneLister: canned pack -> zone refs, with an optional error.
type fakeLister struct {
	zones map[string][]string
	err   error
}

func (f fakeLister) PackZones(_ context.Context, pack string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.zones[pack], nil
}

// fakeLocator is a test ZoneLocator: a set of hosted zone refs, with an optional error.
type fakeLocator struct {
	hosted map[string]bool
	err    error
}

func (f fakeLocator) ZoneHosted(_ context.Context, zone string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.hosted[zone], nil
}

func TestPrunePreview(t *testing.T) {
	cases := []struct {
		name              string
		current, incoming []string
		want              []string
	}{
		{"drop one", []string{"core", "reference", "old"}, []string{"core", "reference"}, []string{"old"}},
		{"drop none (superset)", []string{"core"}, []string{"core", "new"}, nil},
		{"drop none (identical)", []string{"core", "reference"}, []string{"reference", "core"}, nil},
		{"drop several, sorted", []string{"a", "b", "c", "d"}, []string{"b"}, []string{"a", "c", "d"}},
		{"empty incoming drops all", []string{"core", "reference"}, nil, []string{"core", "reference"}},
		{"fresh db (empty current)", nil, []string{"core"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := prunePreview(tc.current, tc.incoming)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("prunePreview(%v, %v) = %v, want %v", tc.current, tc.incoming, got, tc.want)
			}
		})
	}
}

// TestFleetPruneGuardBlocksHostedPack: a pruned pack with a live-hosted zone is blocked; a pruned pack whose
// zones are all unhosted is allowed.
func TestFleetPruneGuardBlocksHostedPack(t *testing.T) {
	lister := fakeLister{zones: map[string][]string{
		"live":  {"midgaard", "darkwood"},
		"stale": {"attic"},
		"defs":  nil, // a shared-defs-only pack owns no zones
	}}
	loc := fakeLocator{hosted: map[string]bool{"darkwood": true}} // only darkwood is hosted
	guard := FleetPruneGuard(loc)

	blocked, err := guard(context.Background(), lister, []string{"stale", "live", "defs"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(blocked, []string{"live"}) {
		t.Fatalf("blocked = %v, want [live] (only the pack with a hosted zone)", blocked)
	}
}

// TestFleetPruneGuardAllowsWhenNothingHosted: no pruned pack has a hosted zone => nothing blocked (the pull
// proceeds).
func TestFleetPruneGuardAllowsWhenNothingHosted(t *testing.T) {
	lister := fakeLister{zones: map[string][]string{"a": {"z1"}, "b": {"z2", "z3"}}}
	loc := fakeLocator{hosted: map[string]bool{}} // nothing hosted
	guard := FleetPruneGuard(loc)

	blocked, err := guard(context.Background(), lister, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 0 {
		t.Fatalf("blocked = %v, want none", blocked)
	}
}

// TestFleetPruneGuardFailsClosedOnLocatorError: a directory error aborts the guard (fail closed on
// incomplete fleet info) rather than silently allowing the strip.
func TestFleetPruneGuardFailsClosedOnLocatorError(t *testing.T) {
	lister := fakeLister{zones: map[string][]string{"a": {"z1"}}}
	boom := errors.New("redis unreachable")
	guard := FleetPruneGuard(fakeLocator{err: boom})

	if _, err := guard(context.Background(), lister, []string{"a"}); !errors.Is(err, boom) {
		t.Fatalf("guard err = %v, want it to wrap the locator error (fail closed)", err)
	}
}

// TestFleetPruneGuardPropagatesListerError: a pack-zones lookup failure aborts the guard.
func TestFleetPruneGuardPropagatesListerError(t *testing.T) {
	boom := errors.New("pg down")
	guard := FleetPruneGuard(fakeLocator{})

	if _, err := guard(context.Background(), fakeLister{err: boom}, []string{"a"}); !errors.Is(err, boom) {
		t.Fatalf("guard err = %v, want it to wrap the lister error", err)
	}
}

// --- #427: the operator override -------------------------------------------------------------------

// TestPruneDecision covers the whole of the override's decision logic. The guard is advisory by design,
// but before #427 a blocked pack aborted the entire pull with no way to say "I know, do it anyway" — and
// since #416 made the guard see instance templates, one idle player in a dungeon copy could hold every
// content deploy indefinitely.
func TestPruneDecision(t *testing.T) {
	cases := []struct {
		name       string
		blocked    []string
		force      bool
		wantErr    bool
		wantForced []string
	}{
		{name: "nothing blocked proceeds", blocked: nil, force: false},
		{name: "nothing blocked, force is a no-op", blocked: nil, force: true},
		{name: "blocked without force REFUSES", blocked: []string{"dungeons"}, force: false, wantErr: true},
		{
			name:    "blocked WITH force proceeds and records what was overridden",
			blocked: []string{"dungeons", "raids"}, force: true,
			wantForced: []string{"dungeons", "raids"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forced, err := pruneDecision("v1", tc.blocked, tc.force)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected a refusal")
				}
				// The refusal must point at the way out, or the operator is stuck with no next step.
				if !strings.Contains(err.Error(), "force") {
					t.Fatalf("the refusal must mention the override; got: %v", err)
				}
				if forced != nil {
					t.Fatalf("a refusal must record nothing as force-pruned; got %v", forced)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(forced, tc.wantForced) {
				t.Fatalf("forced = %v, want %v", forced, tc.wantForced)
			}
		})
	}
}

// TestForceDoesNotSkipTheGuard is the distinction the whole design turns on. Force must DOWNGRADE the
// veto, never bypass the check: the blocked list has to be computed so it can be reported back and logged.
// A "fix" that short-circuits the guard when force is set would pass every other test here and quietly
// destroy the audit trail — so this asserts the packs survive into the result.
func TestForceDoesNotSkipTheGuard(t *testing.T) {
	forced, err := pruneDecision("v1", []string{"dungeons"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(forced) != 1 || forced[0] != "dungeons" {
		t.Fatalf("a forced prune must REPORT what it overrode (got %v) — otherwise the operator and the "+
			"post-incident log never learn which packs were stripped against advice", forced)
	}
}

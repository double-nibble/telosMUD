package contentpull

import (
	"context"
	"errors"
	"reflect"
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

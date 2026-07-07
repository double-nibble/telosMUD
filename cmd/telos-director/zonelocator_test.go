package main

import (
	"context"
	"errors"
	"testing"

	"github.com/double-nibble/telosmud/internal/directory"
)

// fakeShardForZone is a test shardForZoner: it returns owner for a claimed zone, directory.ErrNotFound for
// an unclaimed one, or a preset transient error.
type fakeShardForZone struct {
	owner map[string]string
	err   error
}

func (f fakeShardForZone) ShardForZone(_ context.Context, zone string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if o, ok := f.owner[zone]; ok {
		return o, nil
	}
	return "", directory.ErrNotFound
}

// TestZoneLocatorHostedMapping pins the production translation of directory semantics into the guard's
// boolean: a claimed zone is hosted, ErrNotFound is not-hosted, and any other (transient) error propagates
// so the prune guard fails closed rather than treating a blip as "not hosted".
func TestZoneLocatorHostedMapping(t *testing.T) {
	loc := zoneLocator{dir: fakeShardForZone{owner: map[string]string{"midgaard": "shard-3"}}}

	if hosted, err := loc.ZoneHosted(context.Background(), "midgaard"); err != nil || !hosted {
		t.Fatalf("claimed zone: hosted=%v err=%v, want true,nil", hosted, err)
	}
	if hosted, err := loc.ZoneHosted(context.Background(), "attic"); err != nil || hosted {
		t.Fatalf("unclaimed zone: hosted=%v err=%v, want false,nil", hosted, err)
	}

	boom := errors.New("redis unreachable")
	locErr := zoneLocator{dir: fakeShardForZone{err: boom}}
	if _, err := locErr.ZoneHosted(context.Background(), "midgaard"); !errors.Is(err, boom) {
		t.Fatalf("a transient directory error must propagate (fail closed); got %v", err)
	}
}

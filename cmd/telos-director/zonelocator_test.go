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
	// inUse is the #416 half: templates with live INSTANCES but no lease. tmplErr fails that lookup alone,
	// so a test can prove the instance-aware path fails closed independently of the lease path.
	inUse   map[string]bool
	tmplErr error
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

func (f fakeShardForZone) TemplateInUse(_ context.Context, template string) (bool, error) {
	if f.tmplErr != nil {
		return false, f.tmplErr
	}
	return f.inUse[template], nil
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

// TestZoneLocatorSeesAnUnleasedInstanceTemplate is #416. An instance takes no lease, and a dungeon template
// is typically not in cfg.Zones at all — you do not want the raw template walkable — so the lease lookup
// returns ErrNotFound for a zone with forty live copies and parties inside them. Before this, the prune
// guard read that as "not hosted" and let the pull strip the pack out from under them.
func TestZoneLocatorSeesAnUnleasedInstanceTemplate(t *testing.T) {
	loc := zoneLocator{dir: fakeShardForZone{
		owner: map[string]string{"midgaard": "shard-3"},
		inUse: map[string]bool{"crypt": true},
	}}

	hosted, err := loc.ZoneHosted(context.Background(), "crypt")
	if err != nil {
		t.Fatalf("template lookup: %v", err)
	}
	if !hosted {
		t.Fatal("a template with LIVE INSTANCES but no lease read as not-hosted; the prune guard would then " +
			"let a pull strip the pack while parties are standing inside copies of it (#416)")
	}

	// A template nobody is running is still prunable — the guard must not become a blanket veto on every
	// zone that happens to lack a lease, or no pack could ever be pruned again.
	if hosted, err := loc.ZoneHosted(context.Background(), "attic"); err != nil || hosted {
		t.Fatalf("an unleased, uninstanced zone: hosted=%v err=%v, want false,nil", hosted, err)
	}
}

// TestZoneLocatorFailsClosedOnAnInstanceLookupError. The lease lookup already returned ErrNotFound by the
// time the template lookup runs, so there IS a "not hosted" answer sitting in hand — and degrading to it on
// error is exactly the fail-open this check exists to close. An instance-aware lookup that cannot answer
// must abort the pull, not approve it.
func TestZoneLocatorFailsClosedOnAnInstanceLookupError(t *testing.T) {
	boom := errors.New("redis unreachable")
	loc := zoneLocator{dir: fakeShardForZone{tmplErr: boom}}

	hosted, err := loc.ZoneHosted(context.Background(), "crypt")
	if !errors.Is(err, boom) {
		t.Fatalf("an instance-aware lookup failure must propagate so the prune guard fails closed; got hosted=%v err=%v", hosted, err)
	}
}

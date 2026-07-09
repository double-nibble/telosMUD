package world

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// #212 slice 1 (embedded core pack): a fresh/empty server must still boot a start room and ACCEPT a
// login, rather than reject it ("no rooms"). These prove the core bootstrap zone rescues the
// otherwise-empty world, and that it is hosted as a local, unleased zone.

// TestCoreBootstrapLoginSucceeds builds a shard from core-ONLY content (LoadWithCore with no real
// source — the Postgres-unreachable / unseeded path) hosting the core zone, and logs a fresh
// player in. Unlike the empty-world case, join must SUCCEED and land them in the Nexus start room.
func TestCoreBootstrapLoginSucceeds(t *testing.T) {
	lc, err := content.LoadWithCore(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	shard := NewShardFromContent(lc, []string{content.CoreZone}, content.CoreZone, "", nil, nil).
		WithLocalZones(content.CoreZone)
	z := shard.Zone()
	if z == nil {
		t.Fatal("core shard has no home zone")
	}
	if z.startRoom != content.CoreStartRoom {
		t.Fatalf("core zone start room = %q, want %q", z.startRoom, content.CoreStartRoom)
	}
	if len(z.rooms) == 0 {
		t.Fatal("core zone should have rooms")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	out := make(chan *playv1.ServerFrame, 16)
	var cz atomic.Pointer[Zone]
	z.post(attachMsg{character: "Pioneer", out: out, curZone: &cz})

	got := nextOutput(t, &session{character: "Pioneer", out: out})
	if strings.Contains(got, "no rooms") {
		t.Fatalf("core-bootstrap login was rejected: %q", got)
	}
	// The player should see the Nexus room they landed in.
	if !strings.Contains(got, "Nexus") {
		t.Fatalf("login output does not show the Nexus start room: %q", got)
	}
}

// TestWithLocalZonesMarks confirms the local-zone marker is recorded and read.
func TestWithLocalZonesMarks(t *testing.T) {
	s := newBareShard("home", "", nil, nil).WithLocalZones("core:nexus", "core:other")
	if !s.isLocalZone("core:nexus") || !s.isLocalZone("core:other") {
		t.Fatal("WithLocalZones did not mark the zones local")
	}
	if s.isLocalZone("midgaard") {
		t.Fatal("a normal zone must not be reported local")
	}
	// A nil/empty call is a no-op and never panics.
	newBareShard("home", "", nil, nil).WithLocalZones()
}

// TestReconcileSkipsLocalZone: a KindZone invalidation naming a LOCAL bootstrap zone must NOT drive
// a shape-reconcile against it (which would tear down the lobby's rooms fleet-wide). The reloader
// guard short-circuits before posting to the zone (defense-in-depth on top of the reload-gate
// reject). A normal zone still receives its reconcile.
func TestReconcileSkipsLocalZone(t *testing.T) {
	s := newBareShard("core", "", nil, nil).WithLocalZones("core")
	local := newZone("core")
	s.adopt("core", local)
	normal := newZone("midgaard")
	s.adopt("midgaard", normal)
	r := &reloader{shard: s}

	before := len(local.inbox)
	r.reconcileZone(contentbus.Invalidation{Kind: content.KindZone, Ref: "core", Rooms: []string{"core:room:x"}, Version: 1})
	if len(local.inbox) != before {
		t.Fatalf("reconcile was posted to the local bootstrap zone (inbox %d→%d); the guard must skip it", before, len(local.inbox))
	}
	// Sanity: a normal zone DOES receive the reconcile (proves the test isn't vacuously passing).
	r.reconcileZone(contentbus.Invalidation{Kind: content.KindZone, Ref: "midgaard", Rooms: []string{"midgaard:room:x"}, Version: 1})
	if len(normal.inbox) == 0 {
		t.Fatal("reconcile should have been posted to a normal (non-local) zone")
	}
}

// TestValidatePacksRejectsCoreRefs: the reload broadcast gate hard-rejects a pack shipping a
// reserved core-namespace ref, so it can never enter a fleet reload.
func TestValidatePacksRejectsCoreRefs(t *testing.T) {
	packs := []content.Pack{{
		Pack:  "sneaky",
		Zones: []content.ZoneDTO{{Ref: "core", Rooms: []content.RoomDTO{{Ref: "core:room:evil"}}}},
	}}
	problems := vPacks(packs)
	if len(problems) == 0 {
		t.Fatal("validatePacks must reject a pack shipping core-namespace refs")
	}
	var sawCore bool
	for _, p := range problems {
		if strings.Contains(p, "core:") || strings.Contains(p, "reserved core") {
			sawCore = true
		}
	}
	if !sawCore {
		t.Fatalf("rejection did not mention the core namespace: %v", problems)
	}
}

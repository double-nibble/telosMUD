package main

import (
	"context"
	"testing"

	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/content"
)

// #212 slice 1: the shard-hosting helpers that make the embedded core pack the login fallback.

func TestResolveEnabledPacks(t *testing.T) {
	// Fresh DB (empty registry), no config => the demo default.
	if got := resolveEnabledPacks(config.Config{}, nil); len(got) != 1 || got[0] != content.DemoPack {
		t.Fatalf("default enabled packs = %v, want [demo]", got)
	}
	// Registry populated (a version was pulled), no config override => manifest-driven (the registry).
	got := resolveEnabledPacks(config.Config{}, []string{"mainland", "underdark"})
	if len(got) != 2 || got[0] != "mainland" || got[1] != "underdark" {
		t.Fatalf("manifest-driven packs = %v, want [mainland underdark]", got)
	}
	// Explicit override wins over the registry.
	var cfg config.Config
	cfg.ContentPacks = []string{"pinned"}
	got = resolveEnabledPacks(cfg, []string{"mainland", "underdark"})
	if len(got) != 1 || got[0] != "pinned" {
		t.Fatalf("override should win: got %v, want [pinned]", got)
	}
}

func TestWithCoreZone(t *testing.T) {
	got := withCoreZone([]string{"midgaard", "darkwood"})
	if !contains(got, content.CoreZone) {
		t.Fatalf("core zone not appended: %v", got)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 zones, got %v", got)
	}
	// Idempotent: already present => unchanged length.
	if again := withCoreZone(got); len(again) != 3 {
		t.Fatalf("withCoreZone not idempotent: %v", again)
	}
	// Does not mutate the caller's slice.
	orig := []string{"midgaard"}
	_ = withCoreZone(orig)
	if len(orig) != 1 {
		t.Fatalf("withCoreZone mutated its argument: %v", orig)
	}
}

func TestResolveHosting(t *testing.T) {
	// Real content present: a populated home is kept verbatim and core is NOT hosted — even for a
	// standby that does not host the home zone (won == nil), so a later adoption spawns correctly and
	// s.home is never repointed to the lobby.
	lc, err := content.LoadWithCore(context.Background(), memPacks{"real": {
		Pack:  "real",
		Zones: []content.ZoneDTO{{Ref: "midgaard", Rooms: []content.RoomDTO{{Ref: "midgaard:room:sq"}}}},
	}}, []string{"real"})
	if err != nil {
		t.Fatal(err)
	}
	// Standby (hosts nothing) with real content: keep the real home, do not host core.
	zones, home, coreHosted := resolveHosting(lc, nil, "midgaard")
	if home != "midgaard" || coreHosted || contains(zones, content.CoreZone) {
		t.Fatalf("standby-with-content should keep home=midgaard and NOT host core: zones=%v home=%q core=%v", zones, home, coreHosted)
	}
	// Active shard hosting the real home: unchanged, no core.
	zones, home, coreHosted = resolveHosting(lc, []string{"midgaard"}, "midgaard")
	if home != "midgaard" || coreHosted || len(zones) != 1 {
		t.Fatalf("active shard should host only midgaard: zones=%v home=%q core=%v", zones, home, coreHosted)
	}

	// Fresh/empty server (core-only content): host the core lobby and spawn there, for any shard.
	coreOnly, _ := content.LoadWithCore(context.Background(), nil, nil)
	zones, home, coreHosted = resolveHosting(coreOnly, nil, "midgaard")
	if home != content.CoreZone || !coreHosted || !contains(zones, content.CoreZone) {
		t.Fatalf("empty-content shard should host+home the core lobby: zones=%v home=%q core=%v", zones, home, coreHosted)
	}
}

// memPacks is a tiny in-test content.Source.
type memPacks map[string]content.Pack

func (m memPacks) LoadPacks(_ context.Context, enabled []string) ([]content.Pack, error) {
	var out []content.Pack
	for _, n := range enabled {
		if p, ok := m[n]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

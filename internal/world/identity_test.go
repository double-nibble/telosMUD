package world

import "testing"

// identity_test.go — unit gates for the two zone-locality primitives introduced by #410 (slice 1 of
// #72, instanced zones): Zone.ownsZoneRef and Zone.localRoom.
//
// The refactor that added them is deliberately ZERO-BEHAVIOUR-CHANGE: `template == id` for every
// zone that exists today, so ownsZoneRef is exactly equivalent to the raw comparisons it replaced and
// the existing suite passing is the equivalence proof. What that proves NOTHING about is the case the
// helpers exist FOR — a zone whose id and template differ. Minting does not exist yet, so these tests
// build the instance shape BY HAND (setting `template` directly) and pin the semantics now, while the
// contract is being decided, rather than after a later slice has already built on top of it.

// TestOwnsZoneRef is the table gate on the routing predicate: which zone segments (as returned by
// parseRef) a zone considers its own. The instance rows are the ones with teeth — under the raw
// `zoneID == z.id` this replaced, the "template" row is FALSE, which is what would read every exit
// inside an instance as leaving the zone.
func TestOwnsZoneRef(t *testing.T) {
	plain := newZone("midgaard")
	instance := newZone("midgaard#1")
	instance.template = "midgaard"

	if plain.template != plain.id {
		t.Fatalf("newZone must default template to id (a plain zone IS its own content): id=%q template=%q", plain.id, plain.template)
	}

	tests := []struct {
		name   string
		z      *Zone
		zoneID string
		want   bool
	}{
		// A plain zone: id == template, so "own id" and "template" are the same answer.
		{"plain/bare local ref", plain, "", true},
		{"plain/own id", plain, "midgaard", true},
		{"plain/foreign zone", plain, "darkwood", false},
		{"plain/its own instance's id", plain, "midgaard#1", false},

		// An instance: id and template DIFFER. Both are its own; nothing else is.
		{"instance/bare local ref", instance, "", true},
		{"instance/own id", instance, "midgaard#1", true},
		{"instance/template", instance, "midgaard", true},
		{"instance/foreign zone", instance, "darkwood", false},
		// ISOLATION: a SIBLING instance of the same template is a different zone. ownsZoneRef widens
		// to the template, never to other copies of it.
		{"instance/sibling instance", instance, "midgaard#2", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.z.ownsZoneRef(tc.zoneID); got != tc.want {
				t.Fatalf("zone{id:%q template:%q}.ownsZoneRef(%q) = %v, want %v", tc.z.id, tc.z.template, tc.zoneID, got, tc.want)
			}
		})
	}
}

// TestLocalRoom is the table gate on the resolve-if-mine chokepoint. The load-bearing distinction it
// pins is "not mine" vs "MINE BUT UNKNOWN": both return nil here, which is why move() — the one
// caller that must route the first case cross-zone and refuse the second — deliberately uses
// parseRef + ownsZoneRef directly instead. Every other caller wants exactly this collapse.
func TestLocalRoom(t *testing.T) {
	// Both zones host the SAME authored refs — that is the whole point of an instance: its rooms keep
	// the template's refs, which is what lets every copy share the immutable per-shard protoCache.
	newRooms := func(z *Zone) {
		for _, ref := range []ProtoRef{"midgaard:room:temple", "hall"} {
			e := z.newEntity(ref)
			Add(e, &Room{exits: map[string]ProtoRef{}})
			z.rooms[ref] = e
		}
	}
	plain := newZone("midgaard")
	newRooms(plain)
	instance := newZone("midgaard#1")
	instance.template = "midgaard"
	newRooms(instance)

	tests := []struct {
		name string
		z    *Zone
		ref  ProtoRef
		want ProtoRef // the expected room's ref, or "" for nil
	}{
		{"plain/bare local ref", plain, "hall", "hall"},
		{"plain/own authored ref", plain, "midgaard:room:temple", "midgaard:room:temple"},
		{"plain/mine but unknown room", plain, "midgaard:room:nowhere", ""},
		{"plain/foreign zone", plain, "darkwood:room:grove", ""},
		{"plain/an instance's id", plain, "midgaard#1:room:temple", ""},

		{"instance/bare local ref", instance, "hall", "hall"},
		// THE case: an authored (template-prefixed) ref resolves to the INSTANCE's own room entity.
		{"instance/template-authored ref", instance, "midgaard:room:temple", "midgaard:room:temple"},
		// A ref spelled with the instance's own id is owned, but no room is keyed that way (rooms keep
		// authored refs) — "mine but unknown" -> nil, NOT a panic and not a foreign lookup.
		{"instance/own id, unknown room key", instance, "midgaard#1:room:temple", ""},
		{"instance/mine but unknown room", instance, "midgaard:room:nowhere", ""},
		{"instance/foreign zone", instance, "darkwood:room:grove", ""},
		{"instance/sibling instance", instance, "midgaard#2:room:temple", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.z.localRoom(tc.ref)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("zone{id:%q template:%q}.localRoom(%q) = room %q, want nil", tc.z.id, tc.z.template, tc.ref, got.proto)
				}
				return
			}
			if got == nil {
				t.Fatalf("zone{id:%q template:%q}.localRoom(%q) = nil, want room %q", tc.z.id, tc.z.template, tc.ref, tc.want)
			}
			if got != tc.z.rooms[tc.want] {
				t.Fatalf("zone{id:%q template:%q}.localRoom(%q) resolved to %q, want THIS zone's own %q", tc.z.id, tc.z.template, tc.ref, got.proto, tc.want)
			}
		})
	}
}

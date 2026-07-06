package world

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// TestLintRoomZonePrefixes flags a room whose ref prefix diverges from its owning zone (#194) and stays
// silent for the conventional prefix==zone layout.
func TestLintRoomZonePrefixes(t *testing.T) {
	lc := &content.LoadedContent{
		Zones: []content.ZoneDTO{
			{Ref: "town", Rooms: []content.RoomDTO{
				{Ref: "town:room:square"}, // ok
				{Ref: "elsewhere:room:x"}, // divergent — authored in town but prefixed elsewhere
			}},
			{Ref: "wild", Rooms: []content.RoomDTO{
				{Ref: "wild:room:grove"}, // ok
			}},
		},
	}
	misses := lintRoomZonePrefixes(lc)
	if len(misses) != 1 {
		t.Fatalf("misses = %d, want 1: %+v", len(misses), misses)
	}
	if misses[0].room != "elsewhere:room:x" || misses[0].zone != "town" || misses[0].refZone != "elsewhere" {
		t.Fatalf("unexpected miss: %+v", misses[0])
	}
}

// TestLintRoomZonePrefixesDemoClean asserts the real embedded demo pack keeps prefix==zone (no findings),
// so the lint is silent on shipped content.
func TestLintRoomZonePrefixesDemoClean(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	if misses := lintRoomZonePrefixes(lc); len(misses) != 0 {
		t.Fatalf("demo pack has room/zone prefix mismatches: %+v", misses)
	}
}

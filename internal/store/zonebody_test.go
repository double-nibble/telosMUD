package store

import (
	"encoding/json"
	"testing"
)

// zonebody_test.go — HERMETIC (no Postgres) guards on the zones.body JSONB tail.
//
// The full round-trip assertion lives in tests/integration/store_pack_test.go and needs a real database. This
// file covers the part that can be checked without one: that the encoding the write side produces is the
// encoding the read side consumes, and that an absent/empty body decodes to the FAIL-CLOSED default.
//
// Both halves use the same zoneBody struct, so a field-NAME mismatch is not the risk here — the risk is a
// default-direction mistake, which is the security-relevant one: `instanceable` missing from the JSON must
// mean false (not instanceable), never true. A row written before the body was populated carries the column
// default '{}', and every zone in such a pack would otherwise become an instance template at once.

func TestZoneBodyRoundTrip(t *testing.T) {
	for _, want := range []bool{true, false} {
		b, err := json.Marshal(zoneBody{Instanceable: want})
		if err != nil {
			t.Fatalf("marshal zoneBody{%v}: %v", want, err)
		}
		var got zoneBody
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		if got.Instanceable != want {
			t.Fatalf("zoneBody round-trip: instanceable %v -> %v (%s)", want, got.Instanceable, b)
		}
	}
}

// TestZoneBodyDefaultsClosed pins the direction of every "the flag is not in the JSON" case. `{}` is the
// zones.body column default, so it is what EVERY row written before the body had an occupant decodes as.
func TestZoneBodyDefaultsClosed(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"something_else":1}`} {
		var zb zoneBody
		if err := json.Unmarshal([]byte(raw), &zb); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if zb.Instanceable {
			t.Fatalf("body %s decoded to instanceable=true. An absent opt-in must default to FALSE: "+
				"defaulting it on makes every zone in content an instance template, which is an uncapped "+
				"item faucet (a mint runs the zone's boot resets into a private copy)", raw)
		}
	}
}

package world

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// character_fuzz_test.go fuzzes the StateJSON SERIALIZATION round-trip (W6). StateJSON is THE durable +
// handoff payload (the characters.state JSONB, the Redis checkpoint, and the cross-shard PlayerSnapshot
// all share this shape), so a field that does not survive a marshal→unmarshal is exactly the class of
// silent state-loss bug that hit the store's prototype body twice (the dropped Living, then the dropped
// Lua). This target builds a FULLY-POPULATED StateJSON from fuzz entropy and asserts struct → JSON →
// struct identity: every field, populated, must come back DeepEqual. A new field added without a working
// json tag (or with `json:"-"`, a tag collision, or a non-round-tripping type) is caught the moment the
// builder fills it.
//
// The builder honors the round-trip constraints so DeepEqual is a true identity check, not a false alarm:
//   - collections are nil-for-empty (an empty non-nil map/slice would be dropped by omitempty and return
//     nil), so only ≥1-element collections are allocated;
//   - floats are derived from int8 (finite — json.Marshal errors on NaN/Inf and would mask real findings);
//   - strings are ASCII tokens (invalid UTF-8 is rewritten to U+FFFD by json.Marshal, which is a lossy
//     transform unrelated to the property under test);
//   - RawMessage subtrees (Script) are compact, valid JSON (the form dumpCharacter actually produces).

// fuzzEntropy is a tiny deterministic cursor over the fuzz []byte: each draw consumes bytes to derive a
// field. Past the end it yields zeros (a short input just builds a small StateJSON).
type fuzzEntropy struct {
	b []byte
	i int
}

func (e *fuzzEntropy) u8() byte {
	if e.i >= len(e.b) {
		return 0
	}
	v := e.b[e.i]
	e.i++
	return v
}

func (e *fuzzEntropy) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(e.u8()) % n
}

func (e *fuzzEntropy) bool() bool { return e.u8()&1 == 1 }

// f64 returns a finite float in [-128, 127] (exact in both float64 and JSON). Derived as int-128
// rather than an int8 cast to avoid gosec's byte->int8 narrowing flag; the range is identical.
func (e *fuzzEntropy) f64() float64 { return float64(int(e.u8()) - 128) }

// iv returns a small signed int in [-128, 127].
func (e *fuzzEntropy) iv() int { return int(e.u8()) - 128 }

// tok returns a short ASCII token (valid UTF-8, so it round-trips through json byte-for-byte). May be
// empty — an empty string is a legal, round-tripping key/value.
func (e *fuzzEntropy) tok() string {
	const alpha = "abcdefghijklmnopqrstuvwxyz0123_."
	n := e.intn(8)
	var sb strings.Builder
	for k := 0; k < n; k++ {
		sb.WriteByte(alpha[int(e.u8())%len(alpha)])
	}
	return sb.String()
}

func FuzzStateJSONRoundTrip(f *testing.F) {
	// A handful of byte seeds of varying length so the corpus starts with small + larger states.
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Add([]byte{0xff, 0x02, 0x10, 0x40, 0x80, 0x01, 0x33, 0x07, 0x09})
	f.Add([]byte("a longer seed with many bytes to populate several subtrees at once 0123456789"))

	f.Fuzz(func(t *testing.T, data []byte) {
		e := &fuzzEntropy{b: data}
		var st StateJSON
		st.AppliedSeq = uint64(e.u8())<<8 | uint64(e.u8())

		if n := e.intn(4); n > 0 {
			st.Inventory = make([]ItemJSON, n)
			for k := range st.Inventory {
				it := ItemJSON{ProtoRef: e.tok()}
				if m := e.intn(3); m > 0 { // one nesting level of contents
					it.Contents = make([]ItemJSON, m)
					for j := range it.Contents {
						it.Contents[j] = ItemJSON{ProtoRef: e.tok()}
					}
				}
				st.Inventory[k] = it
			}
		}
		if n := e.intn(3); n > 0 {
			st.Equipment = make(map[string]ItemJSON, n)
			for k := 0; k < n; k++ {
				st.Equipment[e.tok()] = ItemJSON{ProtoRef: e.tok()}
			}
		}
		if n := e.intn(4); n > 0 {
			st.Attributes = make(map[string]float64, n)
			for k := 0; k < n; k++ {
				st.Attributes[e.tok()] = e.f64()
			}
		}
		if n := e.intn(4); n > 0 {
			st.Resources = make(map[string]ResourceJSON, n)
			for k := 0; k < n; k++ {
				st.Resources[e.tok()] = ResourceJSON{Cur: e.iv()}
			}
		}
		if n := e.intn(4); n > 0 {
			st.Affects = make([]AffectJSON, n)
			for k := range st.Affects {
				st.Affects[k] = AffectJSON{ID: e.tok(), Remaining: e.iv(), Mag: e.f64(), Stacks: e.intn(5)}
			}
		}
		if n := e.intn(4); n > 0 {
			st.Flags = make([]string, n)
			for k := range st.Flags {
				st.Flags[k] = e.tok()
			}
		}
		if n := e.intn(4); n > 0 {
			st.Cooldowns = make(map[string]int, n)
			for k := 0; k < n; k++ {
				st.Cooldowns[e.tok()] = e.iv()
			}
		}
		if e.bool() { // Script: a compact, valid JSON object (the dumpCharacter form)
			raw, _ := json.Marshal(map[string]int{e.tok(): e.iv()})
			st.Script = raw
		}
		if e.bool() { // Tells: a non-nil cursor (possibly empty Delivered)
			tc := &TellCursorJSON{}
			if n := e.intn(4); n > 0 {
				tc.Delivered = make(map[string]uint64, n)
				for k := 0; k < n; k++ {
					tc.Delivered[e.tok()] = uint64(e.u8())
				}
			}
			st.Tells = tc
		}
		if e.bool() { // Comms: a non-nil state (possibly all-default)
			cs := &CommsStateJSON{AFK: e.bool(), AFKMsg: e.tok()}
			if n := e.intn(4); n > 0 {
				cs.Channels = make(map[string]bool, n)
				for k := 0; k < n; k++ {
					cs.Channels[e.tok()] = e.bool()
				}
			}
			if n := e.intn(4); n > 0 {
				cs.Ignore = make([]string, n)
				for k := range cs.Ignore {
					cs.Ignore[k] = e.tok()
				}
			}
			st.Comms = cs
		}

		// Marshal must never fail (finite floats, valid strings, valid RawMessage).
		b, err := json.Marshal(st)
		if err != nil {
			t.Fatalf("json.Marshal(StateJSON) failed: %v\nstate: %+v", err, st)
		}
		// Unmarshal must never fail on our own output, and must reproduce the struct exactly.
		var got StateJSON
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("json.Unmarshal of own output failed: %v\njson: %s", err, b)
		}
		if !reflect.DeepEqual(st, got) {
			t.Fatalf("StateJSON did not round-trip through JSON.\n in: %+v\nout: %+v\njson: %s", st, got, b)
		}
	})
}

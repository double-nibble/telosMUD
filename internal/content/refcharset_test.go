package content

import (
	"context"
	"strings"
	"testing"
)

// TestLintRefCharsetClean: refs/verbs/surfaces made only of [A-Za-z0-9_:-] produce no findings, including a
// multi-segment ref, a hyphenated ref, and an empty (omitempty) ref.
func TestLintRefCharsetClean(t *testing.T) {
	p := Pack{
		Pack: "clean",
		Zones: []ZoneDTO{{
			Ref:  "midgaard",
			Name: "The City of Midgaard", // a DISPLAY field — spaces are fine, must NOT be flagged
			Rooms: []RoomDTO{
				// Exit-direction keys are DIRECTIONS (dirCharset): letters/digits/`_ -`, no colon. A dotted
				// VALUE ("far.zone:room") is a target ref that sinks into an equality lookup — never checked.
				{Ref: "midgaard:room:temple", Name: "The Temple Square", Exits: map[string]string{
					"north": "midgaard:room:market", "up": "midgaard:room:tower",
					"north-east": "midgaard:room:gate", "hidden_door": "far.zone:room:vault",
				}},
			},
			Items: []ProtoDTO{{Ref: "midgaard:obj:leather-belt"}}, // hyphen is allowed
			Mobs:  []ProtoDTO{{Ref: "midgaard:mob:orc"}},
		}},
		Channels: []ChannelDTO{{Ref: "gossip", Words: []string{"gossip", "gos"}}}, // verb tokens — checked
		Commands: []CommandDTO{{Verb: "gossip", Aliases: []string{"go", "gos"}}},  // verb + alias tokens
		Attributes: []AttributeDTO{
			{Ref: "max_hp"}, {Ref: ""}, // empty ref skipped
		},
		DisplayDefs: []DisplayDefDTO{{Surface: "score"}},
		// Recipe short-names legitimately carry spaces (multi-word isname/prefix keyword) and must NOT be
		// flagged — RecipeDTO.Aliases is deliberately excluded from the verb-list charset check.
		Recipes: []RecipeDTO{{Ref: "recipe:leather-vest", Aliases: []string{"vest", "leather vest"}}},
		// Trust-tier names are the ladder's identity key (no Ref on TrustTierDTO) — checked with refCharset.
		TrustTiers: []TrustTierDTO{{Name: "player", Rank: 0}, {Name: "builder", Rank: 10}},
	}
	if got := LintRefCharset([]Pack{p}); len(got) != 0 {
		t.Fatalf("clean pack produced findings: %+v", got)
	}
}

// TestLintRefCharsetCatchesBadTokens: a token with a dot, brace, whitespace, or NATS/GMCP metacharacter is
// flagged — at any nesting depth, and across Ref / Verb / Surface fields.
func TestLintRefCharsetCatchesBadTokens(t *testing.T) {
	p := Pack{
		Pack: "bad",
		Zones: []ZoneDTO{{
			Ref: "zone.one", // dot — breaks a GMCP/NATS subject hierarchy
			Rooms: []RoomDTO{
				{Ref: "zone:room:has space"}, // whitespace — breaks the tokenizer
			},
			Mobs: []ProtoDTO{{Ref: "zone:mob:orc{}"}}, // braces — break GMCP JSON / the tokenizer
		}},
		Commands:    []CommandDTO{{Verb: "gos.sip", Aliases: []string{"al.ias"}}}, // bad Verb + bad ALIAS token
		Channels:    []ChannelDTO{{Ref: "ok", Words: []string{"wor{d}"}}},         // bad WORDS verb token (#66 F1)
		ToggleDefs:  []ToggleDTO{{Ref: "ok2", Words: []string{"tog*le"}}},         // bad toggle-verb token (#358)
		DisplayDefs: []DisplayDefDTO{{Surface: "sc*re"}},                          // bad Surface (NATS wildcard)
	}
	got := LintRefCharset([]Pack{p})
	// Collect the offending values for an order-independent assertion.
	bad := map[string]string{} // value -> field
	for _, v := range got {
		if v.Pack != "bad" {
			t.Fatalf("finding attributed to wrong pack: %+v", v)
		}
		bad[v.Value] = v.Field
	}
	for _, want := range []struct{ val, field string }{
		{"zone.one", "Ref"},
		{"zone:room:has space", "Ref"},
		{"zone:mob:orc{}", "Ref"},
		{"gos.sip", "Verb"},
		{"al.ias", "Aliases"}, // a command-alias verb token (type-qualified list field)
		{"wor{d}", "Words"},   // a channel-verb token
		{"tog*le", "Words"},   // a toggle-verb token (#358)
		{"sc*re", "Surface"},
	} {
		if bad[want.val] != want.field {
			t.Errorf("expected %q flagged as field %q; got field %q (all: %+v)", want.val, want.field, bad[want.val], got)
		}
	}
	if len(got) != 8 {
		t.Fatalf("expected exactly 8 findings, got %d: %+v", len(got), got)
	}
}

// TestLintRefCharsetExitDirectionKeys (#234): an exit-direction map KEY reaches a GMCP JSON key + the
// movement parser, so a direction with a dot / brace / space / colon is flagged (field "Exits"); a clean
// direction (incl. hyphen/underscore) is not; and the exit VALUE (a target room ref) is NEVER charset-checked.
func TestLintRefCharsetExitDirectionKeys(t *testing.T) {
	p := Pack{
		Pack: "exits",
		Zones: []ZoneDTO{{
			Ref: "maze",
			Rooms: []RoomDTO{{
				Ref: "maze:room:1",
				Exits: map[string]string{
					"north":          "maze:room:2",     // clean
					"north-east":     "maze:room:3",     // hyphen ok
					"hidden_door":    "maze:room:4",     // underscore ok
					"west":           "far.zone:room:9", // clean KEY, dotted VALUE — value must NOT be flagged
					"":               "maze:room:0",     // empty key — skipped, like an omitempty ref
					"hidden.passage": "maze:room:5",     // dot — bad (GMCP/NATS key)
					"up down":        "maze:room:6",     // space — bad (tokenizer)
					"portal:x":       "maze:room:7",     // colon — a direction is not a segmented ref, bad
					"portal*":        "maze:room:8",     // NATS wildcard — bad
					"orc{}":          "maze:room:9b",    // brace — bad (GMCP JSON / tokenizer)
				},
			}},
		}},
	}
	got := LintRefCharset([]Pack{p})
	bad := map[string]RefCharsetViolation{} // value -> finding
	for _, v := range got {
		if v.Pack != "exits" {
			t.Fatalf("finding attributed to wrong pack: %+v", v)
		}
		bad[v.Value] = v
	}
	for _, want := range []string{"hidden.passage", "up down", "portal:x", "portal*", "orc{}"} {
		if bad[want].Field != "Exits" {
			t.Errorf("expected direction %q flagged as field Exits; got %q (all: %+v)", want, bad[want].Field, got)
		}
		if !strings.Contains(bad[want].Charset, "direction") {
			t.Errorf("direction finding %q must carry the DIRECTION charset label; got %q", want, bad[want].Charset)
		}
	}
	for _, ok := range []string{"north", "north-east", "hidden_door", "west"} {
		if _, flagged := bad[ok]; flagged {
			t.Errorf("clean direction %q was flagged: %+v", ok, got)
		}
	}
	// An empty exit-direction key is skipped (the omitempty-ref rule), not flagged.
	if _, flagged := bad[""]; flagged {
		t.Errorf("empty exit key should be skipped, not flagged: %+v", got)
	}
	// Exit VALUES (target refs) sink into an equality lookup, never a raw subject/key — never charset-checked.
	if _, flagged := bad["far.zone:room:9"]; flagged {
		t.Errorf("exit VALUE far.zone:room:9 must not be charset-checked: %+v", got)
	}
	if len(got) != 5 {
		t.Fatalf("expected exactly 5 direction findings, got %d: %+v", len(got), got)
	}
}

// TestLintRefCharsetTrustTierName (#234): TrustTierDTO has no Ref — the loader keys tiers by Name and the
// world derives rank/flags by it, so a tier Name with a bad char is flagged (field "Name"); a clean tier
// name is not, and the tier's non-identity fields (Flags, a []string not in refListFields) are untouched.
func TestLintRefCharsetTrustTierName(t *testing.T) {
	p := Pack{
		Pack: "tiers",
		TrustTiers: []TrustTierDTO{
			{Name: "builder", Rank: 10, Flags: []string{"builder"}}, // clean; Flags not charset-checked
			{Name: "arch:mage", Rank: 15},                           // colon — CLEAN: tier Name uses refCharset (allows ':'), NOT dirCharset
			{Name: "", Rank: 5},                                     // empty name — skipped
			{Name: "super admin", Rank: 20},                         // space — bad
			{Name: "arch.mage", Rank: 30},                           // dot — bad
		},
	}
	got := LintRefCharset([]Pack{p})
	bad := map[string]RefCharsetViolation{}
	for _, v := range got {
		bad[v.Value] = v
	}
	for _, want := range []string{"super admin", "arch.mage"} {
		if bad[want].Field != "Name" {
			t.Errorf("expected tier name %q flagged as field Name; got %q (all: %+v)", want, bad[want].Field, got)
		}
		// Name is an identity token, judged by refCharset — its finding must NOT carry the direction label.
		if strings.Contains(bad[want].Charset, "direction") {
			t.Errorf("tier-name finding %q must use the REF charset (colon-allowing), not the direction charset; got %q", want, bad[want].Charset)
		}
	}
	// The load-bearing distinction: a colon in a tier name is CLEAN (refCharset allows ':'), whereas the same
	// colon in an exit direction is rejected. This is the only case that pins Name to refCharset, not dirCharset.
	if _, flagged := bad["arch:mage"]; flagged {
		t.Errorf("tier name with a colon must be CLEAN (refCharset allows ':'): %+v", got)
	}
	if _, flagged := bad["builder"]; flagged {
		t.Errorf("clean tier name was flagged: %+v", got)
	}
	// An empty tier name is skipped (the omitempty-ref rule).
	if _, flagged := bad[""]; flagged {
		t.Errorf("empty tier name should be skipped, not flagged: %+v", got)
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 tier-name findings, got %d: %+v", len(got), got)
	}
}

// TestLintRefCharsetRealPacksClean is the no-false-positive guard: the shipped embedded packs (demo + core)
// must produce ZERO findings, so enabling the lint can never wedge a real boot/reload.
func TestLintRefCharsetRealPacksClean(t *testing.T) {
	for _, pack := range []string{DemoPack, CorePack} {
		packs, err := EmbeddedSource{}.LoadPacks(context.Background(), []string{pack})
		if err != nil {
			t.Fatalf("load %q: %v", pack, err)
		}
		if got := LintRefCharset(packs); len(got) != 0 {
			t.Fatalf("shipped pack %q has ref-charset violations (the lint would reject real content): %+v", pack, got)
		}
	}
}

// TestLintRefLengthClean (#483): tokens at or under the bound produce no findings — including one exactly AT
// the limit (the boundary is inclusive: len<=maxLen is fine) and an empty (omitempty) token.
func TestLintRefLengthClean(t *testing.T) {
	maxLen := 32
	p := Pack{
		Pack: "clean",
		Zones: []ZoneDTO{{
			Ref:  strings.Repeat("z", maxLen),                               // exactly AT the limit — must be clean (len<=maxLen)
			Name: strings.Repeat("very long display name with spaces ", 20), // DISPLAY field — never checked, any length
			Rooms: []RoomDTO{{
				Ref:   "z:room:1",
				Exits: map[string]string{strings.Repeat("n", maxLen): strings.Repeat("z:room:", 100)}, // key AT limit ok; VALUE never length-checked
			}},
			Items: []ProtoDTO{{Ref: ""}}, // empty ref skipped
		}},
		Channels: []ChannelDTO{{Ref: "gossip", Words: []string{strings.Repeat("w", maxLen)}}}, // verb AT limit ok
		// RecipeDTO.Aliases carries multi-word craft short-names (excluded from the verb-list checks), so a
		// long one is free text, never length-checked.
		Recipes:    []RecipeDTO{{Ref: "recipe:vest", Aliases: []string{strings.Repeat("keyword ", 50)}}},
		TrustTiers: []TrustTierDTO{{Name: strings.Repeat("t", maxLen), Rank: 1}}, // tier Name AT limit ok
	}
	if got := LintRefLength([]Pack{p}, maxLen); len(got) != 0 {
		t.Fatalf("tokens at/under the bound produced findings: %+v", got)
	}
}

// TestLintRefLengthCatchesLongTokens (#483): a token one byte OVER the bound is flagged, across every
// identity-token shape walkRefFields visits — a scalar Ref, a Verb, a Surface, a refNameFields tier Name, a
// refListFields verb-list element, and a refKeyFields exit-direction KEY. The exit VALUE (a target ref) is
// NEVER length-checked. Field + byte Length are reported for the operator message.
func TestLintRefLengthCatchesLongTokens(t *testing.T) {
	maxLen := 16
	over := maxLen + 1
	longRef := strings.Repeat("a", over)
	longVerb := strings.Repeat("b", over)
	longSurface := strings.Repeat("c", over)
	longName := strings.Repeat("d", over)
	longWord := strings.Repeat("e", over)
	longDir := strings.Repeat("f", over)
	longExitTarget := strings.Repeat("g", over*4) // an exit VALUE — must NOT be flagged
	p := Pack{
		Pack: "bad",
		Zones: []ZoneDTO{{
			Ref: longRef,
			Rooms: []RoomDTO{{
				Ref:   "z:room:1",
				Exits: map[string]string{longDir: longExitTarget},
			}},
		}},
		Commands:    []CommandDTO{{Verb: longVerb}},
		Channels:    []ChannelDTO{{Ref: "ok", Words: []string{longWord}}},
		DisplayDefs: []DisplayDefDTO{{Surface: longSurface}},
		TrustTiers:  []TrustTierDTO{{Name: longName, Rank: 1}},
	}
	got := LintRefLength([]Pack{p}, maxLen)
	byVal := map[string]RefLengthViolation{}
	for _, v := range got {
		if v.Pack != "bad" {
			t.Fatalf("finding attributed to wrong pack: %+v", v)
		}
		if v.Length != over || v.Max != maxLen {
			t.Errorf("finding %q reports Length=%d Max=%d; want %d and %d", v.Value, v.Length, v.Max, over, maxLen)
		}
		byVal[v.Value] = v
	}
	for _, want := range []struct{ val, field string }{
		{longRef, "Ref"},
		{longVerb, "Verb"},
		{longSurface, "Surface"},
		{longName, "Name"},
		{longWord, "Words"},
		{longDir, "Exits"},
	} {
		if byVal[want.val].Field != want.field {
			t.Errorf("expected %q flagged as field %q; got %q (all: %+v)", want.val[:8]+"…", want.field, byVal[want.val].Field, got)
		}
	}
	// The exit VALUE (a target ref, 4× over the bound) sinks into an equality lookup — never length-checked.
	if _, flagged := byVal[longExitTarget]; flagged {
		t.Errorf("exit VALUE must not be length-checked: %+v", got)
	}
	if len(got) != 6 {
		t.Fatalf("expected exactly 6 length findings, got %d: %+v", len(got), got)
	}
}

// TestLintRefLengthCoversStorePrimaryKeys (#483 persistence review): the byte bound must also reach the two
// content-authored store PRIMARY KEYS the CHARSET lint deliberately skips — the pack NAME (pack_meta.pack)
// and each ruleset-formula override NAME (formula_defs (pack, name)) — because an over-long value there fails
// the import transaction at runtime just like an over-long ref. These sink into an equality lookup / composite
// PK, not a NATS subject / GMCP key, so they get the LENGTH bound but not the charset gate.
func TestLintRefLengthCoversStorePrimaryKeys(t *testing.T) {
	maxLen := 16
	over := maxLen + 1
	longPack := strings.Repeat("p", over)
	longFormula := strings.Repeat("f", over)
	p := Pack{
		Pack:     longPack,                                                                           // pack_meta.pack PRIMARY KEY
		Formulas: map[string]string{longFormula: "return 1", "to_hit": strings.Repeat("x", over*10)}, // KEY checked; the Lua BODY (value) is not a PK, never length-checked
	}
	got := LintRefLength([]Pack{p}, maxLen)
	byField := map[string]RefLengthViolation{}
	for _, v := range got {
		byField[v.Field] = v
	}
	if byField["Pack"].Value != longPack {
		t.Errorf("over-long pack name must be flagged (field Pack); got: %+v", got)
	}
	if byField["Formulas"].Value != longFormula {
		t.Errorf("over-long formula name must be flagged (field Formulas); got: %+v", got)
	}
	// The formula BODY (a Lua string value, 10× over the bound) is not a store key — must NOT be flagged.
	for _, v := range got {
		if strings.HasPrefix(v.Value, "x") {
			t.Errorf("a formula Lua body (map VALUE) must not be length-checked: %+v", v)
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 findings (pack name + formula name), got %d: %+v", len(got), got)
	}
}

// TestLintRefLengthCountsBytesNotRunes (#483 review): the bound is a BYTE bound (the btree ceiling and NATS
// subjects count bytes), so a token of few RUNES but many BYTES is judged by its byte length. A 10-rune
// 3-bytes-per-rune ref (30 bytes) exceeds a 20-byte bound even though its rune count does not.
func TestLintRefLengthCountsBytesNotRunes(t *testing.T) {
	maxLen := 20
	multibyte := strings.Repeat("世", 10) // 10 runes, 30 bytes (3 bytes each) — over a 20-BYTE bound, under 20 runes
	if len([]rune(multibyte)) > maxLen {
		t.Fatalf("test setup: want rune count <= maxLen (%d) to isolate the byte-vs-rune distinction, got %d runes", maxLen, len([]rune(multibyte)))
	}
	got := LintRefLength([]Pack{{Pack: "a", Zones: []ZoneDTO{{Ref: multibyte}}}}, maxLen)
	found := false
	for _, v := range got {
		if v.Value == multibyte {
			found = true
			if v.Length != len(multibyte) { // 30 bytes, not 10 runes
				t.Errorf("Length must be BYTE length %d, got %d", len(multibyte), v.Length)
			}
		}
	}
	if !found {
		t.Fatalf("a 30-byte (10-rune) ref must be flagged by a 20-byte bound: %+v", got)
	}
}

// TestLintRefLengthRealPacksClean is the no-false-positive guard mirroring the charset lint's: the shipped
// embedded packs must produce ZERO findings at the SHIPPED RefMaxLen (256), so enabling the length lint can
// never wedge a real boot/reload. (Longest real identity token is 27 bytes — midgaard:obj:leather-gloves.)
func TestLintRefLengthRealPacksClean(t *testing.T) {
	for _, pack := range []string{DemoPack, CorePack} {
		packs, err := EmbeddedSource{}.LoadPacks(context.Background(), []string{pack})
		if err != nil {
			t.Fatalf("load %q: %v", pack, err)
		}
		if got := LintRefLength(packs, RefMaxLen); len(got) != 0 {
			t.Fatalf("shipped pack %q has ref-length violations at RefMaxLen=%d (the lint would reject real content): %+v", pack, RefMaxLen, got)
		}
	}
}

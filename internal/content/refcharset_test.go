package content

import (
	"context"
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
				{Ref: "midgaard:room:temple", Name: "The Temple Square"},
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
		{"sc*re", "Surface"},
	} {
		if bad[want.val] != want.field {
			t.Errorf("expected %q flagged as field %q; got field %q (all: %+v)", want.val, want.field, bad[want.val], got)
		}
	}
	if len(got) != 7 {
		t.Fatalf("expected exactly 7 findings, got %d: %+v", len(got), got)
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

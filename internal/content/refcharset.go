package content

import (
	"reflect"
	"regexp"
)

// refcharset.go — the load-time ref-charset content-lint (#66). Several safety judgments across the engine
// rest on "a ref cannot contain braces / dots / whitespace / control characters" by CONVENTION only — the
// GMCP strip that skips Vitals/Stats/zone/exit KEYS, the comms-subject routing, and the targeting tokenizer
// all assume it. Nothing enforced it at pack load. This lint makes the invariant explicit: every identity
// TOKEN a pack defines (a ref, a channel/command verb, a display surface) must match a conservative charset,
// so a ref like "gos.sip" (breaks a NATS/GMCP subject hierarchy) or "orc{}" (breaks the tokenizer / GMCP
// JSON) is caught before it can reach any of those paths.
//
// It is deliberately REFLECTION-based (like the store "reflect net", #20): it visits every Ref/Verb/Surface
// string field AND the elements of the type-qualified verb-list fields (refListFields) anywhere in the Pack
// tree, so a newly-added content DTO's Ref is covered automatically — the exact failure mode #66 guards
// against is a token class nobody remembered to hand-list. Human DISPLAY fields (Name/DisplayName/Label)
// and multi-word targeting keywords (ProtoDTO.Aliases) are intentionally NOT checked: they are free text
// that legitimately carries spaces and punctuation.
//
// SCOPE / non-goals (documented so nobody over-trusts this lint):
//   - It covers DEFINED identity tokens. Ref-VALUED fields under other names (exit targets, reset protos,
//     loot item refs, …) are NOT charset-checked here — they are safe not because they name a checked def
//     (boot tolerates dangling refs, so a reference CAN carry an undefined token) but because every such
//     reference sinks into an EQUALITY LOOKUP or an integer hash (z.rooms[target], roomNum(dst)), never a
//     raw subject/key concatenation. If a future feature ever routes such a reference RAW into a NATS
//     subject or GMCP key, it must be charset-checked at that new sink.
//   - Map KEYS are covered only for the type-qualified maps in refKeyFields (today: RoomDTO.Exits, an
//     exit-direction key roomInfoJSON emits as a GMCP JSON key + feeds movement matching). Those keys get
//     the DIRECTION charset (dirCharset), not the ref charset — a direction is not a colon-segmented ref.
//     Any OTHER map's keys are still unchecked; if a future feature routes a map key RAW into a subject or
//     GMCP key, add that map to refKeyFields with the right charset (#234).
//   - The charset allows ':' (the ref segment separator), so it does NOT make a colon-composed-then-
//     reparsed identifier unforgeable — a ref with extra ':' segments can shift a parseRef inference. That
//     is a property of the ref scheme, pre-existing, and out of scope here.

// refCharset is the allowed ref/verb/surface charset: ASCII letters and digits plus `_ : -`. Colon is the
// ref segment separator ("midgaard:mob:orc"); hyphen appears in real refs ("tomb-guardian"). Everything
// else — notably `.`, `{`, `}`, whitespace, control characters, and NATS/GMCP metacharacters (`* > %`) — is
// rejected. Anchored + one-or-more, so an empty token never matches here (empties are filtered before this).
var refCharset = regexp.MustCompile(`^[A-Za-z0-9_:-]+$`)

// dirCharset is the allowed EXIT-DIRECTION charset (#234): ASCII letters and digits plus `_ -`. A direction
// ("north", "up", "north-east", "secret_door") is not a colon-segmented ref, so — unlike refCharset — it
// forbids ':' too, alongside the same `.`, `{`, `}`, whitespace, control, and NATS/GMCP metacharacters. It
// bounds an exit key that roomInfoJSON emits verbatim as a GMCP JSON key (internal/world/gmcp.go) and that
// the movement parser matches on, so a direction like "secret.passage" is caught at load, not shipped.
var dirCharset = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// refFieldNames are the SCALAR string field names that carry an identity TOKEN — one that flows into a GMCP
// key, a comms subject, a command verb, or the targeting tokenizer. Everything else is left alone.
var refFieldNames = map[string]bool{"Ref": true, "Verb": true, "Surface": true}

// refNameFields are the <StructType>.<Field> SCALAR string fields that carry an identity token under a
// field name OTHER than Ref/Verb/Surface — a def keyed by a "Name" instead of a "Ref". Type-qualified so a
// generic display "Name"/"Label" is NOT swept in: only these named fields are the def's identity key.
// TrustTierDTO has no Ref — the loader keys tiers by tt.Name and the world derives rank/flags by it, so the
// tier name is an identity token that must obey the ref charset (#234). Checked with refCharset.
var refNameFields = map[string]bool{
	"TrustTierDTO.Name": true, // the trust-ladder identity key (no Ref on TrustTierDTO)
}

// refKeyFields are the <StructType>.<Field> MAP fields whose KEYS are identity tokens reaching a sink.
// Type-qualified because most map keys are safe (they sink into equality lookups); these do not. Each is
// checked with the charset noted. RoomDTO.Exits keys are DIRECTIONS (dirCharset) — a GMCP JSON key +
// movement-match token (#234).
var refKeyFields = map[string]bool{
	"RoomDTO.Exits": true, // exit-direction keys (dirCharset)
}

// refListFields are the <StructType>.<Field> []string fields that carry a SET of single-token, EXACT-DISPATCH
// VERBS — channel/ability invocation words and alternate command spellings. Each element registers into the
// same command/tokenizer table as a scalar Verb, so it is charset-checked too (#66 F1). Type-QUALIFIED on
// purpose: a bare field name is ambiguous — RecipeDTO.Aliases carries MULTI-WORD craft short-names
// ("leather vest") matched by isname/prefix (not exact verb dispatch), which legitimately contain spaces and
// must NOT be charset-checked.
var refListFields = map[string]bool{
	"ChannelDTO.Words":   true, // channel invocation verbs
	"AbilityDTO.Words":   true, // ability command verbs
	"ToggleDTO.Words":    true, // player-toggle verbs (#358; same exact-dispatch table as channel words)
	"CommandDTO.Aliases": true, // alternate command spellings (exact-match dispatch)
}

// charset display labels for operator messages — the allowed set the violated token failed. A DIRECTION
// key is judged by dirCharset (colon-excluding), so a rejected exit like "portal:x" must NOT be told the
// safe set contains ':' (the pre-#234 messages hardcoded the ref label for every class — misleading here).
const (
	refCharsetLabel = "[A-Za-z0-9_:-]"
	dirCharsetLabel = "[A-Za-z0-9_-] (a direction is not a segmented ref, so ':' is disallowed)"
)

// RefCharsetViolation is one finding: an identity token with a character outside the safe charset.
type RefCharsetViolation struct {
	Pack    string // the pack that ships the token
	Field   string // the struct field it came from ("Ref" | "Verb" | "Surface" | "Name" | "Exits")
	Value   string // the offending token
	Charset string // the allowed-charset LABEL the token failed (ref vs direction) — for the operator message
}

// LintRefCharset returns a finding for every identity token (Ref/Verb/Surface, the type-qualified verb
// lists, the refNameFields def-names, and the refKeyFields map KEYS, anywhere in a pack) that contains a
// character outside its charset — refCharset for identity tokens, dirCharset for exit-direction keys.
// Build-time and non-fatal at boot (the caller logs, like the other content-lints); the reload gate treats
// it as a hard reject so a bad token can never enter a fleet reload. Empty tokens are skipped — an omitempty
// ref means "not set", validated (or not) elsewhere.
func LintRefCharset(packs []Pack) []RefCharsetViolation {
	var out []RefCharsetViolation
	for _, p := range packs {
		seen := map[string]bool{} // dedupe an identical (field,value) repeated within one pack
		walkRefFields(reflect.ValueOf(p), func(field, value string, isDir bool) {
			charset, label := refCharset, refCharsetLabel
			if isDir {
				charset, label = dirCharset, dirCharsetLabel
			}
			if value == "" || charset.MatchString(value) {
				return
			}
			key := field + "\x00" + value
			if seen[key] {
				return
			}
			seen[key] = true
			out = append(out, RefCharsetViolation{Pack: p.Pack, Field: field, Value: value, Charset: label})
		})
	}
	return out
}

// walkRefFields recursively visits v, invoking fn(fieldName, value, isDir) for every identity token: an
// exported string struct field whose name is in refFieldNames or whose <Type>.<Field> is in refNameFields
// (isDir=false); every element of a []string field whose <Type>.<Field> is in refListFields (isDir=false);
// and every KEY of a map field whose <Type>.<Field> is in refKeyFields (isDir=true). It descends structs,
// slices, arrays, pointers, interfaces, and map values, so a token nested at any depth (a room inside a
// zone, a verb inside a channel) is reached.
func walkRefFields(v reflect.Value, fn func(field, value string, isDir bool)) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			walkRefFields(v.Elem(), fn)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walkRefFields(v.Index(i), fn)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			walkRefFields(v.MapIndex(k), fn)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			fv := v.Field(i)
			if fv.Kind() == reflect.String { // a terminal scalar token — check it iff it's an identity field
				if refFieldNames[f.Name] || refNameFields[t.Name()+"."+f.Name] {
					fn(f.Name, fv.String(), false)
				}
				continue
			}
			// A verb LIST field ([]string), type-qualified so item targeting keywords aren't caught: each
			// element is a single-token verb (#66 F1).
			if refListFields[t.Name()+"."+f.Name] && (fv.Kind() == reflect.Slice || fv.Kind() == reflect.Array) &&
				fv.Type().Elem().Kind() == reflect.String {
				for j := 0; j < fv.Len(); j++ {
					fn(f.Name, fv.Index(j).String(), false)
				}
				continue
			}
			// A map field whose KEYS are identity tokens (type-qualified). Check each key with the direction
			// charset; the VALUES are exit-target refs that sink into an equality lookup, so they are left to
			// the reference-not-checked scope rule above. Still descend the values for any nested token (#234).
			if refKeyFields[t.Name()+"."+f.Name] && fv.Kind() == reflect.Map && fv.Type().Key().Kind() == reflect.String {
				for _, k := range fv.MapKeys() {
					fn(f.Name, k.String(), true)
				}
			}
			walkRefFields(fv, fn)
		}
	}
}

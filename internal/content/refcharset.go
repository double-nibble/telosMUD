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
//   - It does not cover map KEYS. Room exit-direction keys reach a GMCP key verbatim and are a known gap
//     (a direction-charset follow-up), bounded because JSON escapes them.
//   - The charset allows ':' (the ref segment separator), so it does NOT make a colon-composed-then-
//     reparsed identifier unforgeable — a ref with extra ':' segments can shift a parseRef inference. That
//     is a property of the ref scheme, pre-existing, and out of scope here.

// refCharset is the allowed ref/verb/surface charset: ASCII letters and digits plus `_ : -`. Colon is the
// ref segment separator ("midgaard:mob:orc"); hyphen appears in real refs ("tomb-guardian"). Everything
// else — notably `.`, `{`, `}`, whitespace, control characters, and NATS/GMCP metacharacters (`* > %`) — is
// rejected. Anchored + one-or-more, so an empty token never matches here (empties are filtered before this).
var refCharset = regexp.MustCompile(`^[A-Za-z0-9_:-]+$`)

// refFieldNames are the SCALAR string field names that carry an identity TOKEN — one that flows into a GMCP
// key, a comms subject, a command verb, or the targeting tokenizer. Everything else is left alone.
var refFieldNames = map[string]bool{"Ref": true, "Verb": true, "Surface": true}

// refListFields are the <StructType>.<Field> []string fields that carry a SET of single-token, EXACT-DISPATCH
// VERBS — channel/ability invocation words and alternate command spellings. Each element registers into the
// same command/tokenizer table as a scalar Verb, so it is charset-checked too (#66 F1). Type-QUALIFIED on
// purpose: a bare field name is ambiguous — RecipeDTO.Aliases carries MULTI-WORD craft short-names
// ("leather vest") matched by isname/prefix (not exact verb dispatch), which legitimately contain spaces and
// must NOT be charset-checked.
var refListFields = map[string]bool{
	"ChannelDTO.Words":   true, // channel invocation verbs
	"AbilityDTO.Words":   true, // ability command verbs
	"CommandDTO.Aliases": true, // alternate command spellings (exact-match dispatch)
}

// RefCharsetViolation is one finding: an identity token with a character outside the safe charset.
type RefCharsetViolation struct {
	Pack  string // the pack that ships the token
	Field string // the struct field it came from ("Ref" | "Verb" | "Surface")
	Value string // the offending token
}

// LintRefCharset returns a finding for every identity token (Ref/Verb/Surface, anywhere in a pack) that
// contains a character outside refCharset. Build-time and non-fatal at boot (the caller logs, like the other
// content-lints); the reload gate treats it as a hard reject so a bad token can never enter a fleet reload.
// Empty tokens are skipped — an omitempty ref means "not set", validated (or not) elsewhere.
func LintRefCharset(packs []Pack) []RefCharsetViolation {
	var out []RefCharsetViolation
	for _, p := range packs {
		seen := map[string]bool{} // dedupe an identical (field,value) repeated within one pack
		walkRefFields(reflect.ValueOf(p), func(field, value string) {
			if value == "" || refCharset.MatchString(value) {
				return
			}
			key := field + "\x00" + value
			if seen[key] {
				return
			}
			seen[key] = true
			out = append(out, RefCharsetViolation{Pack: p.Pack, Field: field, Value: value})
		})
	}
	return out
}

// walkRefFields recursively visits v, invoking fn(fieldName, value) for every exported string struct field
// whose name is in refFieldNames, and for every element of a []string field whose name is in
// refListFieldNames. It descends structs, slices, arrays, pointers, interfaces, and map values, so a token
// nested at any depth (a room inside a zone, a verb inside a channel) is reached.
func walkRefFields(v reflect.Value, fn func(field, value string)) {
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
				if refFieldNames[f.Name] {
					fn(f.Name, fv.String())
				}
				continue
			}
			// A verb LIST field ([]string), type-qualified so item targeting keywords aren't caught: each
			// element is a single-token verb (#66 F1).
			if refListFields[t.Name()+"."+f.Name] && (fv.Kind() == reflect.Slice || fv.Kind() == reflect.Array) &&
				fv.Type().Elem().Kind() == reflect.String {
				for j := 0; j < fv.Len(); j++ {
					fn(f.Name, fv.Index(j).String())
				}
				continue
			}
			walkRefFields(fv, fn)
		}
	}
}

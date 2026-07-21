package world

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/logcap"
)

// logcap_content_test.go — #481: a malformed content def carrying a huge builder-controlled field must
// produce a BOUNDED parse-error log line, not a verbatim echo of the field (the log-poisoning /
// disk-fill class #456 closed for builder Lua, here via the content-load channel).

// boundedFieldMax is the largest a single builder value may occupy in a diagnostic: the shared logcap
// bound plus its truncation marker. Every assertion below holds the ECHOED value under this.
const boundedFieldMax = logcap.MaxValueBytes + len(logcap.TruncationMarker)

// hugeField is a ~200KB builder value — the concrete threat the issue names. Non-'d' fill so the dice
// parser fails at the very first split (exercising the error that embeds the raw string).
var hugeField = strings.Repeat("A", 200*1024)

func TestParseDiceErrorBounded(t *testing.T) {
	_, _, err := parseDice(hugeField)
	if err == nil {
		t.Fatal("expected a parse error for a 200KB dice string")
	}
	if len(err.Error()) > boundedFieldMax+64 { // +64 for the fixed "dice %q: ..." wrapper
		t.Errorf("parseDice error not bounded: %d bytes (echoed the raw field)", len(err.Error()))
	}
}

func TestParseDiceSpecErrorBounded(t *testing.T) {
	_, err := parseDiceSpec(hugeField)
	if err == nil {
		t.Fatal("expected a parse error for a 200KB dice spec")
	}
	if len(err.Error()) > boundedFieldMax+64 {
		t.Errorf("parseDiceSpec error not bounded: %d bytes", len(err.Error()))
	}
}

func TestParseFormulaHeadErrorBounded(t *testing.T) {
	// A formula array whose head is a huge unknown token → "formula: unknown head %q".
	_, err := parseFormula([]any{hugeField})
	if err == nil {
		t.Fatal("expected a parse error for a huge formula head")
	}
	if len(err.Error()) > boundedFieldMax+64 {
		t.Errorf("parseFormula head error not bounded: %d bytes", len(err.Error()))
	}
}

func TestParseFormulaUnexpectedNodeBounded(t *testing.T) {
	// A bare string node is not a valid formula → "formula: unexpected node %T (%v)"; the %v must be
	// capped so a huge string node cannot blow up the diagnostic.
	_, err := parseFormula(hugeField)
	if err == nil {
		t.Fatal("expected a parse error for a huge string node")
	}
	if len(err.Error()) > boundedFieldMax+64 {
		t.Errorf("parseFormula node error not bounded: %d bytes", len(err.Error()))
	}
}

// TestBuildLogLineBounded drives the REAL build path (defineGlobals over a LoadedContent with a
// malformed ability) and captures the emitted slog record, proving the whole logged line — not just
// the isolated parser error — is bounded. This is the issue's acceptance criterion.
func TestBuildLogLineBounded(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	lc := &content.LoadedContent{
		Abilities: []content.AbilityDTO{{
			Ref:        "test:ability:huge-dice",
			Invocation: "proc",
			// A deal_damage op whose dice field is 200KB — parseOpList → parseOp → parseDice fails and
			// build.go logs it with "err", err.
			OnResolve: []any{map[string]any{"op": "deal_damage", "dice": hugeField}},
		}},
	}
	defineGlobals(newDefRegistries(), lc)

	out := buf.String()
	if out == "" {
		t.Fatal("expected a parse-error log record; got none")
	}
	// The captured log MUST NOT carry a verbatim run of the 200KB field.
	if strings.Contains(out, strings.Repeat("A", boundedFieldMax+1)) {
		t.Errorf("log line echoed the raw field uncapped (found a run > %d bytes)", boundedFieldMax)
	}
	// The whole record stays small: the bounded err value + fixed JSON scaffolding + the (short) ref.
	if len(out) > boundedFieldMax+512 {
		t.Errorf("log record not bounded: %d bytes", len(out))
	}
}

// TestReloadProblemsBounded covers the reload/boot gate — the primary untrusted vector. A 200KB
// REFERENCE-VALUED ref (an exit target) is deliberately chosen: it is NOT walked by the identity-token
// ref-charset lint, so it can only be bounded at the problems funnel (capProblems). Every problem string
// validatePacks returns — the slice that rides into the reject/boot log sinks AND into the returned
// reloadOutcome.rejected — must be bounded.
func TestReloadProblemsBounded(t *testing.T) {
	pack := content.Pack{
		Pack: "a",
		Zones: []content.ZoneDTO{{Ref: "az", Rooms: []content.RoomDTO{
			// A valid room whose exit targets a 200KB unknown room ref → validateRoomExits emits a
			// problem string embedding the huge (reference-valued) target verbatim.
			r205room("az:room:1", map[string]string{"north": "az:room:" + hugeField}),
		}}},
	}
	problems := validatePacks([]content.Pack{pack}, map[string]bool{"a": true})
	if len(problems) == 0 {
		t.Fatal("expected a dangling-exit rejection for the huge unknown target")
	}
	for i, p := range problems {
		if len(p) > boundedFieldMax {
			t.Errorf("problem[%d] not bounded: %d bytes (echoed the raw ref target)", i, len(p))
		}
	}
}

// TestReloadAdvisoryBounded covers the #309 NON-BLOCKING advisory channel — the second sink the final
// review found bypassing the reject funnel. An out-of-scope zone's exit that still references a 200KB
// (reference-valued) ref the reload removes rides the advisory Warn log + the operator readout; it must
// be bounded like a reject problem.
func TestReloadAdvisoryBounded(t *testing.T) {
	removed := "az:room:" + hugeField // the huge ref A's reload removes; B still points at it
	packA := content.Pack{
		Pack:  "a",
		Zones: []content.ZoneDTO{{Ref: "az", Rooms: []content.RoomDTO{r205room("az:room:1", nil)}}},
	}
	packB := content.Pack{
		Pack: "b",
		Zones: []content.ZoneDTO{{Ref: "bz", Rooms: []content.RoomDTO{
			r205room("bz:room:9", map[string]string{"east": removed}),
		}}},
	}
	scope := newReloadScope([]content.Pack{packA, packB}, map[string]bool{"a": true})

	adv := advisoryReloadRemovals(scope, livingSet(removed, "az:room:1", "bz:room:9"))
	if len(adv) == 0 {
		t.Fatal("expected an advisory for the removed huge-ref room B depends on")
	}
	for i, a := range adv {
		if len(a) > boundedFieldMax {
			t.Errorf("advisory[%d] not bounded: %d bytes (echoed the raw ref target)", i, len(a))
		}
	}
}

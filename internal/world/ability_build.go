package world

// ability_build.go is the DTO -> runtime builder for abilities + the op-list PARSER (docs/PHASE5-PLAN.md
// §2.2). It mirrors buildAffectDef (content_map.go): the content package owns the on-disk DTO; this
// file owns the explicit translation onto the runtime abilityDef + the typed effectOp tree the
// interpreter walks. Build-time only (defineGlobals), never on the hot path.
//
// The op-list is a generic decoded JSON/YAML value (any) — a list of op objects — parsed ONCE here
// into []effectOp so step 8 is a kind switch, never a re-parse. The shape of one op:
//
//	{op: deal_damage, type: fire, dice: "8d6"}            // or {amount: 30}
//	{op: heal, resource: hp, amount: 20}
//	{op: apply_affect, affect: poison, harmful: true}
//	{op: chance, prob: 0.5, then: [ ... ]}
//	{op: if, has_affect: poison, then: [ ... ], else: [ ... ]}
//
// An unparseable op is logged + skipped (content-lint is the real gate, exactly like a malformed
// reset op or attribute formula).

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/double-nibble/telosmud/internal/content"
)

// buildAbilityDef maps an AbilityDTO onto the runtime abilityDef. It parses the target mode +
// disposition, the requires/costs, the tags, and — the load-bearing part — the on_resolve op-list into
// a typed effectOp tree. on_resolve_lua is carried but NEVER executed (reserved Phase 7). Build-time
// only. Returns the def + any op-list parse error (the caller logs it; the def still registers with
// whatever ops parsed, so a partial bad list never aborts boot).
func buildAbilityDef(a content.AbilityDTO) (*abilityDef, error) {
	def := &abilityDef{
		ref:          a.Ref,
		name:         a.Name,
		invocation:   a.Invocation,
		words:        append([]string(nil), a.Words...),
		mode:         parseTargetMode(a.Targeting.Mode),
		disposition:  parseDisposition(a.Targeting.Disposition),
		tags:         append([]string(nil), a.Tags...),
		notPrevented: append([]string(nil), a.Requires.NotPrevented...),
		castTime:     a.CastTime,
		lag:          a.Lag,
		cooldown:     a.Cooldown,
		onResolveLua: a.OnResolveLua, // READ-NOT-RUN (Phase 7)
		msgActor:     a.Messages.Actor,
		msgRoom:      a.Messages.Room,
	}
	if len(a.Requires.Attr) > 0 {
		def.reqAttr = make(map[string]float64, len(a.Requires.Attr))
		for k, v := range a.Requires.Attr {
			def.reqAttr[k] = v
		}
	}
	for _, cost := range a.Costs {
		def.costs = append(def.costs, resourceCost{resource: cost.Resource, amount: cost.Amount})
	}
	ops, err := parseOpList(a.OnResolve)
	if err != nil {
		return def, fmt.Errorf("ability %s on_resolve: %w", a.Ref, err)
	}
	def.ops = ops
	def.onEvent = parseEventMap(a.OnEvent, "ability "+a.Ref)
	return def, nil
}

// parseEventMap parses a content `on_event` map ({"OnHit": [ops...], "OnKill": [ops...]}) into the
// runtime subscription table ([G3], event.go). Keys are validated against the closed engine event set
// (an unknown kind is dropped + lint-logged — content can't invent events). A bad op-list logs and
// carries whatever parsed. nil/empty in => nil out (no subscriptions). Build-time only; `owner` names
// the def for diagnostics.
func parseEventMap(v any, owner string) map[eventKind][]effectOp {
	if v == nil {
		return nil
	}
	m, ok := asMap(v)
	if !ok {
		slog.Error("content: on_event must be a map of event->ops", "def", owner)
		return nil
	}
	var out map[eventKind][]effectOp
	for key, raw := range m {
		kind := eventKind(key)
		if !knownEventKinds[kind] {
			slog.Error("content: on_event references unknown engine event (dropped)", "def", owner, "event", key)
			continue
		}
		ops, err := parseOpList(raw)
		if err != nil {
			slog.Error("content: on_event op-list parse failed", "def", owner, "event", key, "err", err)
		}
		if len(ops) == 0 {
			continue
		}
		if out == nil {
			out = map[eventKind][]effectOp{}
		}
		out[kind] = ops
	}
	return out
}

// parseOpList parses a generic decoded op-list (any -> []effectOp). nil/empty parses to nil (no ops).
// It accepts a top-level list of op maps; each map is parsed by parseOp. This is the same shape an
// affect's on_tick uses (a DoT's [deal_damage]). Build-time only.
func parseOpList(v any) ([]effectOp, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := asList(v)
	if !ok {
		return nil, fmt.Errorf("op-list must be a list, got %T", v)
	}
	var ops []effectOp
	for i, raw := range list {
		op, err := parseOp(raw)
		if err != nil {
			return ops, fmt.Errorf("op[%d]: %w", i, err)
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// parseOp parses one decoded op object into an effectOp. The "op" key names the kind; the rest are the
// argument bag (different ops read different fields). Nested "then"/"else" recurse via parseOpList.
func parseOp(v any) (effectOp, error) {
	m, ok := asMap(v)
	if !ok {
		return effectOp{}, fmt.Errorf("op must be an object, got %T", v)
	}
	op := effectOp{kind: mapStr(m, "op")}
	if op.kind == "" {
		return effectOp{}, fmt.Errorf("op missing 'op' kind")
	}
	op.resource = mapStr(m, "resource")
	op.affect = firstStr(m, "affect", "id")
	op.dmgType = firstStr(m, "type", "damage_type")
	op.amount = mapFloat(m, "amount")
	op.duration = int(mapFloat(m, "duration"))
	op.magnitude = mapFloat(m, "magnitude")
	op.prob = firstFloat(m, "prob", "chance", "p")
	op.text = firstStr(m, "text", "template", "message")
	op.to = mapStr(m, "to")
	op.harmful = mapBool(m, "harmful")
	op.tgt = mapStr(m, "target") // event-handler target selector: "self" | "other" (else ctx.target)
	// `if` reads its condition affect under has_affect (falls back to affect/id above).
	if has := firstStr(m, "has_affect"); has != "" {
		op.affect = has
	}
	// Dice: "<N>d<S>" form for deal_damage; or explicit dice_num/dice_size.
	if dice := mapStr(m, "dice"); dice != "" {
		n, s, err := parseDice(dice)
		if err != nil {
			return effectOp{}, err
		}
		op.diceNum, op.diceSize = n, s
	} else {
		op.diceNum = int(mapFloat(m, "dice_num"))
		op.diceSize = int(mapFloat(m, "dice_size"))
	}
	// Nested branches for flow ops.
	if t, present := m["then"]; present {
		sub, err := parseOpList(t)
		if err != nil {
			return effectOp{}, fmt.Errorf("then: %w", err)
		}
		op.then = sub
	}
	if e, present := m["else"]; present {
		sub, err := parseOpList(e)
		if err != nil {
			return effectOp{}, fmt.Errorf("else: %w", err)
		}
		op.els = sub
	}
	// The `check` flow op carries its spec (dice/bonus/vs/bands) under a nested `check` object, kept
	// separate from deal_damage's top-level `dice` so the richer notation never entangles the two.
	if op.kind == "check" {
		spec, err := parseCheckSpec(m["check"])
		if err != nil {
			return effectOp{}, fmt.Errorf("check: %w", err)
		}
		op.check = spec
	}
	return op, nil
}

// parseCheckSpec parses a decoded check object (check.go [G2]) into a typed checkSpec. Build-time only.
// Shape:
//
//	check:
//	  label: "Dexterity save"
//	  dice: "1d20"
//	  bonus: ["attr", "$target.dex_save"]
//	  vs: ["attr", "$source.spell_dc"]        # or a literal 15, or {contested: <spec>}
//	  visibility: hide                        # hide | show | summary (default: hide)
//	  bands:
//	    - { margin_min: 0, label: success, ops: [ ... ] }   # made the save
//	    - { label: failure, ops: [ ... ] }                  # default (no test)
func parseCheckSpec(v any) (*checkSpec, error) {
	m, ok := asMap(v)
	if !ok {
		return nil, fmt.Errorf("check spec must be an object, got %T", v)
	}
	spec := &checkSpec{label: mapStr(m, "label"), visibility: parseVisibility(mapStr(m, "visibility"))}

	dice, err := parseDiceSpec(mapStr(m, "dice"))
	if err != nil {
		return nil, err
	}
	spec.dice = dice

	if b, present := m["bonus"]; present {
		node, err := parseFormula(b)
		if err != nil {
			return nil, fmt.Errorf("bonus: %w", err)
		}
		spec.bonus = node
	}

	vs, err := parseCheckVs(m["vs"])
	if err != nil {
		return nil, fmt.Errorf("vs: %w", err)
	}
	spec.vs = vs

	bandsRaw, ok := asList(m["bands"])
	if !ok {
		return nil, fmt.Errorf("bands must be a list")
	}
	for i, raw := range bandsRaw {
		band, err := parseCheckBand(raw)
		if err != nil {
			return nil, fmt.Errorf("band[%d]: %w", i, err)
		}
		spec.bands = append(spec.bands, band)
	}
	return spec, nil
}

// parseCheckVs parses the `vs` term: a {contested: <spec>} object, else a literal/formula DC. A nil
// vs (absent) is a pure-total check (bands test the total directly).
func parseCheckVs(v any) (checkVs, error) {
	if v == nil {
		return checkVs{}, nil
	}
	if m, ok := asMap(v); ok {
		if cv, present := m["contested"]; present {
			sub, err := parseCheckSpec(cv)
			if err != nil {
				return checkVs{}, fmt.Errorf("contested: %w", err)
			}
			return checkVs{contested: sub}, nil
		}
	}
	node, err := parseFormula(v)
	if err != nil {
		return checkVs{}, err
	}
	return checkVs{dc: node}, nil
}

// parseCheckBand parses one ordered outcome band. Numeric bounds (min/max/margin_min/margin_max) are
// FORMULAS (a bare number is a literal node), absent => nil (unbounded on that side); face_eq/face_count
// are the natural-face test.
func parseCheckBand(v any) (checkBand, error) {
	m, ok := asMap(v)
	if !ok {
		return checkBand{}, fmt.Errorf("band must be an object, got %T", v)
	}
	band := checkBand{
		label:     mapStr(m, "label"),
		faceEq:    mapFloatPtr(m, "face_eq"),
		faceCount: int(mapFloat(m, "face_count")),
	}
	for _, e := range []struct {
		key string
		dst *formulaNode
	}{
		{"min", &band.min}, {"max", &band.max}, {"margin_min", &band.marginMin}, {"margin_max", &band.marginMax},
	} {
		if raw, present := m[e.key]; present {
			node, err := parseFormula(raw)
			if err != nil {
				return checkBand{}, fmt.Errorf("%s: %w", e.key, err)
			}
			*e.dst = node
		}
	}
	ops, err := parseOpList(m["ops"])
	if err != nil {
		return checkBand{}, fmt.Errorf("ops: %w", err)
	}
	band.ops = ops
	return band, nil
}

// parseVisibility maps a content string to a checkVisibility ("" / unknown -> visInherit, the
// engine-default-hide case).
func parseVisibility(s string) checkVisibility {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "hide":
		return visHide
	case "show":
		return visShow
	case "summary":
		return visSummary
	default:
		return visInherit
	}
}

// mapFloatPtr returns a pointer to the float at key, or nil if the key is absent (so a band can
// distinguish "no lower bound" from "lower bound 0").
func mapFloatPtr(m map[string]any, key string) *float64 {
	if _, ok := m[key]; !ok {
		return nil
	}
	f := mapFloat(m, key)
	return &f
}

// maxDice bounds both the dice COUNT and the dice SIZE a "<N>d<S>" spec may declare. rollDice loops
// diceNum times on the zone goroutine, so an unbounded "999999d6" (or a huge size) would spin the
// heartbeat; the cap keeps a runaway content spec from starving the single-writer loop. A few hundred
// is far past any sane skill and still cheap to roll.
const maxDice = 500

// parseDice parses a "<N>d<S>" dice string into (num, size). "8d6" -> (8,6). "d6" -> (1,6). Both the
// count and the size are capped at maxDice so a runaway spec can't spin the zone goroutine.
func parseDice(s string) (int, int, error) {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(s)), "d", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("dice %q: want <N>d<S>", s)
	}
	num := 1
	if parts[0] != "" {
		n, err := strconv.Atoi(parts[0])
		if err != nil || n < 0 {
			return 0, 0, fmt.Errorf("dice %q: bad count", s)
		}
		num = n
	}
	size, err := strconv.Atoi(parts[1])
	if err != nil || size <= 0 {
		return 0, 0, fmt.Errorf("dice %q: bad size", s)
	}
	if num > maxDice {
		num = maxDice
	}
	if size > maxDice {
		size = maxDice
	}
	return num, size, nil
}

// --- generic decoded-value helpers (YAML/JSON give map[string]any or map[any]any) ---------------

// asList coerces a decoded value to a []any (a list). YAML lists decode as []any.
func asList(v any) ([]any, bool) {
	l, ok := v.([]any)
	return l, ok
}

// asMap coerces a decoded value to a string-keyed map. YAML may give map[string]any (json-style) or
// map[any]any (older yaml); normalize both.
func asMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			if ks, ok := k.(string); ok {
				out[ks] = val
			}
		}
		return out, true
	default:
		return nil, false
	}
}

func mapStr(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := mapStr(m, k); s != "" {
			return s
		}
	}
	return ""
}

func mapFloat(m map[string]any, key string) float64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

func firstFloat(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return mapFloat(m, k)
		}
	}
	return 0
}

func mapBool(m map[string]any, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// lintAbilityOps is a placeholder content-lint hook for the op vocabulary (an op-list that references
// an unregistered op kind). It logs unknown kinds at build time so a content author sees them before
// runtime (where runOps skips them). Returns the count of unknown ops found.
func lintAbilityOps(ref string, ops []effectOp) int {
	unknown := 0
	var walk func([]effectOp)
	walk = func(list []effectOp) {
		for i := range list {
			op := &list[i]
			if _, ok := effectOpHandlers[op.kind]; !ok {
				slog.Error("content: ability references unknown effect op", "ability", ref, "op", op.kind)
				unknown++
			}
			walk(op.then)
			walk(op.els)
			if op.check != nil {
				for i := range op.check.bands {
					walk(op.check.bands[i].ops)
				}
			}
		}
	}
	walk(ops)
	return unknown
}

// lintEventMap runs the op-vocabulary lint over each on_event handler op-list ([G3]) so an unknown op
// inside a resource/affect/ability subscription is caught at LOAD (like ability on_resolve), not just
// warn-skipped at runtime. `owner` names the def. Returns the count of unknown ops.
func lintEventMap(owner string, onEvent map[eventKind][]effectOp) int {
	unknown := 0
	for kind, ops := range onEvent {
		unknown += lintAbilityOps(owner+" on_event["+string(kind)+"]", ops)
	}
	return unknown
}

package world

// scripted.go — the Scripted component + the per-entity trigger machinery (slice 7.4c). A
// scripted mob/room/item carries a Lua SOURCE (the `lua` content block) that, run ONCE per
// spawned instance, registers `on(event, fn)` triggers and seeds `self.state`. The engine then
// fires those triggers at lifecycle points (enter/leave/speech/greet/death/…).
//
// Split of state (the why):
//   - Scripted (here) is PROTOTYPE-SHARED + IMMUTABLE: just the source string. Has[*Scripted]
//     answers "is this entity scripted?" and is the flyweight-shared component (every goblin
//     instance shares the one source).
//   - The per-INSTANCE runtime state (the registered handler table + the self.state table) is
//     NOT a component — it holds Lua values bound to a specific zone's LState, so it lives in the
//     per-zone runtime's entityScripts map keyed by RuntimeID (luaentry_triggers.go), built
//     lazily on first fire. This keeps the prototype/component layer free of zone-bound Lua
//     values (the no-Lua-in-the-data-model discipline).

// Scripted is the immutable, prototype-shared script source for an entity. Present iff the
// content `lua` block was non-empty. Read-only after construction (the source is shared across
// every instance of the prototype; the per-instance registered handlers + self.state live in the
// runtime, not here).
type Scripted struct {
	source string // the Lua `lua` block: registers on(event,fn) triggers + seeds self.state
}

func (*Scripted) componentKind() Kind { return KindScripted }

// scriptSource returns the entity's Lua trigger-block source, or "" if it carries no Scripted
// component (a pure-data entity). Read-only.
func scriptSource(e *Entity) string {
	if s, ok := Get[*Scripted](e); ok {
		return s.source
	}
	return ""
}

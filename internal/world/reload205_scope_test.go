package world

import "github.com/double-nibble/telosmud/internal/content"

// reload205_scope_test.go — test shims for the #205 provenance-aware validators. The pre-#205 tests exercise
// the checks over a pack set where EVERYTHING is in scope (the bare-`reload` equivalent, where every finding
// is rejectable). These thin wrappers preserve those tests verbatim by scoping all loaded packs; the #205
// provenance behaviour (a scoped subset with an out-of-scope broken pack) is covered by dedicated tests.

// packNames returns every loaded pack's name as an all-in-scope set (the bare-reload equivalent).
func packNames(loaded []content.Pack) map[string]bool {
	scoped := map[string]bool{}
	for i := range loaded {
		scoped[loaded[i].Pack] = true
	}
	return scoped
}

// scopeAll builds a reloadScope over `loaded` with every pack in scope.
func scopeAll(loaded []content.Pack) *reloadScope { return newReloadScope(loaded, packNames(loaded)) }

func vPacks(loaded []content.Pack) []string     { return validatePacks(loaded, packNames(loaded)) }
func vRoomExits(loaded []content.Pack) []string { return validateRoomExits(scopeAll(loaded)) }
func vResets(loaded []content.Pack) []string    { return validateResets(scopeAll(loaded)) }
func vProtoRefs(loaded []content.Pack) []string { return validateProtoRefs(scopeAll(loaded)) }
func vChannels(loaded []content.Pack) []string  { return validateChannels(scopeAll(loaded)) }

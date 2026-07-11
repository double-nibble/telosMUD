//go:build !race

package luasandbox

// CallDeadline is the per-call wall-clock Lua deadline in MILLISECONDS for a normal (non-race) build — the
// production value. See the note in luasandbox.go: the 100k instruction budget is the primary per-call
// bound; this deadline is the secondary guard against a low-instruction stall (a GC pause / host load).
const CallDeadline = 5

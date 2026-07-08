package contentbus

// pullresult.go carries the director → fleet PULL-RESULT contract (#230). A coordinated `pull <version>`
// acks the builder OPTIMISTICALLY ("the director will install…") and then runs the up-to-5-min
// git+import+broadcast OFF the actor goroutine, so the outcome — success, a validation/import failure, a
// single-flight drop, or a prune-guard refusal — was previously visible only in the director's log. That
// is a real hole for a staff-triggered FLEET mutation. This closes it: when the pull settles, the world
// director broadcasts a PullResult DOWN on the world scope (transient, best-effort — a lost notice is not
// a correctness problem, the import already committed or didn't). Every shard receives it; the one that
// still hosts the requesting builder surfaces the pass/fail line to them (world/scope.go), exactly as
// `reload` reports its own fan-out outcome. It rides the same world-scope down-broadcast path the director
// uses for remote effects, so no new transport is introduced.
//
// The notice is BEST-EFFORT, and `pull` remains re-runnable (idempotent by content SHA), so two boundary
// cases are accepted rather than engineered away: (a) a demoted-then-promoted leader pair can each run the
// (SHA-collapsed) import and each broadcast, so the builder may rarely see a DUPLICATE line — or, if the
// two runs diverge transiently, a fail+success pair; (b) a director that SHUTS DOWN mid-pull cancels the
// in-flight import (atomic rollback) AND suppresses the notice, so a `pull` issued into a restart may
// produce no readout — the builder re-runs it. Neither is a correctness problem: the notice carries no
// state, only the import (committed-or-not) does.

// PullResultEvent is the scoped remote-effect event the world director broadcasts DOWN when a coordinated
// pull settles. Distinct from any on_world content event — the shard consumes it for operator feedback,
// not the Lua on_world path.
const PullResultEvent = "content.pull.result"

// PullResult is the outcome payload: the version that was requested, the builder to notify (the character
// id that ran `pull`), whether it succeeded, and a short human detail (the failure/refusal reason, empty
// on success). Marshaled into the world-scope down-broadcast.
type PullResult struct {
	Version string `json:"version"` // the published content version the builder asked to install
	Actor   string `json:"actor"`   // the builder character id to deliver the outcome to
	OK      bool   `json:"ok"`      // true => installed + hot-reloaded; false => failed/refused/dropped
	Detail  string `json:"detail,omitempty"`
}

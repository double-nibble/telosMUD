package contentbus

// pullrequest.go carries the director-coordinated content-PULL contract (#212 slice 4 PR E). A staff
// `pull <version>` command fires a DURABLE signal UP to the world director, which — as the single fleet
// leader — resolves the published version from the external git store, imports it atomically into
// Postgres, and broadcasts the hot-reload. Routing the pull through the director (vs. an uncoordinated
// telos-pull) is what lets it gate the one thing telos-pull cannot: refusing to hot-strip a pack players
// are currently in (the live-hosted-pack rolling-reboot guard, a later slice).
//
// It rides the SAME scoped signal-up spine as the reload audit (world/scope.go enqueueSignal →
// SignalDurable, consumed by the world director's durable JetStream consumer). The DELIVERY guarantee is
// at-least-once TO THE LEADER: the director gates on leadership before acking, so a request queued on a
// director that then loses the lease is NAK'd and redelivered to the newly-promoted leader rather than
// consumed-and-dropped. Dedup is layered (JetStream ack cursor + the director's in-memory per-source
// high-water). Redelivery is safe because the import (ImportVersion) is IDEMPOTENT by content SHA: a
// re-run of an unchanged SHA is a no-op (no version bump, no re-broadcast).
//
// EXECUTION is best-effort, NOT durably retried: the leader acks the request as soon as it accepts it, then
// runs the up-to-5-min git+import+broadcast OFF the actor goroutine (it cannot hold a durable ack open that
// long). So a leader crash AFTER accepting but DURING the pull loses that execution with no auto-retry — the
// builder re-runs `pull`, which is safe (idempotent by SHA). Concurrent imports (a stale leader racing the
// promoted one) converge via the content_version row lock + SHA-idempotency and the shard-side monotonic,
// sentinel-gated applied-version guard — safety by construction, not by leader-exclusivity (decision E).

// PullRequestEvent is the scoped signal-up event name the shard emits and the world director acts on.
const PullRequestEvent = "content.pull.request"

// PullRequest is the coordinated-pull payload: the published version to install and who asked. Marshaled
// by the shard's `pull` command; consumed by the world director's signal path, which runs the import.
type PullRequest struct {
	Version  string `json:"version"` // the published content version (a git tag/SHA) to install
	Actor    string `json:"actor"`   // the builder character id who ran `pull`
	AtUnixMs int64  `json:"at"`      // wall-clock ms when the request was issued
	// Force asks the director to OVERRIDE the live-hosted-pack prune guard (#427). The guard is explicitly
	// advisory (see contentpull/guard.go), and an advisory check with no override is really a veto: since
	// #416 taught it about instance templates, one idle player inside a dungeon copy blocks every content
	// deploy that would prune any pack, with nothing an operator can do but wait them out.
	//
	// omitempty, and false is the SAFE default, so the wire stays compatible in the direction that matters:
	// an older shard that does not know the field emits no `force` key and the director decodes false —
	// never an accidental override. The shard-side ADMIN capability gate is the only check on this value
	// (the pull signal is not signed the way handoff Commit/Abort is, #314); that is the same trust model
	// `pull` already has — any shard can already request any version — so this widens an existing surface
	// rather than opening a new one.
	Force bool `json:"force,omitempty"`
}

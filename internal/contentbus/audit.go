package contentbus

// audit.go carries the content-reload AUDIT contract (#192 S3 — the director-side advisory audit). After a
// staff `reload` propagates content across the fleet, the triggering shard fires a fire-and-forget DURABLE
// signal UP to the world director, which records who/what/when. It is defense-in-depth accountability, not
// a correctness path: the per-shard applier already fails safe on an unbuildable ref, and the pre-publish
// gate (#192 S1/S2) already blocks definitively-broken content. The audit gives ONE central record of
// every fleet content change — honoring the settled design's "director-side" framing WITHOUT the
// director-routing dependency it deliberately rejected (reload stays shard-published and never-fatal; the
// audit signal is best-effort and a shard with no scoped bus simply emits nothing).
//
// The event rides the existing scoped signal-up path (world/scope.go enqueueSignal → SignalDurable),
// consumed by the world director's durable JetStream consumer, so it survives a broker blip and a director
// restart. Dedup is LAYERED: the JetStream durable-consumer ack cursor gives cross-restart/failover
// apply-once for records already acked, and the director's in-memory per-source high-water suppresses
// in-session redelivery. Both degrade to AT-LEAST-ONCE for a record in flight (unacked) at a
// leadership-failover boundary — an acceptable trade for an append-only audit (never LOSE a record; a rare
// duplicate is harmless and timestamp/seq-distinguishable).

// ReloadAuditEvent is the scoped signal-up event name the shard emits and the world director records.
const ReloadAuditEvent = "content.reload.audit"

// ReloadAudit is the audit payload: who ran `reload`, which packs, the outcome, how many definitions were
// pushed, and when. Marshaled by the shard's reload command; recorded by the world director's signal path.
type ReloadAudit struct {
	Actor     string   `json:"actor"`     // the builder character id who ran reload
	Packs     []string `json:"packs"`     // the packs propagated (or attempted, on a rejection)
	Published int      `json:"published"` // definitions pushed to the fleet (0 on a rejection/total failure)
	Outcome   string   `json:"outcome"`   // propagated | partial | failed | rejected
	AtUnixMs  int64    `json:"at"`        // wall-clock ms when the reload finished
}

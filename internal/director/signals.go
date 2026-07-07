package director

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/double-nibble/telosmud/internal/contentbus"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// signals.go is the director's ORCHESTRATION I/O (docs/WORLD-EVENTS.md §2/§4, Phase 10.4): it CONSUMES
// the signal-up events its zones emit (durable, at-least-once) and lets the director's logic WRITE scope
// state + BROADCAST consequences DOWN. The golden rule holds structurally — a director never reaches into
// a zone; it applies its state (single writer) and signals over the bus, and each zone applies the
// consequence locally. The decision logic is a SignalHandler (the "director script"), invoked on the
// director goroutine so a handler's Set/Broadcast are single-writer and need no lock.

// signalAckTimeout bounds how long the bus's durable-consumer goroutine waits for the actor to ack an
// applied signal, so a wedged actor NAKs (redelivered) rather than blocking the consumer forever.
const signalAckTimeout = 5 * time.Second

// SignalHandler is a director's orchestration logic: invoked once per signal-up event from a zone, ON the
// director goroutine, with the API to read/write scope state + broadcast remote effects. event is the
// script-named event ("boss_slain"); payload is its data. A nil handler means the director records state
// only via its Get/Set API (no event reaction) — the events are still acked (drained), not stuck.
type SignalHandler func(api *API, event string, payload json.RawMessage)

// API is the director-script surface, valid ONLY during a SignalHandler call (it closes over the actor
// goroutine + the call's ctx). Set persists + broadcasts a state change down; Broadcast emits a remote
// effect (a custom event a zone reacts to); Get reads the scope's current state.
type API struct {
	d   *Director
	ctx context.Context
}

// Get returns the current value for key in this director's scope (nil + found=false when unset).
func (a *API) Get(key string) (json.RawMessage, bool) {
	r := a.d.get(a.ctx, key)
	return r.value, r.found
}

// Set writes key=value to this director's authoritative scope state (persisted via the optimistic-
// concurrency CAS) and BROADCASTS the change DOWN so member zones' read-replicas update (world.flag /
// region:get). A nil value deletes the key. A CAS loss surfaces as an error (a failover race); the
// handler may retry. This is the single sanctioned write path — the director is the only writer.
func (a *API) Set(key string, value json.RawMessage) error {
	if err := a.d.set(a.ctx, key, value); err != nil {
		return err
	}
	a.d.broadcastStateDown(a.ctx, key, value)
	return nil
}

// Broadcast emits a REMOTE EFFECT: a custom event (not a state set) on this director's scope, which member
// zones react to via on_world/on_region Lua handlers (10.4b) — e.g. the director commanding a wave of
// zones to spawn an invasion boss. Fire-and-forget (transient); a state change a zone must durably observe
// goes through Set instead.
func (a *API) Broadcast(event string, payload json.RawMessage) {
	a.d.broadcastDown(a.ctx, event, payload)
}

// WithScopeBus wires the director's scoped event bus (Phase 10.4): bus carries the durable signal-up
// consume + the transient broadcast-down. source seeds the down-broadcast author id (a run-unique
// director instance id is ideal). Call before Run. A nil bus leaves the director state-only (10.1).
func (d *Director) WithScopeBus(bus *scopebus.Bus, source string) *Director {
	d.bus = bus
	d.source = source
	return d
}

// WithSignalHandler sets the orchestration logic invoked per signal-up event. Call before Run.
func (d *Director) WithSignalHandler(h SignalHandler) *Director {
	d.handler = h
	return d
}

// scope is the scope this director owns: the world, or its region.
func (d *Director) scope() scopebus.Scope {
	if d.regionID == "" {
		return scopebus.World()
	}
	return scopebus.Region(d.regionID)
}

// consumerID is this director's STABLE durable-consumer name (dot- and colon-free — real JetStream
// rejects a "." in a consumer name, and we avoid ":" too). Stable per scope so a restart RESUMES from the
// last ack rather than replaying every event (the cross-restart dedup that makes apply-once hold).
func (d *Director) consumerID() string {
	if d.regionID == "" {
		return "director-world"
	}
	return "director-region-" + strings.NewReplacer(".", "-", ":", "-").Replace(d.regionID)
}

// syncScopeSubscription binds the durable signal-up consumer to LEADERSHIP: subscribe when this director
// is the live leader and not yet subscribed; tear down when it loses leadership. Called at Run start and
// after every campaign, on the actor goroutine. A standby never consumes — exactly one live owner applies
// a scope's events (the 10.1c invariant carried into the event path).
func (d *Director) syncScopeSubscription(ctx context.Context) {
	if d.bus == nil {
		return
	}
	switch {
	case d.leader.Load() && d.consumer == nil:
		d.subscribeSignals(ctx)
	case !d.leader.Load() && d.consumer != nil:
		d.unsubscribeSignals()
	}
}

// subscribeSignals starts the durable consumer for this director's scope. Each delivered event is posted
// to the inbox (so it applies single-writer) and acked/NAK'd by the actor's reply.
func (d *Director) subscribeSignals(ctx context.Context) {
	cons, err := d.bus.SubscribeDurable(d.scope(), d.consumerID(), func(ev scopebus.DurableEvent) bool {
		ack := make(chan bool, 1)
		d.post(signalMsg{event: ev.Event, payload: ev.Payload, key: ev.Key, source: ev.Source, ack: ack})
		select {
		case ok := <-ack:
			return ok
		case <-ctx.Done():
			return false // NAK: shutting down; the event redelivers to the next leader
		case <-time.After(signalAckTimeout):
			d.log.Warn("director: signal apply timed out; NAK", "event", ev.Event)
			return false
		}
	})
	if err != nil {
		d.log.Warn("director: signal-up subscribe failed; orchestration input disabled", "err", err)
		return
	}
	d.consumer = cons
	d.log.Debug("director: signal-up consumer started", "scope", d.scope().Label(), "consumer", d.consumerID())
}

// unsubscribeSignals stops the durable consumer (on losing leadership or at loop exit). Idempotent.
func (d *Director) unsubscribeSignals() {
	if d.consumer != nil {
		_ = d.consumer.Stop()
		d.consumer = nil
	}
}

// signalMsg is one signal-up event delivered to the actor for application. ack carries the consume result
// back to the bus goroutine (true => applied/suppressed, advance; false => NAK, redeliver).
type signalMsg struct {
	event   string
	payload json.RawMessage
	key     string // "<source>:<seq>" idempotency key
	source  string
	ack     chan bool
}

func (signalMsg) directorMsg() {}

// handleSignal applies one signal-up event on the actor goroutine: dedup a redelivery (apply-once over the
// at-least-once stream), invoke the handler (which may Set/Broadcast), then advance the per-source
// high-water and ack. A handler that is not set still acks (the event is drained, not stuck).
func (d *Director) handleSignal(ctx context.Context, m signalMsg) {
	seq, hasSeq := parseSeq(m.key)
	if hasSeq && seq <= d.applied[m.source] {
		m.ack <- true // already applied this session — idempotently suppressed
		return
	}
	// Native content-reload AUDIT (#192 S3): record who/what/when for every fleet content change, deduped
	// apply-once by the high-water above. Independent of the content SignalHandler — audit is an operational
	// concern the director owns, so it holds even when no director-script is wired. A content handler (if
	// set) still sees the event below for any custom reaction.
	if m.event == contentbus.ReloadAuditEvent {
		d.recordReloadAudit(m.payload, m.source)
	}
	if d.handler != nil {
		d.handler(&API{d: d, ctx: ctx}, m.event, m.payload)
	}
	if hasSeq && seq > d.applied[m.source] {
		d.applied[m.source] = seq
	}
	m.ack <- true
}

// recordReloadAudit writes ONE structured audit-log entry for a fleet content reload (#192 S3): who ran
// it, which packs, the outcome, the definition count, and when. A malformed payload is warned and dropped
// (never fatal — the audit is best-effort accountability). Runs on the director goroutine, so it does not
// race the scope state. It logs rather than persisting scope state: an audit is an append-only operational
// record, not orchestration state a zone read-replica consumes.
func (d *Director) recordReloadAudit(payload json.RawMessage, source string) {
	var a contentbus.ReloadAudit
	if err := json.Unmarshal(payload, &a); err != nil {
		d.log.Warn("director: malformed content-reload audit payload; dropped", "err", err, "source", source)
		return
	}
	d.log.Info("content reload audit",
		"actor", a.Actor, "packs", a.Packs, "published", a.Published,
		"outcome", a.Outcome, "at_unix_ms", a.AtUnixMs, "shard", source)
}

// broadcastStateDown publishes a state delta DOWN on this director's scope (the EventStateSet contract the
// zone read-replica consumes). Transient: a live push to member zones. Runs on the actor goroutine; the
// transient publish is fast and fire-and-forget.
func (d *Director) broadcastStateDown(ctx context.Context, key string, value json.RawMessage) {
	if d.bus == nil {
		return
	}
	body, err := json.Marshal(scopebus.StatePayload{Key: key, Value: value})
	if err != nil {
		d.log.Warn("director: marshal state broadcast", "key", key, "err", err)
		return
	}
	if err := d.bus.Signal(ctx, d.scope(), scopebus.EventStateSet, body, d.source); err != nil {
		d.log.Warn("director: state broadcast-down failed", "key", key, "err", err)
	}
}

// broadcastDown publishes a custom remote-effect event DOWN on this director's scope (transient).
func (d *Director) broadcastDown(ctx context.Context, event string, payload json.RawMessage) {
	if d.bus == nil {
		return
	}
	if err := d.bus.Signal(ctx, d.scope(), event, payload, d.source); err != nil {
		d.log.Warn("director: remote-effect broadcast failed", "event", event, "err", err)
	}
}

// parseSeq splits a "<source>:<seq>" idempotency key into its trailing sequence number. Returns false for
// a key without a parseable trailing seq (then the dedup high-water is not used for that event).
func parseSeq(key string) (uint64, bool) {
	i := strings.LastIndex(key, ":")
	if i < 0 || i == len(key)-1 {
		return 0, false
	}
	n, err := strconv.ParseUint(key[i+1:], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

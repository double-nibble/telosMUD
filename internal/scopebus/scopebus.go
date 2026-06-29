// Package scopebus is the cross-zone SCOPED event bus (docs/WORLD-EVENTS.md §4, Phase 10.2): a
// game-level layer over the Phase-8 comms transport that addresses events by SCOPE — world, a region, or
// a zone — so a director can broadcast DOWN to member zones across shards and a zone can signal UP to a
// director. It is the wire under the Lua signal_*/broadcast_*/on_* surface (10.3+).
//
// The golden rule (WORLD-EVENTS intro) is structural: this bus only moves MESSAGES. A publisher never
// reaches into a subscriber — it signals a scope; each subscriber's hosting zone/director delivers it to
// its own inbox and applies the consequence locally on its own goroutine. The same philosophy as the
// cross-shard handoff and the comms bus.
//
// Addressing: ONE subject per scope (telos.scope.world / .region.<id> / .zone.<id>); the EVENT NAME +
// payload ride the message body, so a subscriber to a scope gets all that scope's events and dispatches
// by name — the channel-subject pattern, no subject-wildcard gymnastics. This slice (10.2a) is the
// TRANSIENT tier (fire-and-forget over commbus.Bus / NATS core); the DURABLE tier (JetStream, idempotent,
// ordered) is 10.2b.
package scopebus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/double-nibble/telosmud/internal/commbus"
)

// SubjectRoot namespaces every scoped-event subject, distinct from the comms namespace (telos.comms.*).
// It IS commbus.ScopeSubjectPrefix — the durable-tier JetStream stream (WORLD_EVENTS) binds telos.scope.>
// from that same const, so the publisher subject and the stream binding can never drift apart.
const SubjectRoot = commbus.ScopeSubjectPrefix

// Scope is an addressable event scope. Kind is "world", "region", or "zone"; ID is the region/zone ref
// (empty for world). It is the WORLD-EVENTS §1 scope hierarchy's two routable upper levels (region,
// world) plus the zone level a director targets with a remote effect.
type Scope struct {
	Kind string
	ID   string
}

// World is the top scope — every region/zone in the deployment.
func World() Scope { return Scope{Kind: "world"} }

// Region is the scope of one region (a named group of zones).
func Region(id string) Scope { return Scope{Kind: "region", ID: id} }

// ZoneScope is the scope of a single zone (a director's remote-effect target).
func ZoneScope(id string) Scope { return Scope{Kind: "zone", ID: id} }

// Subject is the NATS subject for this scope. It validates the id to a safe token set (the subject-
// injection guard, P8-A8 precedent): a region/zone id comes from content/config, never client text, but
// the bus refuses a malformed one rather than build a bogus subject. Returns an error for a bad scope.
func (s Scope) Subject() (string, error) {
	switch s.Kind {
	case "world":
		if s.ID != "" {
			return "", fmt.Errorf("scopebus: world scope takes no id")
		}
		return SubjectRoot + "world", nil
	case "region", "zone":
		if !validScopeID(s.ID) {
			return "", fmt.Errorf("scopebus: invalid %s id %q", s.Kind, s.ID)
		}
		return SubjectRoot + s.Kind + "." + s.ID, nil
	default:
		return "", fmt.Errorf("scopebus: unknown scope kind %q", s.Kind)
	}
}

// Label is a short human form for logging.
func (s Scope) Label() string {
	if s.ID == "" {
		return s.Kind
	}
	return s.Kind + ":" + s.ID
}

// validScopeID gates a region/zone id to a strict, subject-safe charset (alnum, '-', '_', ':') so it
// can never inject a wildcard or extra subject token. Non-empty, bounded.
func validScopeID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c == '-' || c == '_' || c == ':' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// scopeMsg is the on-the-wire body: the event name + its data-only JSON payload. Carrying the event name
// in the body (not the subject) means one subscription per scope receives every event for that scope.
type scopeMsg struct {
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Handler receives one scoped event: the event name, its payload, and the source id (the emitting
// director/zone, used for ordering/idempotency on the durable tier).
type Handler func(event string, payload json.RawMessage, source string)

// Bus is the scoped event bus over a comms transport. It has two tiers: a TRANSIENT one (Signal /
// Subscribe — fire-and-forget over commbus.Bus, for high-rate ephemeral signals) and a DURABLE one
// (SignalDurable / SubscribeDurable over commbus.JetStream, for state-changing events that must survive
// a director/zone restart). The durable tier is optional — without it the durable methods error.
type Bus struct {
	transient commbus.Bus
	durable   commbus.JetStream // nil unless WithDurable was set
	source    string            // the run-unique emitter id seeding durable idempotency keys
	seq       atomic.Uint64     // monotonic per-process counter for the "<source>:<seq>" dedup key
}

// New builds a scoped bus over a transient transport (commbus.Bus — NATS core in prod, MemBus in tests).
func New(transient commbus.Bus) *Bus { return &Bus{transient: transient} }

// WithDurable adds the durable tier. js is the JetStream transport; source is this emitter's RUN-UNIQUE
// id (a director instance id — uuid-per-process), which seeds the "<source>:<seq>" idempotency key so a
// restart's keys never collide with the prior run's (a restart mints a fresh source). Returns the bus.
func (b *Bus) WithDurable(js commbus.JetStream, source string) *Bus {
	b.durable = js
	b.source = source
	return b
}

// Signal publishes a fire-and-forget event to a scope (the transient tier). source identifies the
// emitter. A malformed scope or marshal error is returned without publishing.
func (b *Bus) Signal(ctx context.Context, scope Scope, event string, payload json.RawMessage, source string) error {
	if strings.TrimSpace(event) == "" {
		return fmt.Errorf("scopebus: empty event name")
	}
	subj, err := scope.Subject()
	if err != nil {
		return err
	}
	body, err := json.Marshal(scopeMsg{Event: event, Payload: payload})
	if err != nil {
		return err
	}
	return b.transient.Publish(ctx, subj, commbus.Message{AuthorID: source, Body: string(body)})
}

// Subscribe delivers every event published to scope to handler (on a bus-owned goroutine, serially per
// subscription — the comms-bus ordering guarantee). The returned Subscription stops delivery.
func (b *Bus) Subscribe(scope Scope, handler Handler) (commbus.Subscription, error) {
	subj, err := scope.Subject()
	if err != nil {
		return nil, err
	}
	return b.transient.Subscribe(subj, func(m commbus.Message) {
		var sm scopeMsg
		if err := json.Unmarshal([]byte(m.Body), &sm); err != nil || sm.Event == "" {
			return // a malformed scoped message is dropped, never delivered as a bogus event
		}
		handler(sm.Event, sm.Payload, m.AuthorID)
	})
}

// DurableEvent is one durably-delivered scoped event handed to a DurableHandler. Backlog is true for an
// event already stored when the subscriber STARTED (the restart catch-up — a director that was down
// replays the world events it missed), false for a live one. Key is the "<source>:<seq>" idempotency
// key: because the durable tier is at-least-once, a subscriber that APPLIES state (advances an invasion
// phase) must dedup on Key — track the highest applied (Source, seq) and suppress a redelivery.
type DurableEvent struct {
	Event   string
	Payload json.RawMessage
	Source  string
	Key     string
	Backlog bool
}

// DurableHandler receives one durable scoped event. It returns ack=true to advance past the event
// (applied, or idempotently suppressed) or ack=false to NAK it (redelivered with backoff). A handler
// that returns false on a transient failure gets the event again; one that applied it must return true.
type DurableHandler func(ev DurableEvent) bool

// SignalDurable publishes a state-changing event to a scope on the DURABLE tier: it is stored until a
// subscriber acks, so an event survives a subscriber being down (the restart-survival guarantee behind
// the 10.5 boss ripple). Deduped publish-side by a per-process "<source>:<seq>" idempotency key. Errors
// if the durable tier was not configured (WithDurable) or the scope/event is malformed.
func (b *Bus) SignalDurable(ctx context.Context, scope Scope, event string, payload json.RawMessage) error {
	if b.durable == nil {
		return fmt.Errorf("scopebus: durable tier not configured")
	}
	if strings.TrimSpace(event) == "" {
		return fmt.Errorf("scopebus: empty event name")
	}
	subj, err := scope.Subject()
	if err != nil {
		return err
	}
	body, err := json.Marshal(scopeMsg{Event: event, Payload: payload})
	if err != nil {
		return err
	}
	key := commbus.NewIdempotencyKey(b.source, b.seq.Add(1))
	return b.durable.PublishDurable(ctx, subj, commbus.Message{
		AuthorID:       b.source,
		Body:           string(body),
		IdempotencyKey: key,
	})
}

// SubscribeDurable runs a durable consumer for scope, keyed by consumerID (STABLE per subscriber so a
// restart RESUMES from the last ack, replaying the backlog it missed, rather than restarting). handler
// is called once per event — backlog first (in order), then live — on a bus-owned goroutine, and its
// ack return advances or NAKs the event. Errors if the durable tier is not configured or the scope is
// malformed.
func (b *Bus) SubscribeDurable(scope Scope, consumerID string, handler DurableHandler) (commbus.Consumer, error) {
	if b.durable == nil {
		return nil, fmt.Errorf("scopebus: durable tier not configured")
	}
	subj, err := scope.Subject()
	if err != nil {
		return nil, err
	}
	return b.durable.Consume(subj, consumerID, func(m commbus.Message, backlog bool) bool {
		var sm scopeMsg
		if err := json.Unmarshal([]byte(m.Body), &sm); err != nil || sm.Event == "" {
			return true // a malformed durable message is acked away, not redelivered forever
		}
		return handler(DurableEvent{
			Event:   sm.Event,
			Payload: sm.Payload,
			Source:  m.AuthorID,
			Key:     m.IdempotencyKey,
			Backlog: backlog,
		})
	})
}

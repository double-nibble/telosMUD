package commbus

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// nats.go is the production Bus: a thin wrapper over a NATS connection that publishes comms messages
// on a subject and delivers received ones to a handler. It is the only file importing nats.go, so the
// world/gate packages and the tests depend solely on the Bus interface — the broker is needed only
// for a live process and the gated integration test (mirrors contentbus/nats.go).
//
// Optional, never fatal: Connect returns an error if NATS is unreachable; the caller (the
// world/gate wiring) treats it as "comms disabled" and uses a Disabled() no-op bus — never a boot
// failure, exactly as hot reload degrades when NATS is down.
//
// The PUBLISH ACL (P8-A2) lives on the handle's Role, NOT in the broker: NATS itself has no notion
// of our world/gate split, so the gate handle REFUSES a chan/tell publish in-process, before the
// message can reach the wire. A deployment must therefore only ever hand a gate process a RoleGate
// handle (NewGate); the structural asymmetry is what makes the impersonation gate hold.

// connectTimeout bounds the initial NATS dial so an unreachable broker fails fast into the
// disabled-bus fallback rather than hanging boot (matches contentbus.connectTimeout).
const connectTimeout = 5 * time.Second

// NATSBus is the NATS-backed Bus. It owns the connection and closes it on Close. role is the publish
// capability (RoleWorld for a world process, RoleGate for a gate process) — the in-handle half of the
// ACL.
type NATSBus struct {
	nc   *nats.Conn
	role Role
	log  *slog.Logger
}

// NewWorld / NewGate dial url and return a role-scoped NATSBus, or an error if the broker is
// unreachable (the caller degrades to Disabled()). NewWorld returns a RoleWorld handle (the message
// source — it MAY publish chan/tell); NewGate returns a RoleGate handle (the sink — subscribe-only on
// chan/tell, its Publish on a guarded subject returns ErrPublishForbidden). The role is fixed by the
// constructor, never a mutable field — a gate process gets a gate handle and structurally cannot
// publish chan/tell (P8-A2).
func NewWorld(url string) (*NATSBus, error) { return connect(url, RoleWorld, "telos-commbus-world") }

// NewGate dials url and returns a RoleGate handle (the sink) — see NewWorld.
func NewGate(url string) (*NATSBus, error) { return connect(url, RoleGate, "telos-commbus-gate") }

func connect(url string, role Role, name string) (*NATSBus, error) {
	nc, err := nats.Connect(url,
		nats.Timeout(connectTimeout),
		nats.Name(name),
		// Reconnect quietly in the background: a NATS blip should degrade comms transiently, not
		// permanently disable it. A missed transient chan line during a blip is acceptable
		// (at-most-once); the durable path (8.5) is JetStream, which survives a reconnect.
		nats.RetryOnFailedConnect(false),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return nil, fmt.Errorf("commbus: connect %q: %w", url, err)
	}
	return &NATSBus{nc: nc, role: role, log: slog.With("component", "commbus", "role", role.String())}, nil
}

// Role reports this handle's publish capability.
func (b *NATSBus) Role() Role { return b.role }

// Available reflects the LIVE NATS connection: false during a disconnect/blip (the client reconnects
// in the background — MaxReconnects(-1)), true once (re)connected. A point-in-time read of the conn.
func (b *NATSBus) Available() bool { return b.nc != nil && b.nc.IsConnected() }

// Publish enforces the ACL (P8-A2), marshals msg, and publishes it on subj. The ACL check is FIRST
// and IN-PROCESS: a RoleGate handle publishing a chan/tell subject returns ErrPublishForbidden and
// NOTHING reaches the broker — the impersonation gate holds even though NATS has no role concept.
// msg.Subject is stamped to subj so a wildcard subscriber can dispatch on the concrete subject. A
// closed connection returns ErrBusClosed. Off any zone goroutine (the source world's publish path).
func (b *NATSBus) Publish(_ context.Context, subj string, msg Message) error {
	if b.role == RoleGate && isACLGuarded(subj) {
		return ErrPublishForbidden
	}
	if b.nc == nil || b.nc.IsClosed() {
		return ErrBusClosed
	}
	msg.Subject = subj
	data, err := msg.marshal()
	if err != nil {
		return fmt.Errorf("commbus: marshal message: %w", err)
	}
	if err := b.nc.Publish(subj, data); err != nil {
		return fmt.Errorf("commbus: publish %s: %w", subj, err)
	}
	if err := b.nc.Flush(); err != nil {
		return fmt.Errorf("commbus: flush: %w", err)
	}
	b.log.Debug("comms message published", "subject", subj, "author", msg.AuthorID, "seq", msg.Seq)
	return nil
}

// Subscribe registers handler on subj (which may be a trailing-* wildcard, e.g. telos.comms.chan.*).
// NATS delivers a subscription's messages SERIALLY on its own goroutine, so handler runs off every
// zone goroutine and a single subject preserves order to that subscriber (P8-A3). A malformed payload
// is logged and skipped (never panics the subscriber goroutine).
func (b *NATSBus) Subscribe(subj string, handler func(Message)) (Subscription, error) {
	if b.nc == nil || b.nc.IsClosed() {
		return nil, ErrBusClosed
	}
	sub, err := b.nc.Subscribe(subj, func(m *nats.Msg) {
		msg, err := unmarshalMessage(m.Data)
		if err != nil {
			b.log.Debug("dropping malformed comms message", "subject", m.Subject, "err", err)
			return
		}
		// Trust the broker's delivery subject over a (possibly absent) payload field so a wildcard
		// subscriber dispatches on the concrete subject it was delivered.
		msg.Subject = m.Subject
		handler(msg)
	})
	if err != nil {
		return nil, fmt.Errorf("commbus: subscribe %s: %w", subj, err)
	}
	b.log.Debug("subscribed to comms subject", "subject", subj)
	return &natsSub{sub: sub}, nil
}

// Close drains and closes the NATS connection. Idempotent.
func (b *NATSBus) Close() error {
	if b.nc == nil || b.nc.IsClosed() {
		return nil
	}
	b.nc.Close()
	return nil
}

// natsSub adapts a *nats.Subscription to the Subscription interface.
type natsSub struct{ sub *nats.Subscription }

func (s *natsSub) Unsubscribe() error {
	if s.sub == nil {
		return nil
	}
	return s.sub.Unsubscribe()
}

// Compile-time assertions.
var (
	_ Bus          = (*NATSBus)(nil)
	_ Subscription = (*natsSub)(nil)
)

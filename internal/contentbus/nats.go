package contentbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// nats.go is the production Bus: a thin wrapper over a NATS connection that publishes
// invalidations on Subject and delivers received ones to a handler. It is the only file that
// imports nats.go, so the world package (and the tests) depend solely on the Bus interface — the
// real broker is needed only for a live shard and the gated integration test.
//
// Optional, never fatal: Connect returns an error if NATS is unreachable; the caller (buildShard)
// logs it and runs with a nil Bus (hot reload disabled), exactly as it degrades when Redis or
// Postgres is down.

// ErrBusClosed is returned by Publish/Subscribe on a closed bus.
var ErrBusClosed = errors.New("contentbus: bus is closed")

// connectTimeout bounds the initial NATS dial so an unreachable broker fails fast into the
// disabled-bus fallback rather than hanging shard boot.
const connectTimeout = 5 * time.Second

// NATSBus is the NATS-backed Bus. It owns the connection and closes it on Close.
type NATSBus struct {
	nc  *nats.Conn
	log *slog.Logger
}

// Connect dials url and returns a NATSBus, or an error if the broker is unreachable. The caller
// treats an error as "hot reload disabled" (a nil Bus), never fatal — so a world boots without a
// broker. A short connect timeout keeps an unreachable NATS from stalling boot.
func Connect(url string) (*NATSBus, error) {
	nc, err := nats.Connect(url,
		nats.Timeout(connectTimeout),
		nats.Name("telos-contentbus"),
		// Reconnect quietly in the background: a NATS blip should not permanently disable hot
		// reload, and a missed invalidation during a blip is re-published by the next edit.
		nats.RetryOnFailedConnect(false),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return nil, fmt.Errorf("contentbus: connect %q: %w", url, err)
	}
	return &NATSBus{nc: nc, log: slog.With("component", "contentbus")}, nil
}

// Publish marshals inv and publishes it on Subject. Off any zone goroutine (the content writer).
// The ctx bounds nothing on the NATS publish itself (it is fire-and-forget) but is accepted to
// match the Bus contract and a future request/flush. A closed connection returns ErrBusClosed.
func (b *NATSBus) Publish(_ context.Context, inv Invalidation) error {
	if b.nc == nil || b.nc.IsClosed() {
		return ErrBusClosed
	}
	data, err := inv.marshal()
	if err != nil {
		return fmt.Errorf("contentbus: marshal invalidation: %w", err)
	}
	if err := b.nc.Publish(Subject, data); err != nil {
		return fmt.Errorf("contentbus: publish %s: %w", Subject, err)
	}
	// Flush so the publish is on the wire before the caller (e.g. a one-shot seed CLI) exits.
	if err := b.nc.Flush(); err != nil {
		return fmt.Errorf("contentbus: flush: %w", err)
	}
	b.log.Debug("invalidation published", "kind", inv.Kind, "ref", inv.Ref, "pack", inv.Pack)
	return nil
}

// Subscribe registers handler on Subject. NATS delivers messages for one subscription serially on
// its own goroutine, so handler runs off every zone goroutine and serial per subscription — the
// world's applier (which serializes the cache swap) needs no extra lock. A malformed payload is
// logged and skipped (never panics the subscriber goroutine).
func (b *NATSBus) Subscribe(handler func(Invalidation)) (Subscription, error) {
	if b.nc == nil || b.nc.IsClosed() {
		return nil, ErrBusClosed
	}
	sub, err := b.nc.Subscribe(Subject, func(m *nats.Msg) {
		inv, err := unmarshalInvalidation(m.Data)
		if err != nil {
			b.log.Debug("dropping malformed invalidation", "err", err)
			return
		}
		handler(inv)
	})
	if err != nil {
		return nil, fmt.Errorf("contentbus: subscribe %s: %w", Subject, err)
	}
	b.log.Debug("subscribed to content invalidations", "subject", Subject)
	return &natsSub{sub: sub}, nil
}

// OnReconnect wires cb to the NATS reconnect handler: it fires each time the connection is
// re-established after a drop. The subscription auto-resumes on reconnect, but messages published
// DURING the gap were lost (core NATS), so cb is the shard's chance to catch up (reconcile-on-join).
func (b *NATSBus) OnReconnect(cb func()) {
	if b.nc == nil {
		return
	}
	if cb == nil {
		b.nc.SetReconnectHandler(nil)
		return
	}
	b.nc.SetReconnectHandler(func(_ *nats.Conn) {
		b.log.Info("content bus reconnected; running catch-up")
		cb()
	})
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

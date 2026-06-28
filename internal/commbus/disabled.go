package commbus

import "context"

// disabled.go is the NATS-DOWN fallback (PHASE8-PLAN, the never-fatal rule): when the broker is
// unreachable, the world/gate wiring uses a DISABLED bus whose Publish/Subscribe are safe no-ops, so
// comms degrade to local-unavailable rather than crashing boot — exactly as hot reload degrades when
// NATS is down. It carries a Role only so callers can still observe which role they would have had;
// the role is irrelevant to a no-op (nothing is ever published).
//
// Why a no-op bus and not a nil interface? A nil Bus would force every call site to nil-check before
// Publish/Subscribe. The disabled bus lets the world/gate publish path stay nil-check-free: a publish
// just goes nowhere when comms are down. (The wiring helpers still RETURN the Bus interface so a
// future caller that prefers a nil check can have one; both shapes are supported.)

// disabledBus is the no-op Bus used when NATS is unreachable.
type disabledBus struct{ role Role }

// Disabled returns a no-op Bus with the given role. Publish/Subscribe never error and never deliver;
// Close is a no-op. This is the never-fatal degradation handle.
func Disabled(role Role) Bus { return disabledBus{role: role} }

func (d disabledBus) Role() Role { return d.role }

// Publish is a no-op on a disabled bus — BUT it still honors the ACL so a gate's forbidden publish is
// reported consistently whether or not the broker is up (a gate must never believe it published a
// chan/tell). It returns ErrPublishForbidden for a RoleGate chan/tell publish, nil otherwise.
func (d disabledBus) Publish(_ context.Context, subj string, _ Message) error {
	if d.role == RoleGate && isACLGuarded(subj) {
		return ErrPublishForbidden
	}
	return nil
}

// Subscribe on a disabled bus returns a no-op Subscription (never delivers); the caller needs no
// special-casing — it simply receives nothing while comms are down.
func (d disabledBus) Subscribe(_ string, _ func(Message)) (Subscription, error) {
	return disabledSub{}, nil
}

func (d disabledBus) Close() error { return nil }

// disabledSub is the no-op Subscription handed back by a disabled bus.
type disabledSub struct{}

func (disabledSub) Unsubscribe() error { return nil }

// OpenWorld / OpenGate are the optional/never-fatal wiring helpers (mirror
// cmd/telos-world/openContentBus): they dial url and, on failure, log via logf and return a Disabled
// bus of the right role so boot never fails on an unreachable broker. The caller passes a small log
// hook (so this package needs no logging policy) and gets back a ready-to-use Bus. An empty url also
// yields a Disabled bus (comms simply off) rather than a dial error.
func OpenWorld(url string, logf func(err error)) Bus { return open(url, RoleWorld, logf) }

// OpenGate is OpenWorld for a GATE process — same never-fatal degradation, the RoleGate handle.
func OpenGate(url string, logf func(err error)) Bus { return open(url, RoleGate, logf) }

func open(url string, role Role, logf func(err error)) Bus {
	if url == "" {
		if logf != nil {
			logf(nil)
		}
		return Disabled(role)
	}
	var (
		bus *NATSBus
		err error
	)
	switch role {
	case RoleGate:
		bus, err = NewGate(url)
	default:
		bus, err = NewWorld(url)
	}
	if err != nil {
		if logf != nil {
			logf(err)
		}
		return Disabled(role)
	}
	return bus
}

// Compile-time assertions.
var (
	_ Bus          = disabledBus{}
	_ Subscription = disabledSub{}
)

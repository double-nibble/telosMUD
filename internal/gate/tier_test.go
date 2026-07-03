package gate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/telnet"
)

// tier_test.go — the edge-local promote/demote verbs (#27): parsed at the gate, delegated to the account
// service (authz enforced there), and NOT forwarded to the world.

// tierFakeAccount records the SetAccountTier call and returns a scripted result. Embeds the stub for the
// rest of the AccountClient surface.
type tierFakeAccount struct {
	stubAccountClient
	gotActor, gotTarget, gotTier string
	called                       int
	ok                           bool
	reason, oldTier              string
	err                          error // when set, SetAccountTier returns this (RPC/transport failure)
}

func (f *tierFakeAccount) SetAccountTier(_ context.Context, actor, target, tier string) (bool, string, string, error) {
	f.called++
	f.gotActor, f.gotTarget, f.gotTier = actor, target, tier
	return f.ok, f.reason, f.oldTier, f.err
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestHandleTierCommand(t *testing.T) {
	run := func(ac AccountClient, line string) (bool, string) {
		var out bytes.Buffer
		tc := telnet.NewReadWriter(&bytes.Buffer{}, &out)
		handled := handleTierCommand(context.Background(), tc, ac, "acct-me", line, discardLogger())
		return handled, out.String()
	}

	// Not a tier verb → not intercepted (forwarded to the world).
	if h, _ := run(&tierFakeAccount{}, "say hello"); h {
		t.Fatal("a non-tier line must not be intercepted")
	}

	// `promote Bob builder` → intercepted, delegated with the actor + target + tier; success line shown.
	fa := &tierFakeAccount{ok: true, oldTier: "player"}
	h, out := run(fa, "promote Bob builder")
	if !h {
		t.Fatal("`promote` was not intercepted")
	}
	if fa.called != 1 || fa.gotActor != "acct-me" || fa.gotTarget != "Bob" || fa.gotTier != "builder" {
		t.Fatalf("SetAccountTier got (actor=%q target=%q tier=%q calls=%d), want (acct-me, Bob, builder, 1)",
			fa.gotActor, fa.gotTarget, fa.gotTier, fa.called)
	}
	if !strings.Contains(out, "player -> builder") {
		t.Fatalf("success line should show the transition: %q", out)
	}

	// `demote Bob` → tier "player".
	fa = &tierFakeAccount{ok: true, oldTier: "admin"}
	run(fa, "demote Bob")
	if fa.gotTier != "player" {
		t.Fatalf("demote should set tier=player, got %q", fa.gotTier)
	}

	// A service refusal (non-admin actor) prints the reason, no crash.
	fa = &tierFakeAccount{ok: false, reason: "You are not authorized to change trust tiers."}
	_, out = run(fa, "promote Bob admin")
	if !strings.Contains(out, "not authorized") {
		t.Fatalf("a refusal should print the reason: %q", out)
	}

	// Bad arity → usage, no RPC.
	fa = &tierFakeAccount{}
	_, out = run(fa, "promote Bob")
	if fa.called != 0 || !strings.Contains(out, "Usage:") {
		t.Fatalf("bad arity should print usage without calling the service; out=%q calls=%d", out, fa.called)
	}

	// A transport/RPC error from the account service → the gate makes NO trust decision of its own:
	// it surfaces a GENERIC "unavailable" (never the raw error), and still CONSUMES the line
	// (handled=true) rather than leaking a failed `promote` to the world as a normal command. The
	// service is the sole authority; the edge only relays it, so an error can't fail-open into
	// world dispatch.
	fa = &tierFakeAccount{err: errors.New("connection refused")}
	h, out = run(fa, "promote Bob admin")
	if !h {
		t.Fatal("an RPC error must still be consumed (handled), never forwarded to the world as a command")
	}
	if fa.called != 1 {
		t.Fatalf("the service should have been called exactly once, got %d", fa.called)
	}
	if !strings.Contains(out, "unavailable") {
		t.Fatalf("an RPC error should surface a generic 'unavailable' message (not the raw error): %q", out)
	}
	if strings.Contains(out, "connection refused") {
		t.Fatalf("the raw transport error must NOT be leaked to the player: %q", out)
	}

	// Intercept boundary: only the EXACT verbs `promote`/`demote` are gate-owned. A look-alike prefix
	// (`promotexyz`) must NOT be intercepted — it falls through to the world untouched. Guards against a
	// future refactor to prefix-matching that would silently swallow a bogus command.
	if h, _ := run(&tierFakeAccount{}, "promotexyz Bob admin"); h {
		t.Fatal("a look-alike prefix (`promotexyz`) must not be intercepted as a tier verb")
	}

	// `demote` with wrong arity → usage, no RPC (symmetric with the promote arity case).
	fa = &tierFakeAccount{}
	_, out = run(fa, "demote")
	if fa.called != 0 || !strings.Contains(out, "Usage:") {
		t.Fatalf("bad demote arity should print usage without calling the service; out=%q calls=%d", out, fa.called)
	}

	// Empty actor passthrough: the dev-autoauth path yields accountID=="" and the account SERVICE rejects it
	// (InvalidArgument). The edge must forward the empty actor VERBATIM — never synthesize/fill in an identity —
	// so the authority can reject it. Here we assert the forwarding (the fake can't replicate the real
	// service's rejection): actor is passed through unchanged and the service is still called.
	var out2 bytes.Buffer
	faEmpty := &tierFakeAccount{ok: false, reason: "not authorized"}
	handleTierCommand(context.Background(), telnet.NewReadWriter(&bytes.Buffer{}, &out2), faEmpty, "", "promote Bob admin", discardLogger())
	if faEmpty.called != 1 || faEmpty.gotActor != "" {
		t.Fatalf("empty actor must be forwarded verbatim (never synthesized): gotActor=%q calls=%d", faEmpty.gotActor, faEmpty.called)
	}
}

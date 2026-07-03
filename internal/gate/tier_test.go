package gate

import (
	"bytes"
	"context"
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
}

func (f *tierFakeAccount) SetAccountTier(_ context.Context, actor, target, tier string) (bool, string, string, error) {
	f.called++
	f.gotActor, f.gotTarget, f.gotTier = actor, target, tier
	return f.ok, f.reason, f.oldTier, nil
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
}

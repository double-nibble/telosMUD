package world

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/contentbus"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// pullTestContext builds a Context for cmdPull: a builder session in a zone whose shard carries a
// readable scope signal queue (no signalLoop drains it, so an enqueued signal stays for the test to
// inspect). arg is the verb tail (the version). withScopes=false makes a bare shard (no director bus) to
// exercise the "unavailable here" path.
func pullTestContext(t *testing.T, arg string, builder, withScopes bool) (*Context, *Shard) {
	t.Helper()
	z := newZone("test")
	sh := &Shard{}
	if withScopes {
		sh.scopes = &scopeReplication{signals: make(chan scopeSignalJob, 4), log: slog.Default()}
	}
	z.shard = sh
	s := &session{character: "Builder", tier: tierBuilder, out: make(chan *playv1.ServerFrame, 8), epoch: 1}
	z.newPlayerEntity(s, "Builder")
	if builder {
		setFlag(s.entity, flagBuilder, true)
	}
	return &Context{z: z, s: s, Actor: s.entity, arg: arg}, sh
}

// lastMarkup drains the session's output channel and returns the concatenated markup, for asserting the
// user-facing acknowledgement/refusal text.
func lastMarkup(s *session) string {
	var b strings.Builder
	for {
		select {
		case f := <-s.out:
			b.WriteString(f.GetOutput().GetMarkup())
		default:
			return b.String()
		}
	}
}

// TestPullEnqueuesWorldSignal proves a builder's `pull <version>` fires a world-scoped
// content.pull.request signal-up carrying the version + actor (#212 slice 4 PR E), and acks the builder.
func TestPullEnqueuesWorldSignal(t *testing.T) {
	c, sh := pullTestContext(t, "v1.2.3", true, true)
	if err := cmdPull(c); err != nil {
		t.Fatal(err)
	}
	select {
	case j := <-sh.scopes.signals:
		if j.event != contentbus.PullRequestEvent {
			t.Fatalf("event = %q, want %q", j.event, contentbus.PullRequestEvent)
		}
		if j.scope.Label() != scopebus.World().Label() {
			t.Fatalf("scope = %q, want world (a content install is a fleet event)", j.scope.Label())
		}
		var req contentbus.PullRequest
		if err := json.Unmarshal(j.payload, &req); err != nil {
			t.Fatal(err)
		}
		if req.Version != "v1.2.3" || req.Actor != "Builder" || req.AtUnixMs == 0 {
			t.Fatalf("pull request payload = %+v", req)
		}
	default:
		t.Fatal("no content.pull.request signal enqueued for a builder's pull")
	}
	if !strings.Contains(lastMarkup(c.s), "v1.2.3") {
		t.Fatal("builder was not acknowledged with the requested version")
	}
}

// TestPullRequiresBuilderFlag proves the capability gate: a staff session WITHOUT the builder flag is
// refused and enqueues nothing (the dispatch MinRank gate is separate; this is the in-handler check).
func TestPullRequiresBuilderFlag(t *testing.T) {
	c, sh := pullTestContext(t, "v1.2.3", false, true)
	if err := cmdPull(c); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sh.scopes.signals:
		t.Fatal("a non-builder must not enqueue a pull request")
	default:
	}
	if !strings.Contains(lastMarkup(c.s), "builder capability") {
		t.Fatal("a non-builder should be told they lack the builder capability")
	}
}

// TestPullRejectsEmptyVersion proves a bare `pull` (no version) prints usage and enqueues nothing — a
// coordinated install always names a published version.
func TestPullRejectsEmptyVersion(t *testing.T) {
	c, sh := pullTestContext(t, "   ", true, true)
	if err := cmdPull(c); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sh.scopes.signals:
		t.Fatal("an empty-version pull must not enqueue a request")
	default:
	}
	if !strings.Contains(lastMarkup(c.s), "usage") {
		t.Fatal("a bare pull should print usage")
	}
}

// TestPullUnavailableWithoutScopeBus proves a bare/dev shard (no director scope bus) reports the feature
// unavailable rather than silently dropping the request or panicking.
func TestPullUnavailableWithoutScopeBus(t *testing.T) {
	c, _ := pullTestContext(t, "v1.2.3", true, false)
	if err := cmdPull(c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastMarkup(c.s), "not available") {
		t.Fatal("without a scope bus, pull should report the coordinated install unavailable")
	}
}

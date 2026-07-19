package world

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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

// --- #427: the prune-guard operator override ---------------------------------------------------------
//
// The live-hosted-pack prune guard is explicitly advisory, but an advisory check with no override is
// really a veto. Since #416 taught it about instance templates — and an instance is never reaped while
// occupied — one idle player inside a dungeon copy indefinitely blocks every content deploy that would
// prune any pack, with nothing an operator can do but wait them out.
//
// Force is gated on ADMIN rather than builder: `pull` is a builder verb because installing content is a
// builder job, and the guard is precisely what makes handing that to a builder safe. If the holder of the
// power could waive its own gate, the gate would be decorative.

// pullForceContext is pullTestContext plus the admin capability, for the authorized-force cases.
func pullForceContext(t *testing.T, arg string, admin bool) (*Context, *Shard) {
	t.Helper()
	c, sh := pullTestContext(t, arg, true, true)
	if admin {
		setFlag(c.Actor, flagAdmin, true)
	}
	return c, sh
}

// TestPullForceRequiresAdminAndEnqueuesNothing is the negative-power assertion, and the one that matters
// most: a builder WITHOUT admin must not merely be told no, they must not get a request onto the bus at
// all. The shard-side gate is the only check on Force — the pull signal is not signed — so a refusal that
// still enqueued would be no gate whatsoever.
func TestPullForceRequiresAdminAndEnqueuesNothing(t *testing.T) {
	c, sh := pullForceContext(t, "v1.2.3 force", false)
	if err := cmdPull(c); err != nil {
		t.Fatal(err)
	}
	select {
	case j := <-sh.scopes.signals:
		t.Fatalf("a builder without admin must enqueue NOTHING for a forced pull; got %+v", j)
	default:
	}
	if out := lastMarkup(c.s); !strings.Contains(out, "admin") {
		t.Fatalf("refusal should name the missing capability so the builder knows who to ask; got: %s", out)
	}
}

// TestPullForceFromAnAdminSetsTheWireFlag proves the authorized path carries Force onto the bus.
func TestPullForceFromAnAdminSetsTheWireFlag(t *testing.T) {
	c, sh := pullForceContext(t, "v1.2.3 force", true)
	if err := cmdPull(c); err != nil {
		t.Fatal(err)
	}
	req := requirePullRequest(t, sh)
	if req.Version != "v1.2.3" || !req.Force {
		t.Fatalf("pull request = %+v, want version v1.2.3 with Force=true", req)
	}
	// The ack must state the consequence. Force does not evict anybody, and an operator who believes it
	// does will skip the reboot that actually matters.
	if out := lastMarkup(c.s); !strings.Contains(out, "reboot") {
		t.Fatalf("a forced pull's ack must tell the operator a reboot is now required; got: %s", out)
	}
}

// TestPlainPullIsNeverForced pins the safe default from the command side.
func TestPlainPullIsNeverForced(t *testing.T) {
	c, sh := pullForceContext(t, "v1.2.3", true) // admin, but did NOT ask for force
	if err := cmdPull(c); err != nil {
		t.Fatal(err)
	}
	if req := requirePullRequest(t, sh); req.Force {
		t.Fatal("a plain `pull <version>` must never set Force, even for an admin")
	}
}

// TestPullRequestForceDefaultsFalseOnTheWire pins the fail-safe decode. An older shard that predates the
// field emits no `force` key; the director must read that as false. This is what makes the rollout safe in
// the only direction that matters — a mixed fleet can never produce an accidental override.
func TestPullRequestForceDefaultsFalseOnTheWire(t *testing.T) {
	var req contentbus.PullRequest
	if err := json.Unmarshal([]byte(`{"version":"v1","actor":"B","at":1}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Force {
		t.Fatal("a payload with no `force` key must decode to Force=false (fail safe)")
	}
	// And it must not be emitted when unset, so the wire stays clean for old readers.
	out, err := json.Marshal(contentbus.PullRequest{Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "force") {
		t.Fatalf("Force must be omitempty; got %s", out)
	}
}

// TestParsePullArgs covers the tail parsing, including force in either position and the version not being
// case-folded (it is a git tag/SHA, not a verb).
func TestParsePullArgs(t *testing.T) {
	for _, tc := range []struct {
		rest        string
		wantVersion string
		wantForce   bool
	}{
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.3 force", "v1.2.3", true},
		{"force v1.2.3", "v1.2.3", true},
		{"v1.2.3 --force", "v1.2.3", true},
		{"V1.2.3-RC1", "V1.2.3-RC1", false},
		{"", "", false},
		{"force", "", true},
	} {
		gotVersion, gotForce := parsePullArgs(tc.rest)
		if gotVersion != tc.wantVersion || gotForce != tc.wantForce {
			t.Fatalf("parsePullArgs(%q) = (%q,%v), want (%q,%v)", tc.rest, gotVersion, gotForce, tc.wantVersion, tc.wantForce)
		}
	}
}

// requirePullRequest drains the one enqueued signal and decodes it, failing if none was enqueued.
func requirePullRequest(t *testing.T, sh *Shard) contentbus.PullRequest {
	t.Helper()
	select {
	case j := <-sh.scopes.signals:
		var req contentbus.PullRequest
		if err := json.Unmarshal(j.payload, &req); err != nil {
			t.Fatal(err)
		}
		return req
	default:
		t.Fatal("no content.pull.request signal enqueued")
		return contentbus.PullRequest{}
	}
}

// TestForcedPullResultNamesTheStrippedPacksToTheOperator is the render half of the fix for the
// write-only-PruneForced defect. The director carries the overridden pack list back in PullResult.Detail;
// this is where the operator finally reads it. Without this branch the success line for a forced pull was
// byte-identical to an ordinary one, so the only record of a break-glass action lived in a log on another
// host.
func TestForcedPullResultNamesTheStrippedPacksToTheOperator(t *testing.T) {
	z := newZone("test")
	sh := &Shard{}
	z.shard = sh
	sr := &scopeReplication{shard: sh, log: slog.Default()}
	sh.zones = map[string]*Zone{"test": z}

	payload, err := json.Marshal(contentbus.PullResult{
		Version: "v2", Actor: "Admin", OK: true, Detail: "dungeons, raids",
	})
	require.NoError(t, err)
	sr.deliverPullResult(payload)

	m := drainForPullResult(z)
	require.NotNil(t, m, "a pull result must be posted to the hosting zone")
	require.Contains(t, m.summary, "dungeons", "the operator must be told WHICH packs were force-pruned")
	require.Contains(t, m.summary, "raids")
	// The remedy ordering is the load-bearing part: a reboot BEFORE redirecting drops those players and
	// reclaims them to their home start room, so "reboot" alone is the wrong instruction.
	require.Contains(t, m.summary, "REDIRECT")
	require.Contains(t, m.summary, "TELOS_CONTENT_PACKS", "a pinned shard will refuse to boot; say so")
}

// TestOrdinaryPullResultIsUnchanged keeps the new branch from leaking into the common case.
func TestOrdinaryPullResultIsUnchanged(t *testing.T) {
	z := newZone("test")
	sh := &Shard{}
	z.shard = sh
	sr := &scopeReplication{shard: sh, log: slog.Default()}
	sh.zones = map[string]*Zone{"test": z}

	payload, err := json.Marshal(contentbus.PullResult{Version: "v2", Actor: "Builder", OK: true})
	require.NoError(t, err)
	sr.deliverPullResult(payload)

	m := drainForPullResult(z)
	require.NotNil(t, m)
	require.NotContains(t, m.summary, "FORCE-PRUNED", "an ordinary pull must not mention force at all")
	require.Contains(t, m.summary, "installed and hot-reloaded")
}

// drainForPullResult pulls the first pullResultMsg out of z's inbox (the zone loop is not running here, so
// the test goroutine is the single consumer).
func drainForPullResult(z *Zone) *pullResultMsg {
	for {
		select {
		case m := <-z.inbox:
			if pr, ok := m.(pullResultMsg); ok {
				return &pr
			}
		default:
			return nil
		}
	}
}

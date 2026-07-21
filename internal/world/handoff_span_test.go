package world

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
)

// installSpanRecorder makes tracer() record into an in-memory exporter for the duration of a test, and
// restores the no-op provider after. A SYNCER (synchronous export) means a span is visible the instant it
// Ends — no batch delay to race the assertions.
func installSpanRecorder(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(noop.NewTracerProvider())
	})
	return exp
}

// awaitSpan polls the exporter for a span with the given name (beginHandoff Ends its span on a goroutine
// defer, slightly after it posts its result to the zone inbox, so a bare read can race the End).
func awaitSpan(t *testing.T, exp *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, s := range exp.GetSpans() {
			if s.Name == name {
				return s
			}
		}
		if time.Now().After(deadline) {
			var got []string
			for _, s := range exp.GetSpans() {
				got = append(got, s.Name)
			}
			t.Fatalf("span %q was never exported; got %v", name, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func findSpan(exp *tracetest.InMemoryExporter, name string) (tracetest.SpanStub, bool) {
	for _, s := range exp.GetSpans() {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}

// requireForced100 asserts a span requests 100% sampling via obs.AlwaysSample(). We check the
// telos.trace.always_sample=true ATTRIBUTE rather than SpanContext.IsSampled(): the in-memory test provider
// uses OTel's default AlwaysSample sampler, so IsSampled() would be trivially true and prove nothing about
// the carve-out. The attribute is the real signal — it is exactly what the production carveOutSampler reads
// to force the span past an aggressive head ratio (the enforcement itself is tested in internal/obs). This
// fails the instant obs.AlwaysSample() is dropped from the span's start attributes.
func requireForced100(t *testing.T, s tracetest.SpanStub) {
	t.Helper()
	for _, a := range s.Attributes {
		if string(a.Key) == "telos.trace.always_sample" && a.Value.AsBool() {
			return
		}
	}
	t.Fatalf("span %q is missing the telos.trace.always_sample=true attribute — the #465 100%% carve-out "+
		"(obs.AlwaysSample) is not requested, so an aggressive head sampler would drop this high-value trace", s.Name)
}

// okAbortHandoffClient Prepares SUCCESSFULLY (so the flow reaches the directory claim) and answers Abort
// cleanly. Paired with a locator whose SetPlayerShard conflicts, it drives the ROLLBACK/abort path.
type okAbortHandoffClient struct{ rejectingHandoffClient }

func (okAbortHandoffClient) Prepare(context.Context, *handoffv1.PrepareRequest, ...grpc.CallOption) (*handoffv1.PrepareResponse, error) {
	return &handoffv1.PrepareResponse{HandoffToken: "tok", TargetShardAddr: "addr-b"}, nil
}

func (okAbortHandoffClient) Abort(context.Context, *handoffv1.AbortRequest, ...grpc.CallOption) (*handoffv1.AbortResponse, error) {
	return &handoffv1.AbortResponse{}, nil
}

// conflictLocator resolves the destination fine but REFUSES the directory ownership claim (ok=false) — the
// split-brain / lost-the-CAS case that triggers the rollback Abort.
type conflictLocator struct{ epochStubLocator }

func (conflictLocator) SetPlayerShard(context.Context, string, string, string, uint64) (bool, error) {
	return false, nil
}

// assertNoInstanceIDAttr fails if any span OR span-event attribute value looks like a player-mintable
// zone-instance id (#470): unbounded cardinality must never reach a span attribute any more than a metric
// label. Events are scanned too — an event attribute is just as unbounded a dimension as a span attribute.
func assertNoInstanceIDAttr(t *testing.T, spans tracetest.SpanStubs) {
	t.Helper()
	check := func(where, name, key, val string) {
		if strings.Contains(val, instanceSep) {
			t.Fatalf("%s %q attribute %q carries an instance-shaped value %q — that is unbounded, "+
				"player-mintable cardinality (#470) and must not be a span attribute", where, name, key, val)
		}
	}
	for _, s := range spans {
		for _, a := range s.Attributes {
			check("span", s.Name, string(a.Key), a.Value.AsString())
		}
		for _, e := range s.Events {
			for _, a := range e.Attributes {
				check("span event", s.Name+"/"+e.Name, string(a.Key), a.Value.AsString())
			}
		}
	}
}

// TestHandoffSpanFailurePath: a Prepare-rejected handoff produces a 100%-sampled root "world.handoff" span
// with an error status + a distinguishing "handoff_failed" event, a "world.handoff.prepare" child carrying
// the error, bounded attributes only, and no instance id anywhere.
func TestHandoffSpanFailurePath(t *testing.T) {
	exp := installSpanRecorder(t)
	peers := func(string) (handoffv1.HandoffClient, error) { return rejectingHandoffClient{}, nil }
	sh, z, s, pid, _ := handoffFixture(t, epochStubLocator{}, peers, 7)

	sh.beginHandoff(z, &handoffv1.PlayerSnapshot{CharacterId: "Mover", PersistId: string(pid)}, "darkwood", "", s.epoch)
	awaitHandoffFail(t, z)

	root := awaitSpan(t, exp, "world.handoff")
	requireForced100(t, root)
	require.Equal(t, otelcodes.Error, root.Status.Code, "a Prepare-rejected handoff must mark the trace an error")

	require.Equal(t, "darkwood", spanAttr(root, "telos.zone.dest"))
	require.Equal(t, "addr-a", spanAttr(root, "telos.shard.src")) // the fixture's shard addr (== FromShardId)

	// The failure is DISTINGUISHABLE: a handoff_failed event names the reason.
	require.Equal(t, "destination rejected the handoff", failEventReason(t, root),
		"the failure event must carry the reason so a Prepare rejection is distinguishable from an unreachable "+
			"destination or an ownership conflict")

	// The epoch-mint hop is on the trace as its own child (so a slow/sick store shows up as the slow hop).
	_, hasMint := findSpan(exp, "world.handoff.mint_epoch")
	require.True(t, hasMint, "the epoch-mint hop must be a visible child span")

	// The failing HOP is localized: the prepare child span carries the error.
	prep := awaitSpan(t, exp, "world.handoff.prepare")
	require.Equal(t, otelcodes.Error, prep.Status.Code, "the prepare hop span must record the rejection")
	// No directory-claim/abort children exist on a pre-claim failure.
	_, hasClaim := findSpan(exp, "world.handoff.directory_claim")
	require.False(t, hasClaim, "a handoff that failed at Prepare must not have a directory_claim span")

	assertNoInstanceIDAttr(t, exp.GetSpans())
}

// TestHandoffSpanAbortPath: Prepare succeeds but the directory claim conflicts, so the rollback fires. The
// trace shows the prepare child OK, the directory_claim child with claimed=false, and a distinct
// "world.handoff.abort" child — the abort path is visible and separable from the happy path.
func TestHandoffSpanAbortPath(t *testing.T) {
	exp := installSpanRecorder(t)
	peers := func(string) (handoffv1.HandoffClient, error) { return okAbortHandoffClient{}, nil }
	sh, z, s, pid, _ := handoffFixture(t, conflictLocator{}, peers, 7)

	sh.beginHandoff(z, &handoffv1.PlayerSnapshot{CharacterId: "Mover", PersistId: string(pid)}, "darkwood", "", s.epoch)
	awaitHandoffFail(t, z)

	root := awaitSpan(t, exp, "world.handoff")
	require.Equal(t, otelcodes.Error, root.Status.Code)
	require.Equal(t, "ownership conflict", failEventReason(t, root),
		"the abort path must be distinguishable from a Prepare rejection by its reason")

	prep := awaitSpan(t, exp, "world.handoff.prepare")
	require.Equal(t, otelcodes.Unset, prep.Status.Code, "Prepare SUCCEEDED on the abort path, so its span is not an error")

	claim := awaitSpan(t, exp, "world.handoff.directory_claim")
	require.False(t, spanBoolAttr(claim, "telos.handoff.claimed"), "the directory claim conflicted, so claimed=false")

	// The distinguishing structural fact: an abort span exists.
	_, hasAbort := findSpan(exp, "world.handoff.abort")
	require.True(t, hasAbort, "a directory-conflict handoff must roll back with a visible world.handoff.abort span")

	assertNoInstanceIDAttr(t, exp.GetSpans())
}

// TestHandoffSpanSuccessPath: a fully successful handoff marks the root span OK with a "committed" event, has
// prepare + directory_claim(claimed=true) children, and NO abort child.
func TestHandoffSpanSuccessPath(t *testing.T) {
	exp := installSpanRecorder(t)
	peers := func(string) (handoffv1.HandoffClient, error) { return okAbortHandoffClient{}, nil }
	sh, z, s, pid, _ := handoffFixture(t, epochStubLocator{}, peers, 7) // epochStubLocator claims ok=true

	sh.beginHandoff(z, &handoffv1.PlayerSnapshot{CharacterId: "Mover", PersistId: string(pid)}, "darkwood", "", s.epoch)
	// Success posts handedOffMsg + redirectMsg (never a fail). Drain until the redirect lands.
	awaitRedirect(t, z)

	root := awaitSpan(t, exp, "world.handoff")
	require.Equal(t, otelcodes.Ok, root.Status.Code, "a fully successful handoff must mark the trace OK")
	require.True(t, hasEvent(root, "committed"), "the success path records a committed event")

	claim := awaitSpan(t, exp, "world.handoff.directory_claim")
	require.True(t, spanBoolAttr(claim, "telos.handoff.claimed"), "a successful claim sets claimed=true")
	_, hasAbort := findSpan(exp, "world.handoff.abort")
	require.False(t, hasAbort, "a successful handoff must not roll back — no abort span")

	assertNoInstanceIDAttr(t, exp.GetSpans())
}

// awaitRedirect drains the source zone inbox until beginHandoff posts its success redirectMsg (the last of
// the handedOffMsg/redirectMsg pair). Fails on a handoffFailMsg, which would mean the move did not succeed.
func awaitRedirect(t *testing.T, z *Zone) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-z.inbox:
			switch m.(type) {
			case redirectMsg:
				return
			case handedOffMsg:
				// keep draining for the redirect
			case handoffFailMsg:
				t.Fatalf("handoff FAILED where success was expected: %#v", m)
			}
		case <-deadline:
			t.Fatal("beginHandoff never posted a redirect (success) to the source zone")
		}
	}
}

func spanAttr(s tracetest.SpanStub, key string) string {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.AsString()
		}
	}
	return ""
}

func spanBoolAttr(s tracetest.SpanStub, key string) bool {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.AsBool()
		}
	}
	return false
}

func hasEvent(s tracetest.SpanStub, name string) bool {
	for _, e := range s.Events {
		if e.Name == name {
			return true
		}
	}
	return false
}

// failEventReason returns the telos.handoff.reason attribute of the handoff_failed event.
func failEventReason(t *testing.T, s tracetest.SpanStub) string {
	t.Helper()
	for _, e := range s.Events {
		if e.Name == "handoff_failed" {
			for _, a := range e.Attributes {
				if string(a.Key) == "telos.handoff.reason" {
					return a.Value.AsString()
				}
			}
		}
	}
	t.Fatalf("span %q has no handoff_failed event with a reason; events=%v", s.Name, s.Events)
	return ""
}

// --- AdoptZone / lease-handover spans (#465) ---------------------------------------------------

// configLeaser is a ZoneLeaser whose ZoneLease + HandoverZone answers are set per test, to drive the adopt
// span's success, adopt-reject, and flip-refused paths.
type configLeaser struct {
	owner   string
	gen     uint64
	flipOK  bool
	flipErr error
}

func (configLeaser) ClaimZone(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (configLeaser) ReleaseZone(context.Context, string, string) error { return nil }
func (l configLeaser) HandoverZone(context.Context, string, string, string, time.Duration) (bool, error) {
	return l.flipOK, l.flipErr
}

func (l configLeaser) ZoneLease(context.Context, string) (string, uint64, error) {
	return l.owner, l.gen, nil
}

// adoptOKClient answers AdoptZone successfully; every other Handoff RPC is unused here.
type adoptOKClient struct{ rejectingHandoffClient }

func (adoptOKClient) AdoptZone(context.Context, *handoffv1.AdoptZoneRequest, ...grpc.CallOption) (*handoffv1.AdoptZoneResponse, error) {
	return &handoffv1.AdoptZoneResponse{}, nil
}

func adoptSpanShard(t *testing.T, leaser ZoneLeaser, client handoffv1.HandoffClient) *Shard {
	t.Helper()
	peers := func(string) (handoffv1.HandoffClient, error) { return client, nil }
	sh := NewShard("midgaard", "addr-a", epochStubLocator{}, peers)
	sh.WithZoneLeasing(leaser, "shard-a", time.Minute, time.Minute, nil)
	return sh
}

// TestAdoptZoneSpanSuccess: a clean lease handover produces a 100%-sampled root "world.adopt_zone" span with
// OK status, bounded attributes (zone/src/dest shard + lease gen), an rpc child, and a lease_flip child with
// flipped=true.
func TestAdoptZoneSpanSuccess(t *testing.T) {
	exp := installSpanRecorder(t)
	sh := adoptSpanShard(t, configLeaser{owner: "shard-a", gen: 5, flipOK: true}, adoptOKClient{})

	err := sh.handoverZoneTo(context.Background(), "darkwood", "shard-b", "addr-b")
	require.NoError(t, err)

	root := awaitSpan(t, exp, "world.adopt_zone")
	requireForced100(t, root)
	require.Equal(t, otelcodes.Ok, root.Status.Code)
	require.Equal(t, "darkwood", spanAttr(root, "telos.zone"))
	require.Equal(t, "shard-a", spanAttr(root, "telos.shard.src"))
	require.Equal(t, "shard-b", spanAttr(root, "telos.shard.dest"))
	require.EqualValues(t, 5, spanInt64Attr(root, "telos.lease.gen"))

	rpc := awaitSpan(t, exp, "world.adopt_zone.rpc")
	require.Equal(t, otelcodes.Unset, rpc.Status.Code, "the AdoptZone RPC succeeded")
	flip := awaitSpan(t, exp, "world.adopt_zone.lease_flip")
	require.True(t, spanBoolAttr(flip, "telos.lease.flipped"), "a successful handover flips the lease")

	assertNoInstanceIDAttr(t, exp.GetSpans())
}

// TestAdoptZoneSpanAdoptRejected: the peer refuses AdoptZone, so the root is an error and the rpc child
// carries it — and NO lease flip is attempted (visible as the absent lease_flip span).
func TestAdoptZoneSpanAdoptRejected(t *testing.T) {
	exp := installSpanRecorder(t)
	sh := adoptSpanShard(t, configLeaser{owner: "shard-a", gen: 5, flipOK: true}, rejectingHandoffClient{})

	err := sh.handoverZoneTo(context.Background(), "darkwood", "shard-b", "addr-b")
	require.Error(t, err)

	root := awaitSpan(t, exp, "world.adopt_zone")
	require.Equal(t, otelcodes.Error, root.Status.Code)
	rpc := awaitSpan(t, exp, "world.adopt_zone.rpc")
	require.Equal(t, otelcodes.Error, rpc.Status.Code, "the AdoptZone RPC rejection must localize to the rpc hop span")
	_, hasFlip := findSpan(exp, "world.adopt_zone.lease_flip")
	require.False(t, hasFlip, "a rejected adopt must not reach the lease flip")

	assertNoInstanceIDAttr(t, exp.GetSpans())
}

// TestAdoptZoneSpanFlipRefused: adopt succeeds but the fenced flip is refused (we lost the lease), so the
// root is an error and the lease_flip child shows flipped=false.
func TestAdoptZoneSpanFlipRefused(t *testing.T) {
	exp := installSpanRecorder(t)
	sh := adoptSpanShard(t, configLeaser{owner: "shard-a", gen: 5, flipOK: false}, adoptOKClient{})

	err := sh.handoverZoneTo(context.Background(), "darkwood", "shard-b", "addr-b")
	require.Error(t, err)

	root := awaitSpan(t, exp, "world.adopt_zone")
	require.Equal(t, otelcodes.Error, root.Status.Code)
	rpc := awaitSpan(t, exp, "world.adopt_zone.rpc")
	require.Equal(t, otelcodes.Unset, rpc.Status.Code, "adopt SUCCEEDED here; only the flip failed")
	flip := awaitSpan(t, exp, "world.adopt_zone.lease_flip")
	require.False(t, spanBoolAttr(flip, "telos.lease.flipped"), "a refused fenced flip records flipped=false")

	assertNoInstanceIDAttr(t, exp.GetSpans())
}

func spanInt64Attr(s tracetest.SpanStub, key string) int64 {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.AsInt64()
		}
	}
	return 0
}

package world

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc/test/bufconn"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/sessionlock"
)

// attachSpanShard builds a fully-wired shard for the session-attach trace: content + a MemStore (so the
// load-snapshot and ownership-claim hops run), a directory stub (so the epoch-resume hop runs), and an
// in-memory session lock (so the session-lock hop runs). Returns the shard, a play client, and the store.
func attachSpanShard(t *testing.T) (*Shard, playv1.PlayClient, *MemStore) {
	t.Helper()
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemStore()
	lis := bufconn.Listen(1 << 20)
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "addr-a", epochStubLocator{}, nil).
		WithPersistence(mem, mem).
		WithSessionLock(sessionlock.NewMem(), 0, 0)
	play := serveShard(t, sh, lis)
	waitCond(t, "boot zone actors armed", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && len(sh.actorDone) == 2
	})
	return sh, play, mem
}

// TestSessionAttachSpan drives a real gRPC login end-to-end and asserts the login-latency decomposition
// (#466): a "world.session_attach" root span with child spans for the Redis/Postgres/lock hops, bounded
// attributes and NO player identity, and — the load-bearing property — that the span ENDS AT ATTACH, not at
// logout: it is exported while the stream is still open and the session is live.
func TestSessionAttachSpan(t *testing.T) {
	exp := installSpanRecorder(t)
	_, play, mem := attachSpanShard(t)

	// Pre-create the character so the login is a REHYDRATE — that is what makes loadedOK true and drives the
	// ownership-claim hop (a brand-new name skips the claim).
	_, err := mem.CreateCharacter(context.Background(), "Alice", "midgaard", "midgaard:room:temple")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := play.Connect(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(attachWithToken("Alice", "")))
	recvAttached(t, stream) // login handshake complete; the player is in-world and the stream stays OPEN

	// The span must be exported NOW — while the stream is still open and the session is live. If it only
	// appeared after we closed the stream, it would be spanning the whole session (the #466 anti-goal).
	root := awaitSpan(t, exp, "world.session_attach")
	require.Equal(t, otelcodes.Ok, root.Status.Code, "a completed login marks the attach span OK")

	// Bounded attributes only — zone by TEMPLATE and the shard. Critically, NO character name / account id.
	require.Equal(t, "midgaard", spanAttr(root, "telos.zone"))
	require.NotEmpty(t, spanAttr(root, "telos.shard.src"))
	for _, a := range root.Attributes {
		v := a.Value.AsString()
		require.NotEqual(t, "Alice", v, "the character name must NOT be a span attribute (#466): key %q", a.Key)
	}

	// The hop children exist, so login latency is decomposable across Redis/Postgres/lock.
	for _, hop := range []string{
		"session_attach.epoch_resume",  // Redis / directory
		"session_attach.load_snapshot", // Postgres / Redis
		"session_attach.claim",         // Postgres ownership claim
		"session_attach.session_lock",  // Redis single-session lock
	} {
		_, ok := findSpan(exp, hop)
		require.Truef(t, ok, "the %q hop must be a visible child span of the session-attach trace", hop)
	}

	// The children are PARENTED to the root (one trace), not scattered roots.
	for _, s := range exp.GetSpans() {
		if s.Name == "session_attach.claim" {
			require.Equal(t, root.SpanContext.SpanID(), s.Parent.SpanID(),
				"the claim hop must be a child of the session_attach root, not a separate trace")
		}
	}

	// Now close the stream (logout). The span count for world.session_attach must NOT grow — it already
	// ended at attach; nothing about teardown re-opens or re-ends it.
	before := countSpans(exp, "world.session_attach")
	cancel()
	_ = stream.CloseSend()
	time.Sleep(150 * time.Millisecond) // let teardown run
	require.Equal(t, before, countSpans(exp, "world.session_attach"),
		"the session-attach span must be emitted exactly once, at attach — not re-ended at logout")
}

func countSpans(exp *tracetest.InMemoryExporter, name string) int {
	n := 0
	for _, s := range exp.GetSpans() {
		if s.Name == name {
			n++
		}
	}
	return n
}

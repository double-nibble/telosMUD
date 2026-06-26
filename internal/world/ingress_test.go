package world

import (
	"context"
	"strings"
	"testing"
	"time"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/textsan"
)

// nextSay reads frames until the next "You say," Output and returns its markup. Unlike
// recvSay it asserts nothing, so a test can inspect exactly what the world rendered.
func nextSay(t *testing.T, s playv1.Play_ConnectClient) string {
	t.Helper()
	for {
		f, err := s.Recv()
		if err != nil {
			t.Fatalf("recv waiting for say: %v", err)
		}
		if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), "You say,") {
			return o.GetMarkup()
		}
	}
}

// TestWorldIngressStripsControlChars (N1) proves the world re-sanitizes input at its
// own gRPC boundary. The edge normally strips control runes, but a compromised/buggy
// gate or a direct-shard client could send them raw. A control-laden say must come back
// out (and thus reach other players' terminals) with the control bytes gone.
func TestWorldIngressStripsControlChars(t *testing.T) {
	client := startWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Tester"))
	recvAttached(t, s)

	// ESC + BEL embedded in the line: classic terminal-injection bytes.
	send(t, s, inputSeq(1, "say hel\x1blo\x07 there"))
	markup := nextSay(t, s)

	if strings.ContainsAny(markup, "\x1b\x07") {
		t.Fatalf("world ingress let control bytes through: %q", markup)
	}
	if !strings.Contains(markup, "hello there") {
		t.Fatalf("say content was mangled beyond control-strip: %q", markup)
	}
}

// TestWorldIngressCapsLongLine (N1) proves the world re-applies the MaxLineBytes cap at
// its gRPC boundary, so a producer that skipped the edge cannot post an unbounded line
// that then fans out per room occupant.
func TestWorldIngressCapsLongLine(t *testing.T) {
	client := startWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Tester"))
	recvAttached(t, s)

	over := strings.Repeat("a", textsan.MaxLineBytes+2000)
	send(t, s, inputSeq(1, "say "+over))
	markup := nextSay(t, s)

	// The whole input line (command + arg) is capped at MaxLineBytes before parsing, so
	// the echoed content cannot carry the full over-long payload.
	if got := strings.Count(markup, "a"); got >= len(over) {
		t.Fatalf("world ingress did not cap the line: %d 'a' runes in output", got)
	}
	if len(markup) > textsan.MaxLineBytes+64 { // +slack for the "You say, '...'" wrapper
		t.Fatalf("rendered say exceeds the cap: %d bytes", len(markup))
	}
}

// TestWorldIngressSanitizesCharacterId proves a fresh-login character id from the Attach
// frame is sanitized before it becomes the player's display name + targeting keyword.
// This is the same render surface as the handoff-snapshot Name (N2) but reached through
// the normal login door: a bypassed/forged gate could put control bytes in character_id,
// which then fan out to other players via who and the "$n arrives" broadcast.
func TestWorldIngressSanitizesCharacterId(t *testing.T) {
	client := startWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Ev\x1bil\x07Name"))
	recvAttached(t, s)

	// `who` renders every online player's name (entity.Name() == e.short).
	send(t, s, inputSeq(1, "who"))
	for {
		f, err := s.Recv()
		if err != nil {
			t.Fatalf("recv waiting for who: %v", err)
		}
		o := f.GetOutput()
		if o == nil || !strings.Contains(o.GetMarkup(), "Players online:") {
			continue
		}
		markup := o.GetMarkup()
		if strings.ContainsAny(markup, "\x1b\x07") {
			t.Fatalf("character id control bytes reached who output: %q", markup)
		}
		if !strings.Contains(markup, "EvilName") {
			t.Fatalf("sanitized name missing from who output: %q", markup)
		}
		return
	}
}

// TestPrepareSanitizesSnapshotName (N2) proves a rehydrated cross-shard handoff snapshot
// has its display name sanitized. The handoff snapshot is externally-sourced and
// currently unauthenticated (docs/PROTOCOL.md §5): a forged snapshot with a control-laden
// Name would otherwise land as a passively-rendered display name + targeting keyword,
// re-opening terminal injection through a non-edge door.
func TestPrepareSanitizesSnapshotName(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	reply := make(chan error, 1)
	z.prepare(prepareMsg{
		snap:  &handoffv1.PlayerSnapshot{CharacterId: "Walker", Name: "Wa\x1blk\x07er"},
		room:  "midgaard:room:temple",
		epoch: 5,
		token: "tok",
		reply: reply,
	})

	select {
	case err := <-reply:
		if err != nil {
			t.Fatalf("prepare replied error: %v", err)
		}
	default:
		t.Fatal("prepare must reply on the channel")
	}

	s := z.players["Walker"]
	if s == nil || s.entity == nil {
		t.Fatal("prepare did not park a pending entity")
	}
	if got := s.entity.short; got != "Walker" {
		t.Fatalf("snapshot name not sanitized: short = %q, want %q", got, "Walker")
	}
	if len(s.entity.keywords) != 1 || s.entity.keywords[0] != "Walker" {
		t.Fatalf("snapshot name not sanitized into keywords: %q", s.entity.keywords)
	}
}

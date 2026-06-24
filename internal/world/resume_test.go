package world

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// TestExactlyOnceAcrossRedial proves the redirect/replay substrate on a single
// shard (docs/PROTOCOL.md §5). It simulates exactly what the gate will do on a real
// cross-shard Redirect: a player runs some sequenced input, the stream drops, a new
// stream re-attaches to the same character, learns the resume point from the
// Attached frame's ack, and replays the already-applied input. The world must dedup
// the replay so each command runs exactly once — no double-apply, no loss.
func TestExactlyOnceAcrossRedial(t *testing.T) {
	client := startWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// --- first stream: apply seq 1 and 2 ---
	ctx1, drop1 := context.WithCancel(ctx)
	s1, err := client.Connect(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s1, attach("Tester"))
	if ack := recvAttached(t, s1); ack != 0 {
		t.Fatalf("initial resume ack = %d, want 0", ack)
	}
	send(t, s1, inputSeq(1, "say one"))
	if ack := recvSay(t, s1, "one"); ack != 1 {
		t.Fatalf("ack after seq 1 = %d, want 1", ack)
	}
	send(t, s1, inputSeq(2, "say two"))
	if ack := recvSay(t, s1, "two"); ack != 2 {
		t.Fatalf("ack after seq 2 = %d, want 2", ack)
	}

	// --- simulate the gate re-dialing: drop stream 1 (player goes link-dead but
	// survives the grace window), then attach a fresh stream as the same character ---
	drop1()

	s2, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s2, attach("Tester"))
	if ack := recvAttached(t, s2); ack != 2 {
		t.Fatalf("resume ack = %d, want 2 (the high-water mark)", ack)
	}

	// Replay seq 1 and 2 (with deliberately different text so a double-apply would be
	// visible as a 'DUP' say), then a genuinely new seq 3.
	send(t, s2, inputSeq(1, "say DUP1"))
	send(t, s2, inputSeq(2, "say DUP2"))
	send(t, s2, inputSeq(3, "say three"))

	// The first say after the replays MUST be 'three'. If either replay had been
	// applied, we'd see 'DUP1' here instead — that's the exactly-once assertion.
	if ack := recvSay(t, s2, "three"); ack != 3 {
		t.Fatalf("ack after seq 3 = %d, want 3", ack)
	}
}

// --- helpers ---

func startWorld(t *testing.T) playv1.PlayClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	shard := NewDemoShard()
	shard.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	zctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go shard.Run(zctx)

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return playv1.NewPlayClient(cc)
}

func inputSeq(seq uint64, text string) *playv1.ClientFrame {
	return &playv1.ClientFrame{Payload: &playv1.ClientFrame_Input{Input: &playv1.InputLine{Seq: seq, Text: text}}}
}

// recvAttached reads until the Attached frame and returns its ack (the resume point).
func recvAttached(t *testing.T, s playv1.Play_ConnectClient) uint64 {
	t.Helper()
	for {
		f, err := s.Recv()
		if err != nil {
			t.Fatalf("recv waiting for Attached: %v", err)
		}
		if f.GetAttached() != nil {
			return f.GetAckInputSeq()
		}
	}
}

// recvSay reads until the next "You say," Output, asserts it contains want, and
// returns its piggybacked ack_input_seq.
func recvSay(t *testing.T, s playv1.Play_ConnectClient, want string) uint64 {
	t.Helper()
	for {
		f, err := s.Recv()
		if err != nil {
			t.Fatalf("recv waiting for say %q: %v", want, err)
		}
		if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), "You say,") {
			if !strings.Contains(o.GetMarkup(), want) {
				t.Fatalf("exactly-once violated: say = %q, expected it to contain %q", o.GetMarkup(), want)
			}
			return f.GetAckInputSeq()
		}
	}
}

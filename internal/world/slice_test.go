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

// TestVerticalSlice drives the Phase 1 milestone end-to-end over real gRPC:
// attach -> see the room -> move north -> see the next room -> say echoes.
func TestVerticalSlice(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	shard := NewDemoShard()
	shard.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	zoneCtx, cancelZone := context.WithCancel(context.Background())
	t.Cleanup(cancelZone)
	go shard.Run(zoneCtx)

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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	stream, err := playv1.NewPlayClient(cc).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}

	send(t, stream, attach("Tester"))
	expectMarkup(t, stream, "The Temple Square") // look on join

	send(t, stream, input("north"))
	expectMarkup(t, stream, "Market Square")

	send(t, stream, input("say hi"))
	expectMarkup(t, stream, "You say, 'hi'")
}

func attach(name string) *playv1.ClientFrame {
	return &playv1.ClientFrame{Payload: &playv1.ClientFrame_Attach{Attach: &playv1.Attach{CharacterId: name}}}
}

func input(text string) *playv1.ClientFrame {
	return &playv1.ClientFrame{Payload: &playv1.ClientFrame_Input{Input: &playv1.InputLine{Text: text}}}
}

func send(t *testing.T, s playv1.Play_ConnectClient, f *playv1.ClientFrame) {
	t.Helper()
	if err := s.Send(f); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func expectMarkup(t *testing.T, s playv1.Play_ConnectClient, substr string) {
	t.Helper()
	for {
		f, err := s.Recv()
		if err != nil {
			t.Fatalf("recv while waiting for %q: %v", substr, err)
		}
		if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), substr) {
			return
		}
	}
}

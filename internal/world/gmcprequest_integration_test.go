package world

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// gmcpIn builds an inbound GMCP request ClientFrame.
func gmcpIn(pkg, jsonPayload string) *playv1.ClientFrame {
	return &playv1.ClientFrame{Payload: &playv1.ClientFrame_Gmcp{Gmcp: &playv1.GmcpIn{Pkg: pkg, Json: []byte(jsonPayload)}}}
}

// recvGMCPUntil reads frames until it sees a GMCP frame for pkg (returning its JSON), or fails on timeout.
func recvGMCPUntil(t *testing.T, s playv1.Play_ConnectClient, pkg string) string {
	t.Helper()
	for {
		f, err := s.Recv()
		if err != nil {
			t.Fatalf("recv waiting for GMCP %s: %v", pkg, err)
		}
		if g := f.GetGmcp(); g != nil && g.GetPkg() == pkg {
			return string(g.GetJson())
		}
	}
}

// TestGMCPContainerContentsRoundTrip is the #92 INTEGRATION test: a real client, over the gRPC Play stream,
// sends an inbound Char.Items.Contents request and the world replies with the container's Char.Items.List —
// exercising the whole receive path (server.go GmcpIn ingress -> zone -> handler -> reply -> stream Send).
func TestGMCPContainerContentsRoundTrip(t *testing.T) {
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
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	stream, err := playv1.NewPlayClient(cc).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}

	send(t, stream, attach("Looter"))
	expectMarkup(t, stream, "The Temple Square") // login complete
	send(t, stream, input("north"))              // to Market Square, where the wooden chest sits on the floor

	// The room's Char.Items push names the chest with its stable GMCP id — extract it (findRoomItemID reads
	// through the market render + the per-item Adds).
	chestID := findRoomItemID(t, stream, "chest")

	// Open the chest (closed by default — a closed container reveals nothing) then request its contents. Both
	// frames are processed in FIFO order by the zone, so the open lands before the request.
	send(t, stream, input("open chest"))
	send(t, stream, gmcpIn("Char.Items.Contents", `{"container":"`+chestID+`"}`))
	raw := recvGMCPUntil(t, stream, "Char.Items.List")

	// The reply may be for room/inv (steady-state pushes) OR our container — loop until it's our container.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var reply struct {
			Location string `json:"location"`
		}
		_ = json.Unmarshal([]byte(raw), &reply)
		if reply.Location == chestID {
			return // round-trip confirmed: the world answered our container-contents request
		}
		if time.Now().After(deadline) {
			t.Fatalf("never received a Char.Items.List for the requested container %q", chestID)
		}
		raw = recvGMCPUntil(t, stream, "Char.Items.List")
	}
}

// findRoomItemID reads Char.Items GMCP frames until it finds a ROOM item whose name contains `substr`,
// returning that item's id. Handles both the full Char.Items.List (login/re-list) and the incremental
// Char.Items.Add (entering a new room emits per-item Adds, not a full List).
func findRoomItemID(t *testing.T, s playv1.Play_ConnectClient, substr string) string {
	t.Helper()
	type gi struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	match := func(name string) bool { return strings.Contains(strings.ToLower(name), substr) }
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := s.Recv()
		if err != nil {
			t.Fatalf("recv looking for room item %q: %v", substr, err)
		}
		if g := f.GetGmcp(); g != nil {
			switch g.GetPkg() {
			case "Char.Items.List":
				var list struct {
					Location string `json:"location"`
					Items    []gi   `json:"items"`
				}
				_ = json.Unmarshal(g.GetJson(), &list)
				if list.Location == "room" {
					for _, it := range list.Items {
						if match(it.Name) {
							return it.ID
						}
					}
				}
			case "Char.Items.Add":
				var add struct {
					Location string `json:"location"`
					Item     gi     `json:"item"`
				}
				_ = json.Unmarshal(g.GetJson(), &add)
				if add.Location == "room" && match(add.Item.Name) {
					return add.Item.ID
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("never saw a room Char.Items entry containing %q", substr)
		}
	}
}

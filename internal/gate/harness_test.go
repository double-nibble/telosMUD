package gate

// harness_test.go is the reusable in-process integration harness for the gate's
// cross-cutting tests (layer 2 of the test pyramid: real world shards + a real gate
// over a bufconn Play stream, no docker, no network). The original scaffolding lived
// inline in handoff_integration_test.go; it is factored here so a journey or chaos
// test is a few lines:
//
//	h := newHarness(t)
//	sh := h.addShard("midgaard", "addr-a")     // a real world.Shard, served + Run()
//	h.serveGate(directory.Static{Addr: "addr-a"}) // a real gate dialing the shards
//	term := h.dial(t)                           // a scripted "telnet" client
//	term.login(t, "Alice")
//	term.send("look"); term.expect(t, "Temple Square")
//
// Design rules this harness enforces (house rules):
//   - Determinism over sleeps: every wait polls a condition with a deadline
//     (term.expect). Nothing sleeps-and-hopes.
//   - Real seams, fake transport: shards are dialed over bufconn (the same Play /
//     Handoff services production registers), the gate uses its real pool via an
//     injected dialer, the "socket" is a net.Pipe — only the wire is in-process.
//   - Cleanup is automatic: every server, goroutine, and conn is registered with
//     t.Cleanup, so a test never leaks a shard or a hung handle goroutine.

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/world"
)

// harness wires N in-process world shards and one gate so a test can drive a scripted
// player against the real cross-service path. Shards are addressed by the same string
// the directory hands the gate; the gate's pool dials them over their bufconn listeners.
type harness struct {
	t *testing.T

	mu        sync.Mutex
	listeners map[string]*bufconn.Listener // shard addr -> its bufconn listener
	servers   map[string]*grpc.Server      // shard addr -> its grpc server (for stop/drop)
	cancels   map[string]context.CancelFunc

	srv *Server // the gate (set by serveGate)
}

// newHarness builds an empty harness; add shards then call serveGate.
func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{
		t:         t,
		listeners: map[string]*bufconn.Listener{},
		servers:   map[string]*grpc.Server{},
		cancels:   map[string]context.CancelFunc{},
	}
}

// addShard serves one single-zone world shard at addr and starts its zone goroutine.
// peers is the cross-shard Handoff dialer (nil for a single-shard world). Returns the
// live *world.Shard so a test can inspect or drive it (e.g. for a persistence journey
// or to assert directory placement). Registered for automatic teardown.
func (h *harness) addShard(zoneID, addr string, dir world.Locator, peers world.HandoffDialer) *world.Shard {
	h.t.Helper()
	sh := world.NewShard(zoneID, addr, dir, peers)
	h.serveShard(addr, sh)
	return sh
}

// addShardWithComms serves one single-zone world shard at addr wired with a comms SOURCE bus (the
// Phase-8 world-is-source role). comms MUST be a RoleWorld handle (the world publishes channel lines)
// — the cross-shard channel tests pass the WORLD handle of the SAME MemBus whose GATE handle the gate
// is wired with, so a `gossip` line a player types on one shard fans out over the bus to a gate
// subscribed for a player on the OTHER shard. Returns the live shard for inspection.
func (h *harness) addShardWithComms(zoneID, addr string, dir world.Locator, peers world.HandoffDialer, comms commbus.Bus) *world.Shard {
	h.t.Helper()
	sh := world.NewShard(zoneID, addr, dir, peers).WithComms(comms)
	h.serveShard(addr, sh)
	return sh
}

// serveShard installs an already-built shard on its bufconn listener and starts both
// its gRPC server and its zone goroutine. Splitting this from addShard lets a test
// build a shard with custom wiring (WithPersistence, multi-zone) and still get the
// served listener + lifecycle management.
func (h *harness) serveShard(addr string, sh *world.Shard) {
	h.t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	sh.Register(gs)
	go func() { _ = gs.Serve(lis) }()

	zctx, cancel := context.WithCancel(context.Background())
	go sh.Run(zctx)

	h.mu.Lock()
	h.listeners[addr] = lis
	h.servers[addr] = gs
	h.cancels[addr] = cancel
	h.mu.Unlock()

	h.t.Cleanup(func() {
		cancel()
		gs.Stop()
	})
}

// dropShard simulates a world shard disappearing: it stops the shard's gRPC server
// (so the Play stream to it dies) and cancels its zone goroutine. This is the chaos
// primitive — "the shard the player is on goes away" — used by the shard-drop test.
// After this, the gate's pool still has the cached (now-dead) client; a re-dial or a
// new stream to addr fails at the bufconn, exactly as a real shard crash would.
func (h *harness) dropShard(addr string) {
	h.t.Helper()
	h.mu.Lock()
	gs, hasSrv := h.servers[addr]
	cancel, hasCancel := h.cancels[addr]
	lis := h.listeners[addr]
	delete(h.servers, addr)
	delete(h.cancels, addr)
	h.mu.Unlock()
	if hasCancel {
		cancel()
	}
	if hasSrv {
		gs.Stop()
	}
	// Close the listener too so any in-flight or fresh DialContext fails immediately
	// rather than hanging — a dead shard refuses connections, it does not stall them.
	if lis != nil {
		_ = lis.Close()
	}
}

// dialPlay returns a Play client for a served shard addr (over its bufconn). The gate's
// pool is built from this so the gate dials shards exactly as in production, only the
// transport is in-process.
func (h *harness) dialPlay(addr string) (playv1.PlayClient, error) {
	h.mu.Lock()
	lis := h.listeners[addr]
	h.mu.Unlock()
	if lis == nil {
		return nil, fmt.Errorf("harness: no shard served at %q", addr)
	}
	cc, err := h.dialConn(lis)
	if err != nil {
		return nil, err
	}
	return playv1.NewPlayClient(cc), nil
}

// dialHandoff returns a Handoff client for a served shard addr — the source side of a
// cross-shard move uses this to reach the destination shard's Handoff service.
func (h *harness) dialHandoff(addr string) (handoffv1.HandoffClient, error) {
	h.mu.Lock()
	lis := h.listeners[addr]
	h.mu.Unlock()
	if lis == nil {
		return nil, fmt.Errorf("harness: no shard served at %q", addr)
	}
	cc, err := h.dialConn(lis)
	if err != nil {
		return nil, err
	}
	return handoffv1.NewHandoffClient(cc), nil
}

func (h *harness) dialConn(lis *bufconn.Listener) (*grpc.ClientConn, error) {
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}
	h.t.Cleanup(func() { _ = cc.Close() })
	return cc, nil
}

// serveGate builds the gate over a pool that dials this harness's shards (by address,
// over their bufconns) and resolves logins through dir. Call after every shard the
// login can land on is added. The gate's comms bus is a Disabled RoleGate no-op (the
// existing journey tests run without comms; nil normalizes to Disabled in newServer).
func (h *harness) serveGate(dir directory.Directory) {
	h.t.Helper()
	h.serveGateWithComms(dir, nil)
}

// serveGateWithComms is serveGate with an explicit gate-role comms bus injected — the
// comms tests (8.2) pass the GATE handle of a shared MemBus so a separate WORLD handle
// can publish the synthetic message that the gate must render. comms MUST be a
// RoleGate handle (never a world handle): the gate is structurally subscribe-only on
// chan/tell, mirroring how cmd/telos-gate wires commbus.OpenGate (never OpenWorld).
func (h *harness) serveGateWithComms(dir directory.Directory, comms commbus.Bus) {
	h.t.Helper()
	p := newPoolWithDialer(func(addr string) (playv1.PlayClient, error) {
		return h.dialPlay(addr)
	})
	h.srv = newServer(":0", dir, p, comms)
}

// dial opens one scripted player connection to the gate and returns its terminal. The
// gate's handle loop runs on its own goroutine against one end of a net.Pipe; the test
// drives the other end. The handle goroutine is registered to drain on socket close.
func (h *harness) dial(t *testing.T) *terminal {
	t.Helper()
	if h.srv == nil {
		t.Fatal("harness: serveGate not called before dial")
	}
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	hctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		h.srv.handle(hctx, server)
		close(done)
	}()

	term := newTerminal(client)
	term.conn = client
	term.done = done
	return term
}

// --- the scripted "telnet" client ---------------------------------------------------

// terminal reads rendered bytes off the gate's socket via a single background reader
// (net.Pipe reads block forever, so the reader must not be inline) and lets the test
// assert that an expected substring appears within a deadline. The accumulator persists
// across expect calls so output already buffered from a prior step still counts.
type terminal struct {
	bytes chan byte
	acc   strings.Builder

	conn net.Conn      // the player's socket end (for send / close)
	done chan struct{} // closed when the gate's handle goroutine returns

	// resume gates the reader goroutine. The reader receives one token from it before EACH socket
	// read; pauseReader stops feeding tokens (the reader blocks, NOT draining the socket, so the
	// kernel pipe buffer fills and the gate's writes to this terminal block — the slow/blocked-socket
	// primitive). A background feeder normally keeps it topped up; pause/resume flip the feeder.
	resume   chan struct{}
	pauseMu  sync.Mutex
	paused   bool
	pauseSig chan struct{} // closed-and-replaced to wake the feeder when resumed
}

func newTerminal(c net.Conn) *terminal {
	t := &terminal{
		bytes:    make(chan byte, 4096),
		conn:     c,
		resume:   make(chan struct{}),
		pauseSig: make(chan struct{}),
	}
	// Feeder: while not paused, keep a token available so the reader proceeds; while paused, withhold
	// tokens so the reader blocks before its next socket read.
	go func() {
		for {
			t.pauseMu.Lock()
			paused := t.paused
			sig := t.pauseSig
			t.pauseMu.Unlock()
			if paused {
				<-sig // wait until resumed, then loop and start feeding again
				continue
			}
			select {
			case t.resume <- struct{}{}:
			case <-sig: // paused mid-feed: stop, re-evaluate
			}
		}
	}()
	go func() {
		r := bufio.NewReader(c)
		for {
			<-t.resume // gate: a paused terminal stops here, NOT reading the socket
			b, err := r.ReadByte()
			if err != nil {
				close(t.bytes)
				return
			}
			t.bytes <- b
		}
	}()
	return t
}

// pauseReader stops the terminal's reader from draining the socket, so the gate's writes to this
// terminal back up and eventually block — the slow/blocked-consumer primitive (the gate's comms
// delivery goroutine parks on tc.Write). Idempotent.
func (term *terminal) pauseReader() {
	term.pauseMu.Lock()
	term.paused = true
	close(term.pauseSig) // wake the feeder so it sees paused and stops feeding tokens
	term.pauseSig = make(chan struct{})
	term.pauseMu.Unlock()
}

// resumeReader lets the reader drain again. Idempotent.
func (term *terminal) resumeReader() {
	term.pauseMu.Lock()
	if term.paused {
		term.paused = false
		close(term.pauseSig) // wake the feeder so it resumes feeding tokens
		term.pauseSig = make(chan struct{})
	}
	term.pauseMu.Unlock()
}

// send writes one CRLF-terminated line as the player's terminal would.
func (term *terminal) send(t *testing.T, s string) {
	t.Helper()
	if _, err := term.conn.Write([]byte(s + "\r\n")); err != nil {
		t.Fatalf("send %q: %v", s, err)
	}
}

// login walks the gate's name prompt and submits name. It is the common first two
// steps of every journey, factored so a test starts at "in the world".
func (term *terminal) login(t *testing.T, name string) {
	t.Helper()
	term.expect(t, "By what name")
	term.send(t, name)
}

// expect blocks until substr appears in the accumulated output or a deadline elapses.
// It never sleeps-and-hopes: it consumes bytes as they arrive and checks incrementally.
func (term *terminal) expect(t *testing.T, substr string) {
	t.Helper()
	if strings.Contains(term.acc.String(), substr) {
		return
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case b, ok := <-term.bytes:
			if !ok {
				t.Fatalf("socket closed waiting for %q; got %q", substr, term.acc.String())
			}
			term.acc.WriteByte(b)
			if strings.Contains(term.acc.String(), substr) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q; got %q", substr, term.acc.String())
		}
	}
}

// tryExpect is the NON-FATAL bounded variant of expect: it returns true if substr appears within d,
// false on timeout — never failing the test. For retry-until-async-subscription-live loops where a
// comms line sent before a per-player hear-set subscription is established (the async login →
// config-publish → gate-subscribe round-trip) may legitimately be missed, so the test retries to a
// deterministic flowing/stopped state instead of sleep-and-hope against the round-trip latency.
func (term *terminal) tryExpect(substr string, d time.Duration) bool {
	if strings.Contains(term.acc.String(), substr) {
		return true
	}
	deadline := time.After(d)
	for {
		select {
		case b, ok := <-term.bytes:
			if !ok {
				return false
			}
			term.acc.WriteByte(b)
			if strings.Contains(term.acc.String(), substr) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// expectClose asserts the gate closed the socket (the reader saw EOF) within a deadline
// — the observable a player gets when the connection is dropped. It drains any trailing
// bytes so a final message (e.g. a disconnect notice) still lands in acc for inspection.
func (term *terminal) expectClose(t *testing.T) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case b, ok := <-term.bytes:
			if !ok {
				return // reader saw EOF: the socket is closed.
			}
			term.acc.WriteByte(b)
		case <-deadline:
			t.Fatalf("socket did not close within deadline; got %q", term.acc.String())
		}
	}
}

// close drops the player's socket end (the player hanging up / disconnecting) and waits
// for the gate's handle goroutine to return, so a follow-on reconnect is race-free.
func (term *terminal) close(t *testing.T) {
	t.Helper()
	_ = term.conn.Close()
	if term.done == nil {
		return
	}
	select {
	case <-term.done:
	case <-time.After(10 * time.Second):
		t.Fatal("gate handle did not return after socket close")
	}
}

// (a generic poll-until-condition helper lived here; removed as unused — the journey
// tests sync on observable socket output via term.expect, not out-of-band polling.)

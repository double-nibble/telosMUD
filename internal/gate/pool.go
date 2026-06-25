package gate

import (
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// pool caches one gRPC client connection per shard address. On a cross-shard
// redirect the gate dials the target by address (Redirect.target_shard_addr); a
// pool means a popular destination shard is dialed once and the conn is shared by
// every player who walks there, rather than a fresh dial per redirect. gRPC
// ClientConns are safe for concurrent use, so a single conn multiplexes all of a
// shard's Play streams.
//
// The pool is the gate's dial seam: tests inject a dialer that returns bufconn
// clients, production uses the default insecure dialer. It holds no per-player
// state — just the address->conn map — so the gate stays stateless beyond live
// sockets.
type pool struct {
	dial func(addr string) (playv1.PlayClient, error)

	mu    sync.Mutex
	cache map[string]playv1.PlayClient
}

// newPool builds a pool whose dial uses a real (insecure) gRPC client per address.
// TLS/mTLS for inter-service traffic arrives with the transport-security phase.
func newPool() *pool {
	return newPoolWithDialer(func(addr string) (playv1.PlayClient, error) {
		cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, err
		}
		return playv1.NewPlayClient(cc), nil
	})
}

// newPoolWithDialer builds a pool over a custom dialer. Tests use this to point
// the pool at in-process bufconn shards.
func newPoolWithDialer(dial func(addr string) (playv1.PlayClient, error)) *pool {
	return &pool{dial: dial, cache: make(map[string]playv1.PlayClient)}
}

// client returns the (cached) Play client for addr, dialing once on first use.
// grpc.NewClient is lazy, so this never blocks on a connection; the actual TCP/TLS
// handshake happens when a stream is opened.
func (p *pool) client(addr string) (playv1.PlayClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.cache[addr]; ok {
		return c, nil
	}
	c, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	p.cache[addr] = c
	return c, nil
}

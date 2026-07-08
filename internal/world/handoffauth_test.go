package world

import (
	"crypto/ed25519"
	"testing"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
)

// stubLocator is a no-op directory: its mere presence (dir != nil) makes a shard DISCOVERABLE — the signal
// the boot guard keys on. Embedding the interface satisfies it without implementing every method (the guard
// only checks nil-ness, never calls it).
type stubLocator struct{ Locator }

// TestCheckHandoffAuth (#251) pins the boot guard: a shard that can RECEIVE a handoff — discoverable in the
// directory (dir != nil) OR wired with a peer dialer — with NO handoff verify key must fail the check, so cmd
// refuses to boot rather than accept UNAUTHENTICATED handoffs. A single-shard deployment (neither) is fine
// keyless, and a keyed one passes. The guard keys on DISCOVERABILITY (the receive signal), not only the send
// dialer — a receive-only standby (dir set, no dialer) is still a valid destination (distsys review).
func TestCheckHandoffAuth(t *testing.T) {
	dialer := HandoffDialer(func(string) (handoffv1.HandoffClient, error) { return nil, nil })
	dir := Locator(stubLocator{})
	pub, _, _ := ed25519.GenerateKey(nil)

	// Single-shard (no directory, no dialer): keyless is fine — it never receives a handoff.
	if err := NewMultiShard([]string{"midgaard"}, "midgaard", "", nil, nil).CheckHandoffAuth(); err != nil {
		t.Fatalf("single-shard keyless must pass, got %v", err)
	}

	// Every multi-shard SHAPE that can receive a handoff must FAIL loud when keyless.
	keylessShapes := map[string]*Shard{
		"send-capable (peer dialer)":      NewMultiShard([]string{"midgaard"}, "midgaard", "addr", nil, dialer),
		"discoverable (directory)":        NewMultiShard([]string{"midgaard"}, "midgaard", "addr", dir, nil),
		"receive-only standby (dir only)": NewMultiShard([]string{"midgaard"}, "midgaard", "addr", dir, nil),
		"both dir + dialer":               NewMultiShard([]string{"midgaard"}, "midgaard", "addr", dir, dialer),
	}
	for name, s := range keylessShapes {
		if err := s.CheckHandoffAuth(); err == nil {
			t.Fatalf("%s without a handoff verify key must fail the boot check", name)
		}
	}

	// A discoverable shard WITH a verify key passes.
	keyed := NewMultiShard([]string{"midgaard"}, "midgaard", "addr", dir, dialer).WithHandoffKeys(nil, pub)
	if err := keyed.CheckHandoffAuth(); err != nil {
		t.Fatalf("multi-shard with a verify key must pass, got %v", err)
	}
}

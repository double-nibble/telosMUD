package world

import (
	"crypto/ed25519"
	"testing"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
)

// TestCheckHandoffAuth (#251) pins the boot guard: a MULTI-SHARD deployment (a peer dialer is wired) with NO
// handoff verify key must fail the check, so cmd refuses to boot rather than accept UNAUTHENTICATED handoffs.
// A single-shard deployment (no peer dialer) is fine keyless, and a keyed multi-shard passes.
func TestCheckHandoffAuth(t *testing.T) {
	dialer := HandoffDialer(func(string) (handoffv1.HandoffClient, error) { return nil, nil })
	pub, _, _ := ed25519.GenerateKey(nil)

	// Single-shard (peers == nil): keyless is fine — it never receives a handoff.
	if err := NewMultiShard([]string{"midgaard"}, "midgaard", "", nil, nil).CheckHandoffAuth(); err != nil {
		t.Fatalf("single-shard keyless must pass, got %v", err)
	}

	// Multi-shard (peer dialer wired) but NO verify key: must fail loud.
	multiKeyless := NewMultiShard([]string{"midgaard"}, "midgaard", "addr", nil, dialer)
	if err := multiKeyless.CheckHandoffAuth(); err == nil {
		t.Fatal("multi-shard without a handoff verify key must fail the boot check")
	}

	// Multi-shard WITH a verify key: passes.
	multiKeyed := NewMultiShard([]string{"midgaard"}, "midgaard", "addr", nil, dialer).WithHandoffKeys(nil, pub)
	if err := multiKeyed.CheckHandoffAuth(); err != nil {
		t.Fatalf("multi-shard with a verify key must pass, got %v", err)
	}
}

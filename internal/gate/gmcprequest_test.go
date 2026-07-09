package gate

import (
	"log/slog"
	"testing"
)

// TestGMCPHandlerForwardsWhitelistedOnly pins #92's gate half: the inbound handler relays ONLY whitelisted
// request packages (Char.Items.Contents) to the world; Core.* is handled locally and everything else is
// dropped — a client can't push arbitrary GMCP into the world.
func TestGMCPHandlerForwardsWhitelistedOnly(t *testing.T) {
	req := make(chan gmcpForward, 4)
	h := gmcpHandler(newGMCPState(), nil, req, slog.Default())

	h("Char.Items.Contents", []byte(`{"container":"i5"}`)) // whitelisted -> forwarded
	h("Char.Vitals", []byte(`{"hp":1}`))                   // not whitelisted -> dropped (no such request)
	h("Core.Supports.Set", []byte(`["Char 1"]`))           // Core.* handled locally -> not forwarded

	select {
	case f := <-req:
		if f.pkg != "Char.Items.Contents" {
			t.Fatalf("forwarded %q, want Char.Items.Contents", f.pkg)
		}
		if string(f.json) != `{"container":"i5"}` {
			t.Fatalf("forwarded payload = %q", f.json)
		}
	default:
		t.Fatal("Char.Items.Contents was not forwarded to the world")
	}

	select {
	case f := <-req:
		t.Fatalf("a non-whitelisted package was forwarded to the world: %q", f.pkg)
	default: // correct: nothing else forwarded
	}
}

// TestGMCPHandlerForwardDropsWhenFull: a full forward channel drops the request rather than blocking the
// line pump (the client re-asks).
func TestGMCPHandlerForwardDropsWhenFull(t *testing.T) {
	req := make(chan gmcpForward, 1)
	req <- gmcpForward{pkg: "Char.Items.Contents"} // fill it
	h := gmcpHandler(newGMCPState(), nil, req, slog.Default())
	// Must not block or panic; the second request is simply dropped.
	h("Char.Items.Contents", []byte(`{"container":"i5"}`))
	if len(req) != 1 {
		t.Fatalf("forward channel len = %d, want 1 (the extra request dropped, not queued/blocked)", len(req))
	}
}

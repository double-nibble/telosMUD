package directory

import (
	"context"
	"testing"
	"time"
)

// templateuse_test.go — #416: the instance-template in-use signal.
//
// The content-pull prune guard asks the directory "is this zone hosted?" and gets its answer from the zone
// LEASE. An instance takes no lease, and a dungeon template is typically not in cfg.Zones at all, so a
// template with live copies and parties inside them answered "no". These pin the signal that closes it.

// TestTemplateInUseRoundTrip is the basic contract: absent means nobody is running copies, present means
// somebody is, and a re-publish from a second shard is not an error (more than one shard may hold copies).
func TestTemplateInUseRoundTrip(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	// ABSENT IS A DEFINITE NO, not an error. The TTL makes absence the steady state, so this is the answer
	// the guard reads for almost every zone it checks.
	inUse, err := d.TemplateInUse(ctx, "crypt")
	if err != nil {
		t.Fatalf("TemplateInUse on an unused template: %v", err)
	}
	if inUse {
		t.Fatal("a template nobody is running read as in-use; every pack would then be unprunable forever")
	}

	if err := d.SetTemplatesInUse(ctx, []string{"crypt"}, "shard-a", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if inUse, err := d.TemplateInUse(ctx, "crypt"); err != nil || !inUse {
		t.Fatalf("after a shard claimed it: inUse=%v err=%v, want true,nil", inUse, err)
	}

	// A SECOND shard running copies of the same template re-writes the same key. Last writer wins, and that
	// is correct for the question being asked — the key lives as long as ANY shard keeps renewing it, which
	// is exactly "somebody is using this".
	if err := d.SetTemplatesInUse(ctx, []string{"crypt"}, "shard-b", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if inUse, err := d.TemplateInUse(ctx, "crypt"); err != nil || !inUse {
		t.Fatalf("after a second shard claimed it: inUse=%v err=%v, want true,nil", inUse, err)
	}

	// A BATCH claims every template in one write — the shape the shard actually uses.
	if err := d.SetTemplatesInUse(ctx, []string{"tomb", "vault"}, "shard-a", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"tomb", "vault"} {
		if inUse, err := d.TemplateInUse(ctx, ref); err != nil || !inUse {
			t.Fatalf("batched claim of %q: inUse=%v err=%v, want true,nil", ref, inUse, err)
		}
	}

	// Templates do not bleed into each other.
	if inUse, err := d.TemplateInUse(ctx, "darkwood"); err != nil || inUse {
		t.Fatalf("an unrelated template: inUse=%v err=%v, want false,nil", inUse, err)
	}
}

// TestTemplateInUsePropagatesARedisError is the fail-closed half, tested against the REAL adapter rather
// than a fake.
//
// Absence of the key means "nobody is using this template" — a definite no, and the answer that lets a pack
// be pruned. So a Redis failure must never be allowed to look like absence: it has to propagate, up through
// the prune guard, and abort the pull. The director-side mapping is pinned in zonelocator_test.go against a
// fake; this pins that the adapter it wraps actually produces an error to map.
func TestTemplateInUsePropagatesARedisError(t *testing.T) {
	d, mr := newTestRedisWithClock(t)
	mr.Close() // the directory is now unreachable

	inUse, err := d.TemplateInUse(context.Background(), "crypt")
	if err == nil {
		t.Fatalf("an unreachable directory returned inUse=%v with no error; that is indistinguishable from "+
			"\"nobody is using this template\", which is exactly the fail-open a prune guard must not have", inUse)
	}
	if inUse {
		t.Fatal("an errored lookup also reported in-use; the bool must be meaningless when err != nil")
	}
}

// TestTemplateInUseExpires pins the property that makes this cheap enough to be worth having: the claim is
// TTL'd, so a CRASHED shard's claim reaps itself.
//
// This is the whole reason instances get a heartbeat rather than a lease. A zone lease is a permanent Redis
// key carrying an immortal generation — the fence that makes a captured AdoptZone single-use (#315) — so
// leasing an ephemeral, player-minted ref would leak a key per dungeon run and reopen that replay window.
// A TTL'd claim keyed by the AUTHORED template has neither problem: bounded keyspace, self-reaping, and
// nothing is signed against it.
func TestTemplateInUseExpires(t *testing.T) {
	d, mr := newTestRedisWithClock(t)
	ctx := context.Background()

	if err := d.SetTemplatesInUse(ctx, []string{"crypt"}, "shard-a", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if inUse, _ := d.TemplateInUse(ctx, "crypt"); !inUse {
		t.Fatal("precondition: the claim should be live immediately after publishing")
	}

	// The shard crashes: nothing renews the claim. Advance the server clock past the TTL rather than
	// sleeping — miniredis expires on its own clock, and a wall-clock sleep here would be both slow and a
	// flake.
	mr.FastForward(31 * time.Second)

	inUse, err := d.TemplateInUse(ctx, "crypt")
	if err != nil {
		t.Fatal(err)
	}
	if inUse {
		t.Fatal("a template-in-use claim never expired; without expiry a crashed shard makes its templates' " +
			"packs permanently unprunable, and nothing ever reaps the key")
	}
}

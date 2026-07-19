package world

import (
	"context"
	"sync"
	"testing"
	"time"
)

// instance_templateuse_test.go — #416: the shard half of the instance-template in-use signal.
//
// The content-pull prune guard resolves "is this zone hosted" through the zone LEASE, and an instance takes
// none. So a pack whose template had forty live copies read as not-hosted and could be pruned out from
// under the parties inside them. The shard has to advertise what it is running; this is that advertisement.

// fakeTemplateUsePublisher records every claim the shard publishes.
type fakeTemplateUsePublisher struct {
	mu   sync.Mutex
	got  map[string]string // template -> the shard id that claimed it
	ttls map[string]time.Duration
	// batches records the SIZE of every write issued, so a test can assert the sweep sent one batch carrying
	// every template rather than counting calls — the mint kick publishes asynchronously on the shard's own
	// goroutine, so any absolute or delta call count races it.
	batches []int
	err     error
}

func newFakeTemplateUsePublisher() *fakeTemplateUsePublisher {
	return &fakeTemplateUsePublisher{got: map[string]string{}, ttls: map[string]time.Duration{}}
}

func (f *fakeTemplateUsePublisher) SetTemplatesInUse(_ context.Context, templates []string, shardID string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, len(templates))
	if f.err != nil {
		return f.err
	}
	for _, t := range templates {
		f.got[t] = shardID
		f.ttls[t] = ttl
	}
	return nil
}

// setErr swaps the injected failure under the lock, so a test can heal the publisher while the shard's
// publisher goroutine is running.
func (f *fakeTemplateUsePublisher) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

// reset forgets every recorded claim without swapping the publisher out. Swapping was the earlier approach
// and it was racy: the mint KICK publishes asynchronously on the shard's own goroutine, so a kick still in
// flight would land on the REPLACEMENT publisher and look like a fresh claim.
func (f *fakeTemplateUsePublisher) reset() {
	f.mu.Lock()
	f.got = map[string]string{}
	f.ttls = map[string]time.Duration{}
	f.batches = nil
	f.mu.Unlock()
}

func (f *fakeTemplateUsePublisher) snapshot() (map[string]string, map[string]time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	got := make(map[string]string, len(f.got))
	for k, v := range f.got {
		got[k] = v
	}
	ttls := make(map[string]time.Duration, len(f.ttls))
	for k, v := range f.ttls {
		ttls[k] = v
	}
	return got, ttls
}

// TestPublishTemplatesInUseAdvertisesEveryLiveTemplateOnce is the core of #416: a shard running copies
// advertises their TEMPLATES — one claim per distinct template no matter how many copies exist.
//
// The per-template rather than per-instance keying is the load-bearing detail, not an optimization. A
// template ref is authored content, so the keyspace is bounded by the pack. An instance id is minted per
// dungeon run from 128 bits of randomness: keying on those would put an unbounded, player-driven keyspace
// into the directory, which is the thing #411 declined to do when it made instances unleased.
func TestPublishTemplatesInUseAdvertisesEveryLiveTemplateOnce(t *testing.T) {
	pub := newFakeTemplateUsePublisher()
	sh, cancel := runningShardWith(t, []string{"midgaard"}, "midgaard", func(s *Shard) {
		s.WithTemplateUsePublisher(pub)
		s.shardID = "shard-a"
	})
	defer cancel()

	// Two copies of one template plus one of another: three instances, two claims.
	for _, tc := range []struct{ template, account string }{
		{"darkwood", "acct-1"},
		{"darkwood", "acct-2"},
		{"crypt", "acct-3"},
	} {
		if _, err := sh.MintInstance(context.Background(), tc.template, tc.account); err != nil {
			t.Fatalf("mint %s for %s: %v", tc.template, tc.account, err)
		}
	}

	sh.publishTemplatesInUse(context.Background())

	got, ttls := pub.snapshot()
	if len(got) != 2 || got["darkwood"] != "shard-a" || got["crypt"] != "shard-a" {
		t.Fatalf("published claims = %v; want exactly {darkwood, crypt} claimed by shard-a — one claim per "+
			"distinct TEMPLATE, never one per instance", got)
	}
	// The TTL must survive TWO missed renewals, which is the margin the design actually claims — not merely
	// "more than one interval". A claim that lapses between renewals reads to the prune guard as "nobody is
	// using this template", which is the one answer that lets the pack be stripped.
	if want := 3 * templateUseInterval; ttls["darkwood"] < want {
		t.Fatalf("claim TTL = %v with a %v renewal cadence; want >= %v so two consecutive missed renewals "+
			"cannot lapse a claim while parties are inside", ttls["darkwood"], templateUseInterval, want)
	}
	// ONE batched write for the whole sweep, not one per template. A per-template loop under a shared
	// deadline starves a random tail once the budget is spent, and map ordering makes the starved subset
	// rotate every tick — an invisible, unreproducible lapse.
	// The sweep must send ONE write carrying BOTH templates. Asserted as "some batch had size 2" rather than
	// as a call count, because each mint also kicks a single-template claim asynchronously and a count would
	// be racing those. A per-template loop would only ever produce batches of size 1.
	pub.mu.Lock()
	batches := append([]int(nil), pub.batches...)
	pub.mu.Unlock()
	batched := false
	for _, n := range batches {
		if n == 2 {
			batched = true
		}
	}
	if !batched {
		t.Fatalf("write batch sizes were %v; the sweep must send every template in ONE round trip, because a "+
			"per-template loop under a shared deadline starves a rotating random tail once the budget is spent",
			batches)
	}
}

// TestTemplateUseHeartbeatRunsOnItsOwnTicker is the wiring test, and it is the one that would have caught
// the shape this started as.
//
// The heartbeat originally rode the instance reaper's tick. That made its real cadence
// `interval + sweepDuration`, and the sweep is serial over UnhostZone with each call waiting up to
// unhostActorGrace (10s) on an actor (#419) — so five wedged instances stretched the gap past the TTL and
// lapsed EVERY claim on the shard, including healthy templates with parties inside them. A TTL sized
// against a cadence that a colocated operation can stretch without bound is a margin on paper only.
//
// Every other test here calls publishTemplatesInUse directly, so deleting the call site would leave them all
// green while #416 was fully re-opened. This one drives the real goroutine.
func TestTemplateUseHeartbeatRunsOnItsOwnTicker(t *testing.T) {
	interval := templateUseInterval
	templateUseInterval = 10 * time.Millisecond
	t.Cleanup(func() { templateUseInterval = interval })

	pub := newFakeTemplateUsePublisher()
	sh, cancel := runningShardWith(t, []string{"midgaard"}, "midgaard", func(s *Shard) {
		s.WithTemplateUsePublisher(pub)
		s.shardID = "shard-a"
	})
	defer cancel()

	if _, err := sh.MintInstance(context.Background(), "darkwood", "acct-1"); err != nil {
		t.Fatalf("mint: %v", err)
	}
	waitCond(t, "the heartbeat goroutine to publish the claim without anyone calling it directly", func() bool {
		got, _ := pub.snapshot()
		return got["darkwood"] == "shard-a"
	})

	// It RENEWS, rather than claiming once and stopping. A one-shot claim would expire under a live party.
	before := func() int { pub.mu.Lock(); defer pub.mu.Unlock(); return len(pub.batches) }()
	waitCond(t, "the heartbeat to renew on a later tick", func() bool {
		pub.mu.Lock()
		defer pub.mu.Unlock()
		return len(pub.batches) > before
	})
}

// TestMintKicksTheTemplateClaimImmediately. A template's FIRST live copy is the worst case the signal has:
// with no prior claim in the directory, a pull landing before the next tick reads "nobody is using this" for
// a zone a party is standing in. The mint advertises on creation so the lifecycle is
// advertise-on-create → renew-on-tick → expire-on-death, with no cold-start hole.
func TestMintKicksTheTemplateClaimImmediately(t *testing.T) {
	// A deliberately LONG tick: if the claim appears, it can only have come from the mint kick.
	interval := templateUseInterval
	templateUseInterval = time.Hour
	t.Cleanup(func() { templateUseInterval = interval })

	pub := newFakeTemplateUsePublisher()
	sh, cancel := runningShardWith(t, []string{"midgaard"}, "midgaard", func(s *Shard) {
		s.WithTemplateUsePublisher(pub)
		s.shardID = "shard-a"
	})
	defer cancel()

	if _, err := sh.MintInstance(context.Background(), "darkwood", "acct-1"); err != nil {
		t.Fatalf("mint: %v", err)
	}
	waitCond(t, "the mint to advertise its template without waiting for a tick", func() bool {
		got, _ := pub.snapshot()
		return got["darkwood"] == "shard-a"
	})
}

// TestPublishTemplatesInUseClaimsAnInFlightMint. The instance record exists from RESERVATION — before the
// zone is built and published — and that record is positive evidence that somebody is minting copies of this
// template right now.
//
// The asymmetry decides it: over-advertising costs at most one TTL of a template staying unprunable after a
// mint that failed, while under-advertising strips a pack out from under a live party. So a reserved record
// counts, and the earlier "skip anything not yet in s.zones" filter was optimizing precision on a signal
// whose entire job is to fail closed.
func TestPublishTemplatesInUseClaimsAnInFlightMint(t *testing.T) {
	pub := newFakeTemplateUsePublisher()
	sh, cancel := runningShardWith(t, []string{"midgaard"}, "midgaard", func(s *Shard) {
		s.WithTemplateUsePublisher(pub)
		s.shardID = "shard-a"
	})
	defer cancel()

	// A reservation with no published zone behind it: exactly the state a mint occupies while buildZone and
	// seedZone run, which is hundreds of milliseconds to seconds.
	if err := sh.reserveInstanceSlot("darkwood#inflight", "darkwood", "acct-1"); err != nil {
		t.Fatalf("reserve a slot: %v", err)
	}

	sh.publishTemplatesInUse(context.Background())

	if got, _ := pub.snapshot(); got["darkwood"] != "shard-a" {
		t.Fatalf("a mint IN FLIGHT did not advertise its template (claims = %v); the guard would read the "+
			"template as unused and let a pull prune the pack the copy is being built from", got)
	}
}

// TestPublishTemplatesInUseIsSilentWithNoInstances. The signal must not become a blanket veto: a shard with
// no copies claims nothing, so a template nobody is running stays prunable. Otherwise the guard would
// eventually refuse every prune and operators would learn to bypass it.
func TestPublishTemplatesInUseIsSilentWithNoInstances(t *testing.T) {
	pub := newFakeTemplateUsePublisher()
	sh, cancel := runningShardWith(t, []string{"midgaard"}, "midgaard", func(s *Shard) {
		s.WithTemplateUsePublisher(pub)
	})
	defer cancel()

	sh.publishTemplatesInUse(context.Background())

	if got, _ := pub.snapshot(); len(got) != 0 {
		t.Fatalf("a shard with no instances published %v; it must claim nothing", got)
	}
}

// TestPublishTemplatesInUseStopsClaimingAReapedTemplate. The claim is a heartbeat, not a registration: once
// the last copy of a template is gone the shard simply stops renewing, and the key ages out. Nothing has to
// delete it, which is what keeps a crashed shard from leaving a permanent claim behind.
func TestPublishTemplatesInUseStopsClaimingAReapedTemplate(t *testing.T) {
	pub := newFakeTemplateUsePublisher()
	sh, cancel := runningShardWith(t, []string{"midgaard"}, "midgaard", func(s *Shard) {
		s.WithTemplateUsePublisher(pub)
		s.shardID = "shard-a"
	})
	defer cancel()

	inst, err := sh.MintInstance(context.Background(), "darkwood", "acct-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Wait for the claim to arrive via the ASYNC mint kick, not just via a direct publish. That wait is what
	// makes the rest of this test deterministic: it proves the kick has already been consumed, so no late
	// kick can land after the reset below and masquerade as a re-claim.
	waitCond(t, "the mint kick to publish darkwood's claim", func() bool {
		got, _ := pub.snapshot()
		return got["darkwood"] == "shard-a"
	})

	// Retire the copy, then forget the recorded claims so the next sweep's output is the only thing observed.
	// The real key is not deleted in production either — it expires on its TTL, which is the whole point of
	// a heartbeat: stop renewing and it goes away on its own.
	if err := sh.UnhostZone(context.Background(), inst.id); err != nil {
		t.Fatalf("unhost the instance: %v", err)
	}
	pub.reset()
	sh.publishTemplatesInUse(context.Background())

	if got, _ := pub.snapshot(); len(got) != 0 {
		t.Fatalf("the shard re-claimed %v after its last copy was reaped; a stale claim makes the pack "+
			"permanently unprunable", got)
	}
}

// TestPublishTemplatesInUseRecoversFromAPublishFailure. A directory blip must degrade to "this tick's claims
// lapse and the next tick re-establishes them" — the property the code's own comment promises. Asserting
// only that nothing panicked would pass for a publisher that gave up permanently after one error.
func TestPublishTemplatesInUseRecoversFromAPublishFailure(t *testing.T) {
	pub := newFakeTemplateUsePublisher()
	pub.setErr(context.DeadlineExceeded)
	sh, cancel := runningShardWith(t, []string{"midgaard"}, "midgaard", func(s *Shard) {
		s.WithTemplateUsePublisher(pub)
		s.shardID = "shard-a"
	})
	defer cancel()

	if _, err := sh.MintInstance(context.Background(), "darkwood", "acct-1"); err != nil {
		t.Fatalf("mint: %v", err)
	}

	sh.publishTemplatesInUse(context.Background())
	if got, _ := pub.snapshot(); len(got) != 0 {
		t.Fatalf("a failing publisher recorded %v", got)
	}
	// The sweep must still run: the reaper is the only thing that retires instances, and letting a directory
	// blip stop it would trade a transient guard gap for a genuine instance leak.
	sh.reapIdleInstances(context.Background())

	pub.setErr(nil)
	sh.publishTemplatesInUse(context.Background())
	if got, _ := pub.snapshot(); got["darkwood"] != "shard-a" {
		t.Fatalf("the claim was not re-established after the directory recovered (claims = %v); a publish "+
			"failure must lapse one tick, not stop the heartbeat", got)
	}
}

// TestPublishTemplatesInUseIsANoOpWithoutAPublisher. A single-shard or test deployment has no directory and
// no fleet-coordinated pull to guard against, so the whole mechanism has to be optional — the same shape
// every other directory port on the shard takes.
func TestPublishTemplatesInUseIsANoOpWithoutAPublisher(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()

	if _, err := sh.MintInstance(context.Background(), "darkwood", "acct-1"); err != nil {
		t.Fatalf("mint: %v", err)
	}
	sh.publishTemplatesInUse(context.Background()) // no publisher wired: must be a clean no-op
}

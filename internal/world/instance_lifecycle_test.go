package world

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// instance_lifecycle_test.go — #411 (slice 2 of #72): minting, capping and reaping zone instances.
//
// Slice 1 (#410) proved an instance ROUTES correctly once one exists. This file is about the lifecycle that
// creates and destroys one, and about the infrastructure exclusions an instance needs in order not to break
// things that were written before instances existed. Those exclusions are the security-relevant half: every
// one of them is a bug that ships the moment a mint path exists, and several of them (the durable dupe, the
// loot oracle, the SIGTERM drop) cannot be found by testing instances in isolation.
//
// The identity/caps/ingress tests are pure and fast. The lifecycle tests run a real shard so the reaper and
// the drain exercise the same s.mu ordering production does.

// runningShard boots a shard on the demo pack and blocks until its Run loop has published runCtx (which
// MintInstance requires). Returns the shard and its cancel.
func runningShard(t *testing.T, zones []string, home string) (*Shard, context.CancelFunc) {
	t.Helper()
	return runningShardWith(t, zones, home, nil)
}

// runningShardWith is runningShard with a hook to apply With* options BEFORE Run starts.
//
// The hook is not a convenience — it is the contract. The With* options are construction-time: they write
// shard fields that the running shard then READS from other goroutines (instanceLimits is read under mu on
// every mint), so calling one after `go sh.Run(ctx)` is a data race. Several of these tests did exactly that
// and were the working precedent for doing it wrong, which is how the pattern spreads. Configure here.
func runningShardWith(t *testing.T, zones []string, home string, configure func(*Shard)) (*Shard, context.CancelFunc) {
	t.Helper()
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatalf("load embedded demo pack: %v", err)
	}
	sh := NewShardFromContent(lc, zones, home, "", nil, nil)
	if configure != nil {
		configure(sh)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go sh.Run(ctx)
	waitCond(t, "shard Run to publish its run context", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && sh.runWG != nil
	})
	return sh, cancel
}

// withLimits builds a runningShardWith configure hook that sets the instance caps.
func withLimits(perAccount, perShard, mintBurst int, mintWindow time.Duration) func(*Shard) {
	return func(sh *Shard) { sh.WithInstanceLimits(perAccount, perShard, mintBurst, mintWindow) }
}

// mustMint mints an instance or fails the test.
func mustMint(t *testing.T, sh *Shard, template, account string) *Zone {
	t.Helper()
	z, err := sh.MintInstance(context.Background(), template, account)
	if err != nil {
		t.Fatalf("MintInstance(%q, %q): %v", template, account, err)
	}
	return z
}

// --- identity -------------------------------------------------------------------------------------------

// TestInstanceIDShape pins the id grammar every other predicate in this change reads. `#` is the whole
// mechanism: it is outside the authored ref charset, so an instance id can never be an authored zone, and
// parseRef splits on `:` only so it is transparent to the routing path.
func TestInstanceIDShape(t *testing.T) {
	cases := []struct {
		name         string
		id           string
		wantInstance bool
		wantTemplate string
		wantSerial   string
	}{
		{"authored zone", "darkwood", false, "darkwood", ""},
		{"authored zone with colons", "pack:zone:crypt", false, "pack:zone:crypt", ""},
		{"instance", "darkwood#deadbeef", true, "darkwood", "deadbeef"},
		{"instance of a colonful template", "a:b#ff00", true, "a:b", "ff00"},
		{"empty", "", false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isInstanceID(tc.id); got != tc.wantInstance {
				t.Fatalf("isInstanceID(%q) = %v, want %v", tc.id, got, tc.wantInstance)
			}
			tmpl, serial, ok := splitInstanceID(tc.id)
			if ok != tc.wantInstance || tmpl != tc.wantTemplate || serial != tc.wantSerial {
				t.Fatalf("splitInstanceID(%q) = (%q,%q,%v), want (%q,%q,%v)",
					tc.id, tmpl, serial, ok, tc.wantTemplate, tc.wantSerial, tc.wantInstance)
			}
			// The zone-side predicate must agree with the id-side one — they are two views of ONE definition,
			// and a disagreement is how an instance ends up excluded from one guard but not another.
			z := &Zone{id: tc.id, template: tc.wantTemplate}
			if z.isInstance() != tc.wantInstance {
				t.Fatalf("Zone{id:%q}.isInstance() = %v, want %v", tc.id, z.isInstance(), tc.wantInstance)
			}
		})
	}
}

// TestInstanceSerialIsUnguessable is the anti-counter test. A MONOTONIC serial would make live instance ids
// enumerable, which composes with the id-seeded loot stream into a farming oracle (see
// TestInstanceLootStreamsDiverge). 128 bits of crypto/rand is the requirement; this asserts the observable
// consequences of it — width, and no repetition across many mints.
func TestInstanceSerialIsUnguessable(t *testing.T) {
	const n = 500
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		id, err := mintInstanceID("crypt")
		if err != nil {
			t.Fatalf("mintInstanceID: %v", err)
		}
		_, serial, ok := splitInstanceID(id)
		if !ok {
			t.Fatalf("minted id %q is not instance-shaped", id)
		}
		if len(serial) != instanceSerialBytes*2 {
			t.Fatalf("serial %q is %d hex chars, want %d (%d random bytes) — a short serial is a guessable one",
				serial, len(serial), instanceSerialBytes*2, instanceSerialBytes)
		}
		if seen[serial] {
			t.Fatalf("serial %q repeated within %d mints — the serial is not random", serial, n)
		}
		// A counter would produce serials that sort into a dense run; a duplicate check alone would not catch
		// "1,2,3...". Assert the value is not a small integer in disguise.
		if strings.TrimLeft(serial, "0") == "" || len(strings.TrimLeft(serial, "0")) < instanceSerialBytes {
			t.Fatalf("serial %q looks like a small counter, not 128 random bits", serial)
		}
		seen[serial] = true
	}
}

// --- mint-sink validation -------------------------------------------------------------------------------

// TestMintInstanceValidatesTemplate pins the MINT SINK's own validation. It deliberately does not lean on the
// load-time charset lint: refcharset.go's scope note says ref-VALUED fields under other names (exit targets,
// reset protos, Lua string literals) are not charset-checked, so a `#` can reach a new sink from content that
// loaded cleanly. Since the `#` exclusion is what makes an instance id unforgeable, it is re-checked here.
func TestMintInstanceValidatesTemplate(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()

	cases := []struct {
		name     string
		template string
		account  string
		wantErr  string
	}{
		{"empty template", "", "acct", "no template zone named"},
		{"template containing the separator", "darkwood#1", "acct", "may not contain"},
		{"separator anywhere at all", "dark#wood", "acct", "may not contain"},
		{"unknown zone", "no-such-zone", "acct", "no such zone in loaded content"},
		{"no account to charge", "darkwood", "", "no account"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			z, err := sh.MintInstance(context.Background(), tc.template, tc.account)
			if err == nil {
				t.Fatalf("MintInstance(%q, %q) succeeded (zone %q); want a refusal", tc.template, tc.account, z.id)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
			// A refused mint must leave NO trace: no zone, and no cap slot consumed.
			sh.mu.Lock()
			live := len(sh.instances)
			sh.mu.Unlock()
			if live != 0 {
				t.Fatalf("a refused mint left %d instance record(s) behind — the cap slot leaked", live)
			}
		})
	}
}

// TestMintInstanceBuildsFromTemplate is the happy path: the zone is live, hosted, built from the TEMPLATE's
// content despite no content existing under its own id, and its rooms carry the template's AUTHORED refs
// (which is what lets every instance share the shard's immutable protoCache).
func TestMintInstanceBuildsFromTemplate(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()

	z := mustMint(t, sh, "darkwood", "acct-1")
	if !z.isInstance() {
		t.Fatalf("minted zone %q is not instance-shaped", z.id)
	}
	if z.template != "darkwood" {
		t.Fatalf("template = %q, want darkwood", z.template)
	}
	if sh.ZoneByID(z.id) != z {
		t.Fatal("the minted instance is not hosted on the shard (routing cannot find it)")
	}
	if len(z.rooms) == 0 || z.rooms["darkwood:room:grove"] == nil {
		t.Fatalf("instance has no darkwood rooms (%d rooms) — buildZone must resolve content by template", len(z.rooms))
	}
	if z.startRoom != "darkwood:room:grove" {
		t.Fatalf("start room = %q, want the template's darkwood:room:grove", z.startRoom)
	}
	// The template zone itself is untouched: an instance is a COPY, never a rename.
	if sh.ZoneByID("darkwood") == z {
		t.Fatal("the mint returned the template zone itself")
	}
	// Two mints of one template are two distinct zones with distinct room maps — the isolation boundary.
	z2 := mustMint(t, sh, "darkwood", "acct-2")
	if z2 == z || z2.id == z.id {
		t.Fatal("two mints produced the same zone")
	}
	if z2.rooms["darkwood:room:grove"] == z.rooms["darkwood:room:grove"] {
		t.Fatal("two instances share a room entity — they are not isolated copies")
	}
}

// TestMintInstanceTakesNoLease is why MintInstance has its own build+adopt path instead of reusing HostZone.
// HostZone's tail arms lease renewal, which would write the ephemeral instance ref into the directory on the
// very first mint (a permanent Redis key per dungeon run, since releaseZone never deletes a zone hash) and
// later fire unadoptZone against it when the adoption it thinks it is waiting for never confirms.
func TestMintInstanceTakesNoLease(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	leaser := newFakeLeaser()
	sh := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "", nil, nil).
		WithZoneLeasing(leaser, "shard-a", time.Second, 10*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)
	waitCond(t, "shard running", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil
	})

	z := mustMint(t, sh, "darkwood", "acct-1")

	// Give the (nonexistent) renewer several renewal periods to write something.
	time.Sleep(60 * time.Millisecond)
	leaser.mu.Lock()
	claims := leaser.claims[z.id]
	releases := leaser.releases[z.id]
	leaser.mu.Unlock()
	if claims != 0 || releases != 0 {
		t.Fatalf("instance %q touched the directory lease (claims=%d releases=%d); instances must be UNLEASED",
			z.id, claims, releases)
	}
	sh.mu.Lock()
	_, renewing := sh.leaseStop[z.id]
	sh.mu.Unlock()
	if renewing {
		t.Fatal("a lease-renewal goroutine was armed for an instance — that is HostZone's tail, which MintInstance must not reuse")
	}
}

// --- caps -----------------------------------------------------------------------------------------------

// TestInstanceCapsAreChargedToTheAccount pins WHO pays. A per-character or per-script cap routes around
// trivially (alts; one script minting for many players), and quiescence is not a bound on its own because a
// link-dead session stays in z.players for its whole grace window — so a handful of accounts could otherwise
// squat every instance slot on the shard.
func TestInstanceCapsAreChargedToTheAccount(t *testing.T) {
	sh, cancel := runningShardWith(t, []string{"midgaard"}, "midgaard", withLimits(2, 100, 100, time.Minute))
	defer cancel()

	// One account, two different TEMPLATES: the cap is on the account, not on the content, so the second
	// template still consumes the same quota.
	mustMint(t, sh, "darkwood", "acct-1")
	mustMint(t, sh, "crypt", "acct-1")
	if _, err := sh.MintInstance(context.Background(), "darkwood", "acct-1"); err == nil {
		t.Fatal("a third mint for an account limited to 2 succeeded")
	} else if !strings.Contains(err.Error(), "limit 2") {
		t.Fatalf("unexpected refusal: %v", err)
	}
	// A DIFFERENT account is unaffected — the cap partitions by account, it is not a global-in-disguise.
	mustMint(t, sh, "darkwood", "acct-2")
}

// TestInstanceGlobalCapBoundsTheShard: the per-account cap bounds one principal; only a global cap bounds the
// SHARD, which is the resource that actually runs out (a zone object, an actor goroutine and a Lua VM each).
func TestInstanceGlobalCapBoundsTheShard(t *testing.T) {
	sh, cancel := runningShardWith(t, []string{"midgaard"}, "midgaard", withLimits(100, 2, 100, time.Minute))
	defer cancel()

	mustMint(t, sh, "darkwood", "acct-1")
	mustMint(t, sh, "darkwood", "acct-2")
	if _, err := sh.MintInstance(context.Background(), "darkwood", "acct-3"); err == nil {
		t.Fatal("a mint past the shard's global instance cap succeeded")
	} else if !strings.Contains(err.Error(), "instance capacity") {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

// TestInstanceMintRateIsLimitedPerAccount: the concurrent cap alone does not see cheap-to-mint,
// cheap-to-abandon churn — mint, leave, mint again — which is real build work (every room spawned, every
// boot reset run) on the shard for free. The rate limit is what bounds that.
func TestInstanceMintRateIsLimitedPerAccount(t *testing.T) {
	sh, cancel := runningShardWith(t, []string{"midgaard"}, "midgaard", withLimits(100, 100, 2, time.Hour))
	defer cancel()

	mustMint(t, sh, "darkwood", "acct-1")
	mustMint(t, sh, "darkwood", "acct-1")
	if _, err := sh.MintInstance(context.Background(), "darkwood", "acct-1"); err == nil {
		t.Fatal("a mint past the account's rate limit succeeded")
	} else if !strings.Contains(err.Error(), "too fast") {
		t.Fatalf("unexpected refusal: %v", err)
	}
	// Rate is per account, like the concurrent cap.
	mustMint(t, sh, "darkwood", "acct-2")
}

// TestUnhostingAnInstanceReturnsItsCapSlot: the cap must be on LIVE instances, not on lifetime mints, or a
// long-lived shard eventually refuses every account permanently.
func TestUnhostingAnInstanceReturnsItsCapSlot(t *testing.T) {
	sh, cancel := runningShardWith(t, []string{"midgaard"}, "midgaard", withLimits(1, 100, 100, time.Minute))
	defer cancel()

	z := mustMint(t, sh, "darkwood", "acct-1")
	if _, err := sh.MintInstance(context.Background(), "darkwood", "acct-1"); err == nil {
		t.Fatal("the per-account cap did not bite")
	}
	if err := sh.UnhostZone(context.Background(), z.id); err != nil {
		t.Fatalf("UnhostZone: %v", err)
	}
	mustMint(t, sh, "darkwood", "acct-1") // the slot came back
}

// --- reaper ---------------------------------------------------------------------------------------------

// TestReaperRetiresAnIdleInstance drives the sweep directly (no ticker) so the assertion is deterministic.
// The mint grace is stepped over by rewinding the record's mint time, which is what a real idle instance's
// clock would have done.
func TestReaperRetiresAnIdleInstance(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()

	z := mustMint(t, sh, "darkwood", "acct-1")
	agePastGrace(sh, z.id)

	// One tick short of the threshold: still hosted. The multi-tick requirement is deliberate — a single
	// unlucky sample must not reap a dungeon.
	for i := 0; i < instanceIdleTicks-1; i++ {
		sh.reapIdleInstances(context.Background())
	}
	if sh.ZoneByID(z.id) == nil {
		t.Fatalf("instance reaped after only %d consecutive idle ticks; the threshold is %d",
			instanceIdleTicks-1, instanceIdleTicks)
	}
	sh.reapIdleInstances(context.Background())
	if sh.ZoneByID(z.id) != nil {
		t.Fatal("an instance idle for the full threshold was not reaped")
	}
	sh.mu.Lock()
	rec := sh.instances[z.id]
	sh.mu.Unlock()
	if rec != nil {
		t.Fatal("the reaped instance's record (and its cap slot) survived the teardown")
	}
}

// TestReaperHonorsThePostMintGrace: entry is a SEPARATE mechanism (slice 3) that necessarily runs after the
// mint returns, so without a grace window every instance would be reapable before anyone could walk into it.
func TestReaperHonorsThePostMintGrace(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()

	z := mustMint(t, sh, "darkwood", "acct-1") // freshly minted: inside the grace, and quiescent
	for i := 0; i < instanceIdleTicks*3; i++ {
		sh.reapIdleInstances(context.Background())
	}
	if sh.ZoneByID(z.id) == nil {
		t.Fatal("a freshly-minted, not-yet-entered instance was reaped inside its post-mint grace")
	}
}

// TestReaperSparesAnOccupiedInstance: quiescence is the reap predicate, and an occupied zone is never
// quiescent. Includes the link-dead shape implicitly — a session in z.players counts whether or not its
// socket is alive, which is exactly why the CAPS (not quiescence) are what bound squatting.
//
// There is exactly ONE guard here, not two. The reaper's `!z.quiescent()` candidate filter reads the SAME
// three atomics UnhostZone re-checks under s.mu — just earlier, and outside the binding hold — so it is a
// stale sample of the one guard, not an independent layer. Only UnhostZone's re-check is load-bearing, which
// is why this test asserts the OUTCOME (the zone survived) rather than either mechanism.
//
// What the heuristic uniquely buys is two things the re-check cannot give: TEMPORAL HYSTERESIS (N consecutive
// quiescent ticks, so a momentarily-empty dungeon between two players' hops is not reaped — see
// TestReaperRetiresAnIdleInstance), and COST (it avoids an UnhostZone call, and its up-to-10s actor wait, on
// every tick for every occupied instance on the shard).
func TestReaperSparesAnOccupiedInstance(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()

	z := mustMint(t, sh, "darkwood", "acct-1")
	agePastGrace(sh, z.id)
	// Occupy it off the zone goroutine the same way the drain's poll observes occupancy: through the atomic
	// pop mirror. (Placing a real session needs the entry path, which is slice 3.)
	z.pop.Add(1)
	defer z.pop.Add(-1)

	for i := 0; i < instanceIdleTicks*3; i++ {
		sh.reapIdleInstances(context.Background())
	}
	if sh.ZoneByID(z.id) == nil {
		t.Fatal("an OCCUPIED instance was reaped — the party was torn out of its own dungeon")
	}
}

// TestReaperCannotRaceAnEnteringPlayer is the entering-vs-reaping race, pinned.
//
// The claim is: quiescent() folds in `incoming` (#409), `incoming` is taken by claimTransferTarget in the
// SAME hold of s.mu that resolves the destination, and UnhostZone re-checks quiescent() under that same
// mutex. So a player in flight into an instance cannot be reaped out from under.
//
// This test opens exactly that window — a claim taken, the arrival not yet dequeued — and asserts the sweep
// refuses. The idle COUNTING is only a heuristic for picking candidates; it is UnhostZone's re-check that is
// load-bearing, so the test forces the candidate to be picked (full idle budget) and then checks the zone
// survived anyway.
func TestReaperCannotRaceAnEnteringPlayer(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()

	z := mustMint(t, sh, "darkwood", "acct-1")
	agePastGrace(sh, z.id)

	// Take the arrival claim exactly as an intra-shard transfer into this instance would.
	if got := sh.claimTransferTarget(z.id); got != z {
		t.Fatalf("claimTransferTarget(%q) = %v, want the instance", z.id, got)
	}
	if z.quiescent() {
		t.Fatal("a zone with an in-flight arrival reads as quiescent — #409's counter is not wired into the instance path")
	}
	for i := 0; i < instanceIdleTicks*3; i++ {
		sh.reapIdleInstances(context.Background())
	}
	if sh.ZoneByID(z.id) == nil {
		t.Fatal("an instance with a player IN FLIGHT toward it was reaped; the arrival would land on a dead inbox")
	}
	// Release the claim; now it reaps.
	z.incoming.Add(-1)
	for i := 0; i < instanceIdleTicks; i++ {
		sh.reapIdleInstances(context.Background())
	}
	if sh.ZoneByID(z.id) != nil {
		t.Fatal("the instance was never reaped after its in-flight arrival cleared")
	}
}

// TestUnhostInstanceSkipsTheDirectoryRead: UnhostZone's ownership check fails CLOSED, and nothing ever
// re-leases an instance, so a directory blip during a reaper sweep would otherwise leak that instance — its
// zone object, actor goroutine, Lua VM and cap slot — for the life of the process. An instance has no lease
// to read in the first place.
func TestUnhostInstanceSkipsTheDirectoryRead(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	leaser := &erroringLeaser{fakeLeaser: newFakeLeaser()}
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "", nil, nil).
		WithZoneLeasing(leaser, "shard-a", time.Second, time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)
	waitCond(t, "shard running", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil
	})

	inst := mustMint(t, sh, "darkwood", "acct-1")

	// The control: a NORMAL zone still fails closed on the unreadable directory. If this ever stops failing,
	// the test below proves nothing.
	if err := sh.UnhostZone(context.Background(), "darkwood"); err == nil {
		t.Fatal("UnhostZone of a leased zone succeeded despite an unreadable directory — the fail-closed guard is gone")
	}
	if err := sh.UnhostZone(context.Background(), inst.id); err != nil {
		t.Fatalf("UnhostZone of an INSTANCE failed on a directory error it should never have consulted: %v", err)
	}
	if n := leaser.leaseReads(inst.id); n != 0 {
		t.Fatalf("UnhostZone read the directory lease %d time(s) for an unleased instance", n)
	}
}

// --- persistent resets: the durable item dupe -----------------------------------------------------------

// TestPersistentResetRefusedInsideAnInstance is a CARDINALITY assertion, per the #69 lesson: it asserts ZERO
// loader calls rather than "no duplicate observed", because the dupe here is produced by N separate zones
// each loading correctly-once — no single zone ever misbehaves, so only counting the calls can see it.
//
// LoadObjects is keyed by the AUTHORED room ref, identical in every instance, and persistentDone is per-zone
// so it does not dedup between them. N instances would each spawn their own live copy of a UNIQUE durable
// object, which players can loot and carry out into the shared world.
func TestPersistentResetRefusedInsideAnInstance(t *testing.T) {
	op := content.ResetDTO{Op: "spawn_item", Proto: "midgaard:obj:helmet", Room: "test:room:vault", Persistent: true}

	newVault := func(id, template string) (*Zone, *stubObjectLoader) {
		z := newZone(id)
		z.template = template
		r := &Entity{rid: z.rids.alloc(), proto: "test:room:vault", zone: z, comps: componentSet{}}
		z.rooms["test:room:vault"] = r
		z.protos.define("midgaard:obj:helmet", []string{"helmet"}, "an iron helmet", "An iron helmet rests here.", componentSet{})
		loader := &stubObjectLoader{
			objects: []PersistentObject{{ProtoRef: "midgaard:obj:helmet"}},
			done:    make(chan struct{}, 8),
		}
		z.objects = loader
		return z, loader
	}

	// The CONTROL first: an authored zone still loads its durable objects. Without this the assertion below
	// would also pass if persistent resets were broken outright.
	tz, tloader := newVault("test", "test")
	tz.applyReset(&op)
	select {
	case <-tloader.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the authored zone never attempted its persistent load; the control is broken")
	}

	// Two instances of one template. Neither may touch the loader AT ALL.
	//
	// The load is issued on its OWN goroutine (resetPersistent does its I/O off the zone goroutine), so
	// reading the counter straight after applyReset would race the very call being asserted absent — it would
	// pass whether or not the refusal exists. The wait on the loader's own signal is what makes the absence
	// real; the control above establishes that a call, when it happens, arrives well inside this window.
	for _, id := range []string{"test#aaaa", "test#bbbb"} {
		z, loader := newVault(id, "test")
		z.applyReset(&op)
		z.applyReset(&op) // and a repop tick, for good measure
		select {
		case <-loader.done:
			t.Fatalf("instance %q issued a durable LoadObjects call; every instance loading the same authored "+
				"ref is a live duplicate of a unique durable object, which players can loot and carry out", id)
		case <-time.After(500 * time.Millisecond):
		}
		if n := loader.calls.Load(); n != 0 {
			t.Fatalf("instance %q made %d durable LoadObjects call(s), want 0", id, n)
		}
	}
}

// TestNoTimedRepopInsideAnInstance: a template that respawns its boss on the reset timer while the party is
// standing in the lair is farmable — the same instance yields the full loot table every reset_secs forever.
// The BOOT reset still runs (the dungeon is populated exactly once), which is the instanced semantic.
func TestNoTimedRepopInsideAnInstance(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()

	inst := mustMint(t, sh, "darkwood", "acct-1")
	if inst.repopPulse != nil {
		t.Fatal("an instance registered a timed repop pulse")
	}
	// Control: the authored zone DOES repop, so the assertion above is about instancing and not about the
	// demo pack having no reset_secs.
	if tz := sh.ZoneByID("darkwood"); tz == nil {
		t.Skip("this shard does not host the authored darkwood; nothing to compare against")
	}
	// The boot reset still populated the instance.
	if len(inst.rooms["darkwood:room:lair"].contents) == 0 {
		t.Fatal("the instance's boot reset placed nothing; suppressing repop must not suppress the boot fill")
	}
}

// --- RNG ------------------------------------------------------------------------------------------------

// TestInstanceLootStreamsDiverge is the loot-oracle test.
//
// `lootRNG := z.lua.rng` (death.go) and z.lua.rng is seeded by a plain FNV-1a over the zone id (luart.go
// seedFromZoneID). So without a per-mint salt, every mint restarts the loot stream at index 0 from a seed
// that is a pure function of the id — precompute which serials drop the legendary and mint until one comes
// up. Seeding from the TEMPLATE instead is equally wrong in the other direction: every instance would then
// roll identically.
//
// Both failure modes are asserted: divergence between two mints, AND divergence from the plain id-derived
// stream (which is what "we forgot to salt" looks like).
func TestInstanceLootStreamsDiverge(t *testing.T) {
	draw := func(z *Zone) []int {
		out := make([]int, 8)
		for i := range out {
			out[i] = z.lua.rng.Intn(1 << 30)
		}
		return out
	}
	a := newInstanceZone("darkwood#aaaa", "darkwood")
	b := newInstanceZone("darkwood#bbbb", "darkwood")
	sa, sb := draw(a), draw(b)
	if equalInts(sa, sb) {
		t.Fatal("two instances produced IDENTICAL loot streams — the seed is derived from the template, so every copy rolls the same drops")
	}

	// The unsalted shape: seeded purely from the id. Reproduce it and require the real instance to differ.
	unsalted := newZone("darkwood#aaaa")
	if equalInts(sa, draw(unsalted)) {
		t.Fatal("an instance's script/loot stream is exactly seedFromZoneID(id) — it is offline-computable from the id, which is a loot oracle")
	}

	// The combat stream is salted too, for the same reason newZone entropy-seeds it.
	if a.combatRand.Int63() == b.combatRand.Int63() {
		t.Fatal("two instances share a combat RNG stream")
	}
}

// --- metrics --------------------------------------------------------------------------------------------

// TestInstanceMetricsLabelByTemplate: an instance id is minted per dungeon run, so as an OTel attribute it is
// unbounded, player-driven cardinality — thousands of dead series. The zone LOGGER keeps the id (an operator
// needs to know WHICH copy misbehaved); logs and metrics deliberately want opposite answers.
func TestInstanceMetricsLabelByTemplate(t *testing.T) {
	inst := newInstanceZone("darkwood#aaaa", "darkwood")
	if got := inst.metricZone(); got != "darkwood" {
		t.Fatalf("metric label for %q = %q, want the template darkwood", inst.id, got)
	}
	plain := newZone("darkwood")
	if got := plain.metricZone(); got != "darkwood" {
		t.Fatalf("metric label for an authored zone = %q, want its own id", got)
	}
}

// TestInstanceGaugeCountsItsOwnTemplate. The gauge is LABELED by template, so its VALUE has to be the count
// for that template. Reporting the shard-wide total against one template label is wrong twice over: with two
// templates live every series reads the same total (each over-reports by the other's load), and — the part
// that does not self-correct — when the LAST instance of a template is reaped, its series is set to the
// remaining total and never sampled again, so it reports a nonzero count for a template with zero live
// instances FOREVER. This is the one gauge the metrics doc points operators at for instanced load.
//
// It asserts the value that REACHES THE GAUGE, through a real OTel reader, rather than re-asserting
// instanceCountLocked. That distinction is the test's whole value and it was learned the hard way: the first
// version called the real UnhostZone and then checked instanceCountLocked, so it passed green while UnhostZone
// was handing the gauge `len(s.instances)` — the shard-wide total — right next to the narration condemning it.
// A metric test that does not read the metric asserts the helper, not the call site, and the call sites are
// where this bug lives (the reaper's teardown path is UnhostZone; releaseInstanceSlot only ever runs on a
// FAILED mint).
func TestInstanceGaugeCountsItsOwnTemplate(t *testing.T) {
	rdr := instanceGaugeReader(t)
	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()

	a1 := mustMint(t, sh, "darkwood", "acct-1")
	a2 := mustMint(t, sh, "darkwood", "acct-2")
	mustMint(t, sh, "crypt", "acct-3")

	if got, want := instanceGauge(t, rdr, "darkwood"), int64(2); got != want {
		t.Fatalf("series{template=darkwood} = %d, want %d (the shard-wide total is 3 — reporting THAT against "+
			"the darkwood label over-reports it by every other template's load, and every series then reads the "+
			"same number)", got, want)
	}
	if got, want := instanceGauge(t, rdr, "crypt"), int64(1); got != want {
		t.Fatalf("series{template=crypt} = %d, want %d", got, want)
	}

	// Retire every darkwood instance THROUGH UnhostZone, which is the path the reaper takes. Its series must
	// go to ZERO — not to the surviving crypt's count, which is what the shard-wide total leaves behind
	// permanently: nothing ever samples series{template=darkwood} again, so it reports a live count for a
	// template with none for the life of the process.
	for _, z := range []*Zone{a1, a2} {
		if err := sh.UnhostZone(context.Background(), z.id); err != nil {
			t.Fatalf("UnhostZone(%q): %v", z.id, err)
		}
	}
	if got := instanceGauge(t, rdr, "darkwood"); got != 0 {
		t.Fatalf("after UnhostZone retired the last darkwood instance, series{template=darkwood} = %d, want 0. "+
			"UnhostZone reported the shard-wide instance total against darkwood's label; the surviving crypt "+
			"instance is what that %d is, and nothing will ever sample this series again", got, got)
	}
	if got, want := instanceGauge(t, rdr, "crypt"), int64(1); got != want {
		t.Fatalf("retiring darkwood's instances moved series{template=crypt} to %d, want %d", got, want)
	}
	// And the helper agrees with the gauge, so a future change cannot fix one and drift the other.
	sh.mu.Lock()
	helper := sh.instanceCountLocked("crypt")
	sh.mu.Unlock()
	if helper != 1 {
		t.Fatalf("instanceCountLocked(crypt) = %d, want 1", helper)
	}
}

// instanceGaugeReader installs a ManualReader as the process's meter provider so the world tests can read
// what the SetInstances call sites actually recorded. OTel's global delegation re-binds the instruments the
// metrics package created in its init(), so this works after the fact — but only ONCE per process (a second
// provider would not pick up the already-delegated instruments), hence the sync.Once.
func instanceGaugeReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	instanceGaugeOnce.Do(func() {
		instanceGaugeRdr = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(instanceGaugeRdr)))
	})
	return instanceGaugeRdr
}

var (
	instanceGaugeOnce sync.Once
	instanceGaugeRdr  *sdkmetric.ManualReader
)

// instanceGauge reads the current value of telos.zone.instances for one template label, or -1 when that
// series does not exist. -1 rather than 0 so "never recorded" is distinguishable from "recorded as empty" —
// the difference between a missing call site and a correct teardown.
func instanceGauge(t *testing.T, rdr *sdkmetric.ManualReader, template string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "telos.zone.instances" {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("telos.zone.instances is %T, want a Gauge[int64]", m.Data)
			}
			for _, dp := range g.DataPoints {
				if v, found := dp.Attributes.Value(attribute.Key("template")); found && v.AsString() == template {
					return dp.Value
				}
			}
		}
	}
	return -1
}

// --- scope registration ---------------------------------------------------------------------------------

// TestMintedInstanceReceivesRegionDeltas closes the loop the resolver test only half covers.
// TestInstanceResolvesItsRegionByTemplate proves regionFor ANSWERS "heartlands" for an instance; this proves
// a real region delta actually REACHES a minted instance, which additionally requires MintInstance to call
// registerZone (so sr.zoneRegion holds the instance's synthetic id) — region delivery iterates that map, not
// the hosted-zone list, so a missing registration is silent: region:get simply reads empty in every copy of
// the dungeon and content authored against the template is inert with no error anywhere.
func TestMintedInstanceReceivesRegionDeltas(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "", nil, nil)
	sh.WithScopeBus(scopebus.New(commbus.NewMemBus()), lc.Regions)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)
	waitCond(t, "shard Run to publish its run context", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && sh.runWG != nil
	})

	inst := mustMint(t, sh, "darkwood", "acct-1")

	sh.scopes.mu.RLock()
	region := sh.scopes.zoneRegion[inst.id]
	sh.scopes.mu.RUnlock()
	if region != "heartlands" {
		t.Fatalf("sr.zoneRegion[%q] = %q, want heartlands: MintInstance did not register the instance for "+
			"region delivery, so every region delta addressed to heartlands skips it silently", inst.id, region)
	}

	// Now the delivery itself. Region delivery iterates sr.zoneRegion and posts to each member, so this is the
	// half that proves the registration above is load-bearing rather than merely present.
	//
	// A SECOND instance, registered exactly as MintInstance registers one but with no actor running: the
	// inbox is then a stable observation point (a live actor would consume the message out from under the
	// assertion, and z.scopes.region is zone-goroutine-owned so reading it from here would be a race).
	quiet := newInstanceZone("darkwood#quiet", "darkwood")
	quiet.shard = sh
	sh.adopt(quiet.id, quiet)
	sh.scopes.registerZone(quiet)

	payload, err := json.Marshal(scopebus.StatePayload{Key: "war", Value: json.RawMessage(`true`)})
	if err != nil {
		t.Fatal(err)
	}
	sh.scopes.onScopeEvent("region", "heartlands", scopebus.EventStateSet, payload)

	var delivered bool
	for len(quiet.inbox) > 0 {
		if _, ok := (<-quiet.inbox).(scopeDeltaMsg); ok {
			delivered = true
		}
	}
	if !delivered {
		t.Fatalf("a region STATE delta addressed to heartlands never reached instance %q. Region delivery "+
			"iterates sr.zoneRegion, so an unregistered instance is skipped SILENTLY: region:get reads empty "+
			"in every copy of the dungeon and content authored against the template is simply inert", quiet.id)
	}
}

// TestInstanceScopeRegistrationBalances: mint registers, teardown unregisters. Nothing pinned this, and an
// unbalanced pair leaks one sr.zoneRegion entry per dungeon run for the life of the process — a map that
// grows with player activity and never shrinks, holding a zone id whose zone is long gone.
func TestInstanceScopeRegistrationBalances(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "", nil, nil)
	sh.WithScopeBus(scopebus.New(commbus.NewMemBus()), lc.Regions)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)
	waitCond(t, "shard Run to publish its run context", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && sh.runWG != nil
	})

	registered := func(id string) bool {
		sh.scopes.mu.RLock()
		defer sh.scopes.mu.RUnlock()
		_, ok := sh.scopes.zoneRegion[id]
		return ok
	}

	inst := mustMint(t, sh, "darkwood", "acct-1")
	if !registered(inst.id) {
		t.Fatal("a minted instance was never registered for scope replication")
	}
	// Reap it through the REAPER's own path, so the balance is asserted across the real mint→reap cycle rather
	// than a hand-rolled teardown.
	agePastGrace(sh, inst.id)
	for i := 0; i < instanceIdleTicks; i++ {
		sh.reapIdleInstances(context.Background())
	}
	if sh.ZoneByID(inst.id) != nil {
		t.Fatal("the idle instance was not reaped; the balance assertion below would be vacuous")
	}
	if registered(inst.id) {
		t.Fatalf("the reaped instance %q is still in sr.zoneRegion: one entry leaks per dungeon run, forever, "+
			"each naming a zone that no longer exists", inst.id)
	}
}

// --- helpers --------------------------------------------------------------------------------------------

// agePastGrace rewinds an instance's recorded mint time so the reaper's post-mint grace no longer applies.
// It is the clock the test cannot wait out, moved rather than slept through.
func agePastGrace(sh *Shard, id string) {
	sh.mu.Lock()
	if rec := sh.instances[id]; rec != nil {
		rec.minted = time.Now().Add(-2 * instanceMintGrace)
	}
	sh.mu.Unlock()
}

// erroringLeaser is a ZoneLeaser whose ZoneLease READ always fails — a directory blip — while its claim path
// works. It counts reads per zone so a test can assert a read never happened at all.
type erroringLeaser struct {
	*fakeLeaser
	reads map[string]int
}

func (e *erroringLeaser) ZoneLease(_ context.Context, zoneID string) (string, uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.reads == nil {
		e.reads = map[string]int{}
	}
	e.reads[zoneID]++
	return "", 0, context.DeadlineExceeded
}

func (e *erroringLeaser) leaseReads(zoneID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reads[zoneID]
}

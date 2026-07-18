package world

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
)

// instance_entry_test.go — #72 (slice 3): ENTRY, the exit ANCHOR, and EXIT.
//
// The three slices divide cleanly and so do their tests. #410 proved an instance ROUTES correctly once one
// exists; #411 proved one can be created, capped and destroyed without breaking anything written before
// instances existed. This file is about a player being INSIDE one — the first slice in which an instance is
// reachable at all, and therefore the first in which the durable story, the drain, and the respawn path have a
// live subject.
//
// The security-relevant tests are the harm gate on the entry primitive (a forced relocation into
// attacker-chosen private content is the worst movement-grief the engine can express) and the account
// provenance (the caps are charged per account, so a content- or client-supplied account makes every cap
// self-service). The correctness-relevant ones are the anchor's write sites, the async gap's re-validation,
// and the drain eject's claim discipline.
//
// EVERYTHING HERE IS ASSERTED THROUGH OBSERVABLE STATE — the durable store, the placement queue, which zone
// answers a presence probe, and what the player is told — never by reading zone-owned session fields from the
// test goroutine. That is not fastidiousness: s.anchorZone is written by a zone actor, so reading it from here
// while the shard runs is precisely the single-writer violation this package exists to prevent, and -race
// would be right to fail on it.

// --- helpers ----------------------------------------------------------------------------------------------

// entryShard boots a running shard hosting midgaard + darkwood, with a MemStore behind it, and with the crypt
// available as an instance TEMPLATE but NOT hosted.
//
// That asymmetry is the realistic authoring shape and it is load-bearing for this file: a dungeon template is
// content a shard can build from, not a zone anybody stands in. It is also exactly the shape the design panel
// pointed at when it killed the "project the template ref into the durable row" story — ShardForZone("crypt")
// resolves nobody, so a player whose record named it would be unroutable.
func entryShard(t *testing.T, configure ...func(*Shard)) (*Shard, *MemStore, context.CancelFunc) {
	t.Helper()
	mem := NewMemStore()
	sh, cancel := runningShardWith(t, []string{"midgaard", "darkwood"}, "midgaard", func(sh *Shard) {
		sh.WithPersistence(mem, nil)
		sh.shardID = "shard-a"
		for _, c := range configure {
			c(sh)
		}
	})
	return sh, mem, cancel
}

// idleShard builds a shard on the demo pack whose Run was NEVER started, so nothing drains the mint queue.
//
// That is what makes the enqueue OBSERVABLE. Every "did this refusal queue a build?" assertion in this file
// depends on the queue holding still, and on a running shard it cannot: the mint workers are pulling from the
// other end, so an accepted request is consumed before the test can look. An idle shard is also the honest
// model of the thing under test — requestInstanceEntry's whole contract is that it hands work off and returns,
// so who eventually picks that work up is somebody else's test.
func idleShard(t *testing.T) *Shard {
	t.Helper()
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatalf("load embedded demo pack: %v", err)
	}
	return NewShardFromContent(lc, nil, "", "", nil, nil)
}

// entrantIn places a hand-made session in a room of z as a real resident. It mirrors what a connection has (an
// out channel and a currentZone routing pointer) so the transfer paths under test repoint it exactly as they
// would in production, and it seeds a durable row so the save path has something to CAS against.
func entrantIn(t *testing.T, mem *MemStore, z *Zone, name string, room ProtoRef) *session {
	t.Helper()
	pid, err := mem.CreateCharacter(context.Background(), name, z.id, string(room))
	if err != nil {
		t.Fatalf("seed durable row for %s: %v", name, err)
	}
	cur := &atomic.Pointer[Zone]{}
	cur.Store(z)
	s := &session{
		character:   name,
		out:         make(chan *playv1.ServerFrame, 256),
		epoch:       1,
		currentZone: cur,
		account:     "acct-" + name,
	}
	z.newPlayerEntity(s, name)
	s.entity.pid = &pid
	placeTestPlayer(t, z, s, room)
	waitResident(t, z, name)
	return s
}

// waitResident blocks until a zone's goroutine reports it holds the character.
func waitResident(t *testing.T, z *Zone, name string) {
	t.Helper()
	waitCond(t, "the character "+name+" to be resident in "+z.id, func() bool {
		present, ok := probePresent(z, name)
		return ok && present
	})
}

// zoneHolding reports which of the shard's hosted zones currently holds the character, asking each zone's own
// goroutine. It is the observable "where is this player" — and, for the entry tests, the observable that
// distinguishes a copy from the template it was built from.
func zoneHolding(sh *Shard, name string) *Zone {
	for _, z := range sh.zonesList() {
		if present, ok := probePresent(z, name); ok && present {
			return z
		}
	}
	return nil
}

// enterInstance drives the REAL two-phase entry flow for an already-resident player and blocks until they are
// inside the minted instance. It returns the instance zone.
//
// It queues the request on the shard's mint queue directly rather than calling requestInstanceEntry, and that
// split is deliberate rather than a shortcut. requestInstanceEntry must run ON the entrance zone's goroutine
// (it reads and writes session state), and a test cannot get onto a RUNNING zone's goroutine without adding a
// production hook — so the two halves are covered separately: its validation is unit-tested directly against a
// non-running zone (TestRequestInstanceEntryRefusals), and everything downstream of the enqueue — the worker,
// the off-goroutine build, the reply, the re-validation, the claim, the transfer — is exercised here on real
// goroutines, which is where all the concurrency is.
func enterInstance(t *testing.T, sh *Shard, origin *Zone, s *session, template string, anchorRoom ProtoRef) *Zone {
	t.Helper()
	queueEntry(sh, origin, s, template, anchorRoom)
	var inst *Zone
	waitCond(t, "the player to arrive inside a minted instance of "+template, func() bool {
		for _, z := range sh.zonesList() {
			if !z.isInstance() || z.template != template {
				continue
			}
			if present, ok := probePresent(z, s.character); ok && present {
				inst = z
				return true
			}
		}
		return false
	})
	return inst
}

// queueEntry enqueues one mint request without waiting for its outcome.
func queueEntry(sh *Shard, origin *Zone, s *session, template string, anchorRoom ProtoRef) {
	sh.mintQ <- instanceMintReq{
		origin:     origin,
		originRoom: anchorRoom,
		character:  s.character,
		template:   template,
		account:    s.account,
	}
}

// flushByQuit forces the durable write path the anchor rides — a clean leave dumps the character on the zone
// goroutine (dumpCharacter -> durableLocation) and hands the snapshot to the async saver — and returns once
// the store has actually taken the write.
//
// It waits on the row's STATE_VERSION advancing, not on the row's contents. Waiting on the contents is the
// trap this had first: every caller seeds a durable row before the run, so a predicate like `ZoneRef ==
// "midgaard"` is already satisfied by the SEED and returns the pre-quit row — which then asserts against
// state the logout never wrote, and passes or fails for reasons unrelated to the code under test. The version
// is the only field guaranteed to differ between "the row I seeded" and "the row the logout wrote".
func flushByQuit(t *testing.T, z *Zone, mem *MemStore, name string) CharSnapshot {
	t.Helper()
	before, _, err := mem.LoadCharacter(context.Background(), name)
	if err != nil {
		t.Fatalf("read the pre-logout row for %s: %v", name, err)
	}
	z.post(leaveMsg{id: name})
	var got CharSnapshot
	waitCond(t, "the logout flush for "+name+" to reach the store", func() bool {
		row, found, err := mem.LoadCharacter(context.Background(), name)
		if err != nil || !found || row.StateVersion <= before.StateVersion {
			return false
		}
		got = row
		return true
	})
	return got
}

// startRoomlessContent builds a one-zone pack whose zone declares rooms but no usable start_room — the shape
// MintInstance must refuse.
func startRoomlessContent(t *testing.T, startRoom string) *content.LoadedContent {
	t.Helper()
	src := content.NewMemSource()
	src.SetPack(content.Pack{
		Pack: "voidpack",
		Zones: []content.ZoneDTO{{
			Ref:       "voidcrypt",
			Name:      "The Void Crypt",
			StartRoom: startRoom, // "" => declares none; a bogus ref => declares one that does not exist
			// The #72 content opt-in, granted so the START-ROOM refusal is what this fixture actually
			// exercises. Without it the mint is refused one check earlier, for not being instanceable, and the
			// start-room assertions below would pass against the wrong error forever.
			Instanceable: true,
			Rooms:        []content.RoomDTO{{Ref: "voidcrypt:room:hall", Name: "A Hall", Long: "A hall."}},
		}},
	})
	lc, err := content.Load(context.Background(), src, []string{"voidpack"})
	if err != nil {
		t.Fatalf("load the start-roomless test pack: %v", err)
	}
	return lc
}

// assertTold drains a session's outbound frames looking for a substring, without blocking.
func assertTold(t *testing.T, s *session, substr string) {
	t.Helper()
	for {
		select {
		case f := <-s.out:
			if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), substr) {
				return
			}
		default:
			t.Fatalf("player %s was never told %q", s.character, substr)
		}
	}
}

// --- SECURITY: the non-consenting-target gate -------------------------------------------------------------

// TestSendToInstanceIsSelfOnly is the security headline of this slice.
//
// A forced relocation of another player is HARM — the judgement h:teleport and h:recall already make — and
// forcing one into an INSTANCE is the most severe form of it available: the destination is across a zone
// boundary, is private, is chosen by the script's author, and the victim cannot be seen or reached from
// outside it. Without a gate, builder-authored content in any room a player can stand in could pull them out
// of a safe temple into an attacker-chosen dungeon.
//
// THE GATE IS SELF-ONLY, not mayRelocate. This test previously asserted mayRelocate's behavior — including a
// "consenting victim is accepted" control — and that was wrong, because mayRelocate does not cover this
// primitive's threat model: it self-exempts, and (the hole the security panel found) it does not gate a MOB
// ACTOR AT ALL, since guardHarmful -> pvpAllowed short-circuits on !isPlayer(actor) before the safe-room veto.
// Any mob script in a temple could therefore have pulled a non-consenting player into a private dungeon. So the
// rule under test is now the blanket one: the target must BE the invoking actor.
//
// The claim is that the gate runs FIRST — before the template is validated, before the session is resolved,
// before anything is sent — so a refused call is indistinguishable from one that never happened, and in
// particular does not leak whether the named template exists. It is therefore asserted as ZERO MINT REQUESTS
// QUEUED, not merely as a `false` return: a false that still queued the build has already done the harm.
func TestSendToInstanceIsSelfOnly(t *testing.T) {
	sh := idleShard(t) // nothing drains the mint queue, so "was a build queued?" stays observable
	// A bare (non-running) zone standing in for the entrance. Entering its Lua runtime directly from the test
	// goroutine is safe precisely because this zone has no actor of its own.
	z, rt, room := harmZone(t)
	z.shard = sh
	attacker := harmPlayer(z, room, "Attacker")
	victim := harmPlayer(z, room, "Victim")
	// Both sessions carry a verified account, so the ONLY variable in this test is the harm gate. Without it
	// every case would refuse for the account reason instead and the test would pass while proving nothing.
	for _, name := range []string{"Attacker", "Victim"} {
		z.players[name].account = "acct-" + name
	}
	// The victim is a NON-CONSENTING player: no pvp flag, so guardHarmful refuses. This is the ordinary shape
	// of most players in most rooms, not a contrived one.
	setFlag(attacker, flagPvP, true)
	rt.L.SetGlobal("victim", rt.newHandle(victim))
	rt.L.SetGlobal("attacker", rt.newHandle(attacker))

	queued := func() int {
		n := 0
		for {
			select {
			case <-sh.mintQ:
				n++
			default:
				return n
			}
		}
	}
	returned := func() bool { return rt.L.GetGlobal("ok").String() == "true" }

	if err := rt.runChunkWithSelf("grief", `ok = mud.send_to_instance(victim, 'crypt')`, attacker); err != nil {
		t.Fatal(err)
	}
	if returned() {
		t.Fatal("mud.send_to_instance returned true for a NON-CONSENTING player target: builder-authored " +
			"content can pull any player out of a safe room into an attacker-chosen private dungeon")
	}
	if n := queued(); n != 0 {
		t.Fatalf("mud.send_to_instance queued %d mint request(s) for a target the invoker may not harm. The "+
			"gate must run BEFORE anything else, so a refused call is a TOTAL no-op — a queued build is the "+
			"harm regardless of the return value", n)
	}

	// THE CONTROL, which matters as much as the refusal: the gate must not be so broad that the ordinary
	// dungeon-door idiom stops working. Relocating YOURSELF is always allowed (mayRelocate's own rule).
	if err := rt.runChunkWithSelf("self", `ok = mud.send_to_instance(attacker, 'crypt')`, attacker); err != nil {
		t.Fatal(err)
	}
	if !returned() {
		t.Fatal("mud.send_to_instance refused a player entering an instance THEMSELVES — the gate is too " +
			"broad and the ordinary dungeon-door idiom does not work")
	}
	if n := queued(); n != 1 {
		t.Fatalf("self-entry queued %d mint request(s), want 1", n)
	}

	// A CONSENTING victim is refused TOO, and that is the point of self-only rather than a harm gate. Consent
	// to PVP is not consent to be moved out of the observable world into somebody else's private copy of a
	// dungeon; those are different grants, and a pvp flag is not the second one.
	setFlag(victim, flagPvP, true)
	if err := rt.runChunkWithSelf("consenting", `ok = mud.send_to_instance(victim, 'crypt')`, attacker); err != nil {
		t.Fatal(err)
	}
	if returned() || queued() != 0 {
		t.Fatal("mud.send_to_instance accepted a player target because they carry the PVP flag. This primitive " +
			"is SELF-ONLY: consenting to pvp is not consenting to be relocated into private, author-chosen " +
			"content where nobody can see or help you")
	}

	// THE MOB-ACTOR HOLE — the security panel's finding, and the reason this gate is not mayRelocate.
	//
	// mayRelocate -> guardHarmful -> pvpAllowed short-circuits on !isPlayer(ACTOR) before the safe-room veto,
	// so for a mob-actor invocation it returns true UNCONDITIONALLY: none of pvp policy, safe rooms, consent or
	// spawn protection applies. Any mob script — a tick, an aggro handler, a mob-owned ability — in a temple or
	// a newbie inn could therefore transfer a non-consenting player into a private instance. The victim is
	// deliberately NON-consenting here (the flag is cleared again) so that a regression cannot hide behind the
	// pvp flag set above.
	setFlag(victim, flagPvP, false)
	cultist := harmMob(z, room, "cultist")
	if err := rt.runChunkWithSelf("mobactor", `ok = mud.send_to_instance(victim, 'crypt')`, cultist); err != nil {
		t.Fatal(err)
	}
	if returned() || queued() != 0 {
		t.Fatal("a MOB actor sent a non-consenting player into a private instance. The harm gate does not " +
			"police a mob actor (pvpAllowed short-circuits on !isPlayer(actor) BEFORE the safe-room veto), so " +
			"content could pull a player out of a safe room into author-chosen content they cannot be helped in")
	}

	// A MOB target is a clean false, never an error: a door script sweeping its room and offering entry to
	// everyone in it must not blow up on the resident shopkeeper. Instances are anchored on a SESSION (the
	// anchor, the account, the durable location, the reconnect story) and a mob has none of it.
	mob := harmMob(z, room, "shopkeeper")
	rt.L.SetGlobal("mob", rt.newHandle(mob))
	if err := rt.runChunkWithSelf("mob", `ok = mud.send_to_instance(mob, 'crypt')`, attacker); err != nil {
		t.Fatalf("mud.send_to_instance RAISED on a mob target instead of returning false: %v", err)
	}
	if returned() || queued() != 0 {
		t.Fatal("mud.send_to_instance accepted a MOB target")
	}
}

// --- entry validation -------------------------------------------------------------------------------------

// TestRequestInstanceEntryRefusals pins every refusal the entrance-zone half of entry makes, and pins each as
// "nothing was queued" rather than as a return value — a refusal that still queues a build has already spent
// the account's mint rate limit and a worker's time on a player who is not going anywhere.
//
// The ACCOUNT case is the security-relevant one. The caps are charged per ACCOUNT (a per-character cap is
// routed around by alts; a per-script cap by one script minting for many players), and a session's account
// comes only from a signature-checked assertion. A session with none — the dev/unverified path, or an arrival
// over an insecure keyless handoff, both of which STRIP it deliberately — must be refused rather than handed a
// shared bucket: an unattributable mint is an uncapped mint.
func TestRequestInstanceEntryRefusals(t *testing.T) {
	sh := idleShard(t) // nothing drains the mint queue, so "was a build queued?" stays observable

	// Each sub-test builds its own bare zone + session, so one refusal cannot mask another.
	newCase := func(instance bool) (*Zone, *session) {
		var z *Zone
		if instance {
			z = newInstanceZone("harm#deadbeef", "harm")
		} else {
			z = newZone("harm")
		}
		z.shard = sh
		room := z.newEntity("harm:room:hall")
		Add(room, &Room{exits: map[string]ProtoRef{}})
		z.rooms["harm:room:hall"] = room
		s := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 32), epoch: 1, account: "acct-1"}
		z.newPlayerEntity(s, "Hero")
		Move(s.entity, room)
		z.setPlayer("Hero", s)
		return z, s
	}

	cases := []struct {
		name    string
		mutate  func(*session)
		gated   bool
		wantMsg string // a substring the player must be told; "" for a silent refusal
	}{
		{
			// The fail-closed default. A caller that cannot answer "may this actor relocate this target" has
			// no business moving anybody, and defaulting to yes would silently un-gate every future call site.
			name: "no harm-gate decision supplied", gated: false,
		},
		{
			name: "no verified account", gated: true,
			mutate: func(s *session) { s.account = "" }, wantMsg: "does not open",
		},
		{
			name: "a mint is already in flight", gated: true,
			mutate: func(s *session) { s.instanceMintPending = true }, wantMsg: "already opening",
		},
		{
			// move() refuses to walk while fighting, and the reason is sharper here: a `fighting` pointer must
			// never cross a zone boundary, and an entry IS a zone crossing.
			name: "fighting", gated: true,
			mutate: func(s *session) { setPosition(s.entity, posFighting) }, wantMsg: "fighting",
		},
		{
			name: "frozen mid-handoff", gated: true,
			mutate: func(s *session) { s.frozen = true },
		},
		{
			name: "link-dead", gated: true,
			mutate: func(s *session) { s.detached = true },
		},
		{
			name: "not placed in a room", gated: true,
			mutate: func(s *session) { Move(s.entity, nil) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			z, s := newCase(false)
			if tc.mutate != nil {
				tc.mutate(s)
			}
			if z.requestInstanceEntry(s, "crypt", tc.gated) {
				t.Fatalf("requestInstanceEntry ACCEPTED despite: %s", tc.name)
			}
			select {
			case req := <-sh.mintQ:
				t.Fatalf("requestInstanceEntry queued a mint (%+v) despite: %s. The refusal has already spent "+
					"the account's rate limit and a worker's time", req, tc.name)
			default:
			}
			if tc.wantMsg != "" {
				assertTold(t, s, tc.wantMsg)
			}
		})
	}

	// Nesting is refused with a SENTENCE rather than a build failure. The mint sink would reject it anyway
	// (the template would carry a '#'), but the rule belongs where a content author will read it.
	t.Run("from inside an instance", func(t *testing.T) {
		z, s := newCase(true)
		if z.requestInstanceEntry(s, "crypt", true) {
			t.Fatal("requestInstanceEntry accepted an entry from INSIDE an instance (nesting)")
		}
		assertTold(t, s, "from inside one")
	})

	// A full mint queue is a clean refusal, never a block. The enqueue runs on a zone ACTOR and the queue is
	// drained by workers doing store I/O, so blocking here would turn a slow mint into a frozen entrance zone
	// — the exact failure the whole async design exists to prevent.
	t.Run("mint queue full", func(t *testing.T) {
		z, s := newCase(false)
		// A shard whose Run was never started, so nothing DRAINS the queue and it stays full for the length of
		// the assertion. Filling a running shard's queue is unstable by construction — its workers are pulling
		// from the other end.
		for i := 0; i < cap(sh.mintQ); i++ {
			sh.mintQ <- instanceMintReq{origin: z, character: "filler", template: "crypt", account: "acct-x"}
		}
		defer func() {
			for len(sh.mintQ) > 0 {
				<-sh.mintQ
			}
		}()
		done := make(chan bool, 1)
		go func() { done <- z.requestInstanceEntry(s, "crypt", true) }()
		select {
		case ok := <-done:
			if ok {
				t.Fatal("requestInstanceEntry reported success with a full mint queue")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("requestInstanceEntry BLOCKED on a full mint queue. It runs on a zone actor, so this " +
				"freezes the entrance zone — its heartbeat, its combat rounds, and every occupant's input — " +
				"until a worker drains. It must refuse instead")
		}
		assertTold(t, s, "will not open")
	})

	// THE CONTROL: an eligible player with a verified account is accepted, and the queued request carries the
	// ANCHOR ROOM they are standing in.
	t.Run("accepted", func(t *testing.T) {
		z, s := newCase(false)
		if !z.requestInstanceEntry(s, "crypt", true) {
			t.Fatal("requestInstanceEntry refused an eligible player — the refusals above are too broad")
		}
		if !s.instanceMintPending {
			t.Fatal("an accepted request did not mark the session as having a mint in flight, so a second " +
				"attempt during the (unbounded) gap would drive a second full zone build")
		}
		select {
		case req := <-sh.mintQ:
			if req.origin != z || req.character != "Hero" || req.template != "crypt" ||
				req.account != "acct-1" || req.originRoom != "harm:room:hall" {
				t.Fatalf("queued mint request = %+v, want origin=%q character=Hero template=crypt "+
					"account=acct-1 originRoom=harm:room:hall", req, z.id)
			}
		default:
			t.Fatal("an accepted request queued nothing")
		}
	})
}

// TestMintRefusesATemplateWithNoStartRoom pins the fail-loudly-at-MINT rule.
//
// A start-roomless template is not a cosmetic omission, and discovering it at a player's DEATH — the natural
// place to discover it, hours later, in the middle of somebody's dungeon run — is what this prevents. Two
// independent things break without a start room:
//
//   - Entry lands via transferIn's resolveRoom(""), which IS the start-room fallback. With none it resolves
//     nil, transferIn takes its "destination has no rooms" branch, and the ENTERING PLAYER IS DISCONNECTED,
//     mid-entry, having done nothing wrong.
//   - respawnPlayer moves the victim to resolveRoom(z.startRoom) — nil — so a player who dies at the boss is
//     revived at full health standing in the boss room. The engine has no cross-zone respawn to fall back on.
//
// A start_room naming a room the template does not declare produces the same nil one indirection later, so it
// is refused on the same terms.
func TestMintRefusesATemplateWithNoStartRoom(t *testing.T) {
	cases := []struct{ name, startRoom, wantErr string }{
		{"declares none", "", "declares no start_room"},
		{"names a room it does not declare", "voidcrypt:room:nowhere", "names no room the template declares"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sh := NewShardFromContent(startRoomlessContent(t, tc.startRoom), nil, "", "", nil, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go sh.Run(ctx)
			waitCond(t, "shard Run to publish its run context", func() bool {
				sh.mu.Lock()
				defer sh.mu.Unlock()
				return sh.runCtx != nil && sh.runWG != nil
			})
			_, err := sh.MintInstance(context.Background(), "voidcrypt", "acct-1")
			if err == nil {
				t.Fatal("MintInstance built an instance of a template with no usable start room. Entry into " +
					"one DISCONNECTS the entering player (transferIn's no-rooms branch), and a death inside " +
					"revives them standing in the room they died in")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("MintInstance error = %q, want it to mention %q", err, tc.wantErr)
			}
			// The refusal must not have consumed a cap slot: a builder's typo must not eat the account's quota.
			sh.mu.Lock()
			live := len(sh.instances)
			sh.mu.Unlock()
			if live != 0 {
				t.Fatalf("a refused mint left %d instance record(s) charged to the account", live)
			}
		})
	}

	// THE CONTROL: the demo crypt DOES declare a valid start room, so this is a validation, not a ban.
	sh, _, cancel := entryShard(t)
	defer cancel()
	if _, err := sh.MintInstance(context.Background(), "crypt", "acct-1"); err != nil {
		t.Fatalf("MintInstance refused the crypt, which declares a valid start room: %v", err)
	}
}

// TestMintRefusesANonInstanceableZone is the regression for the uncapped-faucet finding (#72 security panel).
//
// Before the opt-in, validateMintTemplate checked only three things — no '#', the zone exists, it has a start
// room — so EVERY loaded zone was an instance template. That is an uncapped item GENERATION faucet, not a dupe:
// a mint runs the template's boot resets, creating every item and mob the zone declares in a private copy; a
// player alone in that copy strips it, walks out through any foreign-zone exit (the transfer carries their
// whole inventory subtree), and mints again. It scales with mint rate times account count. Worse, it reached
// every zone in content — including zones another builder deliberately gated behind a locked door, a quest or
// a level check, because a private copy has no doorman.
//
// WHICH zones may be instanced is content's decision. The engine's job is to give content a way to MAKE that
// decision and then to enforce it, which is what this asserts on both sides.
func TestMintRefusesANonInstanceableZone(t *testing.T) {
	sh, _, cancel := entryShard(t)
	defer cancel()

	// THE REFUSAL. midgaard is the demo's town: it declares no `instanceable`, so it is not a template. A
	// private copy of a town would duplicate every shopkeeper's stock and route around every locked door in it.
	_, err := sh.MintInstance(context.Background(), "midgaard", "acct-1")
	if err == nil {
		t.Fatal("MintInstance built an instance of midgaard, which never opted in to being instanced. Every " +
			"loaded zone being a template is an uncapped item faucet (a mint runs the zone's boot resets into " +
			"a private copy the player can strip and walk out of) and routes around every in-world access gate")
	}
	if !strings.Contains(err.Error(), "not declared instanceable") {
		t.Fatalf("MintInstance error = %q, want it to name the missing opt-in", err)
	}
	// The refusal must be free: a probe for which zones are instanceable must not consume the account's quota
	// or its mint rate, or the refusal becomes its own denial-of-service against the caller.
	sh.mu.Lock()
	live := len(sh.instances)
	sh.mu.Unlock()
	if live != 0 {
		t.Fatalf("a refused mint left %d instance record(s) charged to the account", live)
	}

	// THE CONTROL, which is what keeps this from being a blanket ban: the demo's crypt DOES opt in, and mints.
	// Without this the test would pass against an engine that had simply removed instancing.
	if _, err := sh.MintInstance(context.Background(), "crypt", "acct-1"); err != nil {
		t.Fatalf("MintInstance refused the crypt, which declares `instanceable: true`: %v", err)
	}
}

// TestDemoPackInstanceableOptIn pins the demo pack's own authored answer, so a content edit that flips a town
// into an instance template has to do it deliberately rather than as an unreviewed side effect of a YAML merge.
func TestDemoPackInstanceableOptIn(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatalf("load embedded demo pack: %v", err)
	}
	want := map[string]bool{"crypt": true, "midgaard": false, "darkwood": false}
	for ref, wantOptIn := range want {
		zd := lc.Zone(ref)
		if zd == nil {
			t.Fatalf("demo pack has no zone %q", ref)
		}
		if zd.Instanceable != wantOptIn {
			t.Fatalf("demo zone %q instanceable = %v, want %v. A shared, persistent-world zone must NOT be an "+
				"instance template: a private copy duplicates its resets and has no doorman for whatever "+
				"in-world gate guards it", ref, zd.Instanceable, wantOptIn)
		}
	}
}

// TestAbandonedMintReleasesTheCapSlotEagerly covers the M3 finding: a mint that succeeded but that its entrant
// never entered must not pin the account's cap slot until the reaper gets around to it.
//
// The cost asymmetry is the point. Reaching this state takes TWO commands (ask to enter, then quit / walk away
// / engage), while the slot stays charged for the mint grace plus instanceIdleTicks reaper ticks — about three
// minutes. For an ordinary player who changed their mind that is a baffling self-lockout ("the way does not
// open" for minutes after they decided not to go); done on purpose with the per-account cap in hand it is a
// self-denial-of-service that costs nothing. Hop 3 KNOWS whether the transfer landed, so the release does not
// need to wait for a sweep.
//
// It also asserts the shape of the fix, not just the effect: the instance RECORD must survive. The reaper walks
// s.instances and resolves each record through s.zones, so a zone with no record is never visited — deleting
// the record to free the slot would trade a three-minute pin for a permanently leaked zone, actor goroutine and
// Lua VM, on a path the caller controls. That would be a strictly worse bug than the one being fixed.
func TestAbandonedMintReleasesTheCapSlotEagerly(t *testing.T) {
	// A per-account cap of ONE, so a pinned slot is directly observable as a refused second mint.
	sh, _, cancel := entryShard(t, func(sh *Shard) { sh.WithInstanceLimits(1, 64, 100, time.Minute) })
	defer cancel()

	z := sh.zoneByID("midgaard")
	if z == nil {
		t.Fatal("midgaard not hosted")
	}
	inst := mustMint(t, sh, "crypt", "acct-1")

	// Hop 3 with NO session present — the "asked to enter, then quit" path, which is the one an abuse takes
	// deliberately. It must mark the instance abandoned.
	z.instanceReady(instanceReadyMsg{character: "Ghost", template: "crypt", zoneID: inst.id})

	sh.mu.Lock()
	rec := sh.instances[inst.id]
	sh.mu.Unlock()
	if rec == nil {
		t.Fatal("abandoning the mint DELETED the instance record. The reaper iterates s.instances and resolves " +
			"each record through s.zones, so a zone with no record is never swept: this frees the cap slot by " +
			"permanently leaking the zone, its actor goroutine and its Lua VM")
	}
	if !rec.abandoned {
		t.Fatal("hop 3 abandoned the entry without marking the instance abandoned, so the account's slot stays " +
			"pinned for the mint grace plus the idle ticks (~3 minutes) at a cost of two commands")
	}

	// THE EFFECT: the same account can mint again immediately, despite a per-account cap of one.
	if _, err := sh.MintInstance(context.Background(), "crypt", "acct-1"); err != nil {
		t.Fatalf("the account could not mint after abandoning its only instance: %v. The abandoned copy is "+
			"empty and unreachable (an instance id is unguessable and never handed to content), so billing the "+
			"account for it locks them out of the mechanism for minutes", err)
	}

	// AND THE ZONE IS STILL ACCOUNTED FOR. The abandoned instance keeps counting toward the SHARD-wide cap,
	// which is an honest bound on live zones — the zone genuinely still exists until the reaper retires it.
	// Only the per-ACCOUNT bill is dropped.
	sh.mu.Lock()
	total := len(sh.instances)
	sh.mu.Unlock()
	if total != 2 {
		t.Fatalf("shard holds %d instance records, want 2 (the abandoned one plus the new one). An abandoned "+
			"instance must still count toward the per-SHARD cap: the zone exists until it is reaped", total)
	}
}

// TestReaperSkipsTheMintGraceForAnAbandonedInstance pins the other half of M3. Freeing the cap slot is only
// half the cleanup — the zone, its actor and its Lua VM are still live, and the post-mint grace would otherwise
// hold them for its full duration.
//
// The grace exists to protect a copy whose entrant has not arrived YET. For an abandoned one that premise is
// already known to be false: hop 3 decided the entrant is never arriving, and nobody else can reach the copy
// (an instance id is unguessable and is never handed to content). So the grace protects nothing and the reaper
// should not wait it out.
func TestReaperSkipsTheMintGraceForAnAbandonedInstance(t *testing.T) {
	// One quiescent tick retires. The GRACE is deliberately left at its production value — it is the thing
	// under test, so shortening it would make both halves of this test vacuous.
	defer swapReaperIdleTicks(t, 1)()

	sh, _, cancel := entryShard(t)
	defer cancel()
	z := sh.zoneByID("midgaard")
	if z == nil {
		t.Fatal("midgaard not hosted")
	}

	// THE CONTROL FIRST: a freshly-minted, NOT-abandoned instance is spared, because its entrant may still be
	// on the way. Without this the test would pass against a reaper that ignored the grace entirely — which
	// would reap dungeons out from under the parties entering them.
	pending := mustMint(t, sh, "crypt", "acct-pending")
	sh.reapIdleInstances(context.Background())
	sh.reapIdleInstances(context.Background())
	if sh.zoneByID(pending.id) == nil {
		t.Fatal("the reaper retired an instance still inside its post-mint grace. Entry necessarily happens " +
			"after the mint returns, so this reaps a copy out from under the party walking into it")
	}

	// NOW THE ABANDONED ONE, minted at the same moment, so the grace is the only difference between them.
	orphan := mustMint(t, sh, "crypt", "acct-orphan")
	z.instanceReady(instanceReadyMsg{character: "Ghost", template: "crypt", zoneID: orphan.id})
	waitCond(t, "the abandoned instance to be reaped despite its grace", func() bool {
		sh.reapIdleInstances(context.Background())
		return sh.zoneByID(orphan.id) == nil
	})
	// The control must STILL be alive: the grace skip has to be scoped to the abandoned record, not disabled.
	if sh.zoneByID(pending.id) == nil {
		t.Fatal("reaping the abandoned instance also retired the un-abandoned one — the grace skip is not " +
			"scoped to the abandoned record")
	}
}

// swapReaperIdleTicks sets how many consecutive quiescent ticks retire an instance, for one test, and returns
// a restore func. It is a package var precisely so a test need not sleep through a production cadence.
//
// It deliberately does NOT touch instanceMintGrace. The grace is what the abandoned-instance test is asserting
// about, and a helper that quietly zeroed it would make that test pass no matter what the reaper did.
func swapReaperIdleTicks(t *testing.T, idleTicks int) func() {
	t.Helper()
	old := instanceIdleTicks
	instanceIdleTicks = idleTicks
	return func() { instanceIdleTicks = old }
}

// --- the async gap ----------------------------------------------------------------------------------------

// TestInstanceEntryDeliversThePlayerIntoACopy is the entry happy path, and its headline assertion is the one
// that distinguishes this whole feature from "walk into the crypt": the player ends up in `crypt#<serial>`,
// which is a LIVE ZONE THAT DID NOT EXIST when they asked, and NOT in `crypt` itself.
//
// It also pins that entry went through the ordinary transfer path. That is not a style preference. Every
// instance allocates rids from its own 1-based allocator in the same order, so the mob in room 3 of copy A and
// the mob in room 3 of copy B carry the SAME rid — a handle resolved against the wrong copy resolves a real,
// plausible, WRONG entity, deterministically. rehomeSubtree (which only the transfer path runs) re-homing the
// arriving subtree into the destination's identity space is what prevents that, and the observable proof is
// that two live copies of the same template hold two different players without either seeing the other.
func TestInstanceEntryDeliversThePlayerIntoACopy(t *testing.T) {
	sh, mem, cancel := entryShard(t)
	defer cancel()
	origin := sh.zoneByID("midgaard")

	alice := entrantIn(t, mem, origin, "Alice", "midgaard:room:guildhall")
	bob := entrantIn(t, mem, origin, "Bob", "midgaard:room:guildhall")

	instA := enterInstance(t, sh, origin, alice, "crypt", "midgaard:room:guildhall")
	instB := enterInstance(t, sh, origin, bob, "crypt", "midgaard:room:guildhall")

	if instA == instB {
		t.Fatal("two entries landed in the SAME instance: the copies are not private, so one party can see " +
			"and loot another's dungeon run")
	}
	for _, inst := range []*Zone{instA, instB} {
		if !inst.isInstance() || inst.template != "crypt" {
			t.Fatalf("landed in %q (template %q), which is not a crypt instance", inst.id, inst.template)
		}
		if inst.id == "crypt" {
			t.Fatal("landed in the TEMPLATE zone rather than a copy of it")
		}
	}
	// Neither is visible to the other's copy — the privacy boundary #410's routing predicate defends.
	if present, ok := probePresent(instA, "Bob"); ok && present {
		t.Fatal("Bob is visible inside Alice's private copy of the crypt")
	}
	if present, ok := probePresent(instB, "Alice"); ok && present {
		t.Fatal("Alice is visible inside Bob's private copy of the crypt")
	}
	// And the shared TEMPLATE zone is not hosted at all, which is the point of the whole indirection: nobody
	// can walk into the dungeon, only into a copy of it.
	if z := sh.zoneByID("crypt"); z != nil {
		t.Fatal("the crypt template is hosted as a live zone; this test is not proving what it claims")
	}
	// Both landed in the template's START room, resolved on the DESTINATION's goroutine (entry passes an
	// empty room ref precisely so it never reads another zone's fields).
	assertTold(t, alice, "A Crumbling Stair")
}

// TestInstanceEntryRevalidatesAcrossTheAsyncGap is about the window between "the player asked" and "the
// instance exists" — which is unbounded (a queued mint behind a slow build) and during which the player is
// LIVE, playing normally, in the entrance room.
//
// Each sub-case is something that genuinely happens and each must produce either a clean player-facing line or
// a bounded, self-cleaning no-op. None may wedge, and none may leave the session in two places.
func TestInstanceEntryRevalidatesAcrossTheAsyncGap(t *testing.T) {
	t.Run("the mint fails", func(t *testing.T) {
		sh, mem, cancel := entryShard(t)
		defer cancel()
		origin := sh.zoneByID("midgaard")
		s := entrantIn(t, mem, origin, "Hero", "midgaard:room:market")
		// An unknown template: MintInstance refuses at its sink. Every other refusal (a cap, the rate limit,
		// a draining shard, a start-roomless template) arrives on the same path.
		queueEntry(sh, origin, s, "no-such-zone", "midgaard:room:market")
		waitCond(t, "the player to be told the way failed", func() bool {
			for {
				select {
				case f := <-s.out:
					if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), "fails to open") {
						return true
					}
				default:
					return false
				}
			}
		})
		// They are still where they were, still playable — a failed mint must not strand anybody.
		if z := zoneHolding(sh, "Hero"); z != origin {
			t.Fatalf("after a failed mint the player is held by %v, want the entrance zone", z)
		}
	})

	t.Run("the player left before the instance was ready", func(t *testing.T) {
		sh, mem, cancel := entryShard(t)
		defer cancel()
		origin := sh.zoneByID("midgaard")
		s := entrantIn(t, mem, origin, "Hero", "midgaard:room:market")
		// Quit FIRST, then let the mint complete. The FIFO inbox guarantees the leave is handled before the
		// worker's reply, so the reply lands on a zone that no longer holds the character.
		origin.post(leaveMsg{id: "Hero"})
		waitCond(t, "the player to leave the entrance zone", func() bool {
			present, ok := probePresent(origin, "Hero")
			return ok && !present
		})
		queueEntry(sh, origin, s, "crypt", "midgaard:room:market")
		// The instance is still minted — the worker had already committed — and is left EMPTY. That is not a
		// leak: it is quiescent by definition, so the reaper retires it once the mint grace elapses. What
		// matters here is that nothing wedged and nobody was moved.
		waitCond(t, "the abandoned instance to exist and be empty", func() bool {
			for _, z := range sh.zonesList() {
				if z.isInstance() && z.template == "crypt" {
					return z.quiescent()
				}
			}
			return false
		})
		if z := zoneHolding(sh, "Hero"); z != nil {
			t.Fatalf("a player who had already quit was moved into %q by the arriving mint result", z.id)
		}
	})

	t.Run("the instance was reaped before the player stepped through", func(t *testing.T) {
		sh, mem, cancel := entryShard(t)
		defer cancel()
		origin := sh.zoneByID("midgaard")
		s := entrantIn(t, mem, origin, "Hero", "midgaard:room:market")
		// Synthesize the arrival of a result naming an instance this shard does not host — exactly what a
		// reaper teardown between the mint and the reply produces. claimTransferTarget returns nil having
		// claimed nothing, so the entry is abandoned with nothing mutated.
		origin.post(instanceReadyMsg{
			character: "Hero", template: "crypt", zoneID: "crypt#gone", originRoom: "midgaard:room:market",
		})
		waitCond(t, "the player to be told the way closed", func() bool {
			for {
				select {
				case f := <-s.out:
					if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), "closes before") {
						return true
					}
				default:
					return false
				}
			}
		})
		if z := zoneHolding(sh, "Hero"); z != origin {
			t.Fatalf("the player is held by %v after an abandoned entry, want the entrance zone", z)
		}
		// And no claim leaked. A leaked `incoming` claim is PERMANENT: the zone can never satisfy quiescent()
		// again, so it can never be unhosted or rebalanced and every later drain burns its full deadline.
		if n := origin.incoming.Load(); n != 0 {
			t.Fatalf("the entrance zone holds %d leaked inbound-transfer claim(s) after an abandoned entry", n)
		}
	})
}

// --- the anchor, through its real consumers ---------------------------------------------------------------

// TestDurableLocationInsideAnInstanceIsTheAnchor is the durable half of the anchor, asserted end-to-end
// through the store rather than through the producer function.
//
// The persisted PAIR is the whole point and it is what #411 could not yet produce. #411 knew an instance id
// must not be persisted, but with no anchor to substitute it could only DECLINE to write the zone, leaving
// room_ref holding the instance's authored room (a real ref, because an instance hosts its template's rooms).
// The row was then a preserved entrance zone paired with a room from a different zone entirely, and a
// reconnect resolved the zone and start-roomed inside it. Both halves must come from the anchor.
func TestDurableLocationInsideAnInstanceIsTheAnchor(t *testing.T) {
	sh, mem, cancel := entryShard(t)
	defer cancel()
	origin := sh.zoneByID("midgaard")
	s := entrantIn(t, mem, origin, "Hero", "midgaard:room:guildhall")
	inst := enterInstance(t, sh, origin, s, "crypt", "midgaard:room:guildhall")

	// Walk deeper into the dungeon first, so the assertion cannot pass by accident from the entry room.
	inst.post(inputMsg{id: "Hero", seq: 1, line: "north"}) // entrance -> ossuary
	waitCond(t, "the player to reach the ossuary", func() bool {
		for {
			select {
			case f := <-s.out:
				if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), "The Ossuary") {
					return true
				}
			default:
				return false
			}
		}
	})

	row := flushByQuit(t, inst, mem, "Hero")
	if row.ZoneRef != "midgaard" {
		t.Fatalf("durable zone_ref after a logout from inside an instance = %q, want the anchor zone "+
			"midgaard. An instance id dangles by construction (it is reaped in minutes) and is a poisoned "+
			"record aimed at the login path's zone_ref read", row.ZoneRef)
	}
	if row.RoomRef != "midgaard:room:guildhall" {
		t.Fatalf("durable room_ref = %q, want the anchor ROOM midgaard:room:guildhall. This is the residual "+
			"#411 left: preserving only the zone pairs the entrance zone with the INSTANCE's authored room "+
			"(here crypt:room:ossuary), so the reconnect resolves the anchor zone, cannot find that room in "+
			"it, and start-rooms the player at the temple instead of the dungeon door", row.RoomRef)
	}
}

// recordingDir is a Locator that RECORDS the placement writes that actually reach the directory.
//
// Asserting on the shard's in-memory queue would be wrong here for a reason worth stating: with a directory
// configured, runPlacementWriter drains that queue on its own goroutine, so a test that reads it is racing the
// production writer for the same ops and will intermittently see none. Recording at the SINK also asserts the
// thing that matters — what a reconnect will read — rather than what was merely intended.
type recordingDir struct {
	mu    sync.Mutex
	calls []placementOp
}

func (d *recordingDir) ShardForZone(context.Context, string) (string, error) { return "shard-a", nil }

func (d *recordingDir) EndpointForShard(context.Context, string) (string, error) {
	return "addr-a", nil
}

func (d *recordingDir) PlayerShard(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (d *recordingDir) PlayerEpoch(context.Context, string) (uint64, bool, error) {
	return 0, false, nil
}

func (d *recordingDir) SetPlayerShard(context.Context, string, string, string, uint64) (bool, error) {
	return true, nil
}

func (d *recordingDir) RegisterPlacement(_ context.Context, player, _, zone string, epoch, nonce uint64) (bool, error) {
	d.record(placementOp{playerID: player, zoneID: zone, epoch: epoch, nonce: nonce})
	return true, nil
}

func (d *recordingDir) ClearPlayerShard(_ context.Context, player, _, zone string, epoch, nonce uint64) (bool, error) {
	d.record(placementOp{playerID: player, zoneID: zone, epoch: epoch, nonce: nonce, clear: true})
	return true, nil
}

func (d *recordingDir) record(op placementOp) {
	d.mu.Lock()
	d.calls = append(d.calls, op)
	d.mu.Unlock()
}

func (d *recordingDir) snapshot() []placementOp {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]placementOp(nil), d.calls...)
}

// TestPlacementInsideAnInstanceRecordsTheAnchor is the ROUTING half of the anchor. The placement record is the
// gate's reconnect key since #320 (it resolves a returning player by asking ShardForZone for the recorded
// zone), and its invariant is "the recorded zone is the zone that holds the session".
//
// An instance is unleased and in no directory, so recording one dead-ends a reconnect. The anchor keeps that
// invariant honest rather than bending it: entry is same-shard by construction (you enter from a room, so the
// entrance zone is hosted here, and MintInstance builds on this shard), so the anchor names a zone THIS shard
// hosts — ShardForZone resolves to us, and we are the shard holding the live instance.
//
// It also asserts the record stays CURRENT while the player is inside, which #411's skip could not do:
// skipping merely left the last good record standing, which was the anchor only by luck of the last walk.
func TestPlacementInsideAnInstanceRecordsTheAnchor(t *testing.T) {
	// The directory is wired BEFORE Run, in the construction hook. Setting sh.dir after `go sh.Run(ctx)` is a
	// data race with runPlacementWriter, which reads it on its own goroutine — the same construction-time
	// discipline runningShardWith's comment spells out for the With* options.
	dir := &recordingDir{}
	sh, mem, cancel := entryShard(t, func(sh *Shard) { sh.dir = dir })
	defer cancel()
	origin := sh.zoneByID("midgaard")
	s := entrantIn(t, mem, origin, "Hero", "midgaard:room:guildhall")

	inst := enterInstance(t, sh, origin, s, "crypt", "midgaard:room:guildhall")
	waitCond(t, "a placement registration written from inside the instance", func() bool {
		for _, op := range dir.snapshot() {
			if op.playerID != "Hero" || op.clear {
				continue
			}
			if op.zoneID == inst.id {
				t.Fatalf("the placement record names the INSTANCE %q. It is unleased and in no directory, so "+
					"ShardForZone resolves nobody and the reconnect dead-ends; and per #320 the gate does not "+
					"fall back to place.ShardID, so nothing recovers it", op.zoneID)
			}
			if op.zoneID == "" {
				continue // the seed registration from before entry
			}
			if op.zoneID != "midgaard" {
				t.Fatalf("the placement record names %q, want the anchor zone midgaard", op.zoneID)
			}
			return true
		}
		return false
	})

	// The clean-logout TOMBSTONE is the write that OUTLIVES both the session and the instance:
	// ClearPlayerShard deliberately preserves `zone` as the reconnect routing key. The anchor is a durable
	// authored ref, so it is safe to outlive the instance in a way the ephemeral id never was. Driven on a
	// BARE instance zone, because the clean-quit path needs s.quitting already set on the session and that is
	// zone-owned state a test cannot write on a running actor.
	bare := bareInstance(t, "crypt")
	bare.shard = sh
	quitter := &session{
		character: "Quitter", out: make(chan *playv1.ServerFrame, 32), epoch: 1, quitting: true,
		anchorZone: "midgaard", anchorRoom: "midgaard:room:guildhall",
	}
	bare.newPlayerEntity(quitter, "Quitter")
	Move(quitter.entity, bare.rooms["crypt:room:tomb"])
	bare.setPlayer("Quitter", quitter)
	bare.detach("Quitter", quitter.out)
	waitCond(t, "the logout tombstone to reach the directory", func() bool {
		for _, op := range dir.snapshot() {
			if op.playerID != "Quitter" || !op.clear {
				continue
			}
			if op.zoneID != "midgaard" {
				t.Fatalf("the logout tombstone carried zone %q, want the anchor midgaard. An instance id here "+
					"is reaped seconds later, ShardForZone then finds no lease, and per #320 the gate will "+
					"NOT fall back to place.ShardID — nothing self-heals it before the next login", op.zoneID)
			}
			return true
		}
		return false
	})
}

// bareInstance builds a real instance of a demo template with NO shard and NO actor goroutine, so a test can
// call zone methods directly and read zone-owned state without racing anybody.
//
// The whole respawn decision, and the anchorless eviction beneath it, are single-goroutine logic — the async
// parts are upstream (the mint) and downstream (the transfer). Driving them through a live shard would mean
// arranging a deterministic death inside a running combat system to observe a decision that is three lines of
// straight-line code. This is the honest level for them.
func bareInstance(t *testing.T, template string) *Zone {
	t.Helper()
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatalf("load embedded demo pack: %v", err)
	}
	z := newInstanceZone(template+"#bare", template)
	// Define the pack's prototypes + globals into this zone's PRIVATE caches, exactly as newDemoZone does for
	// an authored test zone. Without it buildZone spawns every room from an unknown prototype and the zone
	// boots with room map entries whose entities carry nothing — which is the "worst failure shape in this
	// package" buildZone's own comment warns about, arriving through the test harness instead of content.
	defineContent(z.protos, lc)
	defineGlobals(z.defs, lc)
	z.buildZone(lc)
	if len(z.rooms) == 0 {
		t.Fatalf("the bare instance of %q built no rooms", template)
	}
	return z
}

// --- exit: respawn ------------------------------------------------------------------------------------------

// TestRespawnInsideAnInstance pins the respawn DECISION, which had no prior answer at all: there is no
// cross-zone respawn anywhere in the engine, so respawnPlayer moves the victim within its OWN zone.
//
// With a start room that is the right answer and the one a dungeon wants — you die at the boss and wake at the
// dungeon entrance, with the run still going. Ejecting on death would be an engine-level ruling on what a wipe
// MEANS, which is content's call (a template can author whatever it likes on_death).
//
// WITHOUT one, the pre-#72 behavior was silent and bad: resolveRoom returns nil, the move is skipped, and the
// player is left standing in the boss room at FULL HEALTH, alive, having lost nothing. MintInstance now
// refuses such a template outright, so this is the fail-safe for the paths that can still produce one — and it
// must evict to the anchor rather than leave them there.
func TestRespawnInsideAnInstance(t *testing.T) {
	newVictim := func(z *Zone, room ProtoRef) *session {
		s := &session{
			character: "Hero", out: make(chan *playv1.ServerFrame, 64), epoch: 1,
			anchorZone: "midgaard", anchorRoom: "midgaard:room:guildhall",
		}
		z.newPlayerEntity(s, "Hero")
		Move(s.entity, z.rooms[room])
		z.setPlayer("Hero", s)
		return s
	}

	t.Run("with a start room: stays inside the same copy", func(t *testing.T) {
		z := bareInstance(t, "crypt")
		s := newVictim(z, "crypt:room:tomb")
		z.respawnPlayer(s.entity)
		if got := roomRef(s.entity.location); got != "crypt:room:entrance" {
			t.Fatalf("respawned into %q, want the INSTANCE's own start room crypt:room:entrance. A respawn "+
				"that dropped the player into the shared template — or out to the anchor — would end the "+
				"dungeon run, which is a content decision the engine must not make", got)
		}
		if s.entity.zone != z {
			t.Fatalf("the respawned player belongs to zone %v, want the same instance", s.entity.zone)
		}
		if len(z.inbox) != 0 {
			t.Fatalf("a respawn with a usable start room posted %d message(s); the eviction fallback must "+
				"not fire when the instance can respawn its own dead", len(z.inbox))
		}
	})

	t.Run("with no start room: evicts to the anchor", func(t *testing.T) {
		z := bareInstance(t, "crypt")
		z.startRoom = "" // the shape MintInstance refuses; reachable only via a path that bypasses the sink
		s := newVictim(z, "crypt:room:tomb")
		z.respawnPlayer(s.entity)
		// They are revived in place for the width of one queue hop — the eviction is POSTED, never done
		// inline, because respawnPlayer runs deep inside die() and every frame above it goes on touching this
		// entity. A cross-zone transfer from there would hand the entity to another goroutine mid-stack.
		if got := roomRef(s.entity.location); got != "crypt:room:tomb" {
			t.Fatalf("the victim moved to %q inline; the eviction must be deferred to a clean stack", got)
		}
		var evict *evictToAnchorMsg
		for len(z.inbox) > 0 {
			if m, ok := (<-z.inbox).(evictToAnchorMsg); ok {
				evict = &m
			}
		}
		if evict == nil {
			t.Fatal("a death in a start-roomless instance posted NO eviction. The player is left standing in " +
				"the room they died in, alive and at full health, with no way for the engine to get them out")
		}
		if evict.character != "Hero" || evict.drainEject {
			t.Fatalf("the posted eviction = %+v, want character=Hero and drainEject=false (this is not the "+
				"drain path, so it must claim its destination through the ordinary claimTransferTarget)", *evict)
		}
	})
}

// --- exit: the drain eject --------------------------------------------------------------------------------

// TestClaimEjectTargetIsBoundedByTheEjectWindow pins the discipline that makes the drain eject safe, and it is
// the test that would have caught the earlier attempt's third HIGH finding.
//
// The eject has to run INSIDE s.draining (BeginDrain sets that flag first, and the eject must happen before
// any lease moves), so claimTransferTarget — which refuses a draining shard outright — cannot serve it. The
// tempting fix is a claim helper that simply skips the refusal. That is what shipped and was cut: the claim is
// taken now but the ARRIVAL is handled at an unbounded later moment, and if that moment falls after step 3 the
// player lands in a zone that has already been durably flushed and told to disconnect its residents.
//
// The bound is an explicit WINDOW, read in the SAME mu hold the claim is taken in, and open only while
// BeginDrain is blocked on its eject barrier. Outside it, the helper claims nothing.
func TestClaimEjectTargetIsBoundedByTheEjectWindow(t *testing.T) {
	sh, _, cancel := entryShard(t)
	defer cancel()
	anchor := sh.zoneByID("midgaard")

	// CLOSED (the normal state, including during a drain that is not in its eject phase).
	if z := sh.claimEjectTarget("midgaard"); z != nil {
		t.Fatal("claimEjectTarget resolved a destination with the eject window CLOSED. The claim would be " +
			"taken at an unbounded time and could land a player in a zone the drain has already flushed and " +
			"disconnected")
	}
	if n := anchor.incoming.Load(); n != 0 {
		t.Fatalf("a REFUSED claim still bumped incoming to %d. A leaked claim is permanent: the zone can "+
			"never satisfy quiescent() again, so it can never be unhosted and every later drain burns its "+
			"full deadline", n)
	}

	// OPEN, and refusing a draining shard is exactly what it exists to override.
	sh.mu.Lock()
	sh.instanceEjectWindow = true
	sh.draining = true
	sh.mu.Unlock()
	if sh.claimTransferTarget("midgaard") != nil {
		t.Fatal("claimTransferTarget admitted an arrival into a DRAINING shard; the eject helper would then " +
			"be unnecessary and this test is not describing the real constraint")
	}
	got := sh.claimEjectTarget("midgaard")
	if got != anchor {
		t.Fatalf("claimEjectTarget returned %v inside the window, want the anchor zone. Every anchor on a "+
			"draining shard would be refused, so no occupant could be walked out and the SIGTERM behavior "+
			"would silently stay at 'drop every dungeon occupant'", got)
	}
	if n := anchor.incoming.Load(); n != 1 {
		t.Fatalf("a granted claim left incoming = %d, want 1: the in-flight session would be invisible to "+
			"quiescence and the drain could order its flush ahead of the arrival", n)
	}
	anchor.incoming.Add(-1) // stand in for the transferIn that would release it

	// An INSTANCE is never a valid eject destination. An anchor is always an authored zone so this cannot
	// trigger today; "eject" must never come to mean "into another private copy". Minted with the draining
	// flag briefly lowered, since a draining shard refuses to mint at all (which is its own correct rule).
	sh.mu.Lock()
	sh.draining = false
	sh.mu.Unlock()
	inst := mustMint(t, sh, "crypt", "acct-1")
	sh.mu.Lock()
	sh.draining = true
	sh.mu.Unlock()
	if z := sh.claimEjectTarget(inst.id); z != nil {
		t.Fatal("claimEjectTarget resolved an INSTANCE as an eject destination")
	}
	// A zone already handed to a peer is refused, exactly as claimTransferTarget refuses it: admitting a
	// walker into a zone we no longer own keeps this shard writing to a zone whose lease lives elsewhere.
	sh.mu.Lock()
	sh.handedOff["midgaard"] = true
	sh.mu.Unlock()
	if z := sh.claimEjectTarget("midgaard"); z != nil {
		t.Fatal("claimEjectTarget resolved a zone whose lease has already been handed to a peer")
	}
}

// TestDrainEjectsInstanceOccupantsToTheirAnchors is the SIGTERM story, and without it every rolling deploy
// disconnects every player in a dungeon.
//
// #411 excluded instances from the drain HANDOVER, correctly — there is no lease to flip, and drainPlayer
// would hand each occupant off to an instance id no peer can resolve. But that left them RESIDENT, so every
// one of them was a deadline straggler: dropped, and told to reconnect. This walks them back out to the zone
// and room they entered from FIRST, before any lease moves, so by the time the handover runs they are ordinary
// residents of an ordinary leased zone and are redirected with everybody else.
//
// The assertion is on WHERE THEY END UP, not on the tally: the tally can be made to look right by an eject
// that drops people.
func TestDrainEjectsInstanceOccupantsToTheirAnchors(t *testing.T) {
	sh, mem, cancel := entryShard(t)
	defer cancel()
	origin := sh.zoneByID("midgaard")
	s := entrantIn(t, mem, origin, "Hero", "midgaard:room:guildhall")
	inst := enterInstance(t, sh, origin, s, "crypt", "midgaard:room:guildhall")

	// Walk deeper in, so the eject is a real relocation rather than a no-op at the entrance.
	inst.post(inputMsg{id: "Hero", seq: 1, line: "north"})
	waitCond(t, "the player to reach the ossuary", func() bool {
		return drainOutputContains(s, "The Ossuary")
	})

	// No peer, so the handover itself fails and every zone takes the degraded path. That is deliberate: it
	// isolates the EJECT (which runs before any of that) from the redirect machinery, so a pass here cannot be
	// explained by the handoff working.
	choose := func(_ string, _ int) (string, string, error) { return "", "", errNoPeerInTest }
	if _, err := sh.BeginDrain(context.Background(), choose, 2*time.Second); err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}

	// THE ASSERTION: they are standing in their anchor zone, not in the instance.
	if present, ok := probePresent(inst, "Hero"); ok && present {
		t.Fatal("the drain left a player resident INSIDE an instance. There is no lease to hand a peer and " +
			"no id a peer could host, so they are a deadline straggler on every SIGTERM: dropped, and told " +
			"to reconnect, on every rolling deploy")
	}
	if present, ok := probePresent(origin, "Hero"); !ok || !present {
		t.Fatal("the ejected player is not resident in their ANCHOR zone either — the eject moved them " +
			"nowhere, or dropped them")
	}
	// And the anchor is cleared on arrival, so a later save cannot persist a door they are no longer at.
	row := flushByQuit(t, origin, mem, "Hero")
	if row.RoomRef != "midgaard:room:guildhall" {
		t.Fatalf("after the eject the durable room_ref is %q, want the anchor room they were walked back to", row.RoomRef)
	}
}

// TestEjectPhaseIsTimeBoundedOnASaturatedInstance pins the first of the earlier attempt's three HIGH
// findings: it used a blocking post with no time bound on the SIGTERM path, so ONE saturated inbox blocked
// BeginDrain BEFORE step 1 — before any lease moved and before the durable flush — and the whole shard was
// SIGKILLed at the shutdown deadline with nothing written for anybody on it.
//
// It targets ejectInstanceOccupants directly rather than driving a whole BeginDrain, because that is the only
// way to make the assertion discriminating. Step 3's own posts are unbounded except by the caller's context,
// so a full drain against a wedged zone runs to that context either way and a timing assertion on it would
// pass against the bug. Here the context is Background: against an unbounded post this hangs forever, and
// against the fix it returns within the barrier.
//
// It also asserts the eject WINDOW is closed on the way out. The window is what lets claimEjectTarget admit an
// arrival into a draining shard; leaving it open after a timeout would restore precisely the unbounded-claim
// hazard it exists to bound.
func TestEjectPhaseIsTimeBoundedOnASaturatedInstance(t *testing.T) {
	sh, mem, cancel := entryShard(t)
	defer cancel()
	origin := sh.zoneByID("midgaard")
	s := entrantIn(t, mem, origin, "Hero", "midgaard:room:guildhall")
	inst := enterInstance(t, sh, origin, s, "crypt", "midgaard:room:guildhall")

	old := drainEjectBarrier
	drainEjectBarrier = 250 * time.Millisecond
	defer func() { drainEjectBarrier = old }()

	release := stallSourceMidTransfer(t, inst) // parks the actor AND fills its inbox to capacity
	defer release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		sh.ejectInstanceOccupants(context.Background(), []*Zone{inst}) // NO deadline on the context
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the eject phase never returned against a saturated instance inbox with an unbounded " +
			"context. On SIGTERM that blocks BeginDrain before step 1, so the shutdown budget is spent here " +
			"and the shard is SIGKILLed with nothing flushed for ANY of its players")
	}
	sh.mu.Lock()
	open := sh.instanceEjectWindow
	sh.mu.Unlock()
	if open {
		t.Fatal("the eject window is still OPEN after the phase gave up. claimEjectTarget would go on " +
			"admitting arrivals into this draining shard at an unbounded later time — landing players in a " +
			"zone the drain has already flushed and disconnected, which is the hazard the window bounds")
	}
}

// errNoPeerInTest is the target-chooser failure that forces every zone down the degraded drain path, isolating
// the eject from the redirect machinery.
var errNoPeerInTest = fmt.Errorf("no peer in this test")

// drainOutputContains non-blockingly drains a session's frames looking for a substring.
func drainOutputContains(s *session, substr string) bool {
	for {
		select {
		case f := <-s.out:
			if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), substr) {
				return true
			}
		default:
			return false
		}
	}
}

// --- exit: link death -------------------------------------------------------------------------------------

// TestLinkDeathInsideAnInstancePersistsTheAnchor covers the path a quit test cannot: an UNEXPECTED stream
// loss. It matters on its own because the two paths persist at different moments and for different reasons —
// enterLinkDeath flushes IMMEDIATELY (a move is not a flush point, so a player who walked and then dropped
// would otherwise lose that location for the whole 60s grace, and entirely if the shard crashed in it), and
// the reap flushes again 60s later.
//
// A link-dead player is also the case where a stale record hurts most: they are coming BACK, by definition.
func TestLinkDeathInsideAnInstancePersistsTheAnchor(t *testing.T) {
	sh, mem, cancel := entryShard(t)
	defer cancel()
	origin := sh.zoneByID("midgaard")
	s := entrantIn(t, mem, origin, "Hero", "midgaard:room:guildhall")
	inst := enterInstance(t, sh, origin, s, "crypt", "midgaard:room:guildhall")

	inst.post(inputMsg{id: "Hero", seq: 1, line: "north"}) // deeper in, so the anchor is not the current room
	waitCond(t, "the player to reach the ossuary", func() bool { return drainOutputContains(s, "The Ossuary") })

	// The real link-loss signal: the stream dropped without a clean quit.
	inst.post(detachMsg{id: "Hero", out: s.out})

	var row CharSnapshot
	waitCond(t, "the link-death flush to reach the store", func() bool {
		r, found, err := mem.LoadCharacter(context.Background(), "Hero")
		if err != nil || !found {
			return false
		}
		row = r
		return r.RoomRef != ""
	})
	if row.ZoneRef != "midgaard" || row.RoomRef != "midgaard:room:guildhall" {
		t.Fatalf("the link-death snapshot recorded (%q, %q), want the exit anchor "+
			"(midgaard, midgaard:room:guildhall). A link-dead player is BY DEFINITION coming back, so this is "+
			"the record their reconnect resolves: an instance id dead-ends it, and a zone-only preserve pairs "+
			"midgaard with crypt:room:ossuary and start-rooms them at the temple", row.ZoneRef, row.RoomRef)
	}
}

// --- the end-to-end journey -------------------------------------------------------------------------------

// TestInstanceJourney walks one player through the whole feature in one continuous session, on real
// goroutines, because several of the properties this slice is responsible for exist only BETWEEN the steps.
//
// The per-step tests above each pin one transition against a freshly-arranged world. What they cannot show is
// that the state threaded between transitions survives: that the anchor set at entry is still correct after
// walking three rooms and dying, that the durable row written at the END of the run names the door the player
// went in by at the BEGINNING, and that a relog resolves through it. A feature assembled from individually
// correct steps can still be wrong as a sequence.
//
// The journey: walk in -> confirm it is a COPY and not the template -> walk the instance without leaking into
// the shared zone -> die and respawn INSIDE -> walk out under their own power -> quit -> relog and land at the
// anchor.
func TestInstanceJourney(t *testing.T) {
	sh, mem, cancel := entryShard(t)
	defer cancel()
	origin := sh.zoneByID("midgaard")
	s := entrantIn(t, mem, origin, "Pilgrim", "midgaard:room:guildhall")

	// 1. WALK IN. The destination is a zone that did not exist when the player asked for it.
	inst := enterInstance(t, sh, origin, s, "crypt", "midgaard:room:guildhall")
	if inst.id == "crypt" || !inst.isInstance() || inst.template != "crypt" {
		t.Fatalf("entered %q, want a crypt COPY (crypt#<serial>) rather than the shared template", inst.id)
	}
	if sh.zoneByID("crypt") != nil {
		t.Fatal("the crypt template is hosted; the journey is not proving that entry produced a copy")
	}
	if !drainOutputContains(s, "A Crumbling Stair") {
		t.Fatal("the entrant was not shown the instance's start room on arrival")
	}

	// 2. WALK THE INSTANCE. Two rooms deep, still inside the same copy — the exits between crypt rooms name
	// the TEMPLATE's authored refs, so without #410's ownsZoneRef every one of these steps would resolve the
	// shared zone and silently transfer the player out of their own dungeon into the public one.
	inst.post(inputMsg{id: "Pilgrim", seq: 1, line: "north"}) // entrance -> ossuary
	waitCond(t, "the player to reach the ossuary, still inside their copy", func() bool {
		return drainOutputContains(s, "The Ossuary")
	})
	if z := zoneHolding(sh, "Pilgrim"); z != inst {
		t.Fatalf("walking inside the instance moved the player to %v — they leaked out of their private copy", z)
	}

	// 3. FLEE MID-COMBAT. The tomb's guardian is aggressive, so stepping in engages automatically; move()
	// refuses to walk while fighting, which makes flee the ONLY way out. Inside an instance that is the
	// highest-stakes of #410's migrated predicates: under a raw zone-id comparison every exit here reads as
	// cross-zone, flee answers "You can't flee that way" in every room, and a party that wipes is trapped.
	// This is the first slice in which a player can actually be in that position.
	inst.post(inputMsg{id: "Pilgrim", seq: 2, line: "north"}) // ossuary -> tomb, and the guardian engages
	waitCond(t, "the guardian to engage the player in the tomb", func() bool {
		return drainOutputContains(s, "A Sealed Tomb")
	})
	inst.post(inputMsg{id: "Pilgrim", seq: 3, line: "flee"})
	// The flee CONFIRMATION, not the destination room's description: flee does not re-show the room, so
	// waiting on a room name here would wait forever. Under the pre-#410 predicate the answer would instead
	// be "You can't flee that way" for every exit in the instance.
	waitCond(t, "the player to flee out of the tomb", func() bool {
		return drainOutputContains(s, "You flee from combat")
	})
	if z := zoneHolding(sh, "Pilgrim"); z != inst {
		t.Fatalf("fleeing inside the instance moved the player to %v — flee resolved the SHARED zone", z)
	}

	// DEATH AND RESPAWN are covered DETERMINISTICALLY elsewhere rather than here, and the omission is
	// deliberate. Reaching a death from this end means arranging a real combat outcome against a real mob on
	// a real heartbeat, which makes the assertion a race between the player's hp and the guardian's dice —
	// exactly the kind of "usually passes" step that turns a journey test into a flake nobody trusts. The
	// respawn DECISION (stay inside when the template has a start room; evict to the anchor when it does not)
	// is straight-line single-goroutine logic and is pinned in TestRespawnInsideAnInstance.

	// 4. WALK OUT under their own power. Back to the entrance, then `up` — which names
	// midgaard:room:guildhall, a FOREIGN zone, so it takes the ordinary cross-zone path. That is the point of
	// the whole locality chokepoint: leaving an instance needs no exit machinery of its own, because an exit
	// naming another zone already leaves.
	//
	// The escape is RETRIED rather than posted once. The route out passes the ossuary's skeleton and the
	// guardian's opportunity-attack reaction, so a given step can legitimately be refused ("you can't leave
	// while fighting") — which is the combat exclusion working, not a failure. Retrying until the player is
	// out asserts the property that matters (they CAN get out, unaided) without pinning the test to a
	// particular sequence of dice.
	seq := uint64(4)
	waitCond(t, "the player to walk out of the instance into midgaard", func() bool {
		if present, ok := probePresent(origin, "Pilgrim"); ok && present {
			return true
		}
		for _, dir := range []string{"flee", "south", "up"} {
			inst.post(inputMsg{id: "Pilgrim", seq: seq, line: dir})
			seq++
		}
		return false
	})

	// 5. WALK ON, then QUIT. The extra step is what makes the assertion discriminating: walking out lands them
	// in the anchor ROOM, so "the row names the anchor" and "the row names their live location" would be the
	// same string there. One room further and they differ, so this pins that a completed run goes back to
	// recording live locations rather than staying pinned to the door.
	seq++
	origin.post(inputMsg{id: "Pilgrim", seq: seq, line: "north"}) // guildhall -> smithy
	waitCond(t, "the player to walk on to the smithy", func() bool {
		return drainOutputContains(s, "Heat rolls off the forge")
	})
	row := flushByQuit(t, origin, mem, "Pilgrim")
	if row.RoomRef != "midgaard:room:smithy" {
		t.Fatalf("the durable row after the run names room %q, want their live location midgaard:room:smithy",
			row.RoomRef)
	}

	// 6. RELOG. The login path's zone resolution must honor the row — and in particular must not have been
	// handed an instance-shaped ref to refuse, which is the failure mode #411's read guard exists for and the
	// one the anchor makes unreachable from the write side.
	ps := &playServer{shard: sh, log: sh.zoneByID("midgaard").log}
	if got := ps.resolveAttachZone("Pilgrim", "", row, true); got != origin {
		t.Fatalf("a relog after a dungeon run resolved to %v, want the anchor zone midgaard", got)
	}
}

// TestTransferInMaintainsTheAnchorInvariant pins the anchor's LIFECYCLE invariant directly: an anchor is set
// if and only if the session is inside an instance.
//
// This is DEFENCE IN DEPTH rather than a fix, and it is worth saying which. Removing the clear does not break
// anything observable today, because every consumer — durableLocation, placementZoneRef, evictToAnchor —
// independently gates on z.isInstance() first, so a stale anchor outside an instance is inert. What the clear
// buys is that those gates are the SECOND check rather than the only one: it makes "s.anchorZone != ”" mean
// "this player is inside an instance", so a future reader who simplifies any of those three conditions to test
// the anchor alone gets the same answer instead of a location-loss bug. Pinning it here is what keeps that
// true, since no end-to-end test can fail on it.
func TestTransferInMaintainsTheAnchorInvariant(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	// Two bare, non-running destinations: an authored zone and an instance. transferIn is called directly, so
	// the test goroutine is the only writer.
	authored := newDemoZone("midgaard", newProtoCache())
	inst := newInstanceZone("crypt#anchor", "crypt")
	defineContent(inst.protos, lc)
	defineGlobals(inst.defs, lc)
	inst.buildZone(lc)

	cases := []struct {
		name           string
		dest           *Zone
		room           ProtoRef
		wantAnchorKept bool
	}{
		{
			// Arriving in ANOTHER ROOM OF THE INSTANCE keeps it — this is a player walking around inside a
			// dungeon, and losing the anchor here would strand them with no recorded way out.
			name: "arriving inside the instance keeps the anchor",
			dest: inst, room: "crypt:room:ossuary", wantAnchorKept: true,
		},
		{
			// Arriving in an AUTHORED zone clears it: the visit is over. This is the single clear point for
			// every way out at once — walking through a foreign exit, the drain eject, the respawn eviction.
			name: "arriving outside the instance clears the anchor",
			dest: authored, room: "midgaard:room:temple", wantAnchorKept: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &session{
				character: "Hero", out: make(chan *playv1.ServerFrame, 32), epoch: 1,
				anchorZone: "midgaard", anchorRoom: "midgaard:room:guildhall",
			}
			inst.newPlayerEntity(s, "Hero")
			tc.dest.claimInboundTransfer() // the bare form of the claim claimTransferTarget takes under s.mu
			tc.dest.transferIn(transferInMsg{s: s, room: tc.room})
			held := s.anchorZone != "" || s.anchorRoom != ""
			if held != tc.wantAnchorKept {
				t.Fatalf("after arriving in %q the anchor is (%q, %q); want kept=%v. The invariant is that an "+
					"anchor is set if and only if the session is inside an instance, so that every consumer "+
					"can read it as one question rather than two",
					tc.dest.id, s.anchorZone, s.anchorRoom, tc.wantAnchorKept)
			}
		})
	}
}

// --- the signing input's canonicalization -----------------------------------------------------------------

// TestSigningInputHasNoFieldConfusion is the direct regression for the field-confusion collision the security
// panel found, and it is the assertion the pre-existing account tests structurally COULD NOT make.
//
// The bug. Both `tier` and `account` were appended to the digest ONLY WHEN NON-EMPTY. That encoding is sound
// for exactly one optional field (presence is unambiguous — a trailing value can belong to only one field) and
// unsound the moment a second one joins it on the same terms. With two, a snapshot carrying a single non-empty
// optional produces the SAME bytes regardless of WHICH field holds it:
//
//	(tier="", account=X)  and  (tier=X, account="")  ->  identical digest
//
// So one signature verifies for both. The handoff wire is plaintext (world.go dials peers with insecure
// creds) and (tier="", account="<uuid>") is the ordinary-player shape, so an on-path attacker rewrites a
// captured Prepare into (tier="<uuid>", account="") and the destination hands the victim's account id to
// applyTierFlags. Bounded today only by grantedFlags returning nil for an unknown tier name — and the trust
// ladder is CONTENT-DEFINED, so a builder authoring a trust_tier_defs rung named after an account uuid turns
// it into privilege escalation.
//
// Why the older tests could not catch it: TestAccountRidesTheSignedHandoff asserts only that CHANGING the
// account changes the digest (true even with the bug — it compares acct-42 against acct-victim, both in the
// same field) and that the empty-account digest is stable (a tautology). Neither varies WHICH FIELD a value
// sits in, which is the only axis the collision lives on.
func TestSigningInputHasNoFieldConfusion(t *testing.T) {
	mk := func(tier, account string) *handoffv1.PrepareRequest {
		return &handoffv1.PrepareRequest{
			Epoch:        3,
			TargetZoneId: "darkwood",
			TargetRoomId: "darkwood:room:grove",
			Snapshot: &handoffv1.PlayerSnapshot{
				CharacterId: "Hero",
				Name:        "Hero",
				Tier:        tier,
				Account:     account,
			},
		}
	}

	// THE COLLISION ITSELF. The value that migrates is deliberately shaped like an account uuid, because that
	// is the exploit: it is the string that ends up at applyTierFlags.
	const uuid = "acct-3f2a91c0-0e11-4b6d-9c7e-11ab22cd33ef"
	if string(snapshotSigningInput(mk(uuid, ""))) == string(snapshotSigningInput(mk("", uuid))) {
		t.Fatal("FIELD CONFUSION: digest(tier=X, account=\"\") == digest(tier=\"\", account=X). One signature " +
			"therefore authenticates both snapshots, so an on-path attacker can rewrite an ordinary player's " +
			"Prepare (tier=\"\", account=<uuid>) into (tier=<uuid>, account=\"\") and have the destination feed " +
			"the account id to applyTierFlags — privilege escalation the moment content authors a trust tier " +
			"under that name. Optional fields must be written UNCONDITIONALLY so field position is fixed")
	}

	// THE GENERAL SHAPE, not just the one known pair: across the cross-product of both optional fields over a
	// small value set, no two DISTINCT assignments may share a digest. This is the property the encoding is
	// supposed to have, so it is the property asserted — a future third optional field added conditionally
	// fails here even if nobody thinks to write a bespoke pair for it.
	vals := []string{"", "admin", "builder", uuid, "x"}
	seen := map[string]string{} // digest -> the assignment that produced it
	for _, tier := range vals {
		for _, account := range vals {
			d := string(snapshotSigningInput(mk(tier, account)))
			key := fmt.Sprintf("tier=%q account=%q", tier, account)
			if prev, dup := seen[d]; dup {
				t.Fatalf("two DISTINCT field assignments share a digest: %s and %s. A signature over one "+
					"authenticates the other, so a value can migrate between fields in flight", prev, key)
			}
			seen[d] = key
		}
	}
}

// --- the account, across a cross-shard walk ---------------------------------------------------------------

// TestAccountRidesTheSignedHandoff pins the account's provenance rule end to end: it comes only from a
// signature-checked source, and it survives a cross-shard walk.
//
// Both halves matter and they pull in opposite directions. Without the CARRY, a player who walks from shard A
// to shard B arrives with no account and is refused entry to every dungeon on B until they relog — a silent,
// baffling degradation. Without the SIGNATURE BINDING and the keyless STRIP, the account becomes forgeable in
// flight: it is the key the instance caps are charged to, so an attacker could charge their mints to a
// victim's account, exhausting that account's concurrent cap and mint rate limit and mis-attributing the mints
// in the logs. Lower severity than the tier's privilege escalation (the account grants nothing), but bound on
// identical terms: at a FIXED position, written unconditionally. See TestSigningInputHasNoFieldConfusion for
// why the earlier append-only-when-non-empty encoding could not survive a second optional field.
func TestAccountRidesTheSignedHandoff(t *testing.T) {
	s := &session{character: "Hero", account: "acct-42", epoch: 3}
	z := newZone("midgaard")
	z.newPlayerEntity(s, "Hero")
	snap := buildSnapshot(s)
	if snap.GetAccount() != "acct-42" {
		t.Fatalf("the handoff snapshot carries account %q, want acct-42. Without it a cross-shard walk "+
			"silently strips the player's ability to enter any instanced zone until they relog",
			snap.GetAccount())
	}

	// The SIGNATURE must cover it: flipping the account in flight must invalidate the digest.
	req := &handoffv1.PrepareRequest{Snapshot: snap, Epoch: 3, TargetZoneId: "darkwood", TargetRoomId: "darkwood:room:grove"}
	before := snapshotSigningInput(req)
	req.Snapshot.Account = "acct-victim"
	if string(before) == string(snapshotSigningInput(req)) {
		t.Fatal("rewriting the carried ACCOUNT does not change the handoff signing input, so a network " +
			"attacker can re-point an in-flight handoff's account: the destination then charges that " +
			"player's instance mints to a victim's quota, exhausting their cap and rate limit and " +
			"attributing the mints to them")
	}

	// The digest must be DETERMINISTIC for a fixed input — the verifier recomputes it independently, so an
	// unstable digest would fail every handoff.
	req.Snapshot.Account = ""
	withEmpty := snapshotSigningInput(req)
	if string(withEmpty) != string(snapshotSigningInput(req)) {
		t.Fatal("the empty-account digest is not stable")
	}

	// And the DESTINATION adopts it, so the arriving session can mint on the far shard.
	dest := newZone("darkwood")
	room := dest.newEntity("darkwood:room:grove")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	dest.rooms["darkwood:room:grove"] = room
	dest.startRoom = "darkwood:room:grove"
	snap.Account = "acct-42"
	reply := make(chan error, 1)
	dest.prepare(prepareMsg{snap: snap, room: "darkwood:room:grove", epoch: 4, token: "tok", reply: reply})
	if err := <-reply; err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := dest.players["Hero"]; got == nil || got.account != "acct-42" {
		t.Fatalf("the destination session adopted account %q, want acct-42 — the arriving player cannot "+
			"enter an instance on the far shard", got.account)
	}
}

// TestPendingMintFlagDoesNotLatchAcrossAWalk covers a latch that is invisible until it has already happened.
//
// instanceMintPending is scoped to ONE origin zone: instanceReady clears it, but only on the zone that took
// the request. A player who asks to enter and then walks to a sibling zone before the (unbounded) build
// finishes leaves that reply landing on a zone which no longer holds them — it returns having cleared
// nothing, because there is nothing there to clear — while the session travels on with the flag still set.
// From then on requestInstanceEntry answers "the way is already opening" forever and they can never enter a
// dungeon again for the life of the session.
//
// It is asserted at the point of custody change (transferIn), because that is where the fix belongs and
// because reproducing it through a live shard would mean winning a race against the mint on purpose.
func TestPendingMintFlagDoesNotLatchAcrossAWalk(t *testing.T) {
	dest := newDemoZone("darkwood", newProtoCache())
	s := &session{
		character: "Hero", out: make(chan *playv1.ServerFrame, 32), epoch: 1,
		instanceMintPending: true, // asked to enter, then walked away before the build finished
	}
	dest.newPlayerEntity(s, "Hero")
	dest.claimInboundTransfer()
	dest.transferIn(transferInMsg{s: s, room: "darkwood:room:grove"})

	if s.instanceMintPending {
		t.Fatal("the pending-mint flag survived a walk into another zone. The reply it is waiting for will " +
			"be delivered to the zone the player LEFT, which no longer holds them and so clears nothing — so " +
			"the flag latches and every later entry attempt is refused with 'the way is already opening' for " +
			"the rest of the session")
	}
}

package gate

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/assertion"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/world"
)

// instance_journey_test.go — #425: instanced zones (#72), driven through a REAL gate↔world Play stream.
//
// Every other instance test in the tree is a unit test in internal/world calling MintInstance or
// requestInstanceEntry directly. Nothing exercised what a player actually does, so nothing would have caught
// a break anywhere BETWEEN the typed command and the mint: the command parser, the declared entrance (#435),
// the asynchronous three-hop entry, or the re-render on arrival.
//
// # The premise these tests rest on, asserted rather than assumed
//
// Testing an instance black-box is awkward because there is no player-visible signal saying "this is a
// private copy". GMCP's Room.Info.zone derives from the room's AUTHORED ref, so it reads `crypt` inside an
// instance exactly as in the shared zone, and the room NAME is identical by construction. A test that walked
// through the door and asserted "A Crumbling Stair" would pass just as happily against an ordinary
// cross-zone exit into the shared crypt — which is exactly what the guild hall's `down` exit is.
//
// So the arrival is pinned two ways a shared-zone arrival could not satisfy:
//
//   - the shard does NOT host `crypt` at all (asserted at construction, so it cannot rot); and
//   - two players who walk through the same door cannot hear each other, though each is standing in a room
//     of the same name. That isolation IS what an instance is.

// instanceAccount issues real Ed25519-signed assertions, so the session carries a VERIFIED ACCOUNT.
// requestInstanceEntry refuses without one — an unattributable mint is an uncapped mint — so a stub login
// makes this whole file test the refusal path instead of the feature.
type instanceAccount struct {
	stubAccountClient
	chars []string
	priv  ed25519.PrivateKey
}

func (f *instanceAccount) StartDeviceAuth(context.Context, string) (string, string, time.Duration, error) {
	return "DEV", "http://localhost:8080/login/DEV", 5 * time.Millisecond, nil
}

func (f *instanceAccount) PollDeviceAuth(context.Context, string) (string, string, []CharacterInfo, error) {
	out := make([]CharacterInfo, 0, len(f.chars))
	for i, name := range f.chars {
		out = append(out, CharacterInfo{ID: string(rune('a' + i)), Name: name})
	}
	return "authed", "acct-1", out, nil
}

func (f *instanceAccount) IssueSessionAssertion(_ context.Context, account, character, session string) (string, bool, error) {
	tok, err := assertion.Sign(f.priv, assertion.Claims{
		Account: account, Character: character, Session: session,
		Expires: time.Now().Add(time.Minute).Unix(),
	})
	return tok, false, err
}

// newInstanceShard builds a shard on the REAL demo pack, hosting midgaard ONLY.
//
// NewShardFromContent, not NewShard: the bare constructor never calls setContent, so liveContent() is nil and
// every mint refuses at validateMintTemplate — a shard on which this entire file would be vacuously green.
//
// Hosting midgaard alone is the premise assertion made structural, and it is checked here so that a future
// change to the fixture cannot quietly turn these into tests of an ordinary cross-zone exit.
func newInstanceShard(t *testing.T, addr string, pub ed25519.PublicKey, opts ...func(*world.Shard)) *world.Shard {
	t.Helper()
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatalf("load demo pack: %v", err)
	}
	sh := world.NewShardFromContent(lc, []string{"midgaard"}, "midgaard", addr, nil, nil).WithVerifyKey(pub)
	for _, o := range opts {
		o(sh)
	}
	if sh.ZoneByID("crypt") != nil {
		t.Fatal("this shard hosts the SHARED crypt, so arriving in a crypt room proves nothing about " +
			"instancing: every assertion below would pass against the guild hall's ordinary `down` exit")
	}
	return sh
}

// walkToTheDoor logs a terminal in and walks it to the guild hall, where the demo pack declares the
// instance entrance. It returns with the guild hall already rendered.
func walkToTheDoor(t *testing.T, term *terminal) {
	t.Helper()
	term.expect(t, "To sign in, open this link")
	term.expect(t, "Choose a character:")
	term.send(t, "1")
	term.expect(t, "The Temple Square")
	term.send(t, "west")
	term.expect(t, "The Adventurers' Guild")
}

// TestInstanceEntryJourneyThroughTheGate is the issue's headline gap: a player TYPES A DIRECTION and ends up
// in a runtime-minted private zone, through a real gate.
func TestInstanceEntryJourneyThroughTheGate(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	const addr = "addr-a"
	h := newHarness(t)
	sh := newInstanceShard(t, addr, pub)
	h.serveShard(addr, sh)
	h.serveGate(directory.Static{Addr: addr})
	h.srv.WithAccountClient(&instanceAccount{chars: []string{"Delver", "Bystander"}, priv: priv})

	delver := h.dial(t)
	walkToTheDoor(t, delver)

	// A SECOND player in the same room, so the isolation assertion later has a subject — and so we can prove
	// they CAN hear each other here, before proving they cannot once inside.
	bystander := h.dial(t)
	bystander.expect(t, "To sign in, open this link")
	bystander.expect(t, "Choose a character:")
	bystander.send(t, "2")
	bystander.expect(t, "The Temple Square")
	bystander.send(t, "west")
	bystander.expect(t, "The Adventurers' Guild")

	// THE POSITIVE CONTROL for the isolation test below. Without it, "Bystander did not hear Delver inside
	// the instance" is equally satisfied by a broken `say`, a dropped stream, or a wrong room — none of which
	// have anything to do with instancing.
	delver.send(t, "say meet me at the door")
	bystander.expect(t, "meet me at the door")

	// Walk through the declared entrance (#435). The mint is asynchronous — a full zone build on a shard
	// worker — so the acknowledgement comes first and the arrival lands hops later. term.expect polls to a
	// deadline rather than sleeping, which is the right wait for exactly this.
	delver.send(t, "enter")
	delver.expect(t, "The way begins to open")
	delver.expect(t, "A Crumbling Stair")

	// The shard still hosts no zone called `crypt`, so what Delver is standing in was minted, not entered.
	if sh.ZoneByID("crypt") != nil {
		t.Fatal("the shared crypt appeared on this shard, so the arrival above is no longer evidence of a mint")
	}

	// Bystander stayed put: entering an instance is not a room-wide event.
	if bystander.tryExpect("A Crumbling Stair", 500*time.Millisecond) {
		t.Fatal("the bystander was shown the crypt without walking anywhere")
	}

	// THE ISOLATION PROOF. Bystander now walks through the SAME door, so both are in a room named "A
	// Crumbling Stair" — and they must not be able to hear each other, because they are in different copies.
	bystander.send(t, "enter")
	bystander.expect(t, "The way begins to open")
	bystander.expect(t, "A Crumbling Stair")

	delver.send(t, "say can you hear me")
	if bystander.tryExpect("can you hear me", 750*time.Millisecond) {
		t.Fatal("two players who each walked through the dungeon door are in the SAME room: they can hear " +
			"each other, so they were routed into one shared zone rather than into private copies. This is " +
			"the assertion the room name cannot make — both copies render an identical name")
	}

	// And the instance is a real, walkable zone rather than a single room: move deeper, then back out through
	// the anchor. expectCount, because "The Adventurers' Guild" is already in the buffer from before the mint
	// — a plain expect would be satisfied by that stale render and would pass even if the exit did nothing.
	delver.send(t, "north")
	delver.expect(t, "The Ossuary")
	delver.send(t, "south")
	delver.send(t, "up")
	delver.expectCount(t, "The Adventurers' Guild", 2)

	delver.close(t)
	bystander.close(t)
}

// TestReconnectFromInsideAnInstanceLandsAtTheDoor pins the exit ANCHOR through the real attach-routing path.
//
// An instance is ephemeral and unleased, so its id can never be a durable location: a player who quits inside
// one must come back at the door they entered by. That contract lives in durableLocation, and the failure it
// guards against — routing a reconnect at a zone that no longer exists — has shipped before.
func TestReconnectFromInsideAnInstanceLandsAtTheDoor(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	const addr = "addr-a"
	h := newHarness(t)
	sh := newInstanceShard(t, addr, pub, func(s *world.Shard) {
		s.WithPersistence(world.NewMemStore(), nil) // a durable location to come back to
	})
	h.serveShard(addr, sh)
	h.serveGate(directory.Static{Addr: addr})
	h.srv.WithAccountClient(&instanceAccount{chars: []string{"Returner"}, priv: priv})

	term := h.dial(t)
	walkToTheDoor(t, term)
	term.send(t, "enter")
	term.expect(t, "A Crumbling Stair")

	// Walk DEEPER before quitting. This is what makes the test mean what it says: if the player quit in the
	// instance's start room, "came back at the door" would be indistinguishable from "came back where I was",
	// and a total failure to record the anchor could still look correct.
	term.send(t, "north")
	term.expect(t, "The Ossuary")
	term.send(t, "quit")
	term.close(t)

	// Reconnect. The anchor is the guild hall — the room they walked in FROM — not the ossuary they were
	// standing in, and not the instance's start room.
	back := h.dial(t)
	back.expect(t, "To sign in, open this link")
	back.expect(t, "Choose a character:")
	back.send(t, "1")
	back.expect(t, "The Adventurers' Guild")

	// Asserted as a negative too, because "landed at the guild hall" alone could be satisfied by a fresh
	// spawn that lost the character's location entirely — and the crypt rooms are the specific wrong answers.
	if back.tryExpect("The Ossuary", 300*time.Millisecond) {
		t.Fatal("the reconnect landed INSIDE the instance the player quit from. That zone is ephemeral and " +
			"unleased — nothing will be hosting it after a restart, so this is how a character becomes " +
			"unreachable")
	}
	back.close(t)
}

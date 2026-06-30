package gate

import (
	"context"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/directory"
)

// chargen_journey_test.go — Phase 15.4 BLACK-BOX prompt-driven creation: a brand-new account (no characters)
// authes via the device flow, then walks the content chargen prompts (pick a race, allocate a point-buy, name)
// and spawns into the world.

// chargenFakeAccount authes a device login to a 0-character account and serves a small chargen flow; it accepts
// any valid creation. It embeds stubAccountClient for the unused methods.
type chargenFakeAccount struct {
	stubAccountClient
}

func (chargenFakeAccount) StartDeviceAuth(context.Context, string) (string, string, time.Duration, error) {
	return "DEV", "http://localhost:8080/login/DEV", 5 * time.Millisecond, nil
}

func (chargenFakeAccount) PollDeviceAuth(context.Context, string) (string, string, []CharacterInfo, error) {
	return "authed", "acct-1", nil, nil // authed, but no characters yet
}

func (chargenFakeAccount) GetChargenFlow(context.Context) (bool, []ChargenStep, []ChargenBundleOption, int, error) {
	return true,
		[]ChargenStep{
			{Kind: "bundle_choice", ID: "race", Prompt: "Choose your race", BundleKind: "race"},
			{Kind: "point_buy", ID: "attrs", Prompt: "Allocate your attributes", Attributes: []string{"strength"}, Points: 9, Base: 8, Min: 8, Max: 15},
		},
		[]ChargenBundleOption{{Ref: "elf", Kind: "race", Label: "Elf"}, {Ref: "dwarf", Kind: "race", Label: "Dwarf"}},
		3, // max characters
		nil
}

func (chargenFakeAccount) CreateChargenCharacter(_ context.Context, _, name string, _ map[string]string, _ map[string]map[string]int) (string, string, bool, error) {
	return "id-" + name, "", false, nil
}

// atCapFakeAccount authes to an account already holding the max (2) characters, with a 2-character cap.
type atCapFakeAccount struct {
	chargenFakeAccount
}

func (atCapFakeAccount) PollDeviceAuth(context.Context, string) (string, string, []CharacterInfo, error) {
	return "authed", "acct-1", []CharacterInfo{{ID: "c1", Name: "Anya"}, {ID: "c2", Name: "Byron"}}, nil
}

func (atCapFakeAccount) GetChargenFlow(context.Context) (bool, []ChargenStep, []ChargenBundleOption, int, error) {
	configured, steps, options, _, err := chargenFakeAccount{}.GetChargenFlow(context.Background())
	return configured, steps, options, 2, err // cap == 2, already full
}

// TestChargenAtCapacityStaysOnSelect: a full account is NOT offered "create" — it sees only its characters and
// the limit note, so it stays at the selection menu (the user-reported fix).
func TestChargenAtCapacityStaysOnSelect(t *testing.T) {
	const addr = "addr-a"
	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})
	h.srv.WithAccountClient(&atCapFakeAccount{})

	term := h.dial(t)
	term.expect(t, "To sign in, open this link")
	term.expect(t, "Choose a character:")
	term.expect(t, "1) Anya")
	term.expect(t, "2) Byron")
	term.expect(t, "limit") // the at-limit note; NO "Create a new character" option
	term.send(t, "2")
	term.expect(t, "The Temple Square") // picking an existing character still works
	term.close(t)
}

func TestChargenCreateJourney(t *testing.T) {
	const addr = "addr-a"
	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})
	h.srv.WithAccountClient(&chargenFakeAccount{})

	term := h.dial(t)

	term.expect(t, "To sign in, open this link")
	// Authed with no characters -> straight into prompt-driven creation.
	term.expect(t, "You have no characters yet")
	term.expect(t, "Choose your race")
	term.expect(t, "1) Elf")
	term.expect(t, "2) Dwarf")
	term.send(t, "1") // Elf

	term.expect(t, "Allocate your attributes")
	term.expect(t, "strength")
	term.send(t, "15")

	term.expect(t, "Name your character")
	term.send(t, "Newbie")

	term.expect(t, "Character created!")
	term.expect(t, "The Temple Square") // the new character spawns into the world
	term.close(t)
}

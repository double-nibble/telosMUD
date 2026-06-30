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

func (chargenFakeAccount) GetChargenFlow(context.Context) (bool, []ChargenStep, []ChargenBundleOption, error) {
	return true,
		[]ChargenStep{
			{Kind: "bundle_choice", ID: "race", Prompt: "Choose your race", BundleKind: "race"},
			{Kind: "point_buy", ID: "attrs", Prompt: "Allocate your attributes", Attributes: []string{"strength"}, Points: 9, Base: 8, Min: 8, Max: 15},
		},
		[]ChargenBundleOption{{Ref: "elf", Kind: "race", Label: "Elf"}, {Ref: "dwarf", Kind: "race", Label: "Dwarf"}},
		nil
}

func (chargenFakeAccount) CreateChargenCharacter(_ context.Context, _, name string, _ map[string]string, _ map[string]map[string]int) (string, string, error) {
	return "id-" + name, "", nil
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

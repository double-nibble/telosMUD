package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/account"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/tests/helpers"
)

// account_chargen_journey_test.go — Phase 14 CAPSTONE (gated, real Postgres). It ties the account-side chargen
// path to the durable store using the REAL demo content flow: an OAuth account is created, the account Service
// validates a chargen submission (pick a race + class, allocate the point-buy) against the loaded content, and
// the character is written; the world's load path then reads back the exact first-spawn marker (the chosen
// bundles + bought attributes) it will apply on first spawn. The world-side application + the reload-without-
// reapply guarantee are proven hermetically in internal/world; this capstone proves the account validation +
// the real DB write/read that feed it.
func TestChargenAccountJourneyCapstone(t *testing.T) {
	p := helpers.OpenTestPool(t)
	ctx := context.Background()

	// Load the real demo chargen flow + bundle options (exactly as telos-account does at boot).
	lc, err := content.Load(ctx, p, []string{content.DemoPack})
	require.NoError(t, err)
	require.NotEmpty(t, lc.Chargens, "the demo pack must define a chargen flow")
	flow := lc.Chargens[0]
	var options []content.ChargenBundleOption
	for _, b := range lc.Bundles {
		if b.Kind == "profession" {
			continue
		}
		options = append(options, content.ChargenBundleOption{Ref: b.Ref, Kind: b.Kind, Label: b.Ref})
	}

	// A real account (the OAuth identity path mints the accounts row the character FK needs).
	uid := "cap-" + time.Now().Format("150405.000000")
	acct, err := p.CreateAccountWithIdentity(ctx, "github", uid, "", "Capstone Tester")
	require.NoError(t, err)

	svc := account.New(p, nil, "midgaard", "midgaard:room:temple").WithChargen(flow, options)

	name := "CapChar" + time.Now().Format("150405")
	picks := map[string]string{"race": "elf", "class": "fighter"}
	allocs := map[string]map[string]int{"attributes": {"strength": 15, "intellect": 13, "constitution": 13}} // 9+5+5 = 19 <= 27

	id, reason, err := svc.BuildCharacter(ctx, acct, name, picks, allocs)
	require.NoError(t, err)
	require.Empty(t, reason, "the submission should validate against the demo flow")
	require.NotEmpty(t, id)

	// The world's load path reads back the recorded first-spawn marker: the chosen bundles + bought stats.
	snap, found, err := p.LoadCharacter(ctx, name)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, snap.PendingChargen, "a chargen-built character must carry its first-spawn marker until it spawns")
	assert.ElementsMatch(t, []string{"elf", "fighter"}, snap.PendingChargen.Bundles)
	assert.Equal(t, 15.0, snap.PendingChargen.Attrs["strength"])
	assert.Equal(t, 13.0, snap.PendingChargen.Attrs["constitution"])

	// A bad submission (a class ref in the race slot) is rejected with a user-facing reason, no character.
	_, reason, err = svc.BuildCharacter(ctx, acct, name+"X", map[string]string{"race": "fighter", "class": "fighter"}, allocs)
	require.NoError(t, err)
	require.NotEmpty(t, reason, "a wrong-kind race pick must be rejected")
}

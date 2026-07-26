package world

import (
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// credits_test.go — the `credits` verb (#519): renders the loaded packs' license/attribution metadata.

// TestCmdCredits lists every crediting pack's license id + attribution notice, in order.
func TestCmdCredits(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.defBundle().packCredits = []content.PackCredit{
		{Pack: "demo", License: "CC0-1.0", Attribution: "public domain dedication"},
		{Pack: "srd", License: "CC-BY-4.0", Attribution: "SRD 5.1 (c) WotC, CC-BY-4.0"},
	}
	drainOutputs(caster)
	z.dispatch(caster, "credits")
	out := strings.Join(drainOutputs(caster), "\n")
	for _, want := range []string{
		"Content credits", "demo", "CC0-1.0", "public domain dedication",
		"srd", "CC-BY-4.0", "SRD 5.1 (c) WotC, CC-BY-4.0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("credits output missing %q; got:\n%s", want, out)
		}
	}
}

// TestCmdCreditsAliases proves the `license` and `copyright` aliases resolve to the same handler.
func TestCmdCreditsAliases(t *testing.T) {
	for _, verb := range []string{"license", "copyright"} {
		t.Run(verb, func(t *testing.T) {
			z, caster := abilityTestZone(t)
			z.defBundle().packCredits = []content.PackCredit{{Pack: "demo", License: "MIT"}}
			drainOutputs(caster)
			z.dispatch(caster, verb)
			out := strings.Join(drainOutputs(caster), "\n")
			if !strings.Contains(out, "demo") || !strings.Contains(out, "MIT") {
				t.Fatalf("`%s` alias did not render credits; got:\n%s", verb, out)
			}
		})
	}
}

// TestCmdCreditsDemoEndToEnd is the WIRING/LIVE-PATH guard (#519): it builds a real demo zone through the
// embedded pack (packtree fold -> Merge -> buildZone stamps d.packCredits) and dispatches `credits`,
// asserting the demo's actual CC0 attribution renders. This pins the whole file->verb chain — including the
// mergePacks scalar fold and the `d.packCredits = lc.Credits` build wiring that unit tests bypass by
// injecting the bundle directly. A drop anywhere on that path (as the packtree fold silently did) fails here.
func TestCmdCreditsDemoEndToEnd(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := newTestPlayerEntity(z, "Crediter")
	Move(s.entity, z.rooms["midgaard:room:temple"])
	drainOutputs(s)
	z.dispatch(s, "credits")
	out := strings.Join(drainOutputs(s), "\n")
	if !strings.Contains(out, "demo") || !strings.Contains(out, "CC0-1.0") {
		t.Fatalf("demo `credits` did not surface the pack's live CC0 license; got:\n%s", out)
	}
	if !strings.Contains(out, "public domain") {
		t.Fatalf("demo `credits` did not surface the attribution notice; got:\n%s", out)
	}
}

// TestCmdCreditsEmpty proves a world with no declared credits reports a clean notice, not an empty listing.
func TestCmdCreditsEmpty(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.defBundle().packCredits = nil
	drainOutputs(caster)
	z.dispatch(caster, "credits")
	out := strings.Join(drainOutputs(caster), "\n")
	if !strings.Contains(out, "no content credits") {
		t.Fatalf("empty credits: want the no-credits notice, got:\n%s", out)
	}
}

// TestCmdCreditsOmitsMissingFields proves a credit with only a license (no attribution) renders without a
// dangling attribution line, and an attribution-only credit renders without the " — <license>" separator.
// The NotContains assertions pin the "no dangling separator" property — a Contains-only test would false-
// green if the separator were emitted unconditionally.
func TestCmdCreditsOmitsMissingFields(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.defBundle().packCredits = []content.PackCredit{
		{Pack: "licenseonly", License: "MIT"},
		{Pack: "attribonly", Attribution: "just an attribution"},
	}
	drainOutputs(caster)
	z.dispatch(caster, "credits")
	out := strings.Join(drainOutputs(caster), "\n")
	if !strings.Contains(out, "licenseonly") || !strings.Contains(out, "MIT") {
		t.Fatalf("license-only credit missing; got:\n%s", out)
	}
	if !strings.Contains(out, "attribonly") || !strings.Contains(out, "just an attribution") {
		t.Fatalf("attribution-only credit missing; got:\n%s", out)
	}
	// Exactly ONE " — " license separator must appear — the license-only credit. If the separator were
	// emitted unconditionally, the attribution-only credit would add a dangling second one. (A substring
	// like "attribonly — " can't be used directly: colorize() inserts an ANSI reset between the pack name
	// and the separator, so the count is the robust pin.)
	if n := strings.Count(out, " — "); n != 1 {
		t.Fatalf("expected exactly one license separator (the license-only credit), got %d; out:\n%s", n, out)
	}
}

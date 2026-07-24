package world

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/stretchr/testify/require"
)

// packbuildhealth_test.go asserts that the SHIPPED packs survive the real content-build path without
// any definition degrading.
//
// # Why this test exists, and why it is not redundant with the per-feature tests
//
// Every content-build failure in this engine is deliberately SWALLOWED so that one bad definition
// cannot turn into an outage: buildAbilityDef's error leaves an ability registered with no ops
// ("registering with parsed ops only"), buildCombatProfile's leaves a profile without its to-hit, a
// malformed op-list registers whatever parsed. That posture is right — refusing to boot on a content
// defect is worse — but it means the ONLY signal is an ERROR log, and a log nobody reads is not a
// signal at all.
//
// Measured before this test existed: introducing a single mistyped key into the demo pack's melee
// to-hit silently converted the profile to auto-hit, and the entire internal/world suite — twenty-odd
// files, including the combat tests — stayed green. The ability path happened to be covered (the
// fireball end-to-end tests fail loudly), the combat-profile path was not.
//
// So this is deliberately generic: it does not know about checks, boons, or any particular feature. It
// asserts the invariant "the packs we ship build cleanly", which catches the next silent degradation
// as well as this one. It is the cheapest possible net for a whole class of defect that is otherwise
// invisible until someone plays the game.
func TestShippedPacksBuildWithoutDegrading(t *testing.T) {
	for _, load := range []struct {
		name string
		fn   func() (*content.LoadedContent, error)
	}{
		{"demo", content.LoadDemoPack},
		{"core", content.LoadCorePack},
	} {
		t.Run(load.name, func(t *testing.T) {
			lc, err := load.fn()
			require.NoError(t, err, "the shipped %q pack must load", load.name)

			// Capture ERROR-level build diagnostics. slog.SetDefault is process-global, so this test
			// cannot run in parallel — the same constraint the existing log-capture tests live with.
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
			defineGlobals(newDefRegistries(), lc)
			slog.SetDefault(prev)

			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				if line == "" {
					continue
				}
				require.NotContains(t, line, `"content:`,
					"the shipped %q pack degraded a definition at build time. In production this is a SILENT "+
						"downgrade (an ability with no ops, a combat profile that cannot resolve a swing) whose "+
						"only trace is this log line", load.name)
			}
		})
	}
}

// TestShippedPacksBuildHealthIsNotVacuous proves the test above can actually fail. A build-health
// assertion that has never been seen red is indistinguishable from one that inspects nothing — and
// this one reads a log buffer, which is exactly the shape that silently stops matching.
func TestShippedPacksBuildHealthIsNotVacuous(t *testing.T) {
	lc, err := content.LoadDemoPack()
	require.NoError(t, err)
	require.NotEmpty(t, lc.CombatProfiles, "the demo pack must ship a combat profile for this to test anything")

	// Inject the exact defect the real experiment used: one mistyped key in a to-hit spec.
	for i := range lc.CombatProfiles {
		if m, ok := lc.CombatProfiles[i].ToHit.(map[string]any); ok {
			m["boon_dize"] = "2d20kh1"
			break
		}
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	defineGlobals(newDefRegistries(), lc)
	slog.SetDefault(prev)

	require.Contains(t, buf.String(), "content:",
		"a mistyped key in a shipped to-hit must produce the ERROR the health test keys on")
	require.Contains(t, buf.String(), "boon_dize",
		"the diagnostic must name the offending key so a builder can fix it")
}

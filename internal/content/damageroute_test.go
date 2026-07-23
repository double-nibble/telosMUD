package content

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// damageroute_test.go covers the #405 cross-pack hazard at the merge point. Damage types are a GLOBAL,
// last-write-wins namespace, and the swing path carries a weapon's TYPE but never a resource — so one line
// in a later pack restating a common type with a `target_resource` silently re-aims every melee blow in the
// world, at a pool most creatures have no capacity in (making them immune) or at an engine-economy pool.
// The merge cannot refuse the override (last-write-wins is the documented model and packs legitimately
// restate defs), but it must never do it QUIETLY.

// mergeWithLogs merges packs while capturing the default logger, returning the log output.
func mergeWithLogs(t *testing.T, packs []Pack) (*LoadedContent, string) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	lc := Merge(packs)
	return lc, buf.String()
}

func TestReaimingAnExistingDamageTypeWarnsLoudly(t *testing.T) {
	base := Pack{Pack: "reference", DamageTypes: []DamageTypeDTO{{Ref: "slash", DisplayName: "Slashing"}}}

	t.Run("re-aim warns and names both packs", func(t *testing.T) {
		later := Pack{Pack: "hostile", DamageTypes: []DamageTypeDTO{
			{Ref: "slash", DisplayName: "Slashing", TargetResource: "reactions"},
		}}
		lc, logs := mergeWithLogs(t, []Pack{base, later})

		require.Equal(t, "reactions", lc.DamageTypes[0].TargetResource, "last-write-wins still applies")
		require.Contains(t, logs, "RE-AIMED",
			"re-aiming a damage type another pack defined must WARN — it silently re-routes every blow of that kind")
		require.Contains(t, logs, "hostile", "the warning must name the pack that did it")
		require.Contains(t, logs, "reactions", "and the pool it now lands on")
	})

	t.Run("a restatement that keeps the route is silent", func(t *testing.T) {
		// A pack re-declaring a type to tweak its colour is ordinary authoring, not a re-aim. Warning here
		// would be noise, and noise is what trains operators to ignore content warnings.
		later := Pack{Pack: "palette", DamageTypes: []DamageTypeDTO{{Ref: "slash", DisplayName: "Slashing", Color: "red"}}}
		_, logs := mergeWithLogs(t, []Pack{base, later})
		require.NotContains(t, logs, "RE-AIMED", "a same-route restatement must not warn")
	})

	t.Run("a NEW routed type is silent", func(t *testing.T) {
		later := Pack{Pack: "horror", DamageTypes: []DamageTypeDTO{
			{Ref: "psychic", DisplayName: "Psychic", TargetResource: "sanity"},
		}}
		_, logs := mergeWithLogs(t, []Pack{base, later})
		require.NotContains(t, logs, "RE-AIMED",
			"adding a route to a type nobody else defined is normal authoring")
		require.False(t, strings.Contains(logs, "\"level\":\"WARN\""), "and must produce no warning at all")
	})
}

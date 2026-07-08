package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/store"
)

// settier.go — the `telos-account set-tier` break-glass CLI (#108). The in-game promote/demote flow can leave
// zero admins (there is no last-admin demote guard), and the config-pin (TELOS_BOOTSTRAP_ADMIN) only applies
// at account CREATION, so it can't recover an existing account. This subcommand is the sanctioned recovery:
// run by whoever has DB/host access (which IS the authorization), it forces an account's tier directly and
// audits it with a system (NULL) actor — the same audit shape as the bootstrap grant.
//
// It deliberately BYPASSES the in-game admin check and the promote ceilings (#165): those protect an in-world
// actor from exceeding its standing, but a host operator running this binary is already fully trusted. It does
// validate the target tier against the loaded content ladder (so a typo can't strand an account on a
// nonexistent tier), which `--force` overrides for the case where the ladder itself is what's broken.
//
// Usage:
//
//	telos-account set-tier --character <name> --tier <tier> [--force yes]
//
// It reuses the service's config (TELOS_* env / config file) for the Postgres DSN and the content pack, so it
// resolves the SAME ladder the running service uses.

// runSetTierCLI executes the set-tier subcommand and returns a process exit code (0 = success).
func runSetTierCLI(args []string) int {
	fs := flag.NewFlagSet("set-tier", flag.ContinueOnError)
	character := fs.String("character", "", "the character NAME whose account tier to set (required)")
	tier := fs.String("tier", "", "the tier to set, e.g. admin (required)")
	force := fs.String("force", "", "set to \"yes\" to skip validating the tier against the content ladder")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: telos-account set-tier --character <name> --tier <tier> [--force yes]")
		fmt.Fprintln(os.Stderr, "\nBreak-glass recovery: forces an account's trust tier directly (host access is the")
		fmt.Fprintln(os.Stderr, "authorization). Audited with a system actor. Bypasses the in-game admin check + ceilings.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *character == "" || *tier == "" {
		fmt.Fprintln(os.Stderr, "set-tier: --character and --tier are required")
		fs.Usage()
		return 2
	}

	cfg, err := config.Load(config.PathFromEnv())
	if err != nil {
		fmt.Fprintf(os.Stderr, "set-tier: config load failed: %v\n", err)
		return 1
	}
	if cfg.Postgres.DSN == "" {
		fmt.Fprintln(os.Stderr, "set-tier: no Postgres DSN configured (set TELOS_POSTGRES_DSN or the config file)")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.Open(ctx, cfg.Postgres.DSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "set-tier: store open failed: %v\n", err)
		return 1
	}
	defer pool.Close()

	// Validate the tier against the SAME ladder the service loads, unless --force. This resolves the content
	// pack (falling back to the built-in ladder on a load error, exactly like the serving path).
	forced := *force == "yes"
	if !forced {
		if msg := validateTier(loadLadder(ctx, pool, cfg.ContentPacks), *tier); msg != "" {
			fmt.Fprintln(os.Stderr, "set-tier: "+msg)
			return 1
		}
	} else {
		fmt.Fprintf(os.Stderr, "set-tier: --force yes given; NOT validating %q against the content ladder\n", *tier)
	}

	// Resolve the character to its owning account.
	acct, found, err := pool.AccountByCharacterName(ctx, *character)
	if err != nil {
		fmt.Fprintf(os.Stderr, "set-tier: resolve character: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintf(os.Stderr, "set-tier: no such character %q\n", *character)
		return 1
	}

	old, err := pool.SetAccountTierSystem(ctx, acct, *tier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "set-tier: write failed: %v\n", err)
		return 1
	}
	fmt.Printf("%s (account %s): %s -> %s  (takes effect on their next login; audited as a system action)\n",
		*character, acct, old, *tier)
	return 0
}

// validateTier is the pure validation DECISION (unit-testable without a DB): it returns an operator-facing
// error message when tier is not a defined rung of ladder, or "" when it is fine. Kept separate from the
// content/DB loading so the branch the CLI turns on has direct coverage.
func validateTier(ladder *content.TrustLadder, tier string) string {
	if ladder.Has(tier) {
		return ""
	}
	names := ladder.Names()
	sort.Strings(names)
	return fmt.Sprintf("%q is not a defined tier (known: %s). Use --force yes to override.",
		tier, strings.Join(names, ", "))
}

// loadLadder resolves the content trust ladder the SAME way the serving path does (#246/#255): the
// registry-resolved enabled pack set (shared content.ResolveEnabledPacks), LoadWithCore'd like the world —
// NOT a hardcoded demo pack, which would validate a real-pack tier as "unknown" (forcing --force) or wrongly
// accept a demo-only tier. On any read/load error it falls back to the built-in default ladder: the CLI is
// break-glass, so it must stay usable when content is unreadable (that is exactly when recovery is needed),
// and --force yes overrides validation entirely if the fallback ladder is wrong.
func loadLadder(ctx context.Context, pool *store.Pool, packsOverride []string) *content.TrustLadder {
	var registryPacks []string
	if info, verr := pool.CurrentContentVersion(ctx); verr == nil {
		registryPacks = info.Packs
	}
	enabled := content.ResolveEnabledPacks(packsOverride, registryPacks)
	lc, err := content.LoadWithCore(ctx, pool, enabled)
	if err != nil || lc == nil {
		return content.NewTrustLadder(nil) // built-in default (player/builder/admin)
	}
	return content.NewTrustLadder(lc.TrustTiers)
}

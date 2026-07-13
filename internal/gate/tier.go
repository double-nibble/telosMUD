package gate

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/double-nibble/telosmud/internal/telnet"
)

// tier.go — the edge-local admin verbs `promote <character> <tier>` and `demote <character>` (#27). They
// change an account's trust tier via the account service. Handled at the GATE (not forwarded to the world)
// because the account client lives here; AUTHORIZATION IS ENFORCED AT THE ACCOUNT SERVICE (the actor must
// be an admin per the authoritative store). The new tier takes effect on the target's NEXT login (the assertion
// re-signs the tier). Like `color`, these are reserved gate verbs.
//
// STAFF-VERB VISIBILITY (#369): before it intercepts anything, handleTierCommand checks canManageTiers — the
// resolved manage-tiers capability the account service returned with the assertion. A NON-staff actor's
// promote/demote is NOT intercepted (returns false), so the line falls through to the world, which has no such
// verb → "Huh?". This gives promote/demote the same wiz-command posture a MinRank world verb has (invisible
// below its rank): no usage hint, no authorization refusal, and NO account-service round-trip leak the verb's
// existence to a mortal. The account service stays AUTHORITATIVE — canManageTiers is defense-in-depth VISIBILITY
// ONLY, never the trust decision: a bit that reads too high still hits the service's refusal (fail-closed), and
// one that reads too low only yields "Huh?" (fail-safe).

// handleTierCommand intercepts promote/demote for a STAFF actor. Returns true when the line was intercepted (so
// the pump does not forward it to the world). For a non-staff actor (canManageTiers=false) it returns false for
// those verbs too, so they fall through to the world as unknown ("Huh?"). actorAccount is the logged-in account
// id; the blocking RPC is bounded by a short timeout so a hung account service can't wedge the input pump.
func handleTierCommand(ctx context.Context, tc *telnet.Conn, ac AccountClient, actorAccount string, canManageTiers bool, line string, log *slog.Logger) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	// Visibility gate FIRST (#369): only the promote/demote verbs are ours, and only for a staff actor. A
	// non-staff actor (or any other verb) falls straight through — the line is never intercepted, so no usage,
	// no refusal, and no RPC ever betrays that these verbs exist.
	switch strings.ToLower(fields[0]) {
	case "promote", "demote":
		if !canManageTiers {
			return false
		}
	default:
		return false
	}
	var target, tier string
	switch strings.ToLower(fields[0]) {
	case "promote":
		if len(fields) != 3 {
			// Tiers are content-defined (#29): the edge does not enumerate them. An unknown tier is refused
			// by the account service with the known-tier list, so the authoritative vocabulary lives there.
			_ = tc.Write("Usage: promote <character> <tier>\r\n")
			return true
		}
		target, tier = fields[1], strings.ToLower(fields[2])
	case "demote":
		if len(fields) != 2 {
			_ = tc.Write("Usage: demote <character>   (sets the account to the baseline tier)\r\n")
			return true
		}
		// An EMPTY tier is the demote-to-BASELINE sentinel (#112): the account service resolves it to the
		// ladder's lowest-rank tier. The edge must NOT hardcode "player" — a content pack may rename or omit
		// that tier, which would make every demote fail closed with "Unknown tier". The gate does not know the
		// ladder vocabulary; the account service (which owns it) does the resolution.
		target, tier = fields[1], ""
	default:
		return false
	}

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ok, reason, oldTier, appliedTier, err := ac.SetAccountTier(rctx, actorAccount, target, tier)
	if err != nil {
		log.Warn("SetAccountTier failed", "target", target, "err", err)
		_ = tc.Write("The tier service is unavailable right now.\r\n")
		return true
	}
	if !ok {
		_ = tc.Write(reason + "\r\n")
		return true
	}
	// appliedTier is what the service actually set — for `demote` that is the RESOLVED baseline (the request
	// sent the empty sentinel), so the confirmation reads the true tier rather than a hardcoded name (#112).
	_ = tc.Write(target + ": " + oldTier + " -> " + appliedTier + " (takes effect on their next login).\r\n")
	return true
}

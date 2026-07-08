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
// be an admin per the authoritative store), so a non-admin's attempt is simply refused with a message — the
// gate makes no trust decision. The new tier takes effect on the target's NEXT login (the assertion re-signs
// the tier). Like `color`, these are reserved gate verbs.

// handleTierCommand intercepts promote/demote. Returns true when the line was one (so the pump does not
// forward it to the world). actorAccount is the logged-in account id; the blocking RPC is bounded by a short
// timeout so a hung account service can't wedge the input pump.
func handleTierCommand(ctx context.Context, tc *telnet.Conn, ac AccountClient, actorAccount, line string, log *slog.Logger) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
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

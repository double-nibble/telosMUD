package world

import "strings"

// toggles.go — runtime-tweakable per-staff "view" toggles (#30, Track 2): session-scoped switches a trust-
// gated staff member flips LIVE to change what they perceive. Registered as staff verbs (MinRank=rankStaff,
// stat.go), so they are invisible to players. This slice ships the two that hook EXISTING consumers:
//
//   - holylight on|off — the see-all switch (visibility.go visibleTo). A staffer whose tier grants
//     holylight (the ladder) may turn it OFF to perceive the world as a mortal does, and back ON. It is
//     the reserved flagHolylight on the entity, so a fresh login RECONCILES it to the tier grant
//     (applyTierFlags) — turning it off is therefore session-scoped (relog restores the grant). Turning it
//     ON is CAPPED: only a tier that actually grants holylight may, so a staffer with no see-all grant
//     cannot self-elevate. Uses the TRUSTED setFlag path (content's set_flag can't touch reserved flags).
//   - rolls on|off — surface the actor's OWN check roll math the engine would otherwise hide by default
//     (check.go emitCheck reads session.showRolls). A pure session pref; content's explicit hide is kept.
//   - wizinvis on|off — staff invisibility: hide from anyone of LOWER trust rank (visibility.go visibleTo).
//     flagWizinvis is reserved (tier.go), so it is content-unsettable, un-persisted, and CLEARED at login
//     (session-scoped — a relog drops it). Its POWER scales with rank: a higher-rank staffer hides from
//     more, so no per-tier cap is needed (unlike holylight); the rank comparison IS the gate.
//
// Deferred (its own design pass): verbose debug echoes — needs a per-session debug-event surface.

// staffToggleCommands returns the staff view-toggle verbs (#30). Registered LAST (low priority) with the
// other staff verbs; each carries MinRank=rankStaff so a player can neither see nor run them.
func staffToggleCommands() []*Command {
	return []*Command{
		{Name: "holylight", MinRank: rankStaff, Flags: CmdHidden, Run: cmdHolylight},
		{Name: "wizinvis", MinRank: rankStaff, Flags: CmdHidden, Run: cmdWizinvis},
		{Name: "rolls", MinRank: rankStaff, Flags: CmdHidden, Run: cmdRolls},
	}
}

// cmdWizinvis toggles staff invisibility (flagWizinvis). Bare `wizinvis` reports; `wizinvis on|off` sets it.
// No tier-grant cap: any staff member may hide, and visibleTo conceals them only from STRICTLY lower ranks,
// so the concealment's reach is bounded by the actor's own rank. Reserved flag => the trusted setFlag path.
func cmdWizinvis(c *Context) error {
	arg := strings.ToLower(strings.TrimSpace(c.Rest()))
	switch arg {
	case "":
		if hasFlag(c.Actor, flagWizinvis) {
			c.Send("Wizinvis is ON — you are hidden from anyone of lower trust rank.")
		} else {
			c.Send("Wizinvis is OFF — you are visible to all. Use `wizinvis on` to hide from lower ranks.")
		}
	case "on", "enable":
		setFlag(c.Actor, flagWizinvis, true)
		c.z.republishPresenceOnConcealChange(c.Actor) // #98: drop from cross-shard who for lower ranks
		c.Send("Wizinvis ON — hidden from lower trust ranks.")
	case "off", "disable":
		setFlag(c.Actor, flagWizinvis, false)
		c.z.republishPresenceOnConcealChange(c.Actor) // #98: reappear in cross-shard who
		c.Send("Wizinvis OFF — now visible to all.")
	default:
		c.Send("Usage: wizinvis on|off")
	}
	return nil
}

// cmdHolylight toggles the actor's see-all (holylight). Bare `holylight` reports; `holylight on|off` sets
// it. Turning ON requires the actor's tier to grant holylight (the ladder) — a staffer with no see-all
// grant cannot self-elevate. Turning OFF is always allowed (a staffer choosing to see as a mortal).
// Reserved flag => the trusted setFlag path.
func cmdHolylight(c *Context) error {
	arg := strings.ToLower(strings.TrimSpace(c.Rest()))
	switch arg {
	case "":
		if hasFlag(c.Actor, flagHolylight) {
			c.Send("Holylight is ON — you see all (invisible things, past concealment).")
		} else {
			c.Send("Holylight is OFF — you see as a mortal. Use `holylight on` to restore see-all.")
		}
	case "on", "enable":
		if !tierGrantsFlag(c.z, c.s.tier, flagHolylight) {
			c.Send("Your trust tier does not grant see-all.")
			return nil
		}
		setFlag(c.Actor, flagHolylight, true)
		c.Send("Holylight ON.")
	case "off", "disable":
		setFlag(c.Actor, flagHolylight, false)
		c.Send("Holylight OFF — you now see as a mortal (restored to your tier's grant on next login).")
	default:
		c.Send("Usage: holylight on|off")
	}
	return nil
}

// cmdRolls toggles the staff show-own-rolls debug pref (session.showRolls). Bare `rolls` reports; `rolls
// on|off` sets it. emitCheck (check.go) reads the pref to upgrade a default-hidden check to full math.
func cmdRolls(c *Context) error {
	s := c.s
	arg := strings.ToLower(strings.TrimSpace(c.Rest()))
	switch arg {
	case "":
		if s.showRolls {
			c.Send("Roll math is ON — your hidden check rolls are shown to you.")
		} else {
			c.Send("Roll math is OFF. Use `rolls on` to see your own check rolls.")
		}
	case "on", "enable":
		s.showRolls = true
		c.Send("Roll math ON.")
	case "off", "disable":
		s.showRolls = false
		c.Send("Roll math OFF.")
	default:
		c.Send("Usage: rolls on|off")
	}
	return nil
}

// tierGrantsFlag reports whether the named tier grants the reserved flag via z's trust ladder (#30). Used
// to CAP `holylight on` — only a tier that actually grants see-all may turn it back on.
func tierGrantsFlag(z *Zone, tier, flag string) bool {
	for _, f := range z.trustLadder().grantedFlags(tier) {
		if f == flag {
			return true
		}
	}
	return false
}

package world

import (
	"sort"
	"strings"

	"github.com/double-nibble/telosmud/internal/textsan"
)

// commscmds.go holds the Phase-8 slice 8.6 player COMMANDS that mutate the receiver-side comms-state
// subtree (docs/PHASE8-PLAN.md 8.6, P8-D7): `channels [on|off <ref>]`, `ignore [<name>]`, `afk [msg]`.
// They are WORLD commands (registered beside say/who/tell) because they mutate persisted character state
// (commsstate.go) and need the live *Entity + the channel_defs to recompute the effective hear-set. Each
// mutating command re-publishes the player's effective {enabled ∩ hearable} hear-set + ignore list to
// their gate (publishCommsConfig), so the gate's receiver HEAR-filter + ignore funnel are always
// recomputed by the authoritative world. All run on the zone goroutine (single-writer over the session
// comms state). They never release ownership, so dispatch prompts on return.

// commsCommands returns the 8.6 comms-toggle command set, appended to the base table (registerCommands).
// They are lower-priority than movement/look/say (registered last) so abbreviation precedence is
// unchanged. `afk` is excluded from the AFK-clear-on-input rule (clearAFKOnInput) so `afk` re-sets afk.
func commsCommands() []*Command {
	return []*Command{
		{Name: "channels", Run: cmdChannels},
		{Name: "history", Run: cmdHistory},
		{Name: "ignore", Run: cmdIgnore},
		{Name: "afk", Run: cmdAFK},
	}
}

// cmdHistory replays a channel's recent SHARD-LOCAL scrollback (`history <channel>`) for #348. It is the
// retrieval half the P8-D3 channel_def deferred, and it honors that def's load-bearing invariant: the
// fetch is gated on def.canHear against the LIVE entity AT FETCH TIME (step b), so a player who LOST hear
// access cannot replay lines from when they had it. It gates on canHear ONLY — a player who toggled the
// channel OFF is still a member and may read history (the toggle is a display preference, not access).
//
// Existence is not secret (the `channels` list already shows every channel + its access state), so an
// unknown ref reports plainly. The buffer is SHARD-LOCAL and thus PARTIAL on a multi-shard fleet
// (channelhistory.go) — this command serves only what the local shard captured.
func cmdHistory(c *Context) error {
	z, s := c.z, c.s
	ref := strings.ToLower(strings.TrimSpace(c.Rest()))
	if ref == "" {
		c.Send("Usage: history <channel>")
		return nil
	}
	// (a) RESOLVE the channel from the loaded channel_defs (mirrors cmdChannels). Existence is not secret.
	def := z.channelDefs().get(ref)
	if def == nil {
		c.Send("There is no channel called '" + ref + "'.")
		return nil
	}
	// (b) FETCH-TIME GATE (the P8-D3 invariant): evaluate the LIVE hear predicate against the LIVE entity
	// now, NOT "was a member when the line was said". A player who dropped below the channel's access
	// (a stripped flag / an attribute back under the floor) must be refused and see ZERO privileged lines.
	if !def.canHear(s.entity) {
		c.Send("You can't hear that channel.")
		return nil
	}
	// Degrade cleanly on a bare/storeless zone (no shard) — never panic (the empty-engine invariant).
	h := z.channelHistory()
	if h == nil {
		c.Send("No recent history.")
		return nil
	}
	// (c) Snapshot the ring and re-apply the FETCHING player's ignore set, so history matches the LIVE
	// gate funnel (which drops an ignored author's line before the player ever sees it — P8-A6). Applying
	// it here keeps replay and live delivery consistent for the same reader.
	entries := h.snapshot(def.ref)
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		if s.comms.ignored(e.authorID) {
			continue
		}
		lines = append(lines, e.body)
	}
	// (d) Render: the bodies are pre-rendered (format + color intact), joined under a header. Nothing
	// buffered (a history==0 channel, an empty ring, or every line ignored) reports plainly.
	if len(lines) == 0 {
		c.Send("No recent history on " + def.name + ".")
		return nil
	}
	var b strings.Builder
	b.WriteString("Recent history on ")
	b.WriteString(def.name)
	b.WriteString(":")
	for _, ln := range lines {
		b.WriteByte('\n')
		b.WriteString(ln)
	}
	c.Send(b.String())
	return nil
}

// cmdChannels lists the player's channels with on/off state (bare `channels`) or toggles one
// (`channels on|off <ref>`). A toggle records the OVERRIDE on the comms state (vs the channel's
// default_on) and re-publishes the hear-set so the gate (un)subscribes the concrete subject. Only a
// channel the player CAN HEAR can be meaningfully toggled on (a no-access channel toggled on is still
// filtered out of the hear-set by canHear — the access predicate is authoritative); we still record the
// override so it takes effect if the player later gains access.
func cmdChannels(c *Context) error {
	z, s := c.z, c.s
	args := strings.Fields(strings.TrimSpace(c.Rest()))
	if len(args) == 0 {
		c.Send(z.renderChannelList(s))
		return nil
	}
	if len(args) < 2 {
		c.Send("Usage: channels on|off <channel>")
		return nil
	}
	verb := strings.ToLower(args[0])
	ref := strings.ToLower(args[1])
	var on bool
	switch verb {
	case "on":
		on = true
	case "off":
		on = false
	default:
		c.Send("Usage: channels on|off <channel>")
		return nil
	}
	def := z.channelDefs().get(ref)
	if def == nil {
		c.Send("There is no channel called '" + ref + "'.")
		return nil
	}
	cs := commsOf(s)
	cs.chanOverride[def.ref] = on
	if on {
		c.Send("You enable the " + def.name + " channel.")
	} else {
		c.Send("You disable the " + def.name + " channel.")
	}
	// Re-publish the effective hear-set so the gate (un)subscribes the concrete channel subject (the
	// receiver HEAR-filter re-subscribe on toggle).
	z.publishCommsConfig(s)
	return nil
}

// renderChannelList renders the player's channels with their on/off state (bare `channels`). A channel
// the player cannot HEAR is shown but marked, so a `channels on` on a restricted channel does not appear
// to silently fail.
func (z *Zone) renderChannelList(s *session) string {
	defs := z.channelDefs().table()
	if len(defs) == 0 {
		return "There are no channels."
	}
	type row struct {
		name, ref string
		on, hear  bool
	}
	rows := make([]row, 0, len(defs))
	for _, def := range defs {
		rows = append(rows, row{
			name: def.name,
			ref:  def.ref,
			on:   s.comms.channelEnabled(def),
			hear: def.canHear(s.entity),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ref < rows[j].ref })
	var b strings.Builder
	b.WriteString("Channels:")
	for _, r := range rows {
		b.WriteByte('\n')
		b.WriteString("  ")
		b.WriteString(r.name)
		b.WriteString(" (")
		b.WriteString(r.ref)
		b.WriteString("): ")
		if r.on {
			b.WriteString("ON")
		} else {
			b.WriteString("OFF")
		}
		if !r.hear {
			b.WriteString(" [no access]")
		}
	}
	return b.String()
}

// cmdIgnore toggles a sender on/off the player's ignore list (`ignore <name>`) or lists it (bare
// `ignore`). The ignore list is the RECEIVER-side funnel input (P8-A6): re-published to the gate so the
// gate drops EVERY inbound chan/tell frame from an ignored author. You cannot ignore yourself.
func cmdIgnore(c *Context) error {
	z, s := c.z, c.s
	arg := strings.TrimSpace(c.Rest())
	if arg == "" {
		c.Send(renderIgnoreList(s))
		return nil
	}
	// Sanitize the target as a player id token (same discipline as a tell target — it is keyed in the
	// ignore set and round-trips through the config payload). Reject a control/metacharacter token.
	target := safeTellTarget(arg)
	if target == "" {
		c.Send("There is no player by that name.")
		return nil
	}
	if target == s.character {
		c.Send("You cannot ignore yourself.")
		return nil
	}
	cs := commsOf(s)
	if _, on := cs.ignore[target]; on {
		delete(cs.ignore, target)
		c.Send("You are no longer ignoring " + target + ".")
	} else {
		if len(cs.ignore) >= commsIgnoreMaxIDs {
			c.Send("Your ignore list is full.")
			return nil
		}
		cs.ignore[target] = struct{}{}
		c.Send("You are now ignoring " + target + ".")
	}
	// Re-publish so the gate's receiver-side ignore funnel is updated.
	z.publishCommsConfig(s)
	return nil
}

// renderIgnoreList renders the player's ignore list (bare `ignore`).
func renderIgnoreList(s *session) string {
	if s.comms == nil || len(s.comms.ignore) == 0 {
		return "You are not ignoring anyone."
	}
	ids := make([]string, 0, len(s.comms.ignore))
	for id := range s.comms.ignore {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("You are ignoring:")
	for _, id := range ids {
		b.WriteByte('\n')
		b.WriteString("  ")
		b.WriteString(id)
	}
	return b.String()
}

// cmdAFK sets or clears the AFK flag (`afk` toggles; `afk <msg>` sets AFK with an auto-reply message).
// AFK is surfaced in `who` (the 8.4 presence flag) and triggers a one-line auto-reply to a tell sender
// ("X is AFK: <msg>"); it clears on the player's NEXT input (clearAFKOnInput). Setting AFK refreshes
// presence so `who` marks the player at once.
func cmdAFK(c *Context) error {
	z, s := c.z, c.s
	msg := textsan.CleanLine(strings.TrimSpace(c.Rest()))
	cs := commsOf(s)
	if cs.afk && msg == "" {
		// Bare `afk` while already AFK clears it (a manual un-AFK).
		cs.afk = false
		cs.afkMsg = ""
		c.Send("You are no longer AFK.")
		z.presenceJoin(s) // refresh the roster entry without the AFK marker
		return nil
	}
	cs.afk = true
	cs.afkMsg = msg
	if msg != "" {
		c.Send("You are now AFK: " + msg)
	} else {
		c.Send("You are now AFK.")
	}
	z.presenceJoin(s) // refresh the roster entry WITH the AFK marker so `who` shows it
	return nil
}

// clearAFKOnInput clears a standing AFK when the player types any command OTHER than `afk` itself
// (so `afk <msg>` can re-set it). Called from dispatch on every non-blank input. Cheap no-op unless the
// player is currently AFK. Refreshes presence so `who` drops the AFK marker. Single-writer (zone
// goroutine).
func (z *Zone) clearAFKOnInput(s *session, verb string) {
	if s == nil || s.comms == nil || !s.comms.afk {
		return
	}
	if verb == "afk" {
		return // the afk command manages its own AFK state
	}
	s.comms.afk = false
	s.comms.afkMsg = ""
	s.send(textFrame("You are no longer AFK."))
	z.presenceJoin(s) // refresh the roster entry without the AFK marker
}

// afkAutoReply returns the auto-reply line a tell SENDER sees when their target is AFK, or "" when the
// target is not AFK. Pure read of the target session's comms state; zone goroutine (the target's zone).
func afkAutoReply(target *session) string {
	if target == nil || target.comms == nil || !target.comms.afk {
		return ""
	}
	name := target.character
	if target.entity != nil {
		name = target.entity.Name()
	}
	if target.comms.afkMsg != "" {
		return name + " is AFK: " + target.comms.afkMsg
	}
	return name + " is AFK."
}

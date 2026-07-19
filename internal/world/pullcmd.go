package world

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/double-nibble/telosmud/internal/contentbus"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// pullcmd.go — the trust-gated staff `pull <version>` command (#212 slice 4 PR E). It requests a
// director-COORDINATED content install: unlike `reload` (which re-reads THIS shard's already-imported
// Postgres rows and republishes), `pull` asks the world director to fetch a PUBLISHED version from the
// external git content store, import it into Postgres, and hot-reload the fleet — the in-game trigger for
// the same pipeline telos-pull runs from CI/ops. The director is the coordinator so the import happens
// once (leader-only, single-flight) and can gate stripping a live-hosted pack (a later slice).
//
// The command does NOT do the import itself: it fires a DURABLE signal UP to the world director (the same
// spine the reload audit uses) and returns immediately. The director validates + imports off its own
// worker; the fleet picks up the result via the content-bus broadcast (or reconcile-on-join).

// cmdPull requests a coordinated content pull. Trust: the dispatch USE gate (MinRank=rankStaff) hides the
// verb below staff, and a CAPABILITY gate requires the builder flag — installing fleet content is a
// builder power, like `reload`.
func cmdPull(c *Context) error {
	if !hasFlag(c.Actor, flagBuilder) {
		c.Send("pull: you lack the builder capability to install content.")
		return nil
	}
	version, force := parsePullArgs(c.Rest())
	if version == "" {
		c.Send("pull: usage: pull <version> [force]  — a published content version (a git tag/SHA in the content store).")
		return nil
	}
	// FORCE is an ADMIN power, not a builder one (#427). `pull` is a builder verb because installing
	// content is a builder job — and the live-hosted-pack prune guard is precisely what makes handing that
	// to a builder safe. Letting the holder of the power waive its own gate would make the gate decorative.
	// Force also changes the KIND of operation: it strips content the fleet may be hosting, so its blast
	// radius scales with the fleet, which is the admin boundary rather than the zone/builder one. Same
	// flagAdmin that gates promote/demote — the other class of verb that reaches other people's sessions.
	if force && !hasFlag(c.Actor, flagAdmin) {
		c.Send("pull: force requires the admin capability — overriding the prune guard strips content the " +
			"fleet may be hosting, and those zones will not come back until a reboot. Ask an admin.")
		return nil
	}
	sh := c.z.shard
	if sh == nil || sh.scopes == nil {
		// No scoped bus (a bare/dev shard) => no director to coordinate with. Report unavailable rather
		// than silently dropping the request.
		c.Send("pull: coordinated content install is not available here (no director scope bus configured).")
		return nil
	}
	payload, err := json.Marshal(contentbus.PullRequest{
		Version:  version,
		Actor:    c.s.character,
		AtUnixMs: time.Now().UnixMilli(),
		Force:    force,
	})
	if err != nil {
		c.Send("pull: internal error preparing the request.")
		return nil
	}
	// World-scoped durable signal UP: a content install is a fleet event, so the WORLD director handles it.
	// enqueueSignal tolerates a nil scopeReplication and a full queue — both a clean no-op.
	sh.scopes.enqueueSignal(scopeSignalJob{
		scope:   scopebus.World(),
		event:   contentbus.PullRequestEvent,
		payload: payload,
	})
	if force {
		// Word this carefully. The obvious phrasing — "players keep playing, then roll a reboot" — is actively
		// misleading, because the reboot IS the harm event: a drain hands each zone to a peer, the peer cannot
		// build a zone whose content was stripped, the handover fails, and those players are reclaimed from
		// durable state (i.e. dropped, and on reconnect routed to their HOME start room, not where they were).
		// So the instruction has to lead with redirect-first, not end with reboot.
		c.Send(fmt.Sprintf("pull: requested content version %q WITH FORCE — the live-hosted-pack prune guard "+
			"will be overridden if it blocks anything.\r\n"+
			"  If it does: players inside a stripped pack's zones keep playing from shard memory for now, but "+
			"no new instance copies can be minted, and a DRAIN OR RESTART cannot rebuild those zones — their "+
			"occupants are disconnected and reclaimed to their home start room.\r\n"+
			"  So REDIRECT those characters FIRST, then roll a reboot. Also confirm no world shard pins the "+
			"stripped pack via TELOS_CONTENT_PACKS, or that shard will refuse to boot.\r\n"+
			"  The result line will name exactly which packs were force-pruned.", version))
		return nil
	}
	c.Send(fmt.Sprintf("pull: requested content version %q — the director will validate, import, and hot-reload the fleet; you'll get a pass/fail notice here when it settles.", version))
	return nil
}

// parsePullArgs splits the `pull` tail into the version and the force flag. Recognized: `pull <version>`
// and `pull <version> force` (also `--force`, in either position, matching parseReloadArgs' tolerance).
// The version is the first non-flag token and is NOT case-folded — it is a git tag/SHA, not a verb.
func parsePullArgs(rest string) (version string, force bool) {
	for _, tok := range strings.Fields(rest) {
		switch strings.ToLower(tok) {
		case "force", "--force", "-f":
			force = true
		default:
			if version == "" {
				version = tok
			}
		}
	}
	return version, force
}

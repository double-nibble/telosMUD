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
	version := strings.TrimSpace(c.Rest())
	if version == "" {
		c.Send("pull: usage: pull <version>  — a published content version (a git tag/SHA in the content store).")
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
	c.Send(fmt.Sprintf("pull: requested content version %q — the director will validate, import, and hot-reload the fleet; you'll get a pass/fail notice here when it settles.", version))
	return nil
}

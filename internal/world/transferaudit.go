package world

// transferaudit.go — #443: make a cross-CHARACTER item transfer DETECTABLE.
//
// #432 added characters.owner_epoch as the ownership fence, which removed the ROLLBACK (a stale shard can no
// longer force-write its snapshot over the live owner). But the residual it left open is a CROSS-ROW one:
// items are prototype-ref flyweights inside characters.state JSONB, there is no global item identity, so when
// a zombie session externalizes wealth before it is detected the item lands in a DIFFERENT characters row —
// written by THAT character's own save, under THAT character's own state_version and owner_epoch. An epoch on
// row X is, by construction, incapable of fencing a write to row Y, so the confederate's copy survives.
//
// The cheap, real first step (#443's own framing) is DETECTABILITY, and the machinery already exists: the
// #350 character_audit trail is durable, append-only, and staff-queryable. Emitting an audit row on every
// item transfer that crosses a character boundary makes this class detectable — a dupe leaves a paired record
// a reconciler can find after the fact. This does NOT prevent a dupe and adds NO conservation invariant; it
// converts "we cannot know" into "we can know", the precondition for the durable-identity work #443 defers.
//
// HOW a boundary crossing is recognized, given there is no `give` command: a transfer goes through the room
// floor / a shared container / a corpse. When a PLAYER drops or puts an item, we stamp it with a transient
// Released marker (who released it). When a player PICKS IT UP, if the releaser is a DIFFERENT saved player,
// that pickup crossed a character boundary and is audited; a self-pickup (you get your own drop back) is not.
// The marker is cleared on every pickup so it reflects only the MOST RECENT release and can never mis-attribute
// a later, unrelated looter.
//
// SCOPE / known limits (documented so nobody over-trusts this, mirroring the ref-charset lint's honesty):
//   - Best-effort like the async auditor: Released is a RUNTIME marker (not persisted, like CorpseOwner), so a
//     transfer spanning a save/reload of the floor item is not recorded. This is fine — the double-own dupe
//     window is a live, ~10s-bounded window, exactly where the marker is present. Note the CORRELATED-FAILURE
//     sharpening (distributed-systems review): the row rides the SAME async best-effort auditor (256-deep
//     shared queue, drop-on-full, 5s IO timeout, no retry), and the shard that emits it is BY DEFINITION the
//     misbehaving (double-owned) shard. If it is zombie because it is partitioned from Postgres, the row is
//     dropped precisely when the dupe happens; if it is zombie from a lost directory/NATS lease while Postgres
//     is still reachable (the common case — those are separate resources), the row lands. So detection is
//     correlated-with but not guaranteed-lost-by the failure — acceptable for DETECTION (not prevention).
//   - It covers the PLAYER command paths (drop/put -> get / get-from) at the COMMAND layer, not the Move()
//     chokepoint. An engine/Lua-driven item move (a future autoloot / Lua give-take / summon) does NOT stamp
//     Released, so it is not recorded — the same "can't-forget" caveat container.go already flags for the
//     corpse-loot gate. Moving the hook down to Move() is the #1 carry-forward for the durable-identity work
//     (#443 defers it to Launch), alongside a NARROWING trigger (value/rarity threshold) + a separate volume
//     budget so this high-frequency kind can't starve the rare death/tier kinds out of the shared queue.
//   - A death-generated corpse's items are unstamped (looting a corpse is a different, visible transfer).
//   - CONTAINER contents ARE covered: stamp + record RECURSE into a dropped/picked-up container, so dropping a
//     full bag stamps (and a pickup records) every carried item — the "stuff loot in a bag, drop the bag"
//     externalization is not a bypass. The one residual is a doubly-indirected engine move: if a player takes
//     a whole bag and re-drops it, cmdDrop re-stamps the bag AND its contents to that player, so attribution
//     stays correct; only a NON-command move of the container (Lua/autoloot) would leave stale inner markers —
//     the same engine-move caveat above.
//   - VOLUME: item_transferred is high-frequency, so it enqueues LOW-PRIORITY (auditor.enqueueLowPriority) and
//     sheds itself once the shared queue is past a reserve watermark — a drop/get flood can never starve the
//     rare, security-critical died/tier_changed kinds out of the queue (security review, Finding B).
//   - Bound items can't cross an owner boundary at all (transferBlocked), so they never reach here.

// Released is a transient per-instance marker: the player who last dropped or put this item into the shared
// world, recorded so a subsequent pickup by a DIFFERENT player can be audited as a cross-character transfer
// (#443). NOT persisted (it is observability for the live double-own window), and overwritten by the next
// release / cleared by the next pickup so it always names the MOST RECENT releaser. Zone-goroutine-owned.
type Released struct {
	pid  string // the releasing player's PersistID (entity.pid); "" if the releaser was not a saved player
	name string // the releasing player's character name (the audit payload from_name)
}

func (*Released) componentKind() Kind { return KindReleased }

// stampReleased marks an item a PLAYER just externalized (dropped to the floor / put in a container) with WHO
// released it (#443), so a later pickup by a DIFFERENT player is recognizable as a cross-character transfer.
// A fresh stamp OVERWRITES any prior one (Add is keyed by type), so the marker always reflects the most
// recent release. It RECURSES into a container's contents so dropping a full bag stamps every carried item —
// otherwise "stuff loot in a bag, drop the bag" would externalize the contents with no record (security
// review, Finding A). Only a SAVED player releaser is recorded; a mob / unsaved actor leaves no marker.
// Gated on auditEnabled so a storeless shard does ZERO work — no marker churn, byte-identical to a pre-#443
// shard (Finding D). Zone goroutine only.
func (z *Zone) stampReleased(item, actor *Entity) {
	if item == nil || !z.auditEnabled() || !isPlayer(actor) || actor.pid == nil {
		return
	}
	rel := &Released{pid: string(*actor.pid), name: actor.Name()}
	z.stampReleasedTree(item, rel, 0)
}

// containerNestDepthMax caps how deep the transfer-audit recursion walks a container's nesting. Containment
// is an acyclic tree today (a nested container is never reachable as a `put` destination — targeting only
// resolves top-level scopes — so a cycle can't be constructed), so this is never hit by real content. It is a
// DEFENSIVE backstop: if a future NON-command move (the deferred Move()-chokepoint hook, a Lua give/take) ever
// let a container become its own descendant, the walk terminates instead of overflowing the stack. Set far
// above any plausible real nesting.
const containerNestDepthMax = 64

// stampReleasedTree adds the marker to item and, recursively, to everything nested inside it (a container's
// contents, and their contents). Containment is a finite acyclic tree, so the recursion terminates; a plain
// item has no contents and is stamped alone. depth is bounded by containerNestDepthMax as a backstop.
func (z *Zone) stampReleasedTree(item *Entity, rel *Released, depth int) {
	// A fresh copy per entity: the marker is per-instance, never shared across items (no aliasing).
	Add(item, &Released{pid: rel.pid, name: rel.name})
	if depth >= containerNestDepthMax {
		return
	}
	for _, child := range item.contents {
		z.stampReleasedTree(child, rel, depth+1)
	}
}

// recordCrossCharTransfer is called when a player PICKS UP an item. If the item (or, recursively, anything
// nested in it — getting a container from the floor moves its contents too) carries a Released marker naming
// a DIFFERENT saved player, that boundary-crossing is audited (#443). The marker is CLEARED on acquisition so
// it reflects only the most recent release (no stale mis-attribution) and the new holder's own future drop
// re-stamps it. A self-pickup (releaser == getter), an unsaved releaser/getter, or a storeless shard records
// nothing. Zone goroutine only — the single writer, so reading the live entities here is race-free.
func (z *Zone) recordCrossCharTransfer(getter, item *Entity) {
	z.recordCrossCharTransferAt(getter, item, 0)
}

// recordCrossCharTransferAt is recordCrossCharTransfer with the container-nesting depth threaded through, so
// the recursion has the same containerNestDepthMax backstop as the stamp side.
func (z *Zone) recordCrossCharTransferAt(getter, item *Entity, depth int) {
	if item == nil || !z.auditEnabled() {
		return // nil (defensive) or storeless shard: nothing to record, and no marker was ever stamped
	}
	if rel, ok := Get[*Released](item); ok {
		Remove[*Released](item) // clear on acquisition (most-recent-release invariant)
		if rel.pid != "" && getter != nil && getter.pid != nil && rel.pid != string(*getter.pid) {
			z.auditItemTransfer(getter, rel, item) // a real cross-character transfer
		}
	}
	if depth >= containerNestDepthMax {
		return
	}
	// Recurse into the contents that rode along inside a picked-up container (closes the full-bag bypass).
	for _, child := range item.contents {
		z.recordCrossCharTransferAt(getter, child, depth+1)
	}
}

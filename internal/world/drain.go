package world

import (
	"context"
	"time"
)

// initiateHandoff freezes the player and kicks off the async cross-shard handoff to (destZone, destRoom).
// It is the shared core of a cross-zone MOVE and a graceful DRAIN, factored out of the move path so both use
// the exact same freeze/snapshot/backstop sequence. departMsg (may be "") is the room line others see as the
// player leaves — a move shows "$n departs east."; a drain is silent (same room, just a different shard).
// Runs on the zone goroutine.
func (z *Zone) initiateHandoff(s *session, from *Entity, destZone string, destRoom ProtoRef, departMsg string) {
	// Combat exclusion is enforced by the move path; disengage anyway (belt-and-suspenders) BEFORE detaching
	// from the room so no `fighting` pointer crosses the shard boundary in the snapshot and any opponent's
	// link to the departing player is dropped while the room scan can still find them.
	z.disengage(s.entity)
	// Freeze first: from now on this shard stops acting for the player. frozenFrom remembers the room so
	// handoffFailed can restore the entity if the handoff can't be initiated (else its location stays nil and
	// the next room action null-derefs). handedOff=false until the directory CAS commits (the freeze-reaper
	// discriminator).
	s.frozen = true
	s.frozenFrom = from
	s.handedOff = false
	if departMsg != "" {
		// Presence concealment (#100): route the cross-shard departure through actConceal like every other
		// departure/arrival announce, so a hidden/sneaking or dark-room mover is silent to viewers who can't
		// see them — not a leaky "Someone departs east." The empty-departMsg DRAIN caller stays silent (the
		// guard above), and actConceal is equivalent to act for a non-concealed mover.
		z.actConceal(departMsg, s.entity, ToRoom)
	}
	Move(s.entity, nil) // detach from the room so they don't linger as a ghost during the in-flight handoff
	z.log.Debug("handoff initiated", "player", s.character, "dest_zone", destZone, "dest_room", destRoom, "epoch", s.epoch)
	// Backstop the freeze: if neither the redirect (success) nor handoffFailed (RPC timeout) resolves the
	// session within freezeTTL, freezeExpire reaps the orphan (handed off) or thaws it in place. The gen guard
	// ignores a stale timer for a session that has since rebound. AfterFunc only POSTS — single-writer holds.
	gen := s.attachGen
	time.AfterFunc(freezeTTL, func() { z.post(freezeExpireMsg{id: s.character, gen: gen}) })
	z.handoff(z, buildSnapshot(s), destZone, string(destRoom), s.epoch)
}

// drainZoneMsg tells a zone to hand every live player off to the zone's NEW owner (the standby the drain
// already flipped the lease to). Each player redirects into the SAME zone + room on the new shard, so the
// socket stays open across the move (zero drop). Phase 16.4b.
type drainZoneMsg struct{}

func (drainZoneMsg) zoneMsg() {}

// drainZone hands every eligible live player off to the zone's new owner. Runs on the zone goroutine, so the
// snapshot of players is race-free; each beginHandoff then runs async. A player already mid-handoff (frozen),
// pending, or link-dead is skipped — the freeze/reap machinery already owns them.
func (z *Zone) drainZone() {
	ids := make([]string, 0, len(z.players))
	for id := range z.players {
		ids = append(ids, id)
	}
	for _, id := range ids {
		z.drainPlayer(id)
	}
}

// drainPlayer hands one player off to the zone's new owner, in place (same zone id + current room). The zone
// lease was already flipped to the target, so beginHandoff resolves the destination to the new shard.
func (z *Zone) drainPlayer(id string) {
	s := z.players[id]
	if s == nil || s.frozen || s.pending || s.detached {
		return // already migrating / not attached — the existing machinery owns it
	}
	room := s.entity.location
	if room == nil {
		return // not placed in a room yet (a just-attaching player); the drain deadline reclaims it
	}
	z.initiateHandoff(s, room, z.id, room.proto, "") // silent, same zone, current room — now owned by the peer
}

// DrainResult reports what a BeginDrain did: Redirected players kept their socket (zero drop); Reclaimed
// players were still resident at the deadline and will be dropped + resume from durable state on reconnect.
type DrainResult struct {
	Redirected int
	Reclaimed  int
}

// TargetChooser selects the peer shard a draining zone's ownership + players go to. It returns the target's
// directory shard id and dial endpoint. Injected so a hermetic test supplies a fixed peer and production
// supplies a live-fleet / director-driven selector (avoiding a draining or overloaded target).
type TargetChooser func(zoneID string) (shardID, addr string, err error)

// BeginDrain gracefully drains this shard for a rolling redeploy (Phase 16.4b): it stops accepting new
// fresh logins, hands each hosted zone's ownership to a chosen peer (the atomic fenced lease flip), fans the
// live players off to that peer over the cross-shard handoff (sockets stay open — zero dropped connections),
// and waits until every zone is empty or the deadline. Stragglers at the deadline are flushed and left to
// reconnect from durable state (counted as Reclaimed, not zero-drop). Runs OFF the zone goroutines (blocking
// directory/RPC I/O); safe to call from the SIGTERM handler. Requires leasing (WithZoneLeasing).
func (s *Shard) BeginDrain(ctx context.Context, choose TargetChooser, deadline time.Duration) (res DrainResult, err error) {
	s.mu.Lock()
	s.draining = true // reject new fresh logins from here on (a handoff bind is still accepted)
	s.mu.Unlock()
	// If the drain ABORTS (a target choice / handover failed before completing), clear the flag so the shard
	// resumes accepting logins for whatever zones it still owns rather than getting stuck rejecting them.
	defer func() {
		if err != nil {
			s.mu.Lock()
			s.draining = false
			s.mu.Unlock()
		}
	}()

	// Local bootstrap zones (#212 core pack) are hosted unleased on EVERY shard, so there is no
	// ownership to hand to a peer (the target already built its own copy) — exclude them from the
	// drain's handoff + redirect accounting. Their players are not redirected (there is nowhere to
	// redirect them to); a clean shutdown still durably flushes them via s.Drain() below.
	zones := make([]*Zone, 0)
	for _, z := range s.zonesList() {
		if s.isLocalZone(z.id) {
			continue
		}
		zones = append(zones, z)
	}

	// Snapshot the population BEFORE draining so Redirected = initial - stragglers-at-deadline.
	initial := int64(0)
	for _, z := range zones {
		initial += z.pop.Load()
	}

	// 1. Hand each zone's ownership to a peer, then tell the zone to fan its players off to it.
	for _, z := range zones {
		targetID, targetAddr, err := choose(z.id)
		if err != nil {
			return res, err
		}
		if err := s.handoverZoneTo(ctx, z.id, targetID, targetAddr); err != nil {
			return res, err
		}
		z.post(drainZoneMsg{})
	}

	// 2. Wait until every zone has emptied (players redirected) or the deadline elapses.
	dl := time.After(deadline)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
wait:
	for {
		remaining := int64(0)
		for _, z := range zones {
			remaining += z.pop.Load()
		}
		if remaining == 0 {
			break wait
		}
		select {
		case <-ctx.Done():
			break wait
		case <-dl:
			break wait
		case <-tick.C:
		}
	}

	// 3. Flush every zone durably (stragglers included) and tally the result.
	s.Drain()
	for _, z := range zones {
		res.Reclaimed += int(z.pop.Load())
	}
	res.Redirected = int(initial) - res.Reclaimed
	if res.Redirected < 0 {
		res.Redirected = 0 // players who quit during the drain aren't "redirected"; keep it non-negative
	}
	return res, nil
}

// isDraining reports whether a graceful drain is in progress (fresh logins are refused). Guarded by mu.
func (s *Shard) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

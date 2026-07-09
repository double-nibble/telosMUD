package director

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/presence"
)

// channelroster.go — the #90 per-channel roster aggregator. GMCP Comm.Channel.Players wants, for each
// channel, the set of players currently LISTENING on it. Membership spans shards, so no single shard can
// author a channel's roster; the LEADER director is the single writer. It periodically reads the whole
// cross-shard presence roster (whose entries now carry each player's hear-set, #90 slice 1), inverts it to
// each channel's listener set, and publishes the CHANGED channels' rosters to a per-channel subject that the
// gate forwards as GMCP. Poll-and-diff (a roster panel tolerates seconds); a full snapshot per change is
// idempotent and coalesces a login storm to one publish per affected channel per poll (costs like one chat
// line on that channel). A roster is convergent STATE, not an event, so a diff alone is under-durable on a
// TRANSIENT subject: a publish dropped for some subscriber (core NATS is fire-and-forget) would be recorded
// as delivered and never retried until the next membership CHANGE. So every rosterResyncEvery-th poll does a
// FULL, un-diffed republish — bounding worst-case panel staleness to rosterResyncEvery*interval and seeding a
// subscriber that joined a quiet channel — which lets us keep the cheap transient subject instead of reaching
// for durability. Concealed players are OMITTED (a broadcast roster can't do per-viewer concealment; the fail-
// safe matches the `who` rule — a hidden player never leaks into anyone's panel).

// rosterResyncEvery is the full-republish period in polls (see the diff/convergence note above). >1 preserves
// the diff's login-storm coalescing for the common no-drop case; the poll counter starts at 0 so the FIRST
// poll after start or a leader failover is always a full resync (a promoted standby republishes every channel).
const rosterResyncEvery = 10

// ChannelRosterSource is the cross-shard presence roster the aggregator reads. *presence.Redis satisfies it;
// a hermetic test injects a fake. It lives here as an interface so the director stays free of a hard Redis
// dependency, mirroring MailReaper / ContentPuller.
type ChannelRosterSource interface {
	List(ctx context.Context) ([]presence.Entry, error)
}

// WithChannelRosterAggregator wires the periodic per-channel roster publish (#90): the LEADER director reads
// the roster every `interval`, inverts it, and publishes changed channels' rosters via `bus`. A nil source/
// bus or a non-positive interval leaves it disabled (standalone/dev, and every region director — only the
// world director is wired). Call before Run.
func (d *Director) WithChannelRosterAggregator(src ChannelRosterSource, bus commbus.Bus, interval time.Duration) *Director {
	if src == nil || bus == nil || interval <= 0 {
		return d
	}
	d.rosterSrc = src
	d.rosterBus = bus
	d.rosterInterval = interval
	d.lastRosters = map[string][]string{}
	return d
}

// maybeAggregateChannelRosters runs the aggregation when due, on the leader-gated director tick. The cooldown
// is checked on the actor goroutine; the blocking List + publishes run OFF the actor (single-flighted), so a
// slow roster read never stalls the director loop or overlaps itself — the same discipline as the mail reap.
func (d *Director) maybeAggregateChannelRosters(ctx context.Context) {
	if d.rosterSrc == nil {
		return
	}
	now := d.now()
	if !d.lastRosterAt.IsZero() && now.Sub(d.lastRosterAt) < d.rosterInterval {
		return
	}
	if !d.rosterInFlight.CompareAndSwap(false, true) {
		return // a prior aggregation is still running; try again next interval
	}
	d.lastRosterAt = now
	d.workers.Add(1)
	go func() {
		defer d.workers.Done()
		defer d.rosterInFlight.Store(false)
		d.aggregateChannelRosters(ctx)
	}()
}

// aggregateChannelRosters reads the roster, inverts it to channel -> sorted member names, and publishes the
// channels whose roster changed. Runs on an off-actor worker; lastRosters is touched ONLY here (single-
// flighted), so it needs no lock.
func (d *Director) aggregateChannelRosters(ctx context.Context) {
	entries, err := d.rosterSrc.List(ctx)
	if err != nil {
		d.log.Debug("channel-roster aggregate: roster list failed", "err", err)
		return
	}
	current := invertRoster(entries)
	// Every rosterResyncEvery-th poll republishes ALL channels regardless of the diff, so a dropped transient
	// publish (and a subscriber that joined a quiet channel) converges rather than waiting for the next change.
	full := d.rosterPolls%rosterResyncEvery == 0
	d.rosterPolls++
	// Otherwise publish only the channels whose roster CHANGED since the last poll. A channel that lost its
	// LAST member drops out of `current` and is not published — the gate only subscribes channels it hears, so
	// an emptied channel has no subscriber to notify; forgetting it lets a later repopulation re-publish.
	for ref, names := range current {
		if full || !equalStringSlice(names, d.lastRosters[ref]) {
			d.publishChannelRoster(ctx, ref, names)
		}
	}
	d.lastRosters = current
}

// invertRoster maps the flat roster to each channel's sorted, de-duplicated listener names. Members are
// deduped by PlayerID as defense-in-depth: the presence store keys one entry per player (owner-guarded), so
// List returns a single entry per player today, but the dedup keeps a player ONE member even if a future
// multi-key layout ever let a handoff's source-stale and dest-fresh entries momentarily coexist. Concealed
// players are dropped.
func invertRoster(entries []presence.Entry) map[string][]string {
	byRef := map[string]map[string]string{} // ref -> playerID -> name
	for _, e := range entries {
		if e.Concealed {
			continue
		}
		for _, ref := range e.Channels {
			if byRef[ref] == nil {
				byRef[ref] = map[string]string{}
			}
			byRef[ref][e.PlayerID] = e.Name
		}
	}
	out := make(map[string][]string, len(byRef))
	for ref, byID := range byRef {
		names := make([]string, 0, len(byID))
		for _, n := range byID {
			names = append(names, n)
		}
		sort.Strings(names)
		out[ref] = names
	}
	return out
}

// publishChannelRoster publishes one channel's current listener set to its roster subject (#90).
func (d *Director) publishChannelRoster(ctx context.Context, ref string, players []string) {
	if players == nil {
		players = []string{}
	}
	body, err := json.Marshal(map[string]any{"channel": ref, "players": players})
	if err != nil {
		return
	}
	if err := d.rosterBus.Publish(ctx, commbus.RosterSubject(ref), commbus.Message{Body: string(body)}); err != nil {
		d.log.Debug("channel-roster publish failed", "channel", ref, "err", err)
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

package world

import (
	"context"
	"log/slog"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// linkDeadGrace is how long a player's in-zone presence survives after its stream
// drops unexpectedly (no clean quit). A re-dial (handoff, docs/PROTOCOL.md §5) or a
// reconnect within this window re-binds to the same player and resumes; otherwise
// the player is reaped. Tunable later.
const linkDeadGrace = 60 * time.Second

// Zone is the actor (docs/ARCHITECTURE.md §3). A single goroutine — Run — owns all
// rooms and players within the zone and is the *only* code that ever reads or
// mutates their state, so game logic needs no locks. Every other goroutine (each
// player's gRPC stream handler in server.go, future cross-zone senders) interacts
// with the zone exclusively by posting messages to inbox; none of them touch zone
// state directly.
//
// Lifecycle of a message: a producer calls post (from any goroutine) -> the message
// lands on the buffered inbox channel -> Run pulls it off and calls handle, which
// runs on the single zone goroutine. From there everything (join/input/leave) is
// sequential and single-threaded.
type Zone struct {
	id        string
	rooms     map[string]*Room
	players   map[string]*player
	startRoom string
	inbox     chan msg     // message queue; the only ingress to zone state
	log       *slog.Logger // scoped logger: component=zone, zone=<id>
}

// msg is anything the zone goroutine processes off its inbox. The interface keeps
// the inbox a single typed channel while letting handle switch on concrete type.
type msg interface{ zoneMsg() }

// joinMsg adds a pre-built player to the zone directly. Used by tests; the network
// path uses attachMsg (which creates or re-binds and then joins).
type joinMsg struct{ p *player }

// attachMsg binds a player's gRPC stream (its out channel) to a character. If the
// character is unknown it creates and joins a new player; if it already exists
// (a re-dial/reconnect within the link-death window) it re-binds the stream to the
// existing player, preserving appliedSeq so input replay dedups correctly.
type attachMsg struct {
	character string
	out       chan *playv1.ServerFrame
}

// inputMsg carries one line of player input. seq is the gate's session-scoped input
// sequence (docs/PROTOCOL.md §5); seq==0 means unsequenced (tests/internal) and is
// always applied. A seq <= the player's appliedSeq is a replay and is dropped.
type inputMsg struct {
	id   string
	seq  uint64
	line string
}

// detachMsg signals that a player's stream dropped. out identifies which stream, so
// a stale detach from a superseded stream (after a re-attach) is ignored. A clean
// quit removes the player immediately; an unexpected drop starts the link-death grace.
type detachMsg struct {
	id  string
	out chan *playv1.ServerFrame
}

// reapMsg fires after the link-death grace to remove a player that never re-attached.
// gen guards against reaping a player that has since re-attached (new generation).
type reapMsg struct {
	id  string
	gen uint64
}

// leaveMsg removes a player from the zone immediately.
type leaveMsg struct{ id string }

func (joinMsg) zoneMsg()   {}
func (attachMsg) zoneMsg() {}
func (inputMsg) zoneMsg()  {}
func (detachMsg) zoneMsg() {}
func (reapMsg) zoneMsg()   {}
func (leaveMsg) zoneMsg()  {}

func newZone(id string) *Zone {
	return &Zone{
		id:      id,
		rooms:   map[string]*Room{},
		players: map[string]*player{},
		inbox:   make(chan msg, 256),
		// Scoped logger so every line this zone emits is tagged with its id; all
		// the verbose control-flow tracing below goes through z.log at Debug.
		log: slog.With("component", "zone", "zone", id),
	}
}

// post enqueues a message for the zone goroutine. Safe to call from any goroutine —
// this is the *only* sanctioned way to reach zone state from outside the loop.
func (z *Zone) post(m msg) { z.inbox <- m }

// Run is the zone's single-threaded event loop and the heart of the actor model.
// It runs on one dedicated goroutine and serially handles inbox messages until ctx
// is cancelled. Because all state mutation funnels through here, no other goroutine
// ever races it.
func (z *Zone) Run(ctx context.Context) {
	z.log.Debug("zone loop start", "rooms", len(z.rooms), "start_room", z.startRoom)
	for {
		select {
		case <-ctx.Done():
			z.log.Debug("zone loop stop", "players", len(z.players))
			return
		case m := <-z.inbox:
			z.handle(m)
		}
	}
}

// handle dispatches one inbox message to the matching handler. Runs only on the
// zone goroutine (called from Run), so all handlers below are lock-free.
func (z *Zone) handle(m msg) {
	switch v := m.(type) {
	case joinMsg:
		z.log.Debug("inbox: join", "player", v.p.id)
		z.join(v.p)
	case attachMsg:
		z.log.Debug("inbox: attach", "player", v.character)
		z.attach(v.character, v.out)
	case inputMsg:
		z.handleInput(v)
	case detachMsg:
		z.log.Debug("inbox: detach", "player", v.id)
		z.detach(v.id, v.out)
	case reapMsg:
		z.reap(v.id, v.gen)
	case leaveMsg:
		z.log.Debug("inbox: leave", "player", v.id)
		z.leave(v.id)
	}
}

// handleInput applies one input line with exactly-once semantics. A sequenced line
// (seq>0) at or below the player's high-water mark is a replay — dropped before it
// can run a second time (docs/PROTOCOL.md §5). Otherwise the high-water advances and
// the line is dispatched.
func (z *Zone) handleInput(v inputMsg) {
	p := z.players[v.id]
	if p == nil {
		// Input for a player the zone no longer knows about (e.g. leave/input race).
		z.log.Debug("inbox: input for unknown player", "player", v.id)
		return
	}
	if v.seq != 0 && v.seq <= p.appliedSeq {
		// Replay of an already-applied line: drop it. No dispatch, no output, so the
		// command's side effects happen exactly once across a re-dial.
		z.log.Debug("duplicate input dropped", "player", v.id, "seq", v.seq, "applied", p.appliedSeq)
		return
	}
	if v.seq != 0 {
		p.appliedSeq = v.seq
	}
	z.log.Debug("inbox: input", "player", v.id, "seq", v.seq, "line", v.line)
	z.dispatch(p, v.line)
}

// join places a newly connected player into the world: it picks a valid room
// (falling back to the start room), registers the player, announces the arrival to
// the room, shows the player their surroundings, and primes the prompt.
func (z *Zone) join(p *player) {
	if p.room == "" || z.rooms[p.room] == nil {
		p.room = z.startRoom
	}
	z.players[p.id] = p
	r := z.rooms[p.room]
	r.occupants[p.id] = true
	z.broadcast(r, p.id, p.name+" arrives.")
	z.lookRoom(p)
	p.send(promptFrame())
	z.log.Debug("player joined", "player", p.id, "room", p.room, "population", len(z.players))
}

// leave removes a player from the world: detaches them from their room, announces
// the departure, and forgets them. Safe to call for an unknown id (no-op).
func (z *Zone) leave(id string) {
	p := z.players[id]
	if p == nil {
		z.log.Debug("leave: unknown player", "player", id)
		return
	}
	if r := z.rooms[p.room]; r != nil {
		delete(r.occupants, id)
		z.broadcast(r, id, p.name+" leaves.")
	}
	delete(z.players, id)
	z.log.Debug("player left", "player", id, "room", p.room, "population", len(z.players))
}

// attach binds a stream's out channel to a character. A new character is created and
// joined; an existing one (a re-dial or reconnect within the link-death window) is
// re-bound to the *same* player, preserving appliedSeq so replayed input dedups
// correctly. Either way an Attached frame goes out first; player.send stamps it with
// the resume point (appliedSeq) via ServerFrame.ack_input_seq.
func (z *Zone) attach(character string, out chan *playv1.ServerFrame) {
	p := z.players[character]
	reattach := p != nil
	if reattach {
		p.out = out
		p.detached = false
	} else {
		p = &player{id: character, name: character, out: out}
	}
	p.attachGen++

	// Attached first: conveys the shard id and (via the ack stamp) the resume point.
	p.send(attachedFrame(z.id))

	if reattach {
		z.log.Debug("player re-attached", "player", character, "applied_seq", p.appliedSeq, "gen", p.attachGen)
		z.lookRoom(p) // re-show the room to the reconnected client
		p.send(promptFrame())
	} else {
		z.join(p) // registers, places, announces arrival, looks, prompts
	}
}

// detach handles a player's stream dropping. out identifies which stream, so a stale
// detach from a stream already superseded by a re-attach is ignored. A clean quit
// removes the player at once; an unexpected drop marks the player link-dead and
// schedules a reap after the grace window (cancelled implicitly if it re-attaches and
// bumps attachGen).
func (z *Zone) detach(id string, out chan *playv1.ServerFrame) {
	p := z.players[id]
	if p == nil {
		return
	}
	if p.out != out {
		z.log.Debug("detach from superseded stream ignored", "player", id)
		return
	}
	if p.quitting {
		z.log.Debug("clean quit, removing player", "player", id)
		z.leave(id)
		return
	}
	p.detached = true
	gen := p.attachGen
	z.log.Debug("player link-dead", "player", id, "grace", linkDeadGrace)
	time.AfterFunc(linkDeadGrace, func() { z.post(reapMsg{id: id, gen: gen}) })
}

// reap removes a link-dead player that never re-attached within the grace window. The
// generation check ensures a player that has since re-attached (bumping attachGen) is
// not removed by a stale timer.
func (z *Zone) reap(id string, gen uint64) {
	if p := z.players[id]; p != nil && p.detached && p.attachGen == gen {
		z.log.Debug("reaping link-dead player", "player", id)
		z.leave(id)
	}
}

// broadcast sends markup to every occupant of r except exceptID (the actor who
// caused the event). Used for arrival/departure/say lines that others should see.
func (z *Zone) broadcast(r *Room, exceptID, markup string) {
	n := 0
	for id := range r.occupants {
		if id == exceptID {
			continue
		}
		if o := z.players[id]; o != nil {
			o.send(textFrame(markup))
			n++
		}
	}
	z.log.Debug("broadcast", "room", r.id, "except", exceptID, "recipients", n)
}

package world

import (
	"context"
	"log/slog"
)

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

// joinMsg adds a connected player to the zone (posted once, after Attach).
type joinMsg struct{ p *player }

// inputMsg carries one line of player input to be parsed and run in the zone.
type inputMsg struct {
	id   string
	line string
}

// leaveMsg removes a player from the zone (stream closed or detached).
type leaveMsg struct{ id string }

func (joinMsg) zoneMsg()  {}
func (inputMsg) zoneMsg() {}
func (leaveMsg) zoneMsg() {}

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
	case inputMsg:
		if p := z.players[v.id]; p != nil {
			z.log.Debug("inbox: input", "player", v.id, "line", v.line)
			z.dispatch(p, v.line)
		} else {
			// Input arrived for a player the zone no longer knows about (e.g. a
			// race between leave and a late input). Nothing to do but note it.
			z.log.Debug("inbox: input for unknown player", "player", v.id)
		}
	case leaveMsg:
		z.log.Debug("inbox: leave", "player", v.id)
		z.leave(v.id)
	}
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

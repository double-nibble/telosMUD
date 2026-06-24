package world

import "context"

// Zone is the actor: a single goroutine (Run) owns all rooms and players within
// it, so game logic needs no locks (docs/ARCHITECTURE.md §3). Other goroutines
// interact only by posting messages to the inbox.
type Zone struct {
	id        string
	rooms     map[string]*Room
	players   map[string]*player
	startRoom string
	inbox     chan msg
}

// msg is anything the zone goroutine processes off its inbox.
type msg interface{ zoneMsg() }

type joinMsg struct{ p *player }
type inputMsg struct {
	id   string
	line string
}
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
	}
}

// post enqueues a message for the zone goroutine. Safe from any goroutine.
func (z *Zone) post(m msg) { z.inbox <- m }

// Run is the zone's single-threaded event loop.
func (z *Zone) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-z.inbox:
			z.handle(m)
		}
	}
}

func (z *Zone) handle(m msg) {
	switch v := m.(type) {
	case joinMsg:
		z.join(v.p)
	case inputMsg:
		if p := z.players[v.id]; p != nil {
			z.dispatch(p, v.line)
		}
	case leaveMsg:
		z.leave(v.id)
	}
}

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
}

func (z *Zone) leave(id string) {
	p := z.players[id]
	if p == nil {
		return
	}
	if r := z.rooms[p.room]; r != nil {
		delete(r.occupants, id)
		z.broadcast(r, id, p.name+" leaves.")
	}
	delete(z.players, id)
}

// broadcast sends markup to every occupant of r except exceptID.
func (z *Zone) broadcast(r *Room, exceptID, markup string) {
	for id := range r.occupants {
		if id == exceptID {
			continue
		}
		if o := z.players[id]; o != nil {
			o.send(textFrame(markup))
		}
	}
}

package world

import (
	"encoding/json"
	"testing"
	"time"
)

// requestContents drives an inbound Char.Items.Contents request for the given container id.
func requestContents(z *Zone, s *session, containerID string) {
	raw, _ := json.Marshal(map[string]string{"container": containerID})
	z.handleGMCPRequest(gmcpRequestMsg{id: s.character, pkg: "Char.Items.Contents", json: raw})
}

// itemsListReply is the Char.Items.List payload shape.
type itemsListReply struct {
	Location string `json:"location"`
	Items    []struct {
		Name string `json:"name"`
	} `json:"items"`
}

// TestGMCPContainerContents pins #92: an inbound Char.Items.Contents request for a container the player
// holds returns a Char.Items.List keyed to the container id, listing its contents.
func TestGMCPContainerContents(t *testing.T) {
	z, s := abilityTestZone(t)
	box := addTestItem(z, s.entity, "chest", []string{"chest"}, &Container{capacity: 5})
	addTestItem(z, box, "sword", []string{"sword"})
	addTestItem(z, box, "shield", []string{"shield"})
	drainGMCP(s) // clear setup frames

	id := itemGMCPID(box)
	requestContents(z, s, id)

	raw, ok := drainGMCP(s)["Char.Items.List"]
	if !ok {
		t.Fatal("no Char.Items.List reply to a valid container-contents request")
	}
	var reply itemsListReply
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Location != id {
		t.Errorf("reply location = %q, want the container id %q", reply.Location, id)
	}
	if len(reply.Items) != 2 {
		t.Fatalf("reply items = %d, want 2 (sword + shield)", len(reply.Items))
	}
}

// TestGMCPRequestRateLimited pins the #92 M1 fix: a burst of inbound requests is throttled per session, so
// a flood can't force unbounded O(container) work on the shared zone goroutine.
func TestGMCPRequestRateLimited(t *testing.T) {
	z, s := abilityTestZone(t)
	box := addTestItem(z, s.entity, "chest", []string{"chest"}, &Container{capacity: 5})
	addTestItem(z, box, "sword", []string{"sword"})
	id := itemGMCPID(box)
	drainGMCP(s)

	const flood = 40
	for i := 0; i < flood; i++ {
		requestContents(z, s, id)
	}
	replies := 0
	for {
		select {
		case f := <-s.out:
			if g := f.GetGmcp(); g != nil && g.GetPkg() == "Char.Items.List" {
				replies++
			}
			continue
		default:
		}
		break
	}
	if replies == 0 {
		t.Fatal("rate limiter dropped everything; a burst should be allowed")
	}
	if replies > int(gmcpReqBurst)+2 { // +2 slack for the sub-microsecond refill across the loop
		t.Fatalf("rate limiter did not throttle: %d replies for %d instantaneous requests", replies, flood)
	}
}

// TestGMCPContainerContentsSecurity pins the trust boundary: a request that can't be legitimately satisfied
// is SILENTLY dropped (no reply) — never revealing whether the entity exists.
func TestGMCPContainerContentsSecurity(t *testing.T) {
	t.Run("unreachable id", func(t *testing.T) {
		z, s := abilityTestZone(t)
		drainGMCP(s)
		requestContents(z, s, "i999999") // an id the player doesn't hold / can't see
		if _, ok := drainGMCP(s)["Char.Items.List"]; ok {
			t.Error("replied to a request for an unreachable container id")
		}
	})

	t.Run("closed container", func(t *testing.T) {
		z, s := abilityTestZone(t)
		box := addTestItem(z, s.entity, "chest", []string{"chest"}, &Container{capacity: 5, closed: true})
		addTestItem(z, box, "gold", []string{"gold"})
		drainGMCP(s)
		requestContents(z, s, itemGMCPID(box))
		if _, ok := drainGMCP(s)["Char.Items.List"]; ok {
			t.Error("revealed the contents of a CLOSED container")
		}
	})

	t.Run("not a container", func(t *testing.T) {
		z, s := abilityTestZone(t)
		rock := addTestItem(z, s.entity, "rock", []string{"rock"}) // no Container component
		drainGMCP(s)
		requestContents(z, s, itemGMCPID(rock))
		if _, ok := drainGMCP(s)["Char.Items.List"]; ok {
			t.Error("replied to a contents request for a non-container item")
		}
	})

	t.Run("corpse in loot window is not peekable by a bystander", func(t *testing.T) {
		z, s := abilityTestZone(t)
		corpse := addTestItem(z, s.entity, "corpse", []string{"corpse"}, &Container{capacity: 10})
		addTestItem(z, corpse, "loot", []string{"loot"})
		// Owned by someone else, window still open — s is a bystander.
		addAny(corpse, &CorpseOwner{owner: "SomeoneElse", until: time.Now().Add(time.Minute)})
		drainGMCP(s)
		requestContents(z, s, itemGMCPID(corpse))
		if _, ok := drainGMCP(s)["Char.Items.List"]; ok {
			t.Error("a bystander peeked into a corpse still in its loot-owner window")
		}
	})
}

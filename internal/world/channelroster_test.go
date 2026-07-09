package world

import (
	"strings"
	"testing"

	roster "github.com/double-nibble/telosmud/internal/presence"
)

// TestSetResidentChannels pins #90 slice 1: a hear-set change updates the resident's roster entry (so the
// cross-shard roster carries per-channel membership); a non-resident is a safe no-op.
func TestSetResidentChannels(t *testing.T) {
	p := newPresenceTracker()
	p.roster = roster.NewMem()
	p.shardID = "shard-a"

	p.join("Ana", "Ana", false, false)
	p.setResidentChannels("Ana", []string{"gossip", "trade"})

	byID := map[string]roster.Entry{}
	for _, e := range p.snapshot() {
		byID[e.PlayerID] = e
	}
	if got := strings.Join(byID["Ana"].Channels, ","); got != "gossip,trade" {
		t.Fatalf("Ana roster channels = %q, want gossip,trade", got)
	}

	// A later toggle replaces the set (not appends).
	p.setResidentChannels("Ana", []string{"gossip"})
	byID = map[string]roster.Entry{}
	for _, e := range p.snapshot() {
		byID[e.PlayerID] = e
	}
	if got := strings.Join(byID["Ana"].Channels, ","); got != "gossip" {
		t.Fatalf("after toggle, Ana channels = %q, want gossip", got)
	}

	// A non-resident is a no-op (no panic, no phantom entry).
	p.setResidentChannels("Ghost", []string{"x"})
	if _, ok := byID["Ghost"]; ok {
		t.Fatal("setResidentChannels created an entry for a non-resident")
	}
}

// TestJoinPreservesChannels pins the review must-fix: join() is also the re-write path for an AFK/conceal
// change, which doesn't recompute the hear-set — so it must CARRY FORWARD the stamped Channels, not wipe
// the player out of every channel roster until the next comms republish.
func TestJoinPreservesChannels(t *testing.T) {
	p := newPresenceTracker()
	p.roster = roster.NewMem()
	p.shardID = "shard-a"

	p.join("Ana", "Ana", false, false)
	p.setResidentChannels("Ana", []string{"gossip", "trade"})
	// An AFK toggle re-joins with the same name (afk=true) — the hear-set is unchanged and must survive.
	p.join("Ana", "Ana", true, false)

	byID := map[string]roster.Entry{}
	for _, e := range p.snapshot() {
		byID[e.PlayerID] = e
	}
	if got := strings.Join(byID["Ana"].Channels, ","); got != "gossip,trade" {
		t.Fatalf("an AFK re-join wiped the channel membership: channels = %q, want gossip,trade", got)
	}
	if !byID["Ana"].AFK {
		t.Fatal("the AFK flag from the re-join was not applied")
	}
}

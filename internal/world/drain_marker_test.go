package world

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeDrainMarker records the SetDraining/ClearDraining calls BeginDrain makes.
type fakeDrainMarker struct {
	setN, clearN int
	lastShard    string
	lastTTL      time.Duration
}

func (m *fakeDrainMarker) SetDraining(_ context.Context, shardID string, ttl time.Duration) error {
	m.setN++
	m.lastShard = shardID
	m.lastTTL = ttl
	return nil
}

func (m *fakeDrainMarker) ClearDraining(_ context.Context, _ string) error {
	m.clearN++
	return nil
}

// TestBeginDrainPublishesAndClearsDrainingMarker pins #41: BeginDrain advertises the shard as draining (so
// peers don't target it during a fleet rollout) and clears the marker when the drain ends.
func TestBeginDrainPublishesAndClearsDrainingMarker(t *testing.T) {
	sh := NewDemoShard() // single non-local zone (midgaard), no directory
	m := &fakeDrainMarker{}
	sh.WithDrainMarker(m)

	// A chooser with no peer does NOT abort the drain: it degrades to reclaim-from-durable (never aborting
	// before the flush). The marker is still published at start and cleared on completion. A bounded ctx caps
	// the reclaim step's wait — this demo shard's zone actor isn't running, so nothing answers the reclaim
	// probe and it falls through on ctx timeout (in production the live zone answers instantly).
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := sh.BeginDrain(ctx, func(string, int) (string, string, error) {
		return "", "", errors.New("no live peer")
	}, 2*time.Second)
	if err != nil {
		t.Fatalf("no-peer drain must degrade to reclaim, not abort: %v", err)
	}
	if m.setN != 1 {
		t.Errorf("SetDraining calls = %d, want 1", m.setN)
	}
	if m.clearN != 1 {
		t.Errorf("ClearDraining calls = %d, want 1 (cleared on completion)", m.clearN)
	}
	if m.lastTTL != 4*time.Second {
		t.Errorf("marker TTL = %v, want 4s (2x the 2s deadline)", m.lastTTL)
	}
}

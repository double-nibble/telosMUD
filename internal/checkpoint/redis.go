// Package checkpoint is the Redis tier of the durability ladder (docs/PERSISTENCE.md §6,
// docs/PHASE4-PLAN.md §4): a frequent (~10s), cheap write-back mirror of a character's
// authoritative state. Its only job is to SHRINK the crash data-loss window — if a shard dies,
// recovery replays the last Redis checkpoint instead of the last (~60s) Postgres flush.
//
// It lives in its own package (not internal/directory) on purpose: the directory package is
// imported by internal/world's tests, and a checkpoint that imports world (for CharSnapshot) would
// form a test-time import cycle directory<->world. Here the dependency is one-directional
// (checkpoint -> world), and world's tests do not import checkpoint, so there is no cycle. It
// reuses the directory's Redis namespacing convention ("<ns>:ckpt:char:<name>") without importing
// it.
package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/double-nibble/telosmud/internal/world"
)

// Redis is the world.Checkpointer over a Redis client. The key is "<ns>:ckpt:char:<name>", keyed
// by character NAME so ANY shard can read the freshest checkpoint on login (the crash-rehydrate-
// by-name primitive, docs/PLACEMENT.md §5-6). A TTL bounds a stale checkpoint (a logged-out
// player's mirror need not live forever; Postgres is the durable record).
type Redis struct {
	rdb *redis.Client
	ns  string
	ttl time.Duration
}

// DefaultTTL bounds how long a character's Redis checkpoint survives without a refresh. It must
// comfortably exceed the ~10s checkpoint cadence so a live player's checkpoint never lapses between
// writes, while still letting a logged-out player's mirror expire (Postgres holds the durable copy).
const DefaultTTL = time.Hour

// NewRedis returns a checkpointer over rdb in namespace ns (default "telos") with the default TTL.
// Shares the same client the directory uses, so a world with Redis already has it for free.
func NewRedis(rdb *redis.Client, ns string) *Redis {
	if ns == "" {
		ns = "telos"
	}
	return &Redis{rdb: rdb, ns: ns, ttl: DefaultTTL}
}

func (r *Redis) key(name string) string { return r.ns + ":ckpt:char:" + name }

// value is the serialized checkpoint: the full CharSnapshot in JSON (human-inspectable in
// redis-cli, and the same at-rest shape as the `state` column). The snapshot carries state_version
// so the load-time freshness comparison is a field read.
type value struct {
	PID          string          `json:"pid"`
	Name         string          `json:"name"`
	ZoneRef      string          `json:"zone_ref"`
	RoomRef      string          `json:"room_ref"`
	StateVersion uint64          `json:"state_version"`
	State        world.StateJSON `json:"state"`
}

// Checkpoint writes snap as the latest checkpoint for snap.Name, overwriting the prior one and
// refreshing the TTL. Off the zone goroutine (the saver). A Redis failure is returned (non-fatal to
// the caller): a missed checkpoint only widens the crash window by one cadence tick.
func (r *Redis) Checkpoint(ctx context.Context, snap world.CharSnapshot) error {
	v := value{
		PID:          string(snap.PID),
		Name:         snap.Name,
		ZoneRef:      snap.ZoneRef,
		RoomRef:      snap.RoomRef,
		StateVersion: snap.StateVersion,
		State:        snap.State,
	}
	// An EMPTY ZoneRef means "leave the stored location alone", never "clear it" — the same contract the
	// Postgres tier states in its UPDATE (zone_ref = COALESCE(...), internal/store/character.go, #411). The
	// world's only producer of ZoneRef (dumpCharacter) returns "" for a player inside a runtime-minted zone
	// INSTANCE, whose ephemeral id must never be persisted; blanking the mirror instead would make THIS tier
	// the one that loses the location, and it is the tier the login path prefers whenever it is fresher.
	//
	// A read-modify-write, not a script: it only runs on the "" path (an instance occupant), so the common
	// checkpoint stays a single SET. It is no less atomic than the write it guards — a checkpoint is a
	// last-writer-wins overwrite of the whole value either way — and a read failure degrades to the empty
	// zone rather than dropping the checkpoint, which is the same "widen the crash window by one field"
	// outcome the tier already tolerates.
	if v.ZoneRef == "" {
		if prev, found, err := r.LoadCheckpoint(ctx, snap.Name); err == nil && found {
			v.ZoneRef = prev.ZoneRef
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("checkpoint: marshal %q: %w", snap.Name, err)
	}
	if err := r.rdb.Set(ctx, r.key(snap.Name), data, r.ttl).Err(); err != nil {
		return fmt.Errorf("checkpoint: write %q: %w", snap.Name, err)
	}
	return nil
}

// LoadCheckpoint returns the last checkpoint for name, or found=false if none exists (or it
// expired). Off the zone goroutine (the login read). The caller compares its state_version against
// the Postgres row and uses whichever is higher (the freshness check).
func (r *Redis) LoadCheckpoint(ctx context.Context, name string) (world.CharSnapshot, bool, error) {
	data, err := r.rdb.Get(ctx, r.key(name)).Bytes()
	if errors.Is(err, redis.Nil) {
		return world.CharSnapshot{}, false, nil
	}
	if err != nil {
		return world.CharSnapshot{}, false, fmt.Errorf("checkpoint: read %q: %w", name, err)
	}
	var v value
	if err := json.Unmarshal(data, &v); err != nil {
		return world.CharSnapshot{}, false, fmt.Errorf("checkpoint: unmarshal %q: %w", name, err)
	}
	return world.CharSnapshot{
		PID:          world.PersistID(v.PID),
		Name:         v.Name,
		ZoneRef:      v.ZoneRef,
		RoomRef:      v.RoomRef,
		StateVersion: v.StateVersion,
		State:        v.State,
	}, true, nil
}

// Compile-time assertion that *Redis satisfies world.Checkpointer.
var _ world.Checkpointer = (*Redis)(nil)

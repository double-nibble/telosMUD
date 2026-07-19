// Package checkpoint is the Redis tier of the durability ladder (docs/PERSISTENCE.md §6,
// docs/PHASE4-PLAN.md §4): a frequent (~10s), cheap write-back mirror of a character's
// authoritative state. Its only job is to SHRINK the crash data-loss window — if a shard dies,
// recovery replays the last Redis checkpoint instead of the last (~60s) Postgres flush.
//
// It lives in its own package (not internal/directory) on purpose: the directory package is
// imported by internal/world's tests, and a checkpoint that imports world (for CharSnapshot) would
// form a test-time import cycle directory<->world. Here the dependency is one-directional
// (checkpoint -> world), and world's tests do not import checkpoint, so there is no cycle. It
// reuses the directory's Redis namespacing convention (see key()) without importing it.
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

// Redis is the world.Checkpointer over a Redis client. The key is "<ns>:ckpt:char:v2:<name>" (see
// key() for why the format is versioned), keyed by character NAME so ANY shard can read the freshest
// checkpoint on login (the crash-rehydrate-by-name primitive, docs/PLACEMENT.md §5-6). The value is a
// HASH of {data, epoch} so the ownership guard can compare epochs without decoding the blob. A TTL
// bounds a stale checkpoint (a logged-out player's mirror need not live forever; Postgres is the
// durable record).
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

// key names the checkpoint slot. The `:v2:` segment is a deliberate at-rest format break (#432): the
// value went from a bare string (SET/GET) to a HASH carrying `data` + `epoch`, so that the ownership
// guard can compare the stored epoch without decoding the blob. HGET against a pre-#432 string key
// raises WRONGTYPE, and a Redis Lua error propagates — so reusing the old key would make every
// checkpoint write fail for the duration of a rolling deploy, and an old shard SETting over a new
// shard's hash would keep breaking it afterwards.
//
// Splitting the namespace instead costs exactly one TTL window (DefaultTTL) of degraded crash
// recovery during the upgrade: old-code shards keep reading/writing `:ckpt:char:`, new-code shards
// use `:v2:`, neither sees the other's, and the abandoned keys expire on their own. The checkpoint is
// a crash-window optimization over an authoritative Postgres tier, so a window where it is merely
// cold — never wrong — is the right thing to trade.
func (r *Redis) key(name string) string { return r.ns + ":ckpt:char:v2:" + name }

// value is the serialized checkpoint: the full CharSnapshot in JSON (human-inspectable in
// redis-cli, and the same at-rest shape as the `state` column). The snapshot carries state_version
// so the load-time freshness comparison is a field read.
type value struct {
	PID          string `json:"pid"`
	Name         string `json:"name"`
	ZoneRef      string `json:"zone_ref"`
	RoomRef      string `json:"room_ref"`
	StateVersion uint64 `json:"state_version"`
	// OwnerEpoch is the writing session's ownership generation (#432). It is a SEPARATE hash field as
	// well as a JSON field: the guard script must compare it without decoding the blob, and the login
	// read must order candidates by it. Omitting it here (this struct is its own hand-written
	// serialization, distinct from CharSnapshot and from the `state` JSONB) would leave this tier
	// unfenced while the others were fenced — which is exactly where a bypass lives.
	OwnerEpoch uint64          `json:"owner_epoch"`
	State      world.StateJSON `json:"state"`
}

// checkpointGuard is the OWNERSHIP guard on the checkpoint write (#432). The checkpoint is one key per
// character name, written every ~10s by every live copy of that character, with no CAS — so a stale
// copy was the last writer roughly half the time. Since state_version only advances on a durable
// Postgres CAS, the stale and live copies sat at the SAME version for the whole window between a
// login and its first flush, and the login read's tie-break then preferred the checkpoint. The
// Postgres fence was intact and completely bypassed one tier up.
//
// The stored epoch lives in its own hash field so the script compares a number instead of decoding
// JSON. A refusal returns 0, which Checkpoint reports as world.ErrCheckpointNotOwner rather than
// swallowing: this tier pulses every ~10s against the Postgres fence's ~60s, so it is the EARLIEST
// detector of a double-own and the saver routes the refusal to the same eviction. An absent epoch
// field (a pre-#432 value still inside its TTL) compares as 0 and therefore loses to any real claim,
// which self-heals on the first guarded write.
//
// The TTL is refreshed only when the write applies, so a zombie cannot keep a stale value alive.
var checkpointGuard = redis.NewScript(`
local cur = redis.call('HGET', KEYS[1], 'epoch')
local mine = tonumber(ARGV[2])
if cur and tonumber(cur) > mine then
  return 0
end
redis.call('HSET', KEYS[1], 'data', ARGV[1], 'epoch', mine)
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1
`)

// Checkpoint writes snap as the latest checkpoint for snap.Name, overwriting the prior one and
// refreshing the TTL, subject to the ownership guard. Off the zone goroutine (the saver). A Redis
// failure is returned (non-fatal to the caller): a missed checkpoint only widens the crash window by
// one cadence tick. An OWNERSHIP refusal is returned as world.ErrCheckpointNotOwner and is a different
// thing entirely — the caller must treat it as a zombie verdict, not as a lost tick.
func (r *Redis) Checkpoint(ctx context.Context, snap world.CharSnapshot) error {
	v := value{
		PID:          string(snap.PID),
		Name:         snap.Name,
		ZoneRef:      snap.ZoneRef,
		RoomRef:      snap.RoomRef,
		StateVersion: snap.StateVersion,
		OwnerEpoch:   snap.OwnerEpoch,
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
	applied, err := checkpointGuard.Run(ctx, r.rdb, []string{r.key(snap.Name)},
		data, snap.OwnerEpoch, r.ttl.Milliseconds()).Int()
	if err != nil {
		return fmt.Errorf("checkpoint: write %q: %w", snap.Name, err)
	}
	if applied != 1 {
		// A newer owner holds this slot: this writer is a zombie. Report it as a DISTINCT sentinel rather
		// than swallowing it — this tier pulses every ~10s while the Postgres fence only fires on the
		// ~60s flush, so it is the earliest detector of a double-own, and the saver routes it to the same
		// eviction the Postgres verdict does.
		return world.ErrCheckpointNotOwner
	}
	return nil
}

// LoadCheckpoint returns the last checkpoint for name, or found=false if none exists (or it
// expired). Off the zone goroutine (the login read). The caller compares its state_version against
// the Postgres row and uses whichever is higher (the freshness check).
func (r *Redis) LoadCheckpoint(ctx context.Context, name string) (world.CharSnapshot, bool, error) {
	data, err := r.rdb.HGet(ctx, r.key(name), "data").Bytes()
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
		OwnerEpoch:   v.OwnerEpoch,
		State:        v.State,
	}, true, nil
}

// Compile-time assertion that *Redis satisfies world.Checkpointer.
var _ world.Checkpointer = (*Redis)(nil)

# Protocol design — gate<->world & shard<->shard

Defines the two contracts the whole distributed topology rides on: the
per-player `Play` stream (gate <-> world) and the `Handoff` RPCs (world <->
world).

Guiding principles:
- **Single writer per player.** Exactly one shard owns a player at any instant, tracked by
  an `epoch`. Stale writes from a previous owner are rejected.
- **The gate owns the socket; the world owns the player.** The TCP connection never moves;
  only the *stream target* moves during a handoff.
- **The snapshot is the player in transit.** Movement does not depend on the database being
  up; the durable store is a periodic backstop, not the transfer mechanism.
- **No lost input.** The gate sequences input; shards ack a consumed offset; redirects replay
  from the ack point.

---

## 1. `Play` service (gate <-> world)

One bidirectional stream per connected player. The gate decodes telnet/GMCP into structured
frames; the world emits semantic output the gate renders for the specific terminal.

```proto
syntax = "proto3";
package telosmud.play.v1;

service Play {
  rpc Connect(stream ClientFrame) returns (stream ServerFrame);
}

// ---- gate -> world ----
message ClientFrame {
  oneof payload {
    Attach    attach = 1;   // ALWAYS the first frame on a stream
    InputLine input  = 2;   // a line the player typed
    GmcpIn    gmcp   = 3;   // client GMCP message (Core.Supports, Char.Login, Ping...)
    Resize    resize = 4;   // NAWS window-size change
    Pong      pong   = 5;   // keepalive reply
    Detach    detach = 6;   // clean quit or link-death
  }
}

message Attach {
  string     session_id    = 1;  // gate-generated, STABLE across a redirect
  string     account_id    = 2;
  string     character_id  = 3;
  ClientInfo client        = 4;
  string     handoff_token = 5;  // set ONLY when re-dialing after a Redirect
  uint64     input_seq     = 6;  // next input seq the gate will send (resume point)
}

message ClientInfo {
  string          remote_addr   = 1;
  string          terminal_type = 2;   // TTYPE / MTTS name
  uint32          width         = 3;   // NAWS
  uint32          height        = 4;
  string          charset       = 5;   // utf-8 / latin-1
  bool            mccp          = 6;
  bool            gmcp          = 7;
  repeated string gmcp_supports = 8;   // Core.Supports.Set contents
  uint32          mtts          = 9;   // MTTS capability bitvector (256-color, UTF-8, ...)
}

message InputLine { uint64 seq = 1; string text = 2; int64 client_ts_ms = 3; }
message GmcpIn    { string package = 1; bytes json = 2; }
message Resize    { uint32 width = 1; uint32 height = 2; }
message Pong      { uint64 nonce = 1; }
message Detach    { DetachReason reason = 1; }
enum DetachReason { DETACH_UNSPECIFIED = 0; DETACH_QUIT = 1; DETACH_LINKDEAD = 2; }

// ---- world -> gate ----
message ServerFrame {
  oneof payload {
    Attached     attached   = 1;  // ack of Attach
    Output       output     = 2;  // text to render
    GmcpOut      gmcp       = 3;
    PromptUpdate prompt     = 4;  // emitted without trailing newline
    Control      control    = 5;  // echo on/off, MSSP...
    Ping         ping       = 6;
    Redirect     redirect   = 7;  // migrate to another shard
    Disconnect   disconnect = 8;  // server-initiated close
  }
  uint64 ack_input_seq = 9;        // piggybacked on EVERY frame: input high-water mark
}

message Attached     { string shard_id = 1; /* ack rides on ServerFrame.ack_input_seq */ }
message Output {
  string       markup       = 1;  // semantic markup; gate renders to the terminal
  OutputClass  class        = 2;  // lets clients/gate route or style
  bool         preformatted = 3;  // true = do NOT word-wrap (maps, tables, ascii art)
  bool         no_newline   = 4;
}
enum OutputClass { OUT_NORMAL = 0; OUT_SYSTEM = 1; OUT_COMBAT = 2; OUT_CHANNEL = 3; OUT_TELL = 4; }

message GmcpOut      { string package = 1; bytes json = 2; }
message PromptUpdate { string markup = 1; }
message Control      { oneof kind { bool echo = 1; bytes mssp = 2; } }
message Ping         { uint64 nonce = 1; }

message Redirect {
  string target_shard_addr = 1;  // gate dials here
  string handoff_token     = 2;  // gate presents this in the new Attach
  // the replay point rides on the destination's Attached frame (ServerFrame.ack_input_seq),
  // not the redirect — the gate replays from what the new shard has actually consumed.
}
message Disconnect { string reason = 1; bool reconnectable = 2; }
```

### Notes
- **First frame must be `Attach`.** The world replies `Attached` with the input offset it has
  already consumed (nonzero on a migration resume), so the gate knows where to replay from.
- **Input sequencing.** `InputLine.seq` is monotonic per `session_id` (not per stream). After
  a redirect the gate replays any input the new shard hasn't acked. Shards dedupe by `seq`
  (drop `seq <= applied`) and report their high-water mark on every `ServerFrame.ack_input_seq`;
  the gate prunes its replay buffer to that value. `seq == 0` means unsequenced (always apply).
- **GMCP is structured, not bytes.** `GmcpIn/Out` carry `package` + raw JSON; the gate does
  telnet subnegotiation framing. The world never sees `IAC SB 201...`.
- **Keepalive.** Server `Ping`/client `Pong` measures real latency and detects half-open
  sockets independently of TCP.

---

## 2. `Handoff` service (world <-> world)

Cross-shard movement is a **two-phase** migration over direct shard-to-shard gRPC (not NATS —
we need ordered request/response with a payload and an ack). NATS is used only for the
directory-change broadcast.

```proto
syntax = "proto3";
package telosmud.handoff.v1;

service Handoff {
  rpc Prepare(PrepareRequest) returns (PrepareResponse); // rehydrate in PENDING state
  rpc Commit (CommitRequest)  returns (CommitResponse);  // activate after gate re-attaches
  rpc Abort  (AbortRequest)   returns (AbortResponse);   // roll back on failure/timeout
}

message PrepareRequest {
  string         session_id     = 1;
  PlayerSnapshot snapshot       = 2;
  string         target_zone_id = 3;
  string         target_room_id = 4;
  uint64         epoch          = 5;  // ownership epoch; destination records it
  string         from_shard_id  = 6;
}
message PrepareResponse {
  string handoff_token     = 1;  // gate presents this on re-dial
  string target_shard_addr = 2;  // public dial address for the gate
  uint64 pending_ttl_ms    = 3;  // how long the destination holds the pending entity
}

message CommitRequest  { string handoff_token = 1; }
message CommitResponse {}
message AbortRequest   { string handoff_token = 1; string reason = 2; }
message AbortResponse  {}
```

### PlayerSnapshot — the migration payload
The authoritative in-memory player state, carried so the destination resumes with **zero DB
round-trips**.

```proto
message PlayerSnapshot {
  string              character_id  = 1;
  string              account_id    = 2;
  string              name          = 3;
  CoreStats           stats         = 4;   // str/dex/int/... , level, xp
  Vitals              vitals        = 5;   // hp/mp/mv + maxes
  repeated Item       inventory     = 6;
  repeated Equipped   equipment     = 7;
  repeated Affect     affects       = 8;   // buffs/debuffs with remaining duration
  repeated SkillState skills        = 9;
  map<string,string>  flags         = 10;  // volatile flags not worth a column
  uint64              state_version = 11;  // optimistic-concurrency guard vs the DB
  uint64              applied_seq   = 12;  // highest input seq applied at freeze (§5)
  string              persist_id    = 13;  // characters.id, so the destination CASes the same row
  string              comms_state   = 14;  // per-channel toggles / ignore list / AFK (JSON)
  string              state_json    = 15;  // remaining content state subtree (the full carry)
}
```

(`Item`, `Equipped`, `Affect`, `SkillState`, `CoreStats`, `Vitals` are defined in
`common.proto`; `PlayerSnapshot` itself lives in `handoff.proto`.) Position/combat target are
intentionally absent — **you cannot change zones while in combat** (classic MUD rule), so
there is no fight to transfer.

---

## 3. Cross-shard handoff, step by step

1. Source shard A resolves an exit whose destination zone lives on shard B.
2. A **freezes** the player (stops processing their input), **bumps the epoch**, and
   serializes a `PlayerSnapshot`.
3. A calls `Prepare(snapshot)` on B. B rehydrates the player in the target room in a
   **pending** state (present in the world but not yet bound to a stream) and returns a
   `handoff_token` + its dial address.
4. A updates the **directory** (`player -> B`, new epoch) — broadcast on NATS for cache
   invalidation.
5. A sends `Redirect{B addr, token}` down the player's stream to the gate.
6. The gate stops sending to A, dials B's `Play.Connect`, and sends `Attach{handoff_token,
   input_seq}`. B replies `Attached` with the input seq it has already consumed; the gate
   replays from there, then resumes live forwarding.
7. B (or A on B's signal) calls `Commit`; B activates the player, A drops the frozen copy and
   closes the old stream.

**Failure handling:**
- B never readies / `Prepare` fails -> A `Abort`s, un-freezes the player, keeps them. No move.
- Gate never re-attaches (client crash) -> B's pending entity times out (`pending_ttl_ms`); the
  directory already points to B, so on reconnect the player resumes there as link-dead. State
  is not lost.
- Duplicate/late `Prepare` (network retry) -> guarded by `handoff_token` + `epoch`; B ignores
  a lower-or-equal epoch.

---

## 4. Why these shapes

- **Two-phase (Prepare/Commit) over one-shot.** One-shot risks a window where neither shard
  owns the player (in-flight) or both do (retry). Prepare/Commit + epoch gives a clean
  single-writer handoff with a bounded pending state and a real abort path.
- **gRPC streams for I/O, not NATS.** Per-player input must be ordered and back-pressured; a
  dedicated stream gives that for free and lets the gate re-target on redirect. NATS fan-out
  semantics fit chat/presence, not a single player's keystrokes.
- **Fat snapshot over thin+reload.** Zone crossing is the single most common cross-shard
  event; making it a DB read would couple movement latency to store latency and add load
  exactly where players congregate (zone borders). The snapshot is small (one character) and
  self-contained.

---

## 5. Handoff exactly-once invariants

The exactly-once and single-writer guarantees rest on the invariants below. The redirect/
replay substrate — stable per-session input seq, a gate-side input buffer pruned on ack,
world-side dedup by seq, and `Attached.ack_input_seq` — is the foundation the cross-shard exit
is layered on; it works the same way for a re-dial to the same shard as across shards.

- **`applied_seq` is the linchpin.** `PlayerSnapshot.applied_seq` carries the highest input
  seq the source had applied at freeze. The destination initializes its dedup high-water mark
  from it and drops any replayed line with `seq <= applied_seq`; the gate replays from the
  destination's `Attached.ack_input_seq`. Freeze + snapshot + read-`applied_seq` happen
  atomically in a single zone-inbox handler.

- **The gate owns the input buffer.** `session_id` and the input seq are **session-scoped**
  (generated once at login, stable across re-dials), not per-stream. The gate holds un-acked
  input in an ordered buffer keyed by seq, pruned on ack; on re-attach it replays
  `(ack_input_seq, …]` in order, *then* resumes live forwarding. Input not yet acked when the
  gate *process* dies is forfeit — "no lost input" is scoped to graceful redirect, not gate
  crash.

- **FROZEN is a real state.** On beginning a handoff the source enters FROZEN and must **not**
  `leave()` the player on stream-drop; the only exits are Commit-confirmed → discard, or
  Abort → thaw. This stops the frozen copy being destroyed when the gate re-dials away.

- **B drives Commit; Commit is the point of no return.** The destination self-commits the
  instant the gate's stream binds and replay completes. Abort is illegal once Commit begins;
  the source only consumes a "you may discard" signal.

- **Idempotent Prepare/Commit.** `handoff_token` is derived deterministically from
  `(session_id, epoch)`, so a retried Prepare returns the same token and one pending entity.
  Prepare rejects `epoch < current`; the same `(session_id, epoch)` is an idempotent return.
  Commit is idempotent on the token; pending→active is a one-way latch.

- **Abort rewrites the directory.** On Abort / Commit-failure the source reclaims ownership by
  writing `player→A` with a *higher* epoch (CAS) and thaws, so a zombie destination can't
  believe it still owns the player.

- **Monotonic epoch via the directory.** Epoch is allocated by the directory's compare-and-set
  on player placement (`internal/directory`), so it is globally monotonic per player and never
  rolls back. Epoch (in-flight ownership) and `state_version` (DB optimistic lock) stay
  separate counters.

- **Source-side freeze timeout.** Beyond the destination's `pending_ttl_ms`, the source holds a
  frozen player only for a bounded time; on timeout it Aborts (reclaim directory + bump epoch)
  so a dead gate can't leak frozen players. The pending-bind vs TTL-reap race is resolved under
  one lock on the pending entity.

- **Single-session lock held across the handoff.** The session lock does **not** release when
  the source's stream drops mid-handoff. A second login during an in-flight handoff is
  **rejected** until the handoff commits or aborts; normal take-over applies only once the
  player is settled on one shard.

- **Combat exclusion enforced at the exit.** Because position/target are omitted from the
  snapshot, the cross-shard exit resolver refuses the move while the player is in combat (no
  fleeing across a zone boundary mid-fight).

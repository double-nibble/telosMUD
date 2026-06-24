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
}

message Attached     { string shard_id = 1; uint64 ack_input_seq = 2; }
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
  uint64 resume_input_seq  = 3;  // replay client input from here
}
message Disconnect { string reason = 1; bool reconnectable = 2; }
```

### Notes
- **First frame must be `Attach`.** The world replies `Attached` with the input offset it has
  already consumed (nonzero on a migration resume), so the gate knows where to replay from.
- **Input sequencing.** `InputLine.seq` is monotonic per `session_id` (not per stream). After
  a redirect the gate replays any input the new shard hasn't acked. Shards dedupe by `seq`.
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
  uint64              state_version = 11;   // optimistic-concurrency guard vs the DB
}
```

(`Item`, `Equipped`, `Affect`, `SkillState`, `CoreStats`, `Vitals` defined in
`common.proto`.) Position/combat target are intentionally absent — **you cannot change zones
while in combat** (classic MUD rule), so there is no fight to transfer.

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
5. A sends `Redirect{B addr, token, resume_seq}` down the player's stream to the gate.
6. The gate stops sending to A, dials B's `Play.Connect`, and sends `Attach{handoff_token,
   input_seq}`. B binds the stream to the pending entity and replays any un-acked input.
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

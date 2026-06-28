# Phase 8 — Comms over NATS (cross-shard channels, tells, who, presence, mail) — IMPLEMENTATION PLAN

Status: **proposal / planning** — the design + sliced plan for ROADMAP Phase 8. This is a sign-off
doc for the human owner; it implements **nothing**. Confirm §1 (the topology decision) and §2 (the
subject taxonomy) before slice 8.1.

Phase 8 is the first phase whose primary state is **player-scoped and cross-shard**, not zone-scoped.
Every system before it (rooms, combat, affects, Lua) lives *inside* a zone goroutine and never reaches
past it; cross-zone interaction is the handoff (PROTOCOL.md) or a reserved Phase-10 hook. Comms breaks
that frame on purpose: a `gossip` line typed by a player on shard A must reach the socket of a player
on shard B **regardless of which zone either is in**. So the central question is not "what does a
channel do" (that is easy) but **where the cross-shard fan-out terminates** — which process holds the
subscription that turns a NATS message into bytes on a particular terminal. §1 answers that first; the
rest hangs off it.

**Done when** (ROADMAP Phase 8): two players on **different shards** chat on a channel and see each
other in `who`. The capstone (§8) makes that concrete and adds the failure-mode demonstrations (a
crashed shard's players age out of `who`; an offline tell is delivered on next login exactly once).

This phase builds on, and must not re-invent:
- **The NATS wiring + mem-fallback pattern** already proven by the content bus
  (`internal/contentbus/nats.go`, `membus.go`, `contentbus.go`): a small `Bus` interface, a NATS impl,
  an in-process `MemBus` that mirrors the NATS observable semantics so the whole feature is
  unit-testable with **no live broker** (the cross-shard tests run many shards in one process against
  one `MemBus`). Phase 8 ships a **parallel comms bus** with the same shape. It does **not** widen the
  content bus — that bus carries `(kind,ref,pack)` invalidations and nothing else.
- **The directory** (`internal/directory/`): `PlayerPlacement`/`SetPlayerShard` already track which
  shard a player lives on, with a monotonic epoch. Tell-routing and presence reuse this — the
  directory is the existing player→shard map.
- **The gate↔world Play stream** (`internal/gate/`, `internal/world/server.go`): the per-player socket
  and the `out chan *playv1.ServerFrame` that already carries every line to a terminal. Comms output
  is a new producer into that **existing** channel.
- **The durability ladder** (PERSISTENCE.md): Redis for operational/ephemeral (`presence` is already
  listed there), Postgres+Redis for durable player state (`mail` is **already listed there**). Phase 8
  does not invent a new store tier; it uses the ones the ladder already names.

It does **not** build: GMCP structured comm emit (`Comm.Channel.Text` — Phase 9; Phase 8 emits plain
text frames), the **cross-zone scoped world-event bus** (Phase 10, WORLD-EVENTS.md), auth/accounts
(Phase 14 — Phase 8 keeps the stub login, and a "player" is the login name as today). The Phase-8 ↔
Phase-10 boundary is load-bearing and is drawn explicitly in §7.

---

## 0. Where Phase 8 sits on the existing code

| Existing | Phase 8 change |
|---|---|
| `internal/contentbus/` — `Bus` interface + NATS impl + `MemBus`, one subject, JSON payload, optional/never-fatal | A **new sibling package `internal/commbus/`** with the **same shape** (interface + NATS impl + `MemBus`), but a **subject taxonomy** (not one subject) and **JetStream** for durable tells/mail. Reuses the connection/degradation discipline verbatim; does not import or extend contentbus. |
| `internal/directory/redis.go` — `PlayerPlacement`/`SetPlayerShard`/`PlayerEpoch` (player→shard, epoch-monotonic) | Tell routing reads `PlayerPlacement` to find a target's shard subject. Presence **may** reuse the same Redis (a `who` roster) or a NATS mechanism — §1 P8-D4 decides. No change to the placement CAS. |
| `internal/gate/` — `Server.handle` runs the per-connection bridge; `session` owns the input buffer; the writer goroutine drains `out chan ServerFrame` to the socket | **P8-D1 decides whether the gate or the world subscribes** for a player's channels. If the gate subscribes (the recommendation), the gate gains a **comms client** per connection and a new producer into the writer path. |
| `internal/world/commands.go` — `cmdSay`/`cmdWho` (zone-local), `cmdQuit`; `zone.who` lists only `z.players` | `say` stays zone-local (it is a room verb). **New commands** `gossip`/`newbie`/channel verbs, `tell`, a **cross-shard `who`**, `reply`, `mail`, channel toggles, `ignore`, `afk`. Whether these live in the world command table or a comms tier depends on P8-D1. |
| `internal/world/zone.go` — `presenceMsg` (zone-local presence query), `linkDeadGrace` (a player's in-zone presence survives a stream drop briefly) | A **presence heartbeat** publisher (per shard, listing its live players) feeds the cross-shard roster; the link-dead/quit lifecycle is the disconnect signal that ages a player out. |
| `internal/world/character.go` — `StateJSON` + `state` JSONB, saved on the existing cadence/ladder | **Player comms state** (channel on/off, ignore list, AFK) is a new **data-only subtree** of `StateJSON` (the established additive-JSONB pattern), saved on the existing cadence. |
| `cmd/telos-world/main.go` / `cmd/telos-gate/main.go` — wire NATS/Redis/directory, optional/never-fatal | The comms bus is wired the same way: `TELOS_NATS_URL` (the existing `cfg.NATS.URL`), optional, never fatal — **NATS down ⇒ comms degraded (local-only), never a boot failure**, exactly like hot reload degrades. |
| The empty-boot invariant (`NewShardFromContent` with no content ⇒ empty zones; `empty_world_test.go`) | **Channels are content** (`channel_defs`): a pack with no `channel_defs` ⇒ **no channels** and the empty-boot test stays green. Tells/who/presence/mail are engine mechanism (no content needed) but are inert with no players. |

The riskiest *structural* point is **the trust/ordering boundary the comms bus introduces** — a
message authored on one shard and rendered on another crosses a process boundary with no single-writer
to serialize it. So the plan leads with the **transport + topology** (8.1) and gets the
attribution/ordering/abuse boundary reviewed **before** any feature (channels, tells, who, mail) hangs
off it. This mirrors PHASE7-PLAN leading with the sandbox: review the boundary before the capabilities.

---

## 1. Design decisions (confirm before slice 8.1)

### P8-D1 — The comms topology: **gate-subscribes** (the central decision)

**The question.** A channel/tell line is *player-scoped*: its output must reach a player's terminal no
matter which zone or shard that player is currently on. Three processes could hold the NATS
subscription that turns a comms message into a frame on that terminal:

- **(A) World subscribes (per shard).** Each world shard subscribes to the channels its resident
  players are on, and pushes the rendered line into each player's `out chan ServerFrame`.
- **(B) Gate subscribes (per connection).** The gate — which already owns the socket and the
  `out`-equivalent writer path, and is *stable across the cross-shard handoff* — subscribes on behalf
  of each connected player and writes comms lines straight to the terminal.
- **(C) A new comms tier.** A dedicated `telos-comms` service holds all subscriptions and fans out to
  gates.

**Decision: (B) gate-subscribes, with the world as the message *source*.** The gate is the natural
home for player-scoped cross-shard output because:
1. **The gate already survives the handoff.** When a player walks A→B, the gate keeps the same
   `session` and socket and merely re-dials the Play stream (PROTOCOL.md §5; `gate.go` re-dial loop).
   A *world-held* subscription (option A) would have to be **torn down on shard A and re-established on
   shard B mid-handoff** — a new neither/both-subscribed window layered on top of the existing
   neither/both-*own* handoff window. The gate subscription does not move when the player's zone moves.
   This is the decisive argument: comms subscription lifetime should track the **connection**, which is
   the gate's unit, not the **zone ownership**, which moves.
2. **The gate already multiplexes one socket.** Channel text, tell text, and room output all become
   `ServerFrame_Output` on the same writer; the gate is where they converge to bytes anyway.
3. **No new tier to deploy/operate** (rejecting C for v1). C is the right answer at extreme scale (it
   decouples channel fan-out from gate count and lets channels shard independently) — but it is a
   premature tier now and is called out as a **scale escape hatch** in §3, not built.

**The gate↔world responsibility split that follows:**
- **World is the SOURCE.** A player types `gossip hi` → it reaches the zone goroutine as input (the
  existing path) → the world command handler **publishes** a channel message to NATS (author identity,
  channel ref, text, a monotonic-per-author sequence, an idempotency key). The world does **not**
  deliver it to anyone's socket directly (not even co-located players — they receive it via the bus
  too, so there is exactly one delivery path and no double-render). The world publishes because **it
  holds the authoritative author identity** (the `*Entity`, its name, its flags) and the
  access/format rules (channel is content — P8-D3); the gate must never be trusted to attribute a
  message (P8-A2, impersonation).
- **Gate is the SINK.** Each connection's gate-side comms client is subscribed to the channels the
  player has on (and to that player's personal tell/notify subject). On a message it renders per the
  channel format and writes a frame. The gate applies **receiver-side** policy that is cheap and
  socket-local: the receiver's channel-off toggle and ignore list (so a blocked sender's line is
  dropped at the receiver — defense in depth beside the sender-side checks; P8-A6).
- **Commands** that *emit* comms (`gossip`, `tell`, who-as-broadcast) are **world commands** (they need
  the author entity + content rules), registered in the world command table beside `say`. Commands
  that are **pure local toggles** (`channels on/off`, `ignore`, `afk`) are also world commands because
  they mutate persisted character state (the JSONB subtree, P8-D7) — but the gate caches the resulting
  filter set so receiver-side filtering needs no per-message world round-trip.

> **The trust line (write it down):** the **world authors** (it owns identity + content rules); the
> **gate renders + receiver-filters** (it owns the socket). A message's attribution is set by the
> source world from the live `*Entity`, **never** by the gate and **never** carried as a
> client-supplied field. This is the impersonation gate (P8-A2) and is the single most security-
> sensitive invariant of the phase.

*Rejected:* (A) world-subscribes — the subscription would migrate on every handoff, multiplying the
handoff's neither/both window onto comms, and a shard hosting 0 of a channel's listeners would still
carry the subscription churn. (C) comms tier — correct at scale, premature now (§3 escape hatch).

### P8-D2 — NATS subject taxonomy + JetStream streams

A **subject hierarchy** (not contentbus's single subject), namespaced under a `telos.comms.` root so it
never collides with `content.invalidate`. Proposed taxonomy (confirm the exact tokens at sign-off):

| Purpose | Subject | Delivery | Notes |
|---|---|---|---|
| Channel message | `telos.comms.chan.<channelRef>` | **NATS core** (transient, at-most-once) | Channels are ephemeral chat: a missed line during a NATS blip is acceptable (you were not listening). Every gate subscribed to that channel ref receives it. `<channelRef>` is the content channel id (`gossip`/`newbie`), so a gate subscribes per-channel-the-player-has-on. |
| Online tell (notify) | `telos.comms.tell.<targetPlayerId>` | **NATS core** when the target is online | The sender's world looks up the target via `directory.PlayerPlacement`; if present/online it publishes to the target's personal subject, which the target's gate is subscribed to. |
| Presence heartbeat | `telos.comms.presence` (or a NATS **KV** bucket — P8-D4) | **NATS core** fan-in to the roster owner, OR KV | Per-shard heartbeats listing live players; feeds the `who` roster. P8-D4 picks the mechanism. |
| Durable tell (offline) | JetStream stream `COMMS_TELL`, subject `telos.comms.dtell.<targetPlayerId>` | **JetStream** (at-least-once + dedup) | An offline target's tell is persisted; delivered on next login. Idempotency via `Nats-Msg-Id` (publish-side dedup window) + a consumer-side delivered-cursor (P8-D5). |
| Mail | JetStream stream `COMMS_MAIL` **or Postgres** (P8-D6) | durable | Persistent inbox; read/send model. P8-D6 chooses the store. |

**Wildcard subscription.** A gate subscribes to `telos.comms.chan.gossip`, `telos.comms.chan.newbie`,
… per the player's enabled channels (re-subscribing on a toggle), plus its own
`telos.comms.tell.<self>` and `telos.comms.dtell.<self>`. It does **not** use a `telos.comms.chan.>`
wildcard (that would receive every channel including ones the player has off, pushing the filter to the
gate for *every* line — the per-channel subscribe keeps the broker doing the fan-out cut). *(Open
question OQ-3: per-channel subscribe vs one wildcard + gate-filter — a fan-out-vs-subscription-churn
trade; recommended per-channel, revisit under load.)*

**Mem fallback.** `internal/commbus/membus.go` mirrors these semantics in-process (a subject→subscriber
map with per-sub ordered delivery, exactly like contentbus's `MemBus`), and a **mem JetStream stand-in**
(an in-memory append log with a delivered-cursor) so the durable-tell/mail slices are testable without a
broker. The cross-shard tests run N shards + N gates in one process against one `MemBus`.

### P8-D3 — Channels are CONTENT (`channel_defs`), not hardcoded

Per the engine pillar (PRINCIPLES.md: nothing hardcoded; MEMORY: extensibility across game systems), a
channel is a **content definition**, not an engine enum. A `channel_def` carries:

- `ref` (stable id: `gossip`, `newbie`, `auction`, an OOC channel, a guild channel later)
- display `name` and the **command verb(s)** that emit on it (so `gossip hi` works because the pack
  defined a `gossip` channel with that verb; an empty pack ⇒ no such verb)
- `color`/markup template and the **format** strings (the speaker-perspective and listener-perspective
  templates, e.g. `"[$channel] $name: $t"`), rendered with the existing `act()`-style `$`-substitution
  so a `%`/`$` in user text is data, never a template (the `cmdSay` precedent, commands.go)
- `access` predicate (who may listen/speak — by flag/attribute/level; later by guild membership). A
  content predicate, evaluated engine-side against the author `*Entity` — **never** trusting the client.
- `default_on` (is a new character subscribed by default), `history` size (recent-lines buffer for a
  late joiner — optional, deferred shape).

This is a **new definition table + loader mapping + content-bus invalidation kind** (`channel`), so
editing a channel's color/access hot-reloads like any other def (the Phase-4 pattern). The empty-boot
invariant: **no `channel_defs` ⇒ no channels ⇒ no channel verbs ⇒ `empty_world_test.go` stays green.**
`tell`/`who`/`mail`/presence are **engine mechanism** (not content — they exist with zero packs), but a
*channel* is content.

> Tells, who, mail, presence: **engine**. Channels: **content**. This split is deliberate — directed
> player↔player messaging and the online roster are universal mechanism; named broadcast channels with
> colors/access/format are world-flavor.

### P8-D4 — Cross-shard `who` / presence: **Redis roster + heartbeat, with a NATS-core liveness ping**

`who` must list players across **all** shards, and a crashed shard's players must **age out** (the
stated failure mode). Options:

- **(A) Redis presence roster** (recommended). Each shard maintains, in Redis, the set of its live
  players with a **per-player TTL** refreshed by a heartbeat (the same pattern as the directory's
  shard/zone leases, `directory/redis.go`). `who` is a single Redis scan of the roster (filtered by
  visibility). **Staleness handling is automatic**: a crashed shard stops refreshing, and its players'
  entries **expire** (TTL ≈ 2–3× the heartbeat interval) — they age out of `who` without any explicit
  cleanup. PERSISTENCE.md **already lists `presence` under the Redis/operational tier**, so this is the
  ladder's intended home.
- **(B) NATS KV presence bucket** — equivalent semantics (per-key TTL, watchable), but introduces a
  second store for a job Redis already does, and Redis is already wired (`cmd/telos-world/main.go`).
- **(C) Scatter-gather query** — on `who`, broadcast a request and aggregate replies with a timeout.
  Rejected: adds tail-latency to a common command and a partition makes `who` flaky; the roster is
  strictly better for a read-mostly online list.

**Recommendation: (A) Redis roster.** Each shard writes `presence:<playerId> = {name, shardId, flags,
lastSeen}` with a TTL on join and refreshes it on a heartbeat (and on the existing link-dead/quit
lifecycle removes it eagerly on a clean quit — `cmdQuit`/`leave` in zone.go). `who` reads the roster.

**Crash age-out invariant (the failure the phase must demonstrate):** a shard that crashes leaves its
players in the roster only until their TTL lapses; after that they are gone from `who`. The TTL is the
**only** mechanism that recovers a crashed shard's presence — never an explicit "shard died, clean its
players" step (which itself could be lost). This is the same lease-expiry recovery the directory uses
for zones (`DefaultZoneLease`). **Tune the heartbeat/TTL** so the age-out window is bounded (target:
≤ ~30s, matching the directory leases) — OQ-2.

**Presence is operational/ephemeral, never authoritative.** A stale presence entry must never let a
**tell** route to a dead shard and be lost — tell routing reads the **directory** (`PlayerPlacement`,
which is the placement-epoch-authoritative map), not the presence roster, and falls back to the durable
offline path if the publish has no live consumer (P8-D5). Presence answers *"is this name in `who`"*;
the directory answers *"which shard owns this player right now"*. Conflating them is a bug (P8-A4).

### P8-D5 — Tell routing: online via directory, offline via JetStream (at-least-once + idempotent)

A `tell <name> <msg>` resolves in the **source** world (it owns the sender identity + the gate/ignore
checks the sender side can do):

1. **Resolve the target** via `directory.PlayerPlacement(targetId)` (the authoritative player→shard
   map). If the target has a live placement → publish to `telos.comms.tell.<targetId>` (NATS core); the
   target's gate is subscribed and renders it. Echo "You tell X" to the sender.
2. **Offline / no live placement** → publish to the **JetStream** durable stream
   (`telos.comms.dtell.<targetId>`), persisted until the target logs in. On login the target's gate (or
   world — OQ-4) **consumes** the durable backlog and renders "While you were away, X told you …".

**At-least-once + idempotent + ordered:**
- JetStream is **at-least-once**; a redelivery (consumer ack lost, a reconnect) must not double-deliver
  a tell. Each durable tell carries an **idempotency key** (`<senderId>:<senderSeq>` — the sender's
  monotonic per-author sequence) set as the `Nats-Msg-Id` header → **publish-side dedup** within the
  stream's dedup window, **plus** a **consumer-side delivered-cursor** (the last-delivered sequence,
  persisted per player in their character state or Redis) so a redelivery **after** the dedup window
  (minutes later) is still suppressed at render time. Belt and suspenders: the broker dedups recent
  duplicates; the cursor dedups old ones (P8-A5, redelivery storms).
- **Ordering** is per-target, single durable consumer per player, acked in order; the sender's
  monotonic sequence gives a total order *per sender* and the consumer renders in stream order. We do
  **not** promise a global order across different senders (no shared clock; not worth a sequencer) —
  only "messages from one sender arrive in send order," which is what users perceive.
- **The online→offline race** (the target logs out between the directory read and the publish): if a
  core-NATS tell is published to a target's subject with **no subscriber**, it is silently dropped
  (NATS core is at-most-once). Mitigation: route ambiguity favors **durable**. Recommended:
  resolve-then-publish-core, **and** if the directory read showed the target only *recently* present or
  the publish cannot confirm a subscriber, **fall through to the durable stream** so a logging-out
  target still gets the tell on next login. *(OQ-1: do we want a JetStream-with-immediate-consumer
  model for ALL tells — durable always, online delivery is just a fast consumer — to eliminate the
  core/durable split and the race entirely? Simpler correctness, slightly more JetStream load. Strong
  candidate; flagged for the owner.)*

### P8-D6 — Mail: **Postgres** (recommended) vs JetStream

Mail is durable, queryable (list inbox, read item N, delete, mark-read), and long-lived — a
relational/document store fits better than a log:

- **(A) Postgres** (recommended). A `mail` table (`to_player`, `from_player`, `subject`, `body`,
  `sent_at`, `read_at`). PERSISTENCE.md **already lists `mail` under the Postgres+Redis durable player
  tier** — this is the documented home. Read/send is ordinary CRUD on the existing pool
  (`internal/store`), with the same optional/never-fatal degradation (no Postgres ⇒ mail disabled,
  not a crash).
- **(B) JetStream** stream per recipient. Works for delivery but is awkward for the **read model** (mark-
  read, delete a single item, list with subjects) — JetStream is a log, not an inbox. Rejected for mail
  as the primary store, though the *send notification* ("you have new mail") can ride the comms bus.

**Recommendation: (A) Postgres for the mail store; the comms bus only carries the "new mail" ping.**
Mail send = INSERT; mail read = SELECT; the recipient gets a transient `telos.comms.tell.<self>`-style
notify if online. This keeps the **offline tell** (transient, fire-and-forget, JetStream) and **mail**
(a durable inbox, Postgres) as distinct models rather than forcing both into one.

### P8-D7 — Player comms state: a data-only `StateJSON` subtree

Channel on/off toggles, the ignore/block list, and the AFK flag/message are **per-character, durable,
and small** — they ride the **existing character `state` JSONB** as a new data-only subtree (the
`StateJSON.Script` precedent in `character.go`, the established additive-JSONB pattern). Saved on the
existing cadence/ladder; loaded on login. The gate **caches** the receiver-side filter set (channels-on,
ignore list) at attach and on change, so per-message receiver filtering is socket-local and needs no
world round-trip.

- **Channel toggles**: which `channel_def` refs the player is subscribed to (default from
  `channel_def.default_on`). Drives the gate's per-channel NATS subscriptions.
- **Ignore list**: sender ids the player blocks — applied **both** sender-side (the source world drops a
  channel/tell from an author the *target* ignores — but the source only knows the *sender's* ignores,
  so the authoritative block is **receiver-side at the gate**) and as defense in depth. The blocked-
  sender-bypass threat (P8-A6) is why the **gate (receiver) is the authoritative ignore enforcement
  point** — it sees every inbound line and the receiver's own list.
- **AFK**: a flag + optional message; an AFK target's tell still delivers (or auto-replies "X is AFK")
  and the sender is told. Presence carries the AFK flag so `who` can mark it.

This subtree is **data-only** (strings/bools/lists of ids) — no code, no handles — and is size-guarded
like the Lua state subtree. Nothing here is content; it is per-player runtime state.

---

## 2. Threat / abuse model (the security-auditor's checklist for the phase)

Comms is the first **player-authored, cross-shard, fan-out** surface. Unlike the Lua sandbox (a trust
boundary against a *content author*), this is a trust boundary against **every connected player** — the
adversary is an ordinary hostile user with a telnet client. Every slice that adds a comms surface
carries its row's mitigation.

| # | Attack surface | Invariant | Enforced by | Tested by |
|---|---|---|---|---|
| **P8-A1** | **Spam / flood** — a player floods a channel or tells, drowning others / loading the broker. | Per-author comms is **rate-limited**; a flood throttles the **sender**, never degrades other players' delivery. | A token-bucket per author per channel/tell, enforced in the **source world** (it holds the author identity) **before** publish; over-limit lines are dropped with a "you are doing that too much" to the sender only. The broker never sees the dropped line. Slow-consumer protection: the gate's writer path already drops/backpressures a slow socket (the existing `out chan` discipline) so one slow terminal can't stall fan-out. | a flood test: author rate-limited, other players unaffected; a slow-socket test: fan-out not stalled by one slow gate. |
| **P8-A2** | **Impersonation** — a player forging another's name as the channel/tell author. | A message's **author identity is set by the source world from the live `*Entity`**, never from a client field. | The world publishes the author id/name; the wire payload's author field is **engine-set**; the gate **renders** but never **authors** (P8-D1). No client frame carries an author name. A receiver gate trusts the bus author field **only because the source world is the only publisher** — the comms bus subjects are published to **by world shards only**, never by gates (gates are subscribe-only on channel/tell subjects). | a test that a crafted client input cannot change the rendered author; a test that a gate cannot publish to a `chan.*`/`tell.*` subject (publish ACL / it simply has no publish path). |
| **P8-A3** | **Cross-shard message ordering** — two shards interleave a channel such that one player sees A-before-B and another B-before-A; or a sender's own lines reorder. | **Per-sender order** is preserved for every receiver; no global order is promised (and none is needed). | Each author stamps a **monotonic per-author sequence**; a channel is a single NATS subject so the **broker imposes one publish order** per channel (all subscribers see the same order for that subject). For tells, the per-player durable consumer acks in stream order. We do **not** claim cross-sender global order (no shared clock). | a two-sender interleave test: each sender's lines are in-order for every receiver; a tell-order test per sender. |
| **P8-A4** | **Presence spoofing / staleness** — a player appears online when crashed/gone, or a tell routes to a dead shard and is lost. | `who` is **best-effort and self-healing** (TTL age-out); **tell routing never trusts presence** — it uses the authoritative directory and falls back to durable. | Presence is TTL-leased (P8-D4): a crashed shard's players expire. A player cannot write another's presence (the shard writes only its own residents' keys, keyed by the player it actually hosts). Tell routing reads `directory.PlayerPlacement` (epoch-authoritative), not the roster; a publish with no live consumer falls to the durable stream (P8-D5) so it is not lost. | a crashed-shard age-out test (players gone from `who` after TTL); a tell-to-logging-out-target test (delivered durably, not lost); a test that a shard cannot write a non-resident's presence. |
| **P8-A5** | **JetStream redelivery storm** — a consumer reconnect / ack-loss redelivers a backlog, double-rendering tells or hammering a just-logged-in player. | A redelivered tell is **suppressed idempotently**; redelivery is **bounded** and never amplifies. | The `Nats-Msg-Id` idempotency key + the per-player **delivered-cursor** (P8-D5) suppress duplicates at render even past the dedup window; the durable consumer has **bounded redelivery** (max-deliver + backoff) so a poison message is parked, not infinitely redelivered; the login-time backlog drain is **paced** (cap lines/sec to the freshly-joined gate). | a redelivery test: the same tell delivered twice renders once; a max-deliver test: a failing message parks; a backlog-pacing test. |
| **P8-A6** | **Blocked-sender bypass** — an ignored sender's channel line / tell still reaches the receiver via a path that skips the block. | A receiver's **ignore list is enforced at the receiver gate** — the one place that sees **every** inbound comms line for that player — so no source path can bypass it. | The gate (P8-D1 sink) applies the receiver's ignore list to **every** channel/tell frame before rendering; sender-side checks are defense-in-depth, not the authority. A new comms path (mail notify, a future channel type) inherits the gate filter automatically because it funnels the same render point. | a test that an ignored sender's gossip is dropped at the receiver; a test that a *new* comms frame type is also ignore-filtered (the funnel, not per-path). |
| **P8-A7** | **Text injection** — a player embeds telnet control bytes, ANSI, or `$`/`%` format tokens to corrupt another's terminal or spoof channel markup. | User-supplied comms text is **data**, never markup/template; control bytes are sanitized. | Channel/tell text is substituted as a `$t` **data argument** (the `cmdSay` precedent — a `$`/`%` in it is literal), and run through the existing terminal sanitizer (the gate's `textsan` path used for speech) so a player can't inject ANSI/telnet IAC or fake a `[gossip] Admin:` prefix. The channel **format template** comes from content (trusted), the **text** from the user (sanitized). | a test that a `$n`/ANSI/IAC in tell text renders literally and cannot forge a channel prefix; reuse the existing speech-sanitization test pattern. |
| **P8-A8** | **Subject-injection / channel access bypass** — a player speaks on a channel they lack access to, or names a `<channelRef>` that injects into the subject space. | A player can only emit on a channel whose **content `access` predicate** they satisfy; `<channelRef>` is validated against the loaded `channel_defs`, never free-form into a subject. | The source world checks `channel_def.access` against the author `*Entity` before publish; the subject is built from a **validated, known** channel ref (a ref not in the loaded defs is rejected — no arbitrary subject). The gate subscribes only to channels the player has access to + on. | an access-denied test (speak on a no-access channel rejected); a bogus-channel-ref test (rejected, no subject injection). |

**Construction note (the load-bearing trust line):** the comms bus subjects under `telos.comms.chan.*`
and `telos.comms.tell.*` are **published to by world shards only**; **gates are subscribe-only** on
them. This is what makes P8-A2 (impersonation) hold — a receiver trusts the author field *because the
only writers are source worlds that set it from the live entity*. If a deployment ever lets a gate
publish (it must not), the impersonation gate is gone. The security-auditor signs off on this
publish/subscribe asymmetry before 8.1 lands, and on the rate-limit (P8-A1), the receiver-side ignore
funnel (P8-A6), and the text sanitization (P8-A7).

---

## 3. Risks & out-of-scope

### Explicitly OUT of scope
- **The cross-zone scoped world-event bus = Phase 10** (WORLD-EVENTS.md). Phase 8's comms bus is a
  **player-presence/channel layer**: it carries *player-authored chat and player-directed messages*
  between **gates and worlds**. The Phase-10 bus carries *world-state consequences* (a boss death
  rippling a region) between **zones/directors** with `transient`+`durable` scopes and leader-elected
  directors. **They are different buses with different payloads, owners, and lifecycles** — do not
  conflate them, do not build one on the other. A Phase-8 channel message is not a world event; a
  Phase-10 region event is not a chat line. (They may *share the NATS connection and the JetStream
  server*, but not the subject space or the code.) This boundary is the §7 note's whole point.
- **GMCP `Comm.Channel.Text` structured emit = Phase 9.** Phase 8 emits **plain `ServerFrame_Output`
  text** (the existing frame). When Phase 9 lands GMCP negotiation, a channel message *also* emits a
  structured `Comm.Channel.Text` package — but the binding shape is reserved, not built here. Phase 8
  channels render as text now.
- **Auth / accounts / real identity = Phase 14.** A "player" in Phase 8 is the **login name** (the
  current stub), and `playerId` is that name (as the directory already keys it). Single-session lock
  (PERSISTENCE.md) already prevents one name being two live sessions; cross-account block lists, real
  account identity, and friend lists wait for Phase 14.
- **A dedicated `telos-comms` tier (topology option C) = a scale escape hatch, not v1.** At millions of
  concurrent players, channel fan-out (every gossip line × every subscriber) can dominate. The escape
  hatch (documented, not built): introduce a comms tier that (a) terminates channel subscriptions so
  the gate count and channel count scale independently, (b) shards a hot channel across subjects, and
  (c) coalesces presence. Because Phase 8 keeps the **world=source / gate=sink** split behind a `Bus`
  interface, inserting a comms tier later is a wiring change, not a redesign. Capacity note: with the
  gate-subscribes model, a channel with N subscribers across M gates costs **M subscriptions and one
  broker fan-out of N**; the broker fan-out is the ceiling to watch (§ capacity, below).
- **Channel history / scrollback** beyond a small recent-lines buffer — deferred shape (P8-D3 notes the
  optional `history` size).

### Capacity / scale notes (quantify where we can)
- **Channel fan-out is the dominant write-amplification.** One gossip line to a channel with `S`
  subscribers is **one publish, S deliveries** by the broker. At `S` = tens of thousands on a global
  channel, a chatty channel is a fan-out hotspot — this is the metric to watch and the reason the
  comms-tier escape hatch exists. **Mitigation levers (documented, mostly deferred):** per-author rate
  limits (P8-A1, built), channel access narrowing the audience (P8-D3), and channel sharding (escape
  hatch). The *who* roster read is a Redis scan — bounded by online player count, cache/paginate at
  scale.
- **Presence write rate.** Each shard heartbeats its residents' presence TTLs. Batch the refresh (one
  pipelined Redis write per shard per heartbeat, not one per player per beat) so presence write rate is
  **O(shards / heartbeat-interval)**, not O(players). OQ-2 tunes the interval against the age-out window.
- **Tell volume** is point-to-point (one publish, one delivery) — not a fan-out concern; the durable
  stream's storage is the bound (offline tells accumulate until login; cap per-player backlog depth +
  TTL old durable tells).

### Integration risks
1. **The comms bus is a new cross-process trust + ordering boundary (security + distsys).** No single-
   writer serializes a cross-shard channel; correctness rests on the per-author sequence + per-subject
   broker order (P8-A3) and the world=source publish ACL (P8-A2). **security-auditor + distributed-
   systems-architect review 8.1** before any feature hangs off it.
2. **The gate subscription lifecycle must track the connection, not the handoff (distsys).** A
   subscription leak (a gate that doesn't unsubscribe on disconnect) is a slow resource leak and a
   ghost-presence bug; the subscription must be torn down on the **same** disconnect signal that drops
   the session. The handoff must **not** touch comms subscriptions (the whole reason for P8-D1-B). The
   distsys reviewer confirms the subscribe/unsubscribe pairs with the connection lifecycle in `gate.go`,
   and that a handoff is comms-transparent.
3. **Presence vs directory must not be conflated (distsys).** Tell routing uses the **directory**
   (authoritative, epoch); `who` uses **presence** (best-effort, TTL). Routing on presence would lose
   tells to a crashed shard (P8-A4). distsys reviewer confirms the two are never crossed.
4. **JetStream durability window vs Postgres (persistence).** Offline tells live in JetStream; mail
   lives in Postgres (P8-D6). The persistence-engineer confirms the durable-tell stream's
   retention/dedup window and the per-player delivered-cursor placement (character state vs Redis), and
   that the mail table follows the existing store patterns + optional/never-fatal degradation.
5. **NATS-down degradation (orchestration).** With NATS down, the comms bus is unreachable: channels and
   cross-shard tells **degrade to unavailable** (a clear "comms are temporarily offline" to the player),
   **never a crash** — exactly as hot reload degrades (`openContentBus`). `say`/`who`-within-shard still
   work (zone-local). orchestration reviewer confirms the never-fatal wiring and the player-facing
   degradation message.

### Cross-cutting reviewers (per the subagent-review-after-every-step rule)
- **comms/world-engineer (owning):** every slice — the comms bus, the topology wiring, the commands,
  presence, mail, the state subtree.
- **distributed-systems-architect:** **8.1** (the transport + the trust/ordering boundary, the
  publish/subscribe asymmetry), **8.2** (the gate subscription lifecycle vs the handoff —
  comms-transparency), **8.4** (presence TTL age-out + the directory/presence non-conflation),
  **8.5** (tell online/offline routing race + ordering), the Phase-8/Phase-10 boundary (every slice).
- **security-auditor:** **8.1** (publish ACL / impersonation gate), **8.3** (channel access predicate +
  text sanitization + rate limit), **8.5** (redelivery idempotency + bounded redelivery), **8.6**
  (receiver-side ignore funnel), the §2 threat model is the checklist; each slice carries its row.
- **edge/transport-engineer:** **8.2** (the gate-side comms client + the new producer into the writer
  path, slow-consumer backpressure), **8.3** (rendering channel frames on the existing socket path).
- **persistence-engineer:** **8.5** (durable-tell stream + delivered-cursor), **8.6** (mail store),
  **8.7** (the comms-state JSONB subtree + cadence).
- **orchestration-engineer:** **8.1** (NATS wiring, optional/never-fatal), the degradation behavior.
- **rpg-systems-designer (acceptance):** **8.3** (channel_defs are the right content shape — color/
  access/format/verb — and express a real channel set: gossip/newbie/auction/OOC).

---

## 4. Sliced implementation plan (ordered, independently committable)

The spine is **transport + topology → gate wiring → channels (content) → presence/who → tells (online
+ offline durable) → ignore/toggles → mail → comms state persistence**. Smallest, riskiest-first: 8.1
lands the bus + the trust/ordering boundary so the security + distsys reviewers sign off the boundary
**before** any feature hangs off it (the PHASE7 sandbox-first discipline). Each slice is a commit with
prior tests green and its owning + cross-cutting reviewers signing off. The **bare-engine invariant**
(a pack with no `channel_defs` ⇒ no channels; `empty_world_test.go` stays green) is a done-when on every
content-touching slice.

| Slice | Scope | Done when | Tests added |
|---|---|---|---|
| **8.1 — The comms bus transport + topology skeleton (the trust/ordering boundary)** | New `internal/commbus/`: the `Bus` interface (publish to a subject, subscribe to a subject/wildcard, close) + a **NATS impl** (subject taxonomy P8-D2, `telos.comms.*` root, **publish ACL: worlds publish, gates subscribe**) + a **`MemBus`** mirroring the semantics (per-sub ordered delivery) for hermetic tests + a **mem-JetStream stand-in** (append log + delivered-cursor). The **author-identity-is-engine-set** wire shape (P8-A2): the message payload carries an engine-set author id/name + a **monotonic per-author sequence** (P8-A3) + an idempotency key. **No channels/tells/who yet** — just the transport, the payload shape, and a round-trip across two in-process shards+gates over a `MemBus`. Optional/never-fatal wiring (NATS down ⇒ comms disabled), mirroring `openContentBus`. | A message published by shard-A's "world" is received by shard-B's "gate" subscriber over the `MemBus`; the author field is set by the publisher and a gate **cannot** publish on a `chan.*`/`tell.*` subject; per-author sequence is monotonic and a single subject preserves order; NATS-down ⇒ the bus is a nil/disabled no-op (no crash). Bare-zone unchanged. | **publish-ACL test (P8-A2, security)**; **per-subject-order + per-author-sequence test (P8-A3)**; mem-vs-(gated)NATS parity test; disabled-bus no-op test; round-trip-across-shards test. |
| **8.2 — Gate-side comms client + the writer-path producer (the sink)** | Wire P8-D1-B: each gate connection gets a **comms client** (subscribe-on-attach, **unsubscribe-on-disconnect** — the lifecycle tracks the **connection**, not the handoff), and a new producer that renders a comms message into a `ServerFrame_Output` on the **existing** writer path (`gate.go` writer). **The handoff must be comms-transparent**: a re-dial (A→B) does **not** touch the comms subscription. Slow-consumer: the comms producer respects the existing `out`-channel backpressure (one slow socket never stalls fan-out, P8-A1). **No channels defined yet** — drive it with a synthetic test message. | A comms message reaches a connected gate's socket; the subscription is **torn down on disconnect** (no leak); a **cross-shard handoff leaves the comms subscription untouched** (the player keeps receiving channel lines across an A→B walk — the load-bearing P8-D1 proof); a slow socket doesn't stall a sibling. | **subscription-lifecycle test (subscribe/unsubscribe paired with connect/disconnect, distsys)**; **handoff-comms-transparency test (distsys — the central topology proof)**; slow-consumer-no-stall test (edge). |
| **8.3 — Channels as content (`channel_defs`) + the channel verbs** | The `channel_def` table + loader mapping + a `channel` content-bus invalidation kind (P8-D3): ref, name, verb(s), color/format template, `access` predicate, `default_on`, `history` size. The **source-world publish path**: a channel verb (`gossip`) → world handler → **access check** (P8-A8) → **rate-limit** (P8-A1) → **sanitize text** as `$t` data (P8-A7) → publish to `telos.comms.chan.<ref>` with the engine-set author. The gate subscribes per the player's enabled channels and renders per the content format + **receiver access filter**. **Channels are CONTENT** — empty pack ⇒ no channel verbs. | A pack defines `gossip`; a player types `gossip hi` and a co-located AND a cross-shard player both see it rendered with the channel's color/format; speaking on a no-access channel is refused; flooding rate-limits the sender only; a `$`/ANSI/IAC in the text renders literally and can't forge a prefix; **no `channel_defs` ⇒ no `gossip` verb, `empty_world_test.go` green**. | **cross-shard channel delivery test (the Phase-8 done-when half)**; channel-access-denied test (P8-A8, security); rate-limit test (P8-A1, security); text-sanitization test (P8-A7, security); content hot-reload-of-a-channel test; **empty-boot-no-channels test**. |
| **8.4 — Cross-shard presence + `who`** | The Redis presence roster (P8-D4): each shard writes `presence:<playerId>={name,shardId,flags,lastSeen}` with a TTL on join, **batched heartbeat refresh**, eager removal on clean quit/leave (`zone.go` lifecycle). `cmdWho` becomes a **cross-shard roster read** (filtered by visibility), replacing the zone-local `z.who`. **TTL age-out** is the crashed-shard recovery (no explicit cleanup). Presence carries the AFK flag. | Two players on **different shards** both appear in `who` (the Phase-8 done-when, completed); a **crashed shard's players age out of `who`** after the TTL (the failure-mode demonstration); a clean quit removes the player from `who` immediately; a shard cannot write a non-resident's presence (P8-A4). | **cross-shard `who` test (done-when)**; **crashed-shard age-out test (P8-A4, distsys)**; clean-quit-eager-removal test; presence-write-authority test (P8-A4, security); batched-heartbeat-write-rate test. |
| **8.5 — Tells: online routing + offline durable (JetStream)** | `tell <name> <msg>` / `reply`: resolve target via **`directory.PlayerPlacement`** (P8-D5) → online: publish `telos.comms.tell.<target>` (core); offline: publish the **JetStream** durable stream with `Nats-Msg-Id` idempotency key + per-author sequence. Login-time **durable backlog drain** (paced) renders "while you were away…". **Idempotency**: the `Nats-Msg-Id` dedup window + the per-player **delivered-cursor** (P8-A5); **bounded redelivery** (max-deliver + backoff). The online→offline race favors durable (OQ-1). | A tell to an **online cross-shard** target arrives; a tell to an **offline** target is **delivered on next login exactly once** (the JetStream done-when); a **redelivery renders once** (idempotent); a target logging out mid-tell still gets it (durable fallback, not lost, P8-A4); per-sender order holds; a poison message parks (max-deliver). | **cross-shard online tell test**; **offline-tell-delivered-on-login test (done-when)**; **redelivery-idempotency test (P8-A5, security)**; logging-out-race test (P8-A4); per-sender-order test (P8-A3); bounded-redelivery test. |
| **8.6 — Channel toggles + ignore/AFK (receiver-side enforcement)** | `channels on/off <ref>`, `ignore <name>`, `afk [msg]` — world commands mutating the persisted comms-state subtree (P8-D7); the gate **caches** the filter set and re-subscribes channels on toggle. The **receiver gate** is the **authoritative ignore enforcement point** (P8-A6): every inbound channel/tell frame passes the receiver's ignore list before render — a **single funnel** so a new comms path inherits it. AFK auto-reply/marker. | Toggling `gossip` off stops its lines (gate unsubscribes); an **ignored sender's channel line AND tell are both dropped at the receiver** (P8-A6); a **new comms frame type is also ignore-filtered** (the funnel, not per-path); AFK marks the player in `who` and auto-replies a tell. | **toggle-unsubscribe test**; **receiver-side-ignore funnel test (channel + tell + a synthetic new type) (P8-A6, security)**; AFK test. |
| **8.7 — Mail (Postgres durable inbox) + comms-state persistence** | The `mail` table (P8-D6) + send/list/read/delete CRUD on the existing store pool (optional/never-fatal: no Postgres ⇒ mail disabled); a "new mail" notify over the comms bus when online. The **comms-state subtree** (P8-D7) round-trips through `StateJSON` on the existing save cadence/ladder (channels-on, ignore list, AFK) — survives logout/login and a crash-rehydrate. | Sending mail to an offline player, who reads it on login; mail list/read/delete works; a "new mail" ping reaches an online recipient; the comms-state subtree (channel toggles + ignore list) **survives logout/login and a crash-rehydrate**; no Postgres ⇒ mail cleanly disabled (not a crash). | mail send/read/delete round-trip test; offline-mail-on-login test; **comms-state-survives-restart test (persistence)**; mail-disabled-without-postgres test; new-mail-notify test. |

**Adjustment / justification.** 8.1–8.2 land the **transport + the topology** first so the trust/
ordering boundary (the publish ACL, the per-author sequence, the gate-subscription-vs-handoff
lifecycle) is reviewed **before** channels/tells/who hang off it — the central P8-D1 proof (handoff
comms-transparency) is a 8.2 done-when, not an afterthought. 8.3 (channels) is the first user-visible
feature and completes **half** the phase done-when (cross-shard channel chat). 8.4 (presence/who)
completes the **other half** (cross-shard `who`) and demonstrates the crashed-shard age-out failure
mode. 8.5 (tells incl. offline JetStream) is the at-least-once/idempotency-heaviest slice and lands
after the bus is proven. 8.6 (toggles/ignore) layers the receiver-side enforcement funnel. 8.7 (mail +
state persistence) is last (it depends on the comms paths + the store). **If 8.5 proves large**, split
online-tell (core) from offline-durable-tell (JetStream) into two commits — the JetStream
idempotency/redelivery machinery is the heavier, security-reviewed half.

---

## 5. Schema / loader / proto touchpoints

- **`internal/commbus/` (new package)** — the comms `Bus` interface + NATS impl + `MemBus` + mem-
  JetStream stand-in (8.1). Mirrors `internal/contentbus/` structurally; **does not** import or widen
  it. Subject root `telos.comms.*`; **gates subscribe-only, worlds publish-only** on chan/tell subjects.
- **`channel_def` content table + loader mapping (8.3)** — a new definition kind (ref, name, verb(s),
  color/format template, `access` predicate, `default_on`, `history`), parsed by the content mapper
  like the other def tables, and a new **`channel` content-bus invalidation kind** so channel edits
  hot-reload (the Phase-4 pattern). Empty packs ⇒ no channels.
- **`mail` table (Postgres, 8.7)** — `to_player`, `from_player`, `subject`, `body`, `sent_at`,
  `read_at`; CRUD on the existing `internal/store` pool; optional/never-fatal (PERSISTENCE.md already
  lists `mail` under the durable tier).
- **`StateJSON` comms subtree (8.7, P8-D7)** — a new **data-only** field on `character.go`'s
  `StateJSON` (the `Script`-subtree precedent): channel toggles, ignore list, AFK. Saved on the
  existing cadence; loaded on login; size-guarded. Pre-8.7 saves load with none (the established
  backward-compat default).
- **Redis presence keys (8.4)** — `presence:<playerId>` with a TTL, written by the resident shard,
  read by `who`. Operational/ephemeral (PERSISTENCE.md's Redis tier already names `presence`).
- **JetStream streams (8.5)** — `COMMS_TELL` (subject `telos.comms.dtell.<target>`), with a per-player
  durable consumer, `Nats-Msg-Id` dedup, bounded redelivery; the delivered-cursor (per-player; placement
  in character state vs Redis is a persistence-reviewer call — OQ-4).
- **New gate↔world Play frames?** — **none required for v1.** Comms output is the existing
  `ServerFrame_Output` text frame (the gate renders the channel format). Comms *input* is ordinary
  player input (the existing `ClientFrame_Input`) parsed by world command handlers. (Phase 9 adds the
  structured `Comm.Channel.Text` GMCP package — a *new* emit, reserved, not Phase 8.) The only new wire
  is the **comms bus** (NATS), not the Play stream.
- **`cmd/telos-world` / `cmd/telos-gate` wiring** — connect the comms bus from `cfg.NATS.URL` (the
  existing config), optional/never-fatal (mirror `openContentBus`). The gate gains a comms-bus
  connection (it is now a comms subscriber, P8-D1-B) — a new dependency the gate did not have (today
  the gate only knows the directory + the Play pool).

---

## 6. Open questions for sign-off — RESOLVED (owner sign-off 2026-06-28)

1. **OQ-1 — Tells: DECIDED → durable-always.** Every tell is a JetStream message; online delivery is
   just a fast durable consumer. This eliminates the online→offline logout race and the dual code path
   (correctness over the lighter-JetStream split). **P8-D5 / slice 8.5 adopt durable-always** — there is
   no separate NATS-core online-tell path; "online" is simply the durable consumer being live. The 8.5
   split-if-large note (core vs durable) is therefore moot; if 8.5 is large, split the
   send/publish path from the login-drain/cursor path instead.
2. **OQ-2 — Presence TTL: ACCEPTED rec.** Heartbeat well within the directory's ~15s lease cadence;
   crashed-shard age-out ≤ ~30s.
3. **OQ-3 — ACCEPTED rec:** per-channel subscribe (the broker does the fan-out cut); revisit only if
   toggle-churn subscription cost dominates under load.
4. **OQ-4 — ACCEPTED rec:** the **world** drains the offline backlog on login and emits via the same
   source path (the gate stays a pure sink); the per-player delivered-cursor lives in **character `state`
   JSONB** (durable with the character, rides the ladder). Persistence-reviewer to confirm at slice 8.5/8.7.
5. **OQ-5 — ACCEPTED rec:** key comms identity on the directory's player id today (the stub login name
   until Phase 14 auth); accept the Phase-14 migration for block lists / friends.

---

## 7. The Phase-8 ↔ Phase-10 boundary (read this before conflating the two buses)

Phase 8 and Phase 10 both put a bus over NATS. They are **deliberately separate**:

| | Phase 8 comms bus (`internal/commbus`) | Phase 10 world-event bus (WORLD-EVENTS.md) |
|---|---|---|
| **Carries** | player-authored chat, directed tells, presence, mail notifies | world-state consequences (a boss death → region change), `signal_*`, remote effect commands |
| **Endpoints** | **gate ↔ world** (gate=sink, world=source) | **zone ↔ zone / director** (single-writer scopes) |
| **Owner** | the connection (gate) + the source world | the **director** tier (leader-elected) + zone actors |
| **Scopes** | per-player / per-channel | region / world (supra-zone) |
| **Durability** | core for channels, JetStream for offline tells, Postgres for mail | `transient` (core) + `durable` (JetStream, idempotent, ordered) per the scoped design |
| **Ordering** | per-author / per-subject | per-scope (single-writer per scope) |

They **may share the NATS server and JetStream**, but **not the subject space, the payloads, or the
code**. A Phase-8 channel line is *not* a world event; a Phase-10 region event is *not* chat. Building
comms on the (not-yet-existing) Phase-10 bus, or vice versa, would entangle a player-facing chat layer
with a single-writer world-state layer — different invariants, different failure modes, different
owners. Phase 8 ships the **player-presence/channel layer only**; Phase 10 extends the Phase-6 in-zone
event bus to cross-zone scopes. The `Bus`-interface boundary in 8.1 is exactly what keeps them
swappable and separable.

---

## 8. Done-when (the phase capstone)

The ROADMAP Phase 8 done-when, made concrete on this plan:

1. **Cross-shard channel chat** — two players on **different shards** (A and B) both `gossip` and each
   sees the other's lines, rendered with the content channel's color/format (8.3). A pack with **no**
   `channel_defs` has **no** channels and `empty_world_test.go` stays green.
2. **Cross-shard `who`** — those two players, on different shards, **see each other in `who`** (8.4).
3. **Crashed-shard presence age-out** — when shard B crashes, its players **disappear from `who`** after
   the presence TTL, with no explicit cleanup (the self-healing failure-mode demonstration, 8.4).
4. **Offline tell, delivered exactly once** — a `tell` to an offline player is **persisted (JetStream)
   and delivered on next login**, and a redelivery (reconnect/ack-loss) **renders once** (8.5).

And the abuse/safety capstone, demonstrated under test: a **flooding** player is rate-limited without
degrading others (P8-A1); an **impersonation** attempt cannot change a rendered author (P8-A2, the
world-is-source publish ACL); an **ignored** sender's channel line and tell are both dropped at the
receiver gate (P8-A6); **text injection** (ANSI/IAC/`$`) renders literally and cannot forge a channel
prefix (P8-A7); a **handoff is comms-transparent** (a player keeps receiving channel lines across an
A→B walk, P8-D1); and **NATS down** degrades comms to a clean "temporarily offline," never a crash.

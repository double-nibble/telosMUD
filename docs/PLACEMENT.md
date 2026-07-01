# Dynamic zone placement & fault tolerance

How zones are assigned to world servers so the fleet can scale in/out and survive
server failure without operator intervention. This is the **policy** layer on top of
the directory **mechanism** (zone leases).

`internal/placement` holds the two pure cores: `ClaimFromPool` for decentralized
liveness and `Plan` / `PlanWeighted` for the balance decision. A `telos-world` server
CLAIMS its zones from the pool at boot and runs as a STANDBY if it wins none;
`telos-director` (the leader) observes the live fleet via `directory.ListShards` and
`Plan`s the desired rebalancing. The graceful zone drain executes over the cross-shard
handoff fanned across the whole zone (`Shard.BeginDrain` → `drainZone` → the destination's
`AdoptZone`), so a rolling redeploy or planned move migrates every live player in a zone
with the socket held open (zero drop).

Prior art: this is **Akka Cluster Sharding** (a singleton `ShardCoordinator` allocates
shards to nodes and rebalances; nodes host the entities; entities recover from
persistence on rebalance/crash). Also Microsoft Orleans (silo membership + grain
directory + reactivation from storage), the Kubernetes scheduler (control plane assigns
pods to nodes; kubelets reconcile; etcd is truth), Vitess resharding, and the lessons
from Kafka consumer-group rebalancing (why the *policy* should be coordinated, not
purely client-side).

---

## 1. What changes, and what doesn't

Unchanged: platform admins configure the full **list of zones**, and launch world
servers. The gate still discovers a zone's owner via `zone → shard-id → endpoint`, and
cross-shard movement still uses the handoff. None of that changes.

Changes: a world server no longer **declares** a fixed zone (`TELOS_ZONES`). It **claims**
zones from the shared pool, and the set of zones a server owns is dynamic — it grows as
the fleet shrinks, shrinks as the fleet grows, and is rebalanced toward an even spread.

---

## 2. Control plane / data plane split

Three roles, mapping onto pieces that mostly already exist:

| Role | Who | Responsibility |
|---|---|---|
| **Control plane** | `telos-director` (leader-elected) | Watch live servers + the zone list; compute the desired assignment; drive rebalancing, standby promotion, and failover **policy**. |
| **Source of truth** | the directory (Redis) | The `zone → shard-id` leases and `shard-id → endpoint` registrations. Read/written directly by servers and gates. |
| **Data plane** | world servers | Claim assigned zones, run the simulation, **fence** on lease loss. |

This is the standard "control plane writes desired state, data plane reconciles" pattern
(k8s scheduler ↔ kubelet ↔ etcd). The placement coordinator is logically the
**"zone coordinator"** role; it is **hosted in the director tier** (co-located with the
director's leader election and supra-zone authority), exactly as Akka co-locates the
`ShardCoordinator` as a cluster singleton.

### Where it lives: core framework, NOT external

Placement belongs in the **core framework (the director)**, not an external k8s operator,
because it is tightly coupled to **live player migration** (draining players via the
handoff) — which is application logic an external orchestrator cannot perform. The clean
division of labor:

- **Kubernetes** (external) manages the **process fleet**: starts/stops world-server pods,
  restarts crashed ones, scales replica count.
- **The director** (internal) manages **zone → server placement** *within* that fleet,
  and the live-player migration that placement changes require.

---

## 3. Liveness vs. balance — the fault-tolerance win

The single most important design choice: **separate "every zone is owned by someone"
(liveness) from "zones are evenly spread" (balance)**, because it keeps the director off
the critical path for availability.

- **Liveness = decentralized, via the lease.** When a zone's lease expires (its owner
  died), *any* world server or standby can claim the orphan. The directory CAS serializes
  the race — exactly one wins — and jittered retries avoid a thundering herd. **This works
  with no director running.** The mechanism already exists (`ClaimZone`, the TTL lease, and
  `renewZoneLease`'s fence-on-loss).

- **Balance = the director optimizes.** The director nudges the system toward
  `floor(Z/S)` zones per server (with `Z mod S` servers carrying one extra) by directing
  graceful drains. If the director is down, zones are still claimed and served — just
  possibly unbalanced — until it recovers and tidies up.

So the director is an **optimizer, not a dependency**. A crashed server's zones get
re-owned with or without it. **Split-brain is a non-issue**: the lease CAS is the final
arbiter, so even a buggy or duplicated director can never make two servers own one zone
(it could at worst issue churny drain instructions, bounded by rebalance hysteresis §7).

---

## 4. Assignment lifecycle

- **First server** claims **all** zones (it's the only owner available).
- **Each new server** claims currently-unclaimed zones on boot; once none are free, the
  director rebalances by draining a share off the busiest servers to the newcomer.
- **A server with no zones** (fleet already saturated) becomes a **standby**: registered
  and heartbeating, holding no leases, ready to take a zone on the next failure. Standbys
  give *fast* failover (warm capacity, no cold start).
- **Scale-down / drain**: a server being retired drains its zones (graceful move, §5) to
  the rest of the fleet before exiting.

---

## 5. Failure & migration — the hard part

Moving a zone means moving the **live players** in it. Two cases with very different
answers:

1. **Graceful move** (rebalance, planned scale-down): the source is **alive**, so it does
   an orderly **zone drain** — freeze every player in the zone, snapshot each, hand them
   off to the new owner, gate re-dials. This is the per-player cross-shard handoff,
   **fanned out over a whole zone** (`Shard.BeginDrain`). Seamless to players.

2. **Crash** (source is **dead**): there is no source to snapshot from, so the zone's
   in-memory state is lost **unless it was persisted**. The new owner **rehydrates each
   player from their last checkpoint** (Postgres/Redis — the durability ladder);
   players lose only the last few seconds of un-checkpointed transient state, and their
   gate stream breaks so they reconnect (the directory now points at the new owner). This
   is textbook actor recovery (Akka Persistence / Orleans reactivation from storage).

**Consequence:** crash-failover of live players is **not fully solvable without
persistence** — the graceful path (§5.1) needs only the handoff, but the crash path (§5.2)
needs the durability ladder to reconstruct a player the new owner never saw.

---

## 6. The persistence dependency

The graceful path (§5.1) rides the handoff alone. The crash path (§5.2) rides the
**durability ladder** (memory → Redis checkpoint → Postgres) and `state_version`
(PERSISTENCE.md) so a *new* owner can reconstruct a player it never saw. The directory
abstraction keeps placement decoupled from gate/handoff code: a zone moves by rewriting one
lease, and no gate or peer names a shard address.

---

## 7. Sharp edges

- **Balance by *load*, not zone count.** `Z/S` assumes zones are equal-cost, but a newbie
  town ≫ an empty wilderness. The planner (`PlanWeighted`) balances by per-zone weight; the
  live occupancy signal that supplies those weights is the remaining piece.
- **Locality vs. balance tension.** ARCHITECTURE.md §4 wants *adjacent* zones colocated so
  common room-to-room movement is an in-process channel send, not a network handoff. A
  naive even-count balancer splits neighbors across servers and turns cheap moves into
  handoffs. The balancer must be locality-aware — which fights perfect balance. A real
  trade-off, not a bug.
- **Rebalance hysteresis.** Moving a zone is expensive (it drains players). Rebalancing
  past an imbalance threshold, with cooldowns, keeps the fleet from thrashing. (Akka exposes
  exactly these knobs.)
- **Claim storm on failure.** When a lease expires, jittered/randomized claim retries plus
  the CAS (one winner) prevent a thundering herd; the director can also arbitrate by
  directing the claim to a chosen standby.

---

## 8. Summary

Director-as-shard-coordinator (control plane) + the directory leases (truth) +
decentralized lease-claim for liveness (so availability never depends on the director),
with graceful drains for planned moves and persistence-backed rehydration for crashes.
It reuses the directory, the handoff, and the durability ladder, degrades gracefully under
every failure mode, and is the Akka Cluster Sharding pattern adapted to TelosMUD.

# Phase 7 — Lua scripting (the curated escape hatch + sandbox) — IMPLEMENTATION PLAN

Status: **proposal / planning** — slices the existing [LUA.md](LUA.md) design (one VM/zone,
handle-not-pointer API, restricted-globals sandbox + instruction-budget + circuit-breaker,
`self.state` persistence, hot reload, per-zone-RNG determinism). The design is the baseline; this
plan **orders** it into shippable slices, foregrounds the **sandbox threat model** (the sharpest
trust boundary in the engine), and **resolves the three open design forks** LUA.md §10 flags but
never wrote. Confirm §1 + §3 before slice 7.1.

Lua is content's escape hatch for the complex ~20% the declarative op-list can't express
([PRINCIPLES.md](PRINCIPLES.md): engine = mechanism, content = flavor; and the second pillar — every
action is hookable, Phase 7 makes the hook *bodies* arbitrary). **Done when** (ROADMAP Phase 7): a
room script greets on entry, a scripted mob reacts to speech, a Lua Counterspell cancels an in-flight
cast, and a pack defines/fires/handles a custom event the engine never heard of — **all edited live,
none able to crash, stall, or cross a zone.**

This phase builds the **Lua runtime** on the Phase 6 substrate (the in-zone event bus, the effect-op
interpreter + the shared `guardHarmful`/`dealDamage`/`applyDebuff` harm funnels, the per-zone pulse
scheduler, the per-zone seeded RNG, the `state` JSONB ladder, the hot-reload applier). It does **not**
build: the cross-zone scoped + durable event bus (Phase 10), GMCP structured emit (Phase 9),
progression/chargen/the track grants Lua will compose (Phase 11), or any new concurrency.

---

## 0. Where Phase 7 sits on the existing code

| Existing (Phase 3–6) | Phase 7 change |
|---|---|
| `zone.go` — the single-goroutine actor (`Run` serial inbox loop + pulse ticker; `dispatchSafe`/`handle` panic nets) | Each zone gains **one `*lua.LState`**, constructed at zone build, called **only** from `Run`'s goroutine. No new goroutine, no lock — the VM rides the existing single-writer invariant. |
| `effect_op.go runOps` + the registered op table; the **one** `guardHarmful`/`guardCrossPlayerWrite` chokepoint; `dealDamage`/`applyDebuff` shared funnels | Lua effect-op handles (`h:damage{}`, `h:apply_affect{}`, …) **call the same Go funnels** — no parallel harm path. A Lua op physically cannot reach a protected player except through `guardHarmful` (the can't-bypass property extended to the Lua surface). |
| `event.go` — the in-zone bus, `knownEventKinds` closed set, `gatherEventHandlers`, `fireEvent` with `maxEventDepth`/`maxEventHandlers` guards | Handlers may now be **Lua bodies** (not just op-lists); the bus grows a **`pack:event` lane** (builder-defined kinds) **and** lights the reserved engine kinds (`OnApplyAffect`/`OnAffectTick`/`OnAffectExpire`, a new `OnEnter`). Lua handlers run under the **same** depth/width budget. |
| `defs.go` reserved Lua fields — `affectDef.onApply`/`onExpire` (read-not-run), `abilityDef.onResolveLua` (read-not-run); `ability_build.go` carries `OnResolveLua` but never executes | Those reserved columns become **live**: compiled to a Lua chunk at content build, invoked through the sandbox + `pcall` + budget. |
| `check.go` — the check primitive + `OnCheck` fire; `formula.go` — the prefix-AST evaluator | The `pvp_allowed` policy hook + ruleset formulas (`to_hit`/`soak`/`regen`/`xp_for`) gain a **Lua alternative** to the prefix-AST (a pack picks data formulas OR a Lua function — never both for one ref). |
| `character.go` `StateJSON` + the save cadence (`dumpCharacter`/`loadCharacter`); the durability ladder | `self.state` is a **data-only** subtree mirrored into `StateJSON.Script` (new field), serialized on the same cadence, size-guarded. No code, no handles, no closures persist. |
| `reload.go` — the hot-reload applier (atomic prototype swap on a `(kind, ref)` content-bus invalidation) | Hot reload also **recompiles the Lua chunk** and **swaps the registered handlers**; `self.state` data survives (it's not code); a generation tag drops stale `mud.after` callbacks. Rides the **existing** invalidation path. |
| `pulse.go` — the per-zone timer wheel (`after`/`every`, resolve-by-id-or-cancel) | `mud.after(pulses, fn)` schedules on **this** wheel — never a real sleep, never a goroutine. The callback runs on the zone goroutine; a generation tag drops callbacks bound to a reloaded chunk. |
| `identity.go` — `RuntimeID` (per-zone uint64) + the target-resolution by RID | Lua **handles** wrap `(RuntimeID, zone)` as validated userdata with a `__tostring` metamethod (never the raw Go pointer — T15); every method re-resolves the entity **still exists and is in this zone** before acting (LUA.md §4). No `*Entity` ever reaches Lua. |

> **Doc-correction note:** [LUA.md](LUA.md) §5 cites `SetMaxStackSize` and "instruction budget via the
> LState context" — **both are wrong for gopher-lua v1.1.1** (verified by the security-auditor's probes
> against the real runtime, 2026-06). `SetMaxStackSize` does not exist (the control is the constructor
> `lua.Options{CallStackSize, RegistrySize/RegistryMaxSize}` — §1.1/T4); the `LState` context bounds
> wall-clock **between** ops only, never instruction count and never inside a Go builtin (P7-D6/T3/T13).
> This plan supersedes LUA.md §5 on both points; LUA.md should be amended when this plan is signed off.

The riskiest *structural* points: (a) **the sandbox is the sharpest trust boundary in the engine** —
builders run arbitrary Lua **in-process, on the zone goroutine** (§ Sandbox threat model — written to
be reviewed by the security-auditor); (b) **the harm-gate must funnel** — a Lua effect op is the
newest harm-injection surface since the gate was built, and like Phase 6's event handlers it must
route the same `guardHarmful` with no second path (§4); (c) **memory** — gopher-lua has no hard
per-VM cap, an acknowledged limitation we bound indirectly, not silently (§ threat model, M-row).

---

## 1. Tech / design decisions (confirm before slice 7.1)

| # | Decision | Recommendation | Trade-off |
|---|----------|----------------|-----------|
| **P7-D1** | **Runtime + VM granularity** (settled — LUA.md §1) | `github.com/yuin/gopher-lua` (already a transitive dep — promote to direct), **one `*lua.LState` per zone**, called only from `Zone.Run`'s goroutine. Per-script sandboxed `_ENV`. | Memory/perf amortized per zone; isolation between zones is automatic (a script can only reach its own zone). Cost: a zone-scoped VM lifecycle wired into build/teardown. The actor model already gives us lock-free single-writer — the VM rides it. |
| **P7-D2** | **API shape: curated handles, not reflection** (settled — LUA.md §4) | A **curated, hand-written binding surface** (handle userdata + a `mud` table) — **never** `gopher-luar`, never a raw `*Entity`. A handle wraps `(RuntimeID, zone-generation)`; every method re-validates in Go. | Content depends on the API, not Go struct layout (refactor-safe); no dangling pointers; no cross-zone reach. Cost: every exposed capability is hand-bound (a feature, not a bug — the API surface *is* the audit surface). |
| **P7-D3** | **Harm funnels reuse, never duplicate** (settled — the Phase 6 boundary) | Every Lua effect op routes the **existing** `dealDamage`/`applyDebuff`/`guardCrossPlayerWrite` — which call `guardHarmful` first. No Lua-specific harm path. A Lua handler on the bus is **not** a gate bypass, exactly like a declarative op-list handler. **Five `effectCtx`-binding invariants (the binding's single most security-sensitive code; refs effect_op.go — effectCtx:38, guardHarmful:252, guardCrossPlayerWrite:293, dealDamage:340, applyDebuff:448):** (1) **actor/source/target are ENGINE-resolved** from the handle's `(rid,zone,gen)` + the invocation context — **never script-supplied** (no `h:apply_affect{source=arbitrary}` attribution-spoofing); (2) **`disp` is engine-set** from the op/def — a script cannot set it helpful to skip the gate; (3) the funnels are the **ONLY write path** — the T8 audit (below) is a **build-failing lint**, not a grep; (4) **`rng` is always** the ctx/zone RNG (P7-D4); (5) **`depth`/`eventBudget` are threaded** from the invoking cascade, **never reset**. | The can't-forget property the security-auditor already trusts extends to Lua for free — **provided the five invariants hold**: each is a way the binding could *silently* re-open the gate the funnel closes. |
| **P7-D4** | **Determinism: the per-zone engine RNG only** (settled — LUA.md §9) | `math.random` is **rebound** to the per-zone seeded RNG; `mud.random`/`mud.roll` draw the same source. **No** `os.time`, `os.clock`, no Lua RNG state, no other entropy. `mud.now()` returns the zone pulse counter (deterministic), not wall-clock. | Combat/loot/procs stay reproducible in tests + replays; a script cannot be a non-determinism injection vector. Cost: the binding must thread the ctx RNG into every Lua-reachable random draw (the `effectCtx.rng` seam already exists). |
| **P7-D5** | **`self.state` is data-only, size-guarded** (settled — LUA.md §7, + the new guard) | `self.state` is a plain Lua table mirrored to/from `StateJSON.Script` JSONB: numbers/strings/booleans/nested tables of those **only**. No functions/closures/userdata/handles (store `h:id()`, re-resolve). A **byte-size + depth + key-count cap** on the marshalled subtree (state-injection bound — § threat model). | Script memory rides the normal durability ladder; a runaway `self.state` can't balloon the snapshot or the VM. Cost: a Lua-table↔JSON marshaller with the type allowlist + the caps, run at save time on the zone goroutine. |
| **P7-D6 (RESOLVED — LUA.md §10 fork 1; USER DECISION 2026-06)** | **Per-call budget: how is the instruction/wall-clock limit enforced, and what are the defaults?** | **THREE layers (user-decided): (1) a vendored gopher-lua fork** adding an instruction-count abort in `mainLoopWithContext` beside the existing `ctx.Done()` select (gopher-lua v1.1.1 has **no** `SetHook`/`MaskCount`/debug-hook — the count must come from the fork); **(2) the `LState` context wall-clock deadline** (`SetContext`+`context.WithTimeout`, armed fresh per call — §4 chokepoint invariant); **(3) capped amplifier builtins** (T13 — the deadline checks between ops, never inside a Go builtin, so `string.rep`/`format`/`gsub`/`table.concat` ship as size-capped wrappers). Default: **deadline = 5ms wall-clock, budget = 100k VM instructions per entry-point call**, both tunable per pack and overridable per def; the fork is pinned + documented. *(7.1 review note: 100k is TIGHT — a plain 50k-iteration arithmetic loop hits it; the rpg-systems-designer acceptance pass must validate the default against real content loops/formula tables and raise it if surprisingly low — it's tunable, not a safety knob.)* | The three layers are complementary, not redundant: the **count** is deterministic + test-reproducible (a tight pure-CPU loop trips it identically every run, unlike wall-clock); the **clock** catches a low-instruction stall (a GC pause, a slow C-side call); the **builtin caps** catch a single-op bomb (`string.rep("A", 2e9)` — one instruction, GB allocated — that neither the count nor the clock can stop, T13). Cost: vendoring + a pinned fork to maintain; the count check adds per-N-instruction overhead (granularity ~1k ops, <2%). **Resolved below (§ open forks); vendoring is slice 7.1's first work item.** **security-auditor must review the fork's abort path + the builtin caps.** |
| **P7-D7 (OPEN — LUA.md §10 fork 2)** | **Hot-reload of in-flight `mud.after` callbacks bound to a now-swapped chunk: complete or drop?** | **DROP by generation tag** (the configurable LUA.md §8 default made concrete). Each compiled chunk carries a monotonic `gen`; `mud.after` captures the gen at schedule time; on fire the wheel skips a callback whose gen != the def's current gen. A pack may opt a specific timer into *complete-anyway* (`mud.after{durable=true}`) for a state-cleanup finalizer. | A live edit must not run **old code** against **new state** (the subtle reload-corruption class); dropping is the safe default. Cost: a gen field on the chunk + the timer; the rare legitimate finalizer gets the opt-in. **Resolved below.** |
| **P7-D8 (OPEN — LUA.md §10 fork 3)** | **Result-altering reactions (Counterspell/Shield/concentration): how does a Lua hook reach INTO an in-flight ability to alter/veto it, within the single-writer model?** | A **reaction context object** passed to the `BeforeCastCommit`/`OnDamageTaken`/check checkpoint hooks (the Phase 6 named checkpoints, already designed-in): `rx:cancel()`, `rx:modify(field, delta)`, `rx:replace_target(h)`, `rx:consume_resource(ref, n)`. **Three hardening invariants (security):** (1) `field` is a **closed per-checkpoint enum resolved by a Go switch** — to-hit allows only `{"ac"}`, `OnDamageTaken` only `{"amount"}` — **never a string indexing an attribute map**; (2) `rx:replace_target(h)` **re-runs `guardHarmful` against the new target** (the original gate ran against the original target — replacing onto a non-consenting player otherwise bypasses it); (3) the reaction path threads the **same `eventBudget` pointer** (effect_op.go:56) so a reaction→checkpoint→reaction loop is bounded by the shared width cap + the depth cap. The engine fires the checkpoint, runs the Lua hook **synchronously inline**, then **re-reads** the reaction object's recorded mutations and applies them at the seam — the **observe-then-recheck** shape the death checkpoint already implements (PRINCIPLES.md). The hook cannot reach past the fields the checkpoint exposes. | Counterspell (`rx:cancel()` on `BeforeCastCommit` if the caster spends a slot + wins a check), Shield (`rx:modify("ac", +5)` on the to-hit checkpoint), concentration (`rx:cancel()` of the concentration affect on a failed `OnDamageTaken` save) all express **without pipeline surgery** — Phase 6 designed the checkpoints, Phase 7 only adds the alter-capable hook bodies. Cost: each checkpoint must publish a typed, bounded reaction object (not a raw pipeline pointer) + the per-checkpoint field enum. **Resolved below.** **security-auditor reviews the mutation allowlist + the re-gate.** |
| **P7-D9** | **Gating: who may author Lua?** (settled — LUA.md preamble) | Lua is **gated to reviewed authors** (a pack-level `lua_trusted` flag content-side); the **sandbox is defense-in-depth regardless** — it must hold even against a hostile author, because the gate is policy and the sandbox is mechanism. | The threat model assumes a hostile author (§) even though policy restricts authoring — the engine never relies on the gate for safety. Cost: none structural; it shapes the threat model's adversary. |
| **P7-D10** | **`pcall` isolation + the circuit breaker** (settled — LUA.md §6, + two hardening calls) | Every entry point is invoked through Go-side `pcall`. A failure **fails just that action**, logs `(zone, kind, ref, stack)`, and increments the script's **error budget**; repeated failures **trip a breaker** that disables *that script* (not the zone), alerts ops, and re-enables on the next successful hot-reload. The player-facing fizzle message is **generic — never the raw Lua error/stack** (that goes to ops logs only — T11/T15). **Two hardening calls:** (a) **breaker scope** — a breaker keyed per-`(kind, ref)` over a **SHARED def** (one ability/affect used by many entities) trips **content-wide**, so a hostile shared def is a content-wide DoS; **recommendation: per-instance breaker for entity-scoped scripts (triggers/`self.state`-bearing defs), per-`(kind,ref)` for genuinely shared defs, and the shared-def blast radius documented**; (b) **separate accounting** — **wall-clock-deadline aborts are weighted/rate-limited DIFFERENTLY from deterministic logic errors**, so a deadline trip under load (a GC pause) doesn't quarantine a correct script, and an attacker can't drive a victim's breaker by inducing latency. | No script can crash a zone; a chronically-broken script is quarantined, not the world — **without** a latency-induced false quarantine or a shared-def cross-content trip being a silent surprise. Cost: a two-mode breaker + the deadline-vs-error split. |

### 1.1 The compiled-chunk lifecycle (P7-D1/D7, the spine)

A scripted def (`ability_def.on_resolve_lua`, an `affect_def.on_apply`/`on_tick`/`on_expire`, a
room/mob/item trigger block, a custom command, a formula, the `pvp_allowed` policy) carries a **Lua
source string** in content. At content build (and hot reload) the source is **compiled once** into a
`*lua.FunctionProto` (the reusable bytecode) tagged with a monotonic **generation**. At invocation the
engine instantiates the proto into the zone's `LState` under a **fresh sandboxed `_ENV`** (so one
script can't clobber another's globals in the shared VM), binds `self`/`ctx`/`ev`/`rx` as appropriate,
and calls it through `pcall` + the budget. The proto + gen live in the per-shard registry beside the
prototype it belongs to; the reloader recompiles + bumps the gen and swaps it via the **existing**
atomic registry swap (`reload.go`). Compilation failures are non-fatal (the def keeps its last-good
proto, like the prototype reloader keeps the last-known on a re-read error).

**The fresh-`_ENV`-per-call claim is INCOMPLETE on its own (T14).** A per-call `_ENV` isolates a
script's **globals**, but it does **not** cover **`L.G`-scoped** (VM-global) state shared across every
`_ENV` in the zone. The probed escape: `getmetatable("")` returns the **string library module itself**
(it lives on `L.G.builtinMts`, is VM-global, and is **writable**); a script doing
`getmetatable("").rep = evil` poisons `("x"):rep()` for **every** other script in the zone — including
trusted policy/formula chunks — and the poison **survives the per-call `_ENV` reset**. So isolation
requires, additionally (slice 7.1, T14): **(a)** at VM build, point `L.G.builtinMts[LTString]` at a
**private, engine-owned table holding the T13 capped wrappers, never exposed as a script-reachable
global** — so no Lua value references a mutable shared table at all (a script needing the
`string.`-namespaced form gets a separate **read-only proxy**, not the live table); and **(b) never
register `getmetatable`/`setmetatable`** (closing the `getmetatable("")` reach). NOTE a plain
read-only-`__index` / write-block on the metatable is **insufficient**: in Lua 5.1 `__newindex` fires
only for *absent* keys, so overwriting an existing `rep`/`gsub` is a raw set it never intercepts, and
method syntax `("x"):rep()` resolves through the shared `builtinMts[LTString]` table regardless of
`_ENV`. The robust fix is the unreachable engine-owned table, not a guard on a still-reachable one.
Fresh-`_ENV` + an unreachable-immutable `L.G` shared table together give the isolation §1.1 needs;
fresh-`_ENV` alone does not.

### 1.2 The handle userdata layer (P7-D2, the no-dangling/no-cross-zone guarantee)

A handle is a `*lua.LUserData` wrapping a small Go struct `{rid RuntimeID, zone *Zone, zoneGen uint64}`
with a metatable of curated methods. **Every method first re-resolves** `rid` → `*Entity` in `zone`
(the existing per-zone RID lookup): if the entity no longer exists, left the zone, or the zone changed
generation, the method is a **safe no-op** returning `nil`/`false` — never a panic, never a stale
pointer (LUA.md §4). The `*Entity` is fetched, used, and dropped **within the single Go method call**;
it never lives in a Lua value across calls. A handle for an entity in **another** zone is invalid here
(the zone pointer mismatch / RID-not-found) — cross-zone interaction must go through engine-mediated
events, preserving the single-writer invariant. This is the structural enforcement of "no script can
reach another zone."

---

## 2. The sandbox threat model (the sharpest trust boundary — written for security-auditor review)

Builders/content authors run **arbitrary Lua in-process, on the zone goroutine**. Even with authoring
gated to reviewed authors (P7-D9), the sandbox is **defense-in-depth that must hold against a hostile
author** — the engine never relies on the gate for safety. This section enumerates the attack surface,
the invariant each carries, how it is enforced, and how it is tested. **Every slice that adds surface
must carry its row's mitigation; no slice ships a capability without its gate.**

| # | Attack surface | Invariant | Enforced by | Tested by |
|---|----------------|-----------|-------------|-----------|
| **T1** | **Code loading / FFI / dynamic eval** — `load`/`loadstring`/`dofile`/`loadfile`/`require`/`package`, any FFI. | A script cannot load or eval new code, link a C library, or escape the bytecode it was compiled from. | The VM is built with a **restricted global table** (LUA.md §5): these globals are **never registered** (we build `_ENV` from an allowlist, not by deleting from the stdlib — deletion can be defeated by `_G` aliasing; allowlisting is the safe construction). `package`/`require` never exist. (slice 7.1) | A unit test asserts each forbidden global is `nil` in a fresh script `_ENV`; a test that `load("return 1")` errors `attempt to call a nil value`. |
| **T2** | **Filesystem / network / process reach** — `os` (`execute`/`getenv`/`remove`/`exit`), `io` (`open`/`popen`/`lines`), any socket. | A script has **zero** filesystem, network, or process reach; cannot read env, spawn a process, or exit the host. | `os`/`io` are **not in the allowlist** (T1's construction). `os.time`/`os.clock` (entropy/timing) are also gone (P7-D4); `mud.now()` returns the deterministic pulse counter. (slice 7.1) | A test asserts `os == nil and io == nil`; a test that `mud.now()` is the pulse counter, monotonic, not wall-clock. |
| **T3** | **CPU exhaustion (multi-op)** — a tight loop, a pathological pattern over many ops. | A single entry-point call **cannot stall the zone goroutine**; it is bounded in both VM instructions and wall-clock. | The **dual budget** (P7-D6): the **vendored-fork instruction-count abort** (in `mainLoopWithContext`, ~1k granularity — gopher-lua v1.1.1 has **no** `MaskCount`/`SetHook`, so the count comes from the fork, not a debug hook) aborts past the per-call instruction budget; the `LState` **context deadline** aborts past the wall-clock limit. Both raise a Lua error caught by the engine `pcall`, log, and count against the error budget (T11, weighted per P7-D10). **Single-op bombs are NOT covered here — see T13.** (slice 7.1 fork + 7.5) | A test that a `while true do end` body aborts within the deadline and the **zone keeps serving** (a second command on the same zone succeeds after); a test that an instruction-heavy-but-fast loop trips the **count** budget (not the clock); a benchmark asserting fork overhead <2%. |
| **T4** | **Stack / recursion blowout** — deep or infinite Lua recursion overflowing the Go goroutine stack. | Deep recursion **errors out** (a catchable Lua error), never overflows the host goroutine stack (which would crash the process). | The constructor option **`lua.Options{CallStackSize: N}`** caps the Lua call stack (gopher-lua v1.1.1 has **no** `SetMaxStackSize` — LUA.md §5 is wrong; the cap is a build-time `Options` field, paired with `RegistrySize`/`RegistryMaxSize` for the value stack); the overflow is a Lua error caught by `pcall`. (slice 7.1, set at VM build) | A test that a self-recursive Lua function errors cleanly and the zone survives; the recursion does **not** SIGSEGV the test binary; a test that `CallStackSize` is set on the constructed VM. |
| **T5** | **Memory exhaustion (gradual)** — allocating many tables/strings over a call; gopher-lua has **no hard per-VM memory cap** (the acknowledged limitation). | Gradual growth is **bounded indirectly and DETECTED observably** — never a silent gap; a single-op allocation bomb is T13. | Layered: the **instruction budget** (T3) caps total work for **multi-op** growth; **`self.state` size/depth/key caps** (P7-D5) bound persisted growth; the **per-zone VM memory metric is DETECTION-ONLY** (it fires **after** the fact — it cannot prevent an allocation, only alert + inform a kill decision); the **single-op bomb is prevented by the capped builtins (T13)**, not by this row. We document the soft-cap explicitly, not silently. (slice 7.1 caps + 7.5 metric + 7.6 state caps) | A test that a `state` table over the byte/depth/key cap is rejected at save (clean error, no balloon); a metric assertion that VM memory is reported per zone; (the prevention test lives in T13). |
| **T6** | **Zone-goroutine starvation** — blocking, sleeping, or spawning. | A script **never blocks** the single-writer loop: no real sleep, no I/O wait, no goroutine, no channel op. | No blocking primitive is in the allowlist (T1); `mud.after(pulses, fn)` schedules on the **zone timer wheel** (pulse.go), not the OS scheduler, and returns control immediately; there is no `mud.spawn_goroutine`, no `mud.sleep`. A script needing zone-absent data **returns**; the engine fetches async and re-invokes. (slice 7.3 for `mud.after`) | A test that `mud.after` schedules on the wheel and the callback runs on the zone goroutine (not a new goroutine — assert via a goroutine-id capture / no race under `-race`); a test that no sleep primitive exists. |
| **T7** | **Cross-zone reach** — a handle smuggled to act on an entity in another zone, or a handle held across the entity's zone change. | A handle is **invalid outside its zone**; any method on a cross-zone or departed entity is a safe no-op. No `*Entity` ever crosses a goroutine. | The handle re-resolves `(rid, zone, zoneGen)` **in Go on every method** (P7-D2/§1.2); a mismatch (entity gone / moved / zone-gen changed) returns `nil`/`false`. The `*Entity` lives only within the Go method call. (slice 7.2) | A test that a handle for a moved-away entity no-ops; a test that a handle captured in `self.state`-adjacent Lua and reused after the entity left the zone does not act and does not panic; a `-race` test that no method dereferences a foreign-zone entity. |
| **T8** | **Hostility / PvP gate bypass** — a Lua effect op harming a protected player without the gate. | **Every** Lua harm vector funnels the **same** `guardHarmful` — a Lua op is not a gate bypass. The gate is at the op, not the API call site. | Lua effect handles call the **existing** `dealDamage`/`applyDebuff`/`guardCrossPlayerWrite` (P7-D3) — there is no Lua-specific harm path; the binding constructs the `effectCtx` (the **five binding invariants**, P7-D3) and the funnel does the gate, fail-closed on a detached actor/target (effect_op.go guardHarmful:252). The "funnels are the only write path" check is a **build-failing CI lint**: any Lua handle method touching `*Entity.living`/affects/flags outside the funnels (incl. `h:set_flag` + any future direct-mutator on a deny-list) **fails the build** — not a grep that can rot. (slice 7.3c) | A test that a Lua `h:damage{}` against a protected player in a safe room is a **clean no-op** (the existing combat-test pattern, now driven from Lua); a test that a Lua bus handler's harmful op is gated **per target**; a Lua `h:apply_affect{source=arbitrary}` cannot spoof attribution; the **build-failing lint** flags a direct-mutator. |
| **T9** | **Determinism / entropy injection** — a script seeding non-reproducibility (wall-clock, Lua RNG state, goroutine timing). | A script's only randomness is the **per-zone seeded engine RNG**; no other entropy source. | `math.random` rebound + `mud.random`/`mud.roll` draw the ctx RNG; `os.time`/`os.clock` absent (T2); `mud.now()` is the pulse counter (P7-D4). (slice 7.3) | A seeded-zone test that two runs of the same scripted ability produce identical rolls; a test that no wall-clock/entropy primitive is reachable. |
| **T10** | **State injection via `self.state`** — persisting code, handles, or an unbounded blob to corrupt load or balloon the snapshot. | `self.state` is **data-only** and **size-bounded**; loading it can never execute code or resurrect a stale pointer. | The marshaller (P7-D5) allowlists number/string/bool/nested-table **only** (functions/closures/userdata/handles rejected at save) and enforces the byte/depth/key caps; load reconstructs a plain table — never a handle (content stores `h:id()` and re-resolves). (slice 7.6) | A test that a `state` carrying a function/handle is rejected at save with a clean error (not a panic, not a silent drop of the rest); a round-trip test that a nested data table survives save/load identically; a cap-exceeded test. |
| **T11** | **Buggy-script blast radius** — a script that errors (or trips a budget) repeatedly. | One bad script **fails just its action** and, if chronic, **disables itself** — never the zone, never the world; the player **never sees the raw error/stack**. | `pcall` isolation + the **error-budget circuit breaker** (P7-D10): an error/abort fails that action with a **generic player-facing message** (the raw Lua error + stack go to **ops logs only** — T15), logs `(zone,kind,ref,stack)`, increments the budget (deadline aborts weighted separately, P7-D10); tripping it disables the script (per-instance or per-`(kind,ref)`, P7-D10) and alerts ops; reset on the next successful reload. (slice 7.5) | A test that an always-erroring trigger fizzles with a **generic** message (no Lua error text leaked to the player), the zone serves the next command, and after N failures the breaker disables it (and a reload re-enables it); a test that a deadline abort doesn't quarantine as fast as a logic error. |
| **T12** | **Reaction-hook over-reach** — a Lua result-altering reaction reaching past the checkpoint's exposed fields (P7-D8). | A reaction hook can mutate **only** the checkpoint's published, typed fields, **re-gated**, under the **shared cascade budget** — not arbitrary pipeline/engine state. | The reaction context object (P7-D8): `modify(field,…)`'s `field` is a **closed per-checkpoint enum resolved by a Go switch** (never a string indexing an attr map); `replace_target(h)` **re-runs `guardHarmful` against the new target**; the path threads the **same `eventBudget`** (effect_op.go:56) so a reaction loop is width+depth bounded. The engine applies only recorded mutations at the seam (observe-then-recheck); no raw pipeline pointer reaches Lua. (slice 7.9) | A test that a non-allowlisted `modify` field is a no-op; a test that `replace_target` onto a non-consenting player is **gate-blocked**; a test that Counterspell `rx:cancel()` cancels exactly the cast and nothing else; a reaction-loop budget-exhaustion test. |
| **T13** | **Single-builtin alloc/CPU bomb** — `string.rep("A", 2e9)` (GB in ONE instruction, probed), `string.format`/`gsub`/`table.concat` width blowups, pathological `string.find`/`match` backtracking. | A single Go-builtin call **cannot allocate unbounded memory or burn unbounded CPU** in one op. | **The deadline checks BETWEEN bytecode ops, never INSIDE a Go builtin, and the instruction count sees it as ONE op — so neither the clock nor the count catches it (probed false on T5's old claim).** Mitigation: **never expose the raw stdlib amplifiers** — ship **length/width/size-capped wrappers** for `string.rep`/`string.format`/`string.gsub`/`table.concat`, and **guard pathological backtracking** in `string.find`/`match` (cap pattern complexity / input length). (slice 7.1 — the capped builtins ARE part of the allowlist construction) | A test that `string.rep("A", 2e9)` is **rejected at the cap** (clean error, no GB allocation); per-wrapper cap tests (`format`/`gsub`/`concat`); a backtracking-pattern test that a known-pathological `match` is bounded. |
| **T14** | **Shared-`L.G` writable-metatable poison** — `string.rep = evil` (the kept `string` global == `L.G.builtinMts[LTString]`) poisons `("x"):rep()` for **every** script in the zone (incl. trusted policy/formula) via method syntax, regardless of `_ENV`, and **survives the per-call `_ENV` reset** (the §1.1 fresh-`_ENV` claim is incomplete — it doesn't cover `L.G`-scoped state). | The shared `L.G.builtinMts[LTString]` table (what method syntax `("x"):m()` dispatches through) is **engine-owned and unreachable** by any script — no Lua value references a mutable copy of it. | At VM build, set `L.G.builtinMts[LTString]` to a **private engine-owned table** holding the **T13 capped wrappers**, **never exposed as a script-reachable global** (scripts get a separate **read-only proxy** for the `string.` form). **Drop `getmetatable`/`setmetatable`** (closes `getmetatable("")`). A read-only-`__index`/`__newindex` guard ALONE is **insufficient** (Lua-5.1 `__newindex` skips existing-key overwrites; method syntax bypasses `_ENV`) — the table must be *unreachable*, not merely guarded. (slice 7.1) | A test that `getmetatable` is `nil`; the **load-bearing test**: a script doing `string.rep = evil` (and any other reach attempt) **cannot change** what `("x"):rep(2)` returns in a **sibling** script — **cross-script method-syntax invariance**, the path that actually bites. |
| **T15** | **Info leak via `tostring`** — `tostring(userdata)` returns the live Go pointer `0x…` (ASLR defeat); raw Lua errors echoing internals to players. | A script **cannot read a Go pointer** or any host-internal address through a handle; players never see engine internals. | **Every handle metatable defines `__tostring`** returning a safe `<entity #rid>` — **no userdata is ever exposed without one** (the default gopher-lua `tostring(ud)` leaks the pointer). Player-facing fizzle messages are generic (T11); raw errors/stacks go to ops logs only. *(7.1 review: bare `tostring(function)`/`tostring(table)` ALSO leak Go pointers — same ASLR-leak class as handles. 7.5's player-facing value-render path must sanitize any `tostring` output reaching a player, not just handle `__tostring` and error strings.)* (slice 7.2 for handles; 7.5 for messages) | A test that `tostring(self)` is `<entity #rid>`, **never** `0x…`; a test that no handle type lacks `__tostring`; a test that a player-facing error/value-render carries no `0x…` pointer. |

**Construction note (the load-bearing detail for T1/T2/T13/T14):** the sandbox `_ENV` is **built from
an allowlist by registering the kept base functions individually** — **NOT** `lua.OpenBase` and **NOT**
`NewState()`-then-delete. `OpenBase` registers `load`/`loadstring`/`dofile`/`loadfile`/`require`/
`module`/`collectgarbage`/`getmetatable`/`setmetatable`/`rawget`/`rawset`/`rawequal`/`next`/`_G`/
`newproxy` **all at once** — exactly the set we must withhold (T14's `getmetatable`/`setmetatable`
write path among them). Deleting after the fact is defeatable (`_G`/`_ENV` aliasing, a kept function
re-exposing a removed one); registering individually means an unsafe capability is *absent*, not
*hidden*. The amplifier builtins (`string.rep`/`format`/`gsub`/`concat`, `find`/`match`) are registered
as **capped wrappers** (T13), never the raw stdlib versions. The kept/dropped sets are enumerated in
**slice 7.1's absence test (§ Allowlist)**. **security-auditor signs off on the allowlist + the capped
wrappers + the frozen string metatable before 7.1 lands.**

### 2.1 Allowlist — keep / drop (slice 7.1's absence test asserts the full DROP set)

**KEEP (register individually, not via `OpenBase`):** `assert`, `error`, `pcall`, `xpcall`, `select`,
`type`, `tostring` (handles supply `__tostring`, T15), `tonumber`, `pairs`, `ipairs`, `unpack`/
`table.unpack`, `print`→`mud.log`; **tables:** `string` (`rep`/`format`/`gsub`/`find`/`match` → **capped
wrappers**, T13), `table` (`concat` **capped**), `math` (`random`/`randomseed` **rebound to the zone
RNG**; `randomseed` ideally a **no-op**, T9).

**DROP / never-register (the absence test must assert ALL):** `load`, `loadstring`, `dofile`,
`loadfile`, `require`, `module`, `package`, `collectgarbage`, `getmetatable`, `setmetatable`, `rawget`,
`rawset`, `rawequal`, `rawlen`, `next`, `_G`, `setfenv`, `getfenv`, `newproxy`, `os`, `io`, `debug`,
`coroutine`, `channel` (gopher-lua's goroutine primitive — T6), `string.dump`, and `math.randomseed`
as an entropy reset (no-op it). This corrects the earlier draft's list, which omitted
`getmetatable`/`setmetatable`/`rawset`/`setfenv`/`getfenv`/`newproxy`/`collectgarbage`/`module`/`next`/
`_G`/`coroutine`/`channel`/`string.dump`.

---

## 3. Resolving the three open design choices (LUA.md §10)

LUA.md line 8 promises "three choices flagged in §10," but the doc ends at §9 — the forks were never
written. The three real open forks, and the resolution for each:

**Fork 1 — Budget enforcement mechanism & defaults (P7-D6) — RESOLVED (user decision, 2026-06).**
*Decision:* a **three-layer budget**, because the security-auditor's probes against the real
gopher-lua v1.1.1 falsified the simpler designs:
1. **A vendored gopher-lua fork** adds the **instruction-count abort** — gopher-lua v1.1.1 has **no**
   `SetHook`/`MaskCount`/debug-hook at all (the earlier draft's `lua.MaskCount` does not exist). The
   fork adds the count check **in `mainLoopWithContext`, beside the existing `ctx.Done()` select** —
   deterministic and test-reproducible (a pure-CPU loop trips it identically every run). Vendor
   `github.com/yuin/gopher-lua`, **pin + document the fork**; this is **slice 7.1's first work item**.
2. **The `LState` context wall-clock deadline** (`SetContext` + `context.WithTimeout`) catches a
   low-instruction stall (a GC pause, a slow C-side call). It checks **between** bytecode ops only.
3. **Capped amplifier builtins** (T13) catch the **single-op bomb** — `string.rep("A", 2e9)`
   allocates GB in **one** instruction (probed), which neither the count (one op) nor the clock
   (no between-op check inside a builtin) can stop. So `string.rep`/`format`/`gsub`/`table.concat`
   ship as size-capped wrappers; the raw stdlib versions are never exposed.

*Defaults:* **5ms wall-clock, 100k VM instructions** per entry-point call, tunable per pack and per def;
fork overhead <2% at ~1k-op granularity. All three abort *that call only*, are caught by `pcall`, and
feed the error budget (deadline aborts weighted separately, P7-D10). *Rejected alternatives:* a debug
hook (does not exist in v1.1.1); wall-clock alone (non-reproducible in tests; misses both the
single-op bomb and a tight loop that re-burns the deadline every call); a goroutine-watchdog
preempting the VM (cross-goroutine `LState` access — violates the single-writer invariant, gopher-lua
is not goroutine-safe).

**Fork 2 — In-flight `mud.after` callbacks across a hot reload (P7-D7).**
*Recommendation:* **drop by generation tag** as the default; an explicit `mud.after{durable=true}`
opt-in for a state-cleanup finalizer. *Why:* the reload hazard is running **old code against new
state**; a timer closure compiled against the pre-edit chunk may assume a `self.state` shape the new
chunk changed. Dropping is the safe default (the edit "starts fresh"); the rare legitimate
finalizer (release a held resource, clear a flag) gets the opt-in. Mirrors the prototype reloader's
"live instances keep the old, next spawn uses the new" semantics — here, "in-flight old-gen timers
drop, new invocations use the new chunk." *Rejected:* always-complete (runs stale code against new
state — the corruption class); always-drop with no opt-in (loses a legitimate finalizer use).

**Fork 3 — Result-altering reactions reaching into an in-flight action (P7-D8).**
*Recommendation:* a **typed, bounded reaction context object** passed to the Phase-6 named checkpoints
(`BeforeCastCommit`, the to-hit checkpoint, `OnDamageTaken`), exposing a small mutation allowlist
(`cancel`/`modify(field,delta)`/`replace_target`/`consume_resource`); the engine fires the checkpoint,
runs the Lua hook **synchronously inline** on the zone goroutine, then **re-reads** the recorded
mutations and applies them at the seam — the **observe-then-recheck** shape the `on_depleted` death
checkpoint already implements (PRINCIPLES.md: the reference before-checkpoint). Three hardening
invariants make the surface auditable (T12): `modify`'s `field` is a **closed per-checkpoint enum
resolved by a Go switch** (to-hit `{"ac"}`, `OnDamageTaken` `{"amount"}` — never a string indexing an
attr map); `replace_target` **re-runs `guardHarmful` against the new target** (the original gate ran
against the original — replacing onto a non-consenting player would otherwise bypass it); the path
threads the **same `eventBudget`** so a reaction→checkpoint→reaction loop is width+depth bounded.
*Why:* Phase 6 deliberately built the checkpoints so Phase 7 adds **hook bodies, not pipeline
surgery**; a typed reaction object (not a raw pipeline pointer) keeps the alter-surface auditable and
single-writer. Counterspell/Shield/concentration all express on this one shape. *Rejected:* handing Lua a raw mutable
pipeline struct (unbounded reach, un-auditable, T12 violation); a post-hoc "undo" model (the action
already had side effects — can't cleanly rewind).

---

## 4. Integration constraints (binding)

- **No new concurrency.** The `LState` is constructed at zone build and called **only** from
  `Zone.Run`'s goroutine (the existing single-writer loop). No goroutine touches it; no lock guards it
  (gopher-lua is not goroutine-safe — and we never need it to be). Lua callbacks (`mud.after`,
  bus handlers, reactions) all run inline on that goroutine. This is the Phase 6 actor-model contract,
  unchanged.
- **Handles never hold `*Entity`.** A Lua value wraps `(RuntimeID, zone, zoneGen)`; the `*Entity` is
  resolved, used, and dropped inside each Go method call (§1.2). This is what makes "no dangling, no
  cross-zone" structural rather than disciplinary.
- **Harm reuses the funnels — no parallel path.** Lua effect ops call `dealDamage`/`applyDebuff`/
  `guardCrossPlayerWrite` (effect_op.go), which call `guardHarmful` first. The binding's job is to
  build a correct `effectCtx` **holding the five binding invariants (P7-D3)** — actor/source/target
  engine-resolved (never script-supplied), `disp` engine-set, the funnels the only write path
  (build-failing lint), `rng` always the ctx/zone RNG, `depth`/`eventBudget` threaded never reset.
  The funnel owns the gate. No Lua-specific damage/affect write exists.
- **One budget chokepoint arms a fresh deadline per call — for EVERY Lua-invoking path.** The
  `LState` context deadline survives inner `pcall` **only if a fresh `context.WithTimeout` is set
  before every Lua entry and cleared after** — a stale/cancelled context makes the **next** call fail
  instantly. The binding invariant: **there is no Lua-invoking path that does not pass through the one
  chokepoint that does `SetContext(fresh) → run → RemoveContext`.** This explicitly includes
  **`mud.after` timer callbacks, reaction hooks, and bus handlers** — not just top-level triggers
  (each is a fresh entry needing its own fresh deadline). The vendored instruction-count budget is
  re-armed at the same chokepoint. **DOUBLY load-bearing (7.1 security review):** the default gopher-lua
  loop is the plain `mainLoop` (no count, no deadline); only `SetContext` swaps to `mainLoopWithContext`
  where **both** layers live — so a path that forgets `SetContext` silently loses the budget *and* the
  deadline (a runaway runs unbounded), not just the deadline. Therefore 7.5 must make a `runChunk`-style
  private method the **SOLE** way to enter Lua (no raw `L.PCall`/`L.Call` reachable from engine code
  outside it — enforce with a build-failing lint like the T8 funnel check), and add a test that a
  budget-armed call with no context is impossible by construction. (7.1's single `runChunk` already does
  this correctly for its one caller — 7.5 generalizes + locks it.)
- **Determinism via the per-zone engine RNG.** The binding threads `effectCtx.rng` into every
  Lua-reachable random draw; `math.random` is rebound; no other entropy is exposed (P7-D4).
- **Hot reload rides the existing content-invalidation path.** The reloader (reload.go) recompiles the
  Lua chunk on the same `(kind, ref)` bus invalidation it already handles for prototypes, bumps the
  gen, and swaps via the existing atomic registry swap. `self.state` data survives; old-gen timers
  drop (P7-D7).
- **The bus budget is shared.** A Lua bus handler runs under the **same** `maxEventDepth`/
  `maxEventHandlers` budget (event.go) as a declarative op-list handler; a Lua reaction increments the
  same depth and decrements the same width budget. Lua adds no new cascade-bounding surface — it reuses
  Phase 6's.
- **Cross-zone consequences are reserved (Phase 10).** A Lua handler needing a cross-zone effect
  enqueues for the (Phase-10) director — a no-op reservation now, exactly like the declarative path.

---

## 5. Slicing (ordered, independently committable)

The spine is **VM + sandbox → handles → API surface → entry points → safety → state → hot reload →
hookability obligations → escape-hatch cases**. Smallest-first, each a commit with the prior phase's
tests green and its owning + cross-cutting reviewers signing off ([subagent-review-after-every-step]).
The **security-auditor reviews every slice that adds sandbox surface** (7.1 — the vendored-fork abort,
the allowlist construction, the capped builtins, the frozen string metatable; 7.2 — `__tostring`; 7.3c
— the harm funnels; 7.5 — the budget chokepoint + breaker; 7.6 — the `state` marshaller; 7.9 — the
reaction mutation allowlist + re-gate) — the threat-model row each slice carries is the review checklist.

| Slice | Scope | Done when | Tests added |
|-------|-------|-----------|-------------|
| **7.1 — Vendor the fork + VM lifecycle + the restricted-globals sandbox** | **First work item: vendor `github.com/yuin/gopher-lua`** — add the instruction-count abort in `mainLoopWithContext` beside the existing `ctx.Done()` select; pin + document the fork (P7-D6 layer 1). Then: construct one `*lua.LState` per zone via **`lua.Options{CallStackSize, RegistrySize/RegistryMaxSize}`** (T4 — the recursion/value-stack caps are build-time options, **not** `SetMaxStackSize`), torn down on stop, called only from `Run`. The **allowlist-built `_ENV` by registering kept base functions individually — NOT `lua.OpenBase`** (which bundles `load`/`require`/`getmetatable`/`setmetatable`/… — T1/T14); the **capped amplifier builtins** (`string.rep`/`format`/`gsub`/`find`/`match`, `table.concat` — T13) instead of the raw stdlib; **drop `get/setmetatable`** (T14); `math.random` rebound, `randomseed` no-op (T9); `print`→`mud.log`. **The shared `L.G.builtinMts[LTString]` points at the private capped-wrapper table, unreachable as a script global — NOT a `__newindex`-guarded still-reachable table (T14).** **No handles, effect ops, or entry points yet.** | A zone boots with a live VM (`CallStackSize` set); a sandboxed `print("hi")` reaches the log; the **§2.1 DROP set is asserted absent in full** (incl. `getmetatable`/`setmetatable`/`rawset`/`setfenv`/`getfenv`/`newproxy`/`collectgarbage`/`module`/`next`/`_G`/`coroutine`/`channel`/`string.dump`); `load(...)` errors; `string.rep("A",2e9)` is capped (T13); `getmetatable("")` is `nil`, and **a script setting `string.rep = evil` does NOT change a sibling script's `("x"):rep(2)`** (T14 cross-script method-syntax invariance). Bare-zone-unchanged. | **§2.1 DROP-set absence test (T1/T2/T14, security)**; **capped-builtin tests (T13)**; **cross-script-method-syntax-invariance test (T14, the load-bearing one)**; `CallStackSize`-set test (T4); allowlist-present + `randomseed`-no-op test; `print`→log + bare-zone-unchanged tests; vendored-fork-pinned check. All Phase 1–6 green. |
| **7.2 — The handle userdata layer** | The `(rid, zone, zoneGen)` userdata + metatable **with a `__tostring` returning `<entity #rid>` — never the raw Go pointer** (T15: default `tostring(ud)` leaks `0x…`, an ASLR defeat); the **re-validate-every-method** Go path (§1.2); the identity/query read methods (`h:id`/`h:name`/`h:short`/`h:attr`/`h:resource`/`h:level`/`h:has_affect`/`h:affect_magnitude`/`h:has_flag`/`h:room`); `self` bound in a trivial trigger context. **Read-only — no effect ops, no harm surface yet.** | A trigger script reads `self:name()`/`self:attr("str")`; `tostring(self)` is `<entity #rid>`, **never** `0x…` (T15); a handle for a **moved-away** entity no-ops (returns `nil`); a handle for an entity in **another zone** is invalid here; no method holds an `*Entity` across the call. | handle-resolve + no-dangling test (**T7**); cross-zone-invalid test (**T7**); **`__tostring`-no-pointer test (T15, security)**; each read-method test; `-race` test that no foreign-zone deref occurs. |
| **7.3 — The curated API surface (3 sub-slices, incremental)** | **7.3a identity/query + traversal:** `h:contents`/`h:equipment`/`h:group`/`h:is_enemy`/`h:distance`/`h:can_see` (handle-returning traversal); the comms ops `h:send`/`h:act`/`h:say`/`h:emote` (no harm). **7.3b the `mud.*` world/util table:** `mud.random`/`mud.roll` (ctx RNG — **T9**), `mud.now` (pulse counter — **T2/T9**), `mud.log`, `mud.scan`/`mud.broadcast`, `mud.spawn`/`mud.transform`/`mud.summon`, `mud.after`/`mud.cancel` (zone-wheel scheduling — **T6**), `mud.pvp_allowed`. **7.3c the effect-op handles (the harm surface):** `h:damage{}`/`h:heal`/`h:modify_resource`/`h:drain`/`h:apply_affect`/`h:remove_affect`/`h:dispel`/`h:move`/`h:teleport`/`h:recall` — **each routing the existing `dealDamage`/`applyDebuff`/`guardCrossPlayerWrite` funnels** (P7-D3, **T8**). | 7.3a: a script greets a room (`h:act`) and walks its `h:contents()`. 7.3b: `mud.after(2, fn)` fires on the zone wheel on the zone goroutine (not a new goroutine); two seeded runs roll identically. 7.3c: a Lua `h:damage{}` against a **protected player in a safe room is a clean no-op** (gate held); a Lua buff on self attaches; harm funnels the same gate as a declarative op. | 7.3a traversal/comms tests; 7.3b `mud.after`-on-wheel + goroutine-id test (**T6**), seeded-RNG determinism test (**T9**), `mud.now`-pulse test (**T2**); **7.3c gate-held-from-Lua test (T8, security)**, funnel-reuse audit-grep test, per-target gate test. |
| **7.4 — Entry points (Lua handler bodies)** | Wire the reserved Lua columns to **run**: ability `on_resolve` in Lua (defs.go `onResolveLua`, ability_build.go — now executed, not read-not-run); affect `on_apply`/`on_tick`/`on_expire`/`on_dispel` (defs.go reserved hooks); **triggers** `on(event, fn)` (room/mob/item `enter`/`leave`/`speech`/`get`/`give`/`attack`/`death`/`tick`/`reset`/`greet`); **custom commands** (content registers a verb implemented in Lua, into the command table); **formulas** (`to_hit`/`soak`/`regen`/`xp_for` as a Lua function alternative to the prefix-AST); the **`pvp_allowed(actor, target)` policy hook** in Lua. Each invoked through `pcall` + the (still-default, un-budgeted) sandbox. **Lua bus handlers** ride the Phase-6 bus (a Lua body where an op-list sat). | A mob's `on("greet", …)` greets a player by name and remembers via `self.state`; a mob's `on("speech", …)` reacts to "amulet"; a Lua `on_resolve` composes effect ops; a custom `dance` verb runs; a Lua `pvp_allowed` policy decides a fight; a Lua handler on `OnHit` builds a resource. | per-entry-point invocation tests (trigger/on_resolve/affect-hook/custom-command/formula/pvp-policy); Lua-bus-handler test (rides the depth/width budget); `pcall`-isolation smoke (a bad body fizzles, zone serves on). |
| **7.5 — The budget chokepoint + circuit breaker + error isolation** | The **one chokepoint** (§4): `SetContext(fresh deadline) → re-arm the vendored instruction count → run → RemoveContext`, wrapping **EVERY** Lua-invoking path — top-level triggers, **`mud.after` callbacks, reaction hooks, AND bus handlers** (a stale cancelled context fails the next call). The vendored count (7.1) + the deadline together are P7-D6 layers 1–2 (layer 3, the builtin caps, landed in 7.1). The **error-budget circuit breaker** (P7-D10, **T11/T15**): **per-instance for entity-scoped scripts, per-`(kind,ref)` for shared defs** (shared-def blast radius documented); **deadline aborts weighted/rate-limited separately from logic errors** (no latency-induced false quarantine); a **generic player-facing message**, raw error/stack to ops logs only; reset-on-reload. The per-zone VM memory **metric (detection-only, T5)**. | A `while true do end` trigger **aborts within the deadline and the zone keeps serving**; **an `mud.after` callback AND a bus handler are each deadline-bounded** (not just a top-level trigger); deep recursion errors cleanly (T4's `CallStackSize`, set in 7.1); a deadline trip doesn't quarantine as fast as a logic error; after N failures a script is **disabled** and a reload re-enables it; no raw Lua error leaks to a player; VM memory is reported. | **chokepoint-arms-fresh-deadline-per-call test incl. `mud.after` + bus handler (T3, security)**; count-vs-clock test; **breaker trip + reload-reset test, per-instance vs shared-def (T11)**; **deadline-vs-error weighting test**; generic-message test (T15); overhead benchmark (<2%); memory-metric test. |
| **7.6 — `self.state` ↔ persisted JSONB** | The data-only Lua-table↔JSON marshaller (P7-D5, **T10**): the type allowlist (number/string/bool/nested-table), the **byte/depth/key-count caps**, rejection of functions/handles/userdata at save; `self.state` mirrored into a new `StateJSON.Script` field (character.go), serialized on the **existing** save cadence, re-hydrated by `loadCharacter` into a plain table. Mob/item script state rides the same path where those entities persist. | A scripted mob's quest counter in `self.state` **survives logout/login** (and a crash-rehydrate); a `state` carrying a function/handle is **rejected cleanly at save** (no panic, no silent partial drop); an over-cap `state` is rejected; a nested data table round-trips identically. | **state round-trip test (T10)**; **reject-code/handle test (T10, security)**; cap-exceeded test; crash-rehydrate test; cadence-integration test (rides the existing ladder). |
| **7.7 — Hot reload (recompile + swap handlers)** | The reloader (reload.go) recompiles the Lua chunk on the **existing** `(kind, ref)` content-bus invalidation, bumps the chunk **generation**, and swaps the proto via the existing atomic registry swap; `self.state` data survives (it's not code); **old-gen `mud.after` callbacks drop** (P7-D7), with the `durable=true` opt-in honored; a compile error keeps the last-good proto (non-fatal, like the prototype reloader); the circuit breaker resets on a successful reload. | Editing a mob's Lua greeting **reloads live** (no restart) and the next greet uses the new text while `self.state` (who's been greeted) persists; an in-flight old-gen `mud.after` timer **drops**; a `durable=true` finalizer **completes**; a syntactically-broken edit keeps the old behavior + logs. | live-reload-swaps-handler test; **state-survives-reload test**; **old-gen-timer-drops test (P7-D7)**; durable-opt-in test; compile-error-keeps-last-good test; breaker-reset-on-reload test. |
| **7.8 — The hookability obligations (custom-event lane + reserved-kind lighting)** | **(a) The content-namespaced custom-event lane** (PRINCIPLES.md pillar 2, ROADMAP): the closed `knownEventKinds` map (event.go) grows a **`pack:event` lane** — builders `mud.fire("pack:OnShipDock", subject, data)` and subscribe `on("pack:OnShipDock", fn)`; still **depth/width-budgeted and gate-funneled** like an engine event, **no privileged status**; namespaced by pack to avoid collision. **(b) Light the reserved engine kinds** whose owners exist by now (event.go consts already named, reserved): `OnApplyAffect`/`OnAffectTick`/`OnAffectExpire` fire from the affect runtime (affect_runtime.go), and a **new `OnEnter`** movement hook fires from the move path — so "a missing hook is an engine bug" holds. (Cross-phase kinds — `OnRest`/`OnLevelUp`/`OnLogin` — stay owned by their phase.) | A pack **defines, fires, and handles** a `pack:OnShipDock` event the engine has **never heard of** — a sailing system's quest hooks it, all in content; the custom fire obeys the depth/width budget and any harmful op in its handler funnels the gate. `OnApplyAffect`/`OnAffectTick`/`OnAffectExpire` and `OnEnter` fire to content/Lua handlers. | custom-event fire+subscribe test; **custom-event budget + gate test (security)**; pack-namespacing/collision test; reserved-kind lighting tests (apply/tick/expire/enter); unknown-kind-still-lints test. |
| **7.9 — The documented escape-hatch cases** | The result-altering reaction model (P7-D8, **T12**): the typed **reaction context object** at the Phase-6 checkpoints — `rx:cancel`/`rx:modify(field,delta)`/`rx:replace_target(h)`/`rx:consume_resource`, with the **three hardening invariants**: `field` a **closed per-checkpoint enum resolved by a Go switch** (to-hit `{"ac"}`, `OnDamageTaken` `{"amount"}`), `rx:replace_target` **re-runs `guardHarmful` against the new target**, and the path threads the **same `eventBudget`** (effect_op.go:56). **Counterspell** (`rx:cancel()` on `BeforeCastCommit`), **Shield** (`rx:modify("ac", +5)` on to-hit), **concentration** [G11] (a concentration affect `rx:cancel()`s itself on a failed `OnDamageTaken` save), the 5e **multiclass spell-slot table** [G7] (a Lua formula over multiple class levels). | A Lua **Counterspell cancels an in-flight cast** (observe-then-recheck); Shield raises AC for the triggering swing only; concentration drops on a failed save; multiclass slots compute correctly. A **non-allowlisted `modify` field is a no-op**; `replace_target` onto a **non-consenting player is gate-blocked**; a reaction loop is **budget-bounded**. | Counterspell/Shield/concentration/multiclass tests; **non-allowlisted-field no-op test (T12, security)**; **`replace_target` re-gate test (T12, security)**; **reaction-loop budget-exhaustion test**. |

**Adjustment / justification.** 7.1–7.2 land the **smallest, riskiest** thing first (the VM + the
sandbox skeleton + the no-pointer handle layer) so the security-auditor reviews the trust boundary
**before** any capability hangs off it — the allowlist and the re-validate-every-method path are the
foundation everything else trusts. 7.3 adds capability **incrementally**, with the **harm surface
(7.3c) last in its trio** and explicitly gated. 7.5 (budgets/breaker) lands **after** the entry points
(7.4) so there is real script work to bound, but **before** the phase is considered safe — every entry
point then runs under the full budget. 7.8 (the hookability obligations) and 7.9 (the escape-hatch
cases) are last because they depend on the full API + the bus integration. **If 7.3 proves large**,
its three sub-slices ship as three commits (recommended). **If 7.9 proves large**, split the
reaction-context mechanism (Counterspell/Shield/concentration) from the multiclass-slot formula (it
depends only on the Lua-formula entry point from 7.4, not the reaction model).

---

## 6. Schema + loader integration

Phase 7 is **light on new tables** — most of it is wiring **existing reserved columns** to run and
adding a **Lua-source tail** to def bodies (additive JSONB, the established pattern — **persistence-
engineer to confirm**).

- **The Lua runtime dependency is a VENDORED fork of `github.com/yuin/gopher-lua`** (P7-D6 / slice
  7.1), not the upstream module: the instruction-count abort (in `mainLoopWithContext`) does not exist
  upstream and must be carried in-tree, pinned + documented. The build references the vendored path;
  the fork's delta is kept minimal (one abort beside the existing `ctx.Done()` select) so rebasing on
  an upstream release stays cheap.
- **Reserved columns become live (no migration):** `ability_def.on_resolve_lua` (already carried,
  ability_build.go `onResolveLua`), `affect_def`'s `on_apply`/`on_tick`/`on_expire` Lua hooks (defs.go
  reserved `onApply`/`onExpire`). These exist; 7.4 compiles + runs them.
- **New JSONB tails (additive, no `ALTER`):** a `lua` / `triggers` block on room/mob/item def bodies
  (the `on(event, fn)` source); a `lua` formula alternative on the ruleset formula refs; a `pvp_lua`
  policy source; a `commands` block registering custom verbs. Parsed by the extended mapper into a
  compiled-proto registry (atomic-swap, like the Phase 5/6 registries).
- **`StateJSON.Script`** (character.go) — the new data-only `self.state` subtree (7.6), serialized on
  the existing cadence; pre-7.6 saves load with none (the established backward-compat default).
- **The compiled-proto registry** is per-shard runtime state (atomic.Pointer-swapped like the
  prototype cache); the reloader recompiles a single proto on a `(kind, ref)` invalidation (7.7).
- **The stdlib pack** (the acceptance content) gains: a **scripted greeter mob** (`on("greet"/
  "speech")` + `self.state`), a **Lua `on_resolve` ability**, a **Lua Counterspell + Shield + a
  concentration spell**, a **multiclass-slot Lua formula**, and a **sailing demo** (a `pack:OnShipDock`
  custom event a dock room fires and a quest handler subscribes to) — the §5 done-when content. The
  bare-engine invariant holds: **no Lua content ⇒ no scripts compiled ⇒ the VM runs nothing** (the
  empty-boot test stays green; Lua is unavailable, not erroring).

---

## 7. Risks & out-of-scope

### Explicitly OUT of scope
- **The cross-zone scoped + durable event bus = Phase 10** ([WORLD-EVENTS.md](WORLD-EVENTS.md)). A Lua
  handler needing a cross-zone consequence enqueues for the (Phase-10) director — reserved no-op. The
  custom-event lane (7.8) is **in-zone** like the rest of the Phase-6 bus.
- **GMCP structured emit = Phase 9.** `mud.gmcp(h, package, data)` is the binding's shape but the
  emit lands with the GMCP negotiation phase; Lua emits `act`/`send` text now.
- **Progression / chargen / the track grants Lua composes = Phase 11.** Phase 7 ships the Lua
  multiclass-slot **formula** (the table math) and the reaction model; the `grant_*` ops and
  `track_defs` Lua composes are Phase 11.
- **`gopher-luar` / reflection-based binding = never** (P7-D2). The curated surface is the audit
  surface; reflection would expose Go struct layout and defeat the API/engine decoupling.
- **A hard per-VM memory cap = not available** (T5, the acknowledged gopher-lua limitation). We bound
  memory indirectly (capped builtins T13, `state` caps) and observably (per-zone metric, **detection-
  only** — fires after the fact) — documented, not silent.

### Integration risks
1. **The sandbox is the sharpest trust boundary in the engine (security) — and the security-auditor's
   probes against real gopher-lua v1.1.1 falsified five mitigations as first drafted.** The corrected
   set: the allowlist-built `_ENV` **by registering kept functions individually, NOT `lua.OpenBase`**
   (T1/T14); the **vendored-fork instruction count** (v1.1.1 has no debug hook — `MaskCount` does not
   exist) + the wall-clock deadline + the **capped amplifier builtins** (single-op bombs the count/clock
   miss — T13); **`lua.Options{CallStackSize}`** for recursion (no `SetMaxStackSize` — T4); the
   **frozen string metatable + dropped `get/setmetatable`** (the shared-`L.G` poison the per-call `_ENV`
   reset doesn't cover — T14); `__tostring` on every handle (the pointer leak — T15); the harm-funnel
   reuse + handle re-validation. The threat model (§2, now **T1–T15**) is the review checklist.
   **security-auditor reviews 7.1, 7.2, 7.3c, 7.5, 7.6, 7.9** — the largest new attack surface since the
   engine began.
2. **The harm gate over the Lua surface (security).** A Lua effect op is the newest harm-injection
   vector; it must funnel the **same** `guardHarmful` with **no** Lua-specific path (T8/P7-D3). The
   binding builds the `effectCtx`; the funnel owns the gate, fail-closed on a detached actor/target.
   **security-auditor reviews 7.3c** — the in-op funnel is what makes it can't-bypass.
3. **No new concurrency (distributed-systems).** The `LState` is single-writer on the zone goroutine;
   no goroutine touches it, no lock guards it. `mud.after` schedules on the zone wheel, not the OS
   scheduler. **distributed-systems-architect confirms** the VM lifecycle adds no cross-goroutine
   access and the reload swap stays on the subscription goroutine (the existing reload.go contract).
4. **Hot reload must not run old code against new state (correctness).** Old-gen `mud.after` timers
   drop by default (P7-D7); `self.state` data survives but the chunk is swapped atomically; a compile
   error keeps the last-good proto. **The reload-corruption class is the subtle risk** the gen tag
   guards.
5. **Memory is a soft cap (security/ops).** gopher-lua has no hard per-VM limit. **A single-op
   allocation bomb (`string.rep("A", 2e9)`) is NOT caught by the instruction count (one op) or the
   wall-clock (no between-op check inside a builtin) — it is prevented by the capped builtins (T13)**,
   not by a budget. Gradual multi-op growth is bounded by the instruction count + `state` caps; the
   per-zone memory metric is **detection-only** (fires after the fact). Documented as a known
   limitation (T5/T13), not silent.
6. **`self.state` is a persistence + injection surface (persistence/security).** Data-only,
   size-bounded, no code/handles (T10). **persistence-engineer confirms** the `StateJSON.Script`
   subtree follows the JSONB-tail + cadence pattern and excludes nothing that should persist; the
   marshaller's allowlist is **security-auditor**'s review (7.6).

### Cross-cutting reviewers (per [subagent-review-after-every-step])
- **scripting-engineer (owning):** every slice — the VM lifecycle, the handle layer, the API surface,
  the entry points, the budgets/breaker, `self.state`, hot reload, the hookability obligations, the
  reaction model.
- **security-auditor:** **7.1** (the vendored-fork abort path + the individually-registered allowlist
  + the capped builtins + the frozen string metatable — the load-bearing construction), **7.2** (handle
  `__tostring`, no pointer leak), **7.3c** (the harm-funnel reuse + the five binding invariants),
  **7.5** (the budget chokepoint over EVERY Lua path + the two-mode breaker + deadline-vs-error
  weighting), **7.6** (the `state` marshaller allowlist + caps), **7.9** (the reaction field enum +
  `replace_target` re-gate + shared budget) — the §2 threat model (T1–T15) is the checklist; each slice
  carries its row's mitigation. **Re-confirm sign-off after this revision** (the GAPS-FOUND fold).
- **distributed-systems-architect:** 7.1 (no cross-goroutine VM access), 7.3b (`mud.after` on the zone
  wheel, not the OS scheduler), 7.7 (the reload swap stays on the subscription goroutine), 7.8 (the
  custom-event lane stays in-zone — the Phase-10 boundary).
- **persistence-engineer:** 7.6 (`StateJSON.Script` JSONB-tail + cadence + size caps; nothing that
  should persist is excluded), 7.7 (state survives the reload).
- **abilities-engineer:** 7.4 (Lua `on_resolve`/affect hooks fit beside the op-list interpreter with no
  lifecycle change), 7.9 (the reaction context object fits the Phase-6 checkpoints additively).
- **combat-engineer:** 7.9 (Counterspell/Shield/concentration reach the checkpoints the Phase-6 swing/
  cast pipeline published — confirm the seam carries what the reactions need).
- **rpg-systems-designer (acceptance):** 7.9 — confirm the escape-hatch cases (result-altering
  reactions, concentration, the multiclass-slot table) are the right complex-20% set and express
  cleanly in the reaction + Lua-formula model.

---

## 8. Done-when (the phase capstone)

The ROADMAP Phase 7 done-when, made concrete on this plan — **all four, all edited live, none able to
crash, stall, or cross a zone:**

1. **A room script greets on entry** — a room/mob `on("enter"/"greet", …)` greets an arriving player
   by name and remembers them via `self.state` (which survives logout/login) — and the greeting text
   is **edited live** (hot reload) without a restart.
2. **A scripted mob reacts to speech** — `on("speech", …)` makes a mob respond to a keyword.
3. **A Lua Counterspell cancels an in-flight cast** — a `BeforeCastCommit` reaction hook (the typed
   reaction context, `rx:cancel()`) reaches into an in-flight ability and **vetoes** it
   (observe-then-recheck), within the single-writer model, no pipeline surgery.
4. **A pack fires and handles a custom event the engine never heard of** — a sailing system defines,
   `mud.fire`s, and `on`-subscribes a `pack:OnShipDock` event entirely in content (the custom-event
   lane), depth/width-budgeted and gate-funneled like an engine event.

And the safety capstone, demonstrated under test: a deliberately runaway script (`while true do end`),
a **single-op allocation bomb** (`string.rep("A", 2e9)` — capped, T13), a deeply recursive one
(`CallStackSize`-bounded, T4), a **string-metatable poison attempt** (`getmetatable("")` is `nil`, the
mt frozen — T14), a chronically-erroring one, and a harm-injecting one **each fail just their own
action** — the zone keeps serving every other player, the breaker quarantines the chronic offender (a
latency-induced deadline trip does NOT), the harm funnels the gate, no Lua error leaks to a player, and
**no script crashes, stalls, or reaches out of its zone.**

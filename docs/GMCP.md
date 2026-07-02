# GMCP — Generic MUD Communication Protocol

GMCP lets rich clients (Mudlet, MUSHclient, custom web clients) receive structured data
alongside the text stream — powering minimaps, HUDs, vitals bars, and inventory panels
without scraping ASCII.

## Wire format

GMCP is a telnet subnegotiation under option **201 (0xC9)**:

```
IAC SB 201  <package-message-name> SP <json-payload>  IAC SE
```

- `<package-message-name>` is dotted, e.g. `Char.Vitals`, `Room.Info`.
- Payload is JSON. Messages with no data may omit the JSON.

### Negotiation
1. Gate sends `IAC WILL GMCP`; capable client replies `IAC DO GMCP`.
2. Client sends `Core.Hello {"client":"Mudlet","version":"4.18"}`.
3. Client advertises supported packages:
   `Core.Supports.Set ["Char 1","Char.Vitals 1","Room 1","Comm 1"]`.
4. Server only emits messages for packages the client advertised (filtered per connection).

## Engine design

- A **GMCP registry** defines packages, each with versioned, schema'd messages.
- Each connection has a **`Core.Supports`** set negotiated at login; the per-connection
  **encoder** drops any message the client didn't ask for, so scripts/engine can always
  `gmcp.Send(player, "Room.Info", payload)` without checking support.
- GMCP emitters subscribe to the **same per-zone event bus** as text output: when an entity's
  vitals change, both the prompt and `Char.Vitals` are emitted from one event.
- **String fields carry STRIPPED display text.** Content-authored text may embed `{{TOKEN}}` color
  markup, which only the telnet edge renders — a GMCP payload must never ship the literal tokens.
  Every payload builder routes content-authored strings through `colormarkup.Strip` at BUILD time
  (world: the `gmcpText` helper in `gmcp.go`; gate: the Comm mirror). Unknown `{{...}}` stays
  literal, matching the color-off telnet projection. Any NEW emitter — including the future
  builder-facing `gmcp.send` Lua lane — must honor this contract.

## Implemented packages

### Core (client housekeeping)
| Message            | Dir | Purpose                                  |
|--------------------|-----|------------------------------------------|
| `Core.Hello`       | C->S | client name/version                      |
| `Core.Supports.Set/Add/Remove` | C->S | advertise package support   |
| `Core.Ping`        | <->   | latency measurement                    |

### Char (the HUD)
| Message            | Dir | Payload                                                       |
|--------------------|-----|--------------------------------------------------------------|
| `Char.Vitals`      | S->C | every **content-defined** resource pool: `"<ref>"` current + `"max<ref>"` max — a pack with hp/mana/move yields all three; the engine names none |
| `Char.Stats`       | S->C | every attribute a pack flagged `stat: true` → its resolved value; derived/internal attributes stay out of the panel |
| `Char.Status`      | S->C | `{"state":"fighting","target":"a goblin"}` — position state + (when fighting) the target name |

`Char.Vitals` and `Char.Stats` carry **no engine-named keys** — which resources and which
attributes appear is entirely content's choice, honoring the "engine = mechanism, content =
flavor" pillar. Both are change-detected (map marshal sorts keys) and re-emitted only on change.

### Char.Items (inventory/equipment panel)
| Message              | Dir | Purpose                                                              |
|----------------------|-----|---------------------------------------------------------------------|
| `Char.Items.List`    | S->C | a `{location, items}` FULL snapshot — sent on login/reconnect/handoff arrival (the baseline)   |
| `Char.Items.Add`     | S->C | `{location, item}` — one entry appeared                              |
| `Char.Items.Remove`  | S->C | `{location, item:{id}}` — the entry with that id is gone (id-only)   |
| `Char.Items.Update`  | S->C | `{location, item}` — an entry changed (worn/removed, or its count)   |

`location` is `"inv"` (what the player carries, worn gear flagged `W`) or `"room"` (ground
items/corpses in the player's room). After the initial `Char.Items.List` baseline, steady-state
changes ride the incremental `Add`/`Remove`/`Update` deltas — a single pickup no longer re-ships
the whole panel. A fresh or reconnected client always receives a full `List` before any delta.

Item entry: `{"id":"i4821","name":"a steel longsword","attrib":"wWc","count":3}`
- `attrib` — single-char flags (`w`=wearable, `c`=container, `W`=currently worn/wielded).
- `count` — present only when identical discrete items COALESCE into one entry (`"a torch"` ×5);
  omitted for a singleton.
- `id` — STABLE across count changes: a coalesced group uses `g<hash>` (so raising/lowering the
  count is a same-id `Update`); a non-grouping entry (worn gear, a material, a container) uses its
  per-instance `i<runtimeID>`.

### Room (the minimap)
| Message            | Dir | Purpose                                                       |
|--------------------|-----|---------------------------------------------------------------|
| `Room.Info`        | S->C | the structured room data — see below; re-emitted only on a room change |

`Room.Info` payload (drives the minimap):
```json
{
  "num": 3001,
  "name": "The Temple Square",
  "zone": "midgaard",
  "environment": "city",
  "coord": [17, 12, 8, 0],            // [zone-id, x, y, z]; omitted when the room has no authored coords
  "exits": { "n": 3002, "e": 3014, "d": 3500 }
}
```
The client builds the minimap from `coord` + `exits` across rooms it has seen; `zone`
groups rooms; `environment` (the room's content sector) picks terrain coloring. `num` and the
exit destinations are stable per-room integer ids mapped from the room's `ProtoRef`.

### Comm (chat routing)
| Message              | Dir | Purpose                                          |
|----------------------|-----|--------------------------------------------------|
| `Comm.Channel.Text`  | S->C | `{"channel":"gossip","talker":"kurt","text":"hi"}` — mirrors a channel line so a client can route it to a per-channel tab (same hear-set filter as the text line) |

## The `Mud.*` namespace

Custom features live under a branded **`Mud.*`** namespace (rather than overloading `Char.*`),
so standard clients ignore what they don't understand.

Content/Lua emits custom frames with the sandboxed handle **`gmcp.send(player, pkg, table)`**
(`internal/world/luagmcp.go`, #51), e.g. a quest tracker or a boss-fight timer:

```lua
gmcp.send(target, "Mud.Quest", { name = "Slay the dragon", step = 2, done = false })
```

Guards (all fail-closed):
- **Namespace allowlist** — `pkg`'s top-level segment must be in the allowlist (`Mud`), so content
  can never name an engine package (`Char.*`/`Core.*`/`Room.*`/`Comm.*`) to spoof a client's HUD.
  The gate's outbound filter only checks the client *advertised* the package; the engine-vs-content
  boundary is enforced in the world, at the source.
- **Charset/length** — same alnum+`.`, ≤64-byte, no-edge-dot rule the gate enforces on inbound names.
- **Bounded payload** — the table is walked by a depth + node + byte bounded encoder (no
  functions/userdata, no cycles); an over-budget or unencodable table is a clean Lua error.

`gmcp.send` returns `true` when it reached a live player session, `false` for a session-less/mob
handle. The frame still rides the outbound support filter, so a client that never advertised `Mud`
stays silent.

## Testing
Mudlet is the reference client (built-in GMCP debugger). The bot-swarm load tool also speaks
GMCP, so vitals/room/inventory correctness can be asserted under load.

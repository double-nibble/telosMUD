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
| Message              | Dir | Purpose                                            |
|----------------------|-----|----------------------------------------------------|
| `Char.Items.List`    | S->C | a `{location, items}` list, re-sent on change      |

Item entry: `{"id":"i4821","name":"a steel longsword","attrib":"wWc"}` — `attrib` is
single-char flags (`w`=wearable, `c`=container, `W`=currently worn/wielded).

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

## Testing
Mudlet is the reference client (built-in GMCP debugger). The bot-swarm load tool also speaks
GMCP, so vitals/room/inventory correctness can be asserted under load.

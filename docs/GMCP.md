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

## Supported packages (v1)

### Core (client housekeeping)
| Message            | Dir | Purpose                                  |
|--------------------|-----|------------------------------------------|
| `Core.Hello`       | C->S | client name/version                      |
| `Core.Supports.Set/Add/Remove` | C->S | advertise package support   |
| `Core.Ping`/`Core.Ping`        | <->   | latency measurement          |
| `Core.Goodbye`     | S->C | reason on disconnect                     |

### Char (the HUD)
| Message            | Dir | Payload (example)                                            |
|--------------------|-----|-------------------------------------------------------------|
| `Char.Login`       | C->S | `{"name":"kurt","password":"..."}` (client-driven auth)     |
| `Char.Vitals`      | S->C | `{"hp":120,"maxhp":150,"mp":40,"maxmp":60,"mv":200,"maxmv":250}` |
| `Char.Stats`       | S->C | `{"str":18,"dex":14,"int":12,"level":23,"xp":45123,"tnl":9877}` |
| `Char.Status`      | S->C | `{"state":"fighting","target":"a goblin","enemies":2}`      |
| `Char.StatusVars`  | S->C | declares labels for Status keys (lets clients render generically) |

### Char.Items (inventory/equipment panels)
| Message              | Dir | Purpose                                            |
|----------------------|-----|----------------------------------------------------|
| `Char.Items.Inv`     | S->C | full inventory list                                |
| `Char.Items.Contents`| S->C | contents of a container the player opened          |
| `Char.Items.Room`    | S->C | items on the ground in the current room            |
| `Char.Items.Add/Remove/Update` | S->C | incremental deltas to any of the above   |

Item entry: `{"id":"i4821","name":"a steel longsword","icon":"sword","attrib":"wWtl"}`.

### Room (the minimap)
| Message            | Dir | Purpose                                                       |
|--------------------|-----|---------------------------------------------------------------|
| `Room.Info`        | S->C | the key one — see below                                       |
| `Room.WrongDir`    | S->C | player tried a non-exit (client can flash the map)            |
| `Room.Players`     | S->C | who else is in the room                                       |

`Room.Info` payload (drives the minimap):
```json
{
  "num": 3001,
  "name": "The Temple Square",
  "zone": "midgaard",
  "environment": "city",
  "coord": [3001, 12, 8, 0],          // [zone-ref, x, y, z] for map layout
  "exits": { "n": 3002, "e": 3014, "d": 3500 },
  "doors": { "e": "closed" },
  "details": ["fountain", "shop"]
}
```
The client builds the minimap from `coord` + `exits` across rooms it has seen; `zone`
groups rooms; `environment` picks terrain coloring.

### Comm (chat routing)
| Message              | Dir | Purpose                                          |
|----------------------|-----|--------------------------------------------------|
| `Comm.Channel.Text`  | S->C | `{"channel":"gossip","talker":"kurt","text":"hi"}` — client can route to a tab |
| `Comm.Channel.List`  | S->C | available channels                               |
| `Comm.Channel.Players`| S->C| who's on a channel                               |

## Our own namespace

Custom features (quest tracker, group/party frames, map overview, cooldowns) live under a
branded package, e.g. **`Mud.*`**:

- `Mud.Quest.List` / `Mud.Quest.Update` — quest log + objective progress
- `Mud.Group` — party member vitals (for party frames)
- `Mud.Cooldowns` — ability cooldown timers (for action-bar HUDs)
- `Mud.Map` — full zone map blob (rooms + coords) on zone entry, so clients render the whole
  area at once instead of accreting room-by-room

Keeping custom data in `Mud.*` (vs. overloading `Char.*`) means standard clients ignore what
they don't understand, and our web client can opt into the richer set.

## Testing
Mudlet is the reference client (built-in GMCP debugger). The bot-swarm load tool also speaks
GMCP so we can assert that vitals/room/inventory deltas are correct under load.

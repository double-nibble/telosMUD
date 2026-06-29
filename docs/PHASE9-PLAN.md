# Phase 9 — GMCP (rich-client data) — plan

Lights up the GMCP plumbing already reserved in the wire: `api/proto/.../play.proto` carries
`GmcpIn{pkg,json}` (client→world), `ServerFrame.gmcp = GmcpOut{pkg,json}` (world→gate), and the
client-capabilities fields (`gmcp`, `gmcp_supports`, `mccp`, width/height/charset/mtts). The telnet
layer currently REFUSES all option negotiation (`handleIAC`), so the work is: negotiate option 201,
parse/encode the `IAC SB 201 … IAC SE` subnegotiation, track per-connection `Core.Supports`, and emit
the package payloads from the engine's existing event/output path.

**Done-when (roadmap):** Mudlet shows a live vitals gauge + a minimap that updates as you walk
(= 9.1 + 9.2 + 9.3). v1 scope (user decision 2026-06-29): the **full client set, 9.1–9.5**. `Mud.*`
(quest/group/cooldowns) defers to its dependent phases (progression/party); `Mud.Map` may ride 9.3.

## Testing mandate (binding — see memory [[testing-standard]])
Every slice ships tests across ALL applicable tiers as it lands: unit + boundary/error matrices,
**property/fuzz** for the subneg parser and any payload (de)serializer, integration (gate↔world),
**e2e** black-box (a client negotiates GMCP and asserts the messages), and **chaos** (malformed/
oversized subneg, a client lying about supports, GMCP under handoff). Per-slice subagent reviews
(owning edge-engineer + a cross-cutting expert) before each commit. No deferring a slice's own tests
to the end-of-roadmap hardening sweep.

## Slices

### 9.1 — Transport foundation + Core.*
- Telnet: answer `IAC WILL GMCP` / handle `IAC DO/DONT GMCP`; parse `IAC SB 201 <pkg> SP <json> IAC SE`
  → `GmcpIn`; encode `GmcpOut` → `IAC SB 201 <pkg> SP <json> IAC SE`. Bounded + fail-closed on malformed.
- Gate: forward `GmcpIn` to the world; maintain the per-connection `Core.Supports` set; a
  per-connection **filtered encoder** drops any `GmcpOut` whose package the client didn't advertise, so
  the engine can always emit without checking support.
- `Core.Hello` (client name/version), `Core.Supports.Set/Add/Remove`, `Core.Ping`→`Core.Ping`,
  `Core.Goodbye` on disconnect.
- Tests: **FuzzGmcpSubneg** (hostile IAC/SB framing → no panic, bounded, fail-closed); supports-filter
  unit matrix; integration (gate negotiates + forwards a round-trip); e2e handshake; chaos (truncated/
  oversized subneg, unadvertised-package drop, supports survives a cross-shard handoff).

### 9.2 — Char.* HUD
- `Char.Vitals` / `Char.Stats` / `Char.Status` built from the entity's resources/attributes/combat
  state, emitted from the SAME event that drives the text prompt (one change → prompt + GMCP), so they
  never drift. `Char.StatusVars` declares Status labels for generic client rendering.
- Tests: payload builders (unit, content-driven resource set — no hardcoded hp/mp); integration (a
  vitals change emits both the prompt and `Char.Vitals`); e2e (gauge updates on damage).

### 9.3 — Room.Info (minimap) + the room-identity prereq
- A stable **ProtoRef↔integer-id** room table (the GMCP `num`/`exits` ids) and room **coord** (x,y,z)
  storage + authoring. `Room.Info` on every move: num/name/zone/environment/coord/exits/doors.
  `Mud.Map` (zone blob) optional here.
- Tests: id-table unit (stable, collision-free, survives reload); coord round-trip; move→`Room.Info`
  exits/coord correctness; e2e (walking updates the map data).

### 9.4 — Char.Items.* panels + deltas
- `Char.Items.Inv/Contents/Room` full lists + `Char.Items.Add/Remove/Update` incremental deltas as
  inventory/equipment/room items change. Item entry `{id,name,icon,attrib}`.
- Tests: item-entry builder + delta computation (unit); get/drop/wear/loot → correct deltas
  (integration); chaos (delta correctness under churn / rapid mutation).

### 9.5 — Comm.Channel.Text routing
- Route Phase-8 channel lines to `Comm.Channel.Text {channel,talker,text}` for client tab routing;
  `Comm.Channel.List/Players`. Reuses the Phase-8 bus + the receiver hear-filter (a muted/ignored
  channel emits no GMCP either).
- Tests: payload (unit); gossip → `Comm.Channel.Text` to a supporting client only (integration); the
  hear-filter/ignore funnel suppresses the GMCP too (chaos-adjacent); e2e.

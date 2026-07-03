package world

import (
	"encoding/json"
	"strings"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// gmcp_test.go covers the world-side GMCP HUD (Phase 9.2): the content-driven Char.Vitals / Char.Status
// payload builders and the change-detected emit in sendPrompt.

// drainGMCP collects the latest GMCP payload per package from a session's out channel (non-blocking).
func drainGMCP(s *session) map[string]string {
	out := map[string]string{}
	for {
		select {
		case f := <-s.out:
			if g := f.GetGmcp(); g != nil {
				out[g.GetPkg()] = string(g.GetJson())
			}
		default:
			return out
		}
	}
}

func TestCharVitalsJSONContentDriven(t *testing.T) {
	z, caster := abilityTestZone(t) // defines hp (max 100) + mana (max 100)
	setResourceCurrent(caster.entity, "hp", 70)
	setResourceCurrent(caster.entity, "mana", 30)

	var m map[string]int
	if err := json.Unmarshal(z.charVitalsJSON(caster.entity), &m); err != nil {
		t.Fatalf("Char.Vitals not valid JSON: %v", err)
	}
	// Content-driven: every registered resource appears as <ref> + max<ref>; the engine names none.
	want := map[string]int{"hp": 70, "maxhp": 100, "mana": 30, "maxmana": 100}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("Char.Vitals[%q] = %d, want %d (full payload %v)", k, m[k], v, m)
		}
	}
}

// TestHUDResourceRefsGaugeFilter proves the #50 gauge filter: the demo flags hp+mana gauge:true and
// leaves the internal per-round `reactions` pool unflagged, so the HUD (Char.Vitals + the vitals prompt)
// shows the two player pools and hides reactions. Char.Vitals therefore no longer leaks the reaction
// budget into a rich client's gauge display.
func TestHUDResourceRefsGaugeFilter(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	refs := z.hudResourceRefs()
	got := map[string]bool{}
	for _, r := range refs {
		got[r] = true
	}
	if !got["hp"] || !got["mana"] {
		t.Errorf("hudResourceRefs missing a gauged pool: %v", refs)
	}
	if got["reactions"] {
		t.Errorf("hudResourceRefs leaked the un-gauged internal reactions pool: %v", refs)
	}
	// Sorted for deterministic HUD output.
	for i := 1; i < len(refs); i++ {
		if refs[i-1] > refs[i] {
			t.Errorf("hudResourceRefs not sorted: %v", refs)
		}
	}
}

// TestHUDResourceRefsFallbackShowsAll proves the backward-compat fallback: when NO resource in a pack
// opts into gauge, every pool is HUD-visible (an un-flagged pack behaves exactly as before #50).
func TestHUDResourceRefsFallbackShowsAll(t *testing.T) {
	z, _ := abilityTestZone(t) // its combat resources set no gauge flag
	table := z.resourceDefs().table()
	for _, def := range table {
		if def.gauge {
			t.Fatalf("test precondition: abilityTestZone unexpectedly flags a gauge pool")
		}
	}
	if len(z.hudResourceRefs()) != len(table) {
		t.Errorf("fallback should surface all %d pools, got %v", len(table), z.hudResourceRefs())
	}
}

func TestRoomInfoJSON(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	temple := z.rooms["midgaard:room:temple"]
	if temple == nil {
		t.Fatal("demo temple room not found")
	}

	var info struct {
		Num   int            `json:"num"`
		Name  string         `json:"name"`
		Zone  string         `json:"zone"`
		Coord []int          `json:"coord"`
		Exits map[string]int `json:"exits"`
	}
	if err := json.Unmarshal(z.roomInfoJSON(temple), &info); err != nil {
		t.Fatalf("Room.Info not valid JSON: %v", err)
	}
	// coord is [zone-id, x, y, z]; the demo temple is the grid origin [0,0,0].
	if len(info.Coord) != 4 || info.Coord[1] != 0 || info.Coord[2] != 0 || info.Coord[3] != 0 {
		t.Errorf("coord = %v, want [zone, 0, 0, 0]", info.Coord)
	}
	if info.Num != roomNum("midgaard:room:temple") {
		t.Errorf("num = %d, want the stable hash %d", info.Num, roomNum("midgaard:room:temple"))
	}
	if info.Zone != "midgaard" {
		t.Errorf("zone = %q, want midgaard", info.Zone)
	}
	if info.Name == "" {
		t.Error("name is empty")
	}
	// The temple exits north→market; the exit target is the destination room's stable num.
	if info.Exits["north"] != roomNum("midgaard:room:market") {
		t.Errorf("exits[north] = %d, want market's num %d", info.Exits["north"], roomNum("midgaard:room:market"))
	}
}

func TestRoomNumStable(t *testing.T) {
	// Deterministic: same ref → same num across calls; distinct refs → distinct nums.
	const ref ProtoRef = "midgaard:room:temple"
	first, second := roomNum(ref), roomNum(ref)
	if first != second {
		t.Fatal("roomNum is not deterministic")
	}
	if first == roomNum("midgaard:room:market") {
		t.Fatal("distinct rooms collided")
	}
}

func TestLookRoomEmitsRoomInfoOnChangeOnly(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	src := &session{character: "Mapper", out: make(chan *playv1.ServerFrame, 64)}
	e := z.newPlayerEntity(src, "Mapper")
	Move(e, z.rooms["midgaard:room:temple"])

	// First look in the temple → Room.Info emitted.
	z.lookRoom(src)
	if _, ok := drainGMCP(src)["Room.Info"]; !ok {
		t.Fatal("lookRoom did not emit Room.Info on room entry")
	}
	// Re-look the SAME room → no re-emit (change-detected).
	z.lookRoom(src)
	if _, ok := drainGMCP(src)["Room.Info"]; ok {
		t.Fatal("Room.Info re-emitted on a re-look of the same room")
	}
	// Move to a new room and look → Room.Info re-emitted with the new room.
	Move(e, z.rooms["midgaard:room:market"])
	z.lookRoom(src)
	got, ok := drainGMCP(src)["Room.Info"]
	if !ok {
		t.Fatal("Room.Info not re-emitted after a room change")
	}
	var info struct {
		Num int `json:"num"`
	}
	if err := json.Unmarshal([]byte(got), &info); err != nil {
		t.Fatalf("Room.Info after move not valid JSON: %v (%s)", err, got)
	}
	if info.Num != roomNum("midgaard:room:market") {
		t.Fatalf("Room.Info after move has num %d, want market %d", info.Num, roomNum("midgaard:room:market"))
	}
}

type gmcpItemList struct {
	Location string `json:"location"`
	Items    []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Attrib string `json:"attrib"`
	} `json:"items"`
}

func TestCharItemsInvAndRoom(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity

	// Inventory: a wearable helm (WORN → attrib "W") and a container chest (attrib "c").
	helm := z.newEntity("test:helm")
	helm.short = "a helm"
	Add(helm, wearableFor(WearLocHead))
	Add(helm, &Physical{})
	Move(helm, e)
	wr := actorWearer(e)
	wr.worn[WearLocHead] = helm // mark it worn

	chest := z.newEntity("test:chest")
	chest.short = "a chest"
	Add(chest, &Container{})
	Move(chest, e)

	// Ground: a sword in the room (an item, not a player/mob).
	sword := z.newEntity("test:sword")
	sword.short = "a sword"
	Add(sword, &Physical{})
	Move(sword, e.location)

	var inv gmcpItemList
	if err := json.Unmarshal(charItemsInvJSON(e), &inv); err != nil {
		t.Fatal(err)
	}
	if inv.Location != "inv" {
		t.Fatalf("inv location = %q", inv.Location)
	}
	byName := map[string]string{} // name → attrib
	for _, it := range inv.Items {
		byName[it.Name] = it.Attrib
	}
	if a, ok := byName["a helm"]; !ok || !contains([]string{a}, "W") || !contains([]string{a}, "w") {
		t.Errorf("helm attrib = %q, want it to include w (wearable) + W (worn)", byName["a helm"])
	}
	if a := byName["a chest"]; !contains([]string{a}, "c") {
		t.Errorf("chest attrib = %q, want c (container)", a)
	}

	var room gmcpItemList
	if err := json.Unmarshal(charItemsRoomJSON(e), &room); err != nil {
		t.Fatal(err)
	}
	if room.Location != "room" {
		t.Fatalf("room location = %q", room.Location)
	}
	var sawSword, sawSelf bool
	for _, it := range room.Items {
		if it.Name == "a sword" {
			sawSword = true
		}
		if it.Name == e.Name() {
			sawSelf = true
		}
	}
	if !sawSword {
		t.Error("room items did not include the ground sword")
	}
	if sawSelf {
		t.Error("room items leaked the player (only ground items belong)")
	}
}

// TestGMCPPayloadsStripColorTokens pins the Track-1 guard rail: content-authored names may carry
// {{TOKEN}} color markup, which only the telnet edge renders — a GMCP payload must never ship the
// literal tokens to a rich client. Covers the three name-shaped fields: Room.Info name, Char.Items
// item names, and the Char.Status combat target.
func TestGMCPPayloadsStripColorTokens(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity

	// Room name with markup → Room.Info name is the stripped text.
	e.location.short = "{{FG_CYAN}}Temple{{RESET}} Square"
	var info struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(z.roomInfoJSON(e.location), &info); err != nil {
		t.Fatal(err)
	}
	if info.Name != "Temple Square" {
		t.Errorf("Room.Info name = %q, want the tokens stripped (%q)", info.Name, "Temple Square")
	}

	// Carried item with markup → Char.Items name is stripped.
	ruby := z.newEntity("test:ruby")
	ruby.short = "{{FG_RED}}a ruby{{RESET}}"
	Add(ruby, &Physical{})
	Move(ruby, e)
	var inv gmcpItemList
	if err := json.Unmarshal(charItemsInvJSON(e), &inv); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range inv.Items {
		if it.Name == "a ruby" {
			found = true
		}
		if strings.Contains(it.Name, "{{") {
			t.Errorf("Char.Items name leaked literal markup: %q", it.Name)
		}
	}
	if !found {
		t.Error("stripped ruby not found in Char.Items")
	}

	// Combat target with markup → Char.Status target is stripped. An UNKNOWN token stays literal,
	// matching what a color-off telnet client sees (the shared-tokenizer contract).
	mob := makeMobTarget(z, e, "{{FG_GREEN}}goblin{{RESET}} {{NOSUCH}}")
	z.startFight(e, mob)
	var st struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(z.charStatusJSON(e), &st); err != nil {
		t.Fatal(err)
	}
	if st.Target != "goblin {{NOSUCH}}" {
		t.Errorf("Char.Status target = %q, want known tokens stripped + unknown literal", st.Target)
	}
}

func TestCharItemsRoomIncludesCorpse(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity

	// A live mob (excluded — it has Living) and the corpse it leaves (a Container with no Physical) in
	// the room. The corpse MUST appear so a client can see lootable remains on the ground.
	mob := makeMobTarget(z, e, "goblin")
	corpse := z.newCorpse(mob)
	Move(corpse, e.location)

	var room gmcpItemList
	if err := json.Unmarshal(charItemsRoomJSON(e), &room); err != nil {
		t.Fatal(err)
	}
	var sawCorpse, sawMob bool
	for _, it := range room.Items {
		if contains([]string{it.Name}, "corpse") {
			sawCorpse = true
			if !contains([]string{it.Attrib}, "c") {
				t.Errorf("corpse attrib = %q, want it to include c (container, so a client knows it's lootable)", it.Attrib)
			}
		}
		if it.Name == "goblin" {
			sawMob = true
		}
	}
	if !sawCorpse {
		t.Fatalf("the corpse did not appear in the room items: %+v", room.Items)
	}
	if sawMob {
		t.Error("the live mob leaked into the room ITEMS (a creature, not an item)")
	}
}

func TestSendPromptEmitsItemsOnInventoryChange(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.sendPrompt(caster) // first prompt: full Char.Items.List snapshot
	if _, ok := drainGMCP(caster)["Char.Items.List"]; !ok {
		t.Fatal("first prompt did not send the full Char.Items.List snapshot")
	}

	// No change → no delta of any kind.
	z.sendPrompt(caster)
	frames := drainGMCP(caster)
	for _, pkg := range []string{"Char.Items.List", "Char.Items.Add", "Char.Items.Remove", "Char.Items.Update"} {
		if _, ok := frames[pkg]; ok {
			t.Fatalf("%s emitted with no inventory change", pkg)
		}
	}

	// Pick up an item → an incremental Char.Items.Add delta (NOT a full-list re-send, #48).
	gem := z.newEntity("test:gem")
	gem.short = "a gem"
	Add(gem, &Physical{})
	Move(gem, caster.entity)
	z.sendPrompt(caster)
	frames = drainGMCP(caster)
	if _, ok := frames["Char.Items.List"]; ok {
		t.Fatal("a single pickup re-sent the whole Char.Items.List instead of a delta")
	}
	add, ok := frames["Char.Items.Add"]
	if !ok {
		t.Fatal("picking up an item did not emit Char.Items.Add")
	}
	if !strings.Contains(add, `"location":"inv"`) || !strings.Contains(add, `"a gem"`) {
		t.Fatalf("Char.Items.Add payload wrong: %s", add)
	}

	// Drop it → a Char.Items.Remove delta.
	Move(gem, caster.entity.location)
	z.sendPrompt(caster)
	frames = drainGMCP(caster)
	if _, ok := frames["Char.Items.Remove"]; !ok {
		t.Fatalf("dropping the item did not emit Char.Items.Remove; frames = %v", frames)
	}
}

// TestCharItemsCoalescesCount pins #26: identical discrete items GROUP into one Char.Items entry carrying
// a count, and adding a third is a same-id Char.Items.Update (count 2 → 3), never a Remove+Add churn.
func TestCharItemsCoalescesCount(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	mk := func() *Entity {
		it := z.newEntity("test:torch") // same prototype → coalesces; each gets a unique runtime id
		it.short = "a torch"
		Add(it, &Physical{})
		Move(it, e)
		return it
	}
	mk()
	mk()

	inv := invItems(e)
	var torch *gmcpItem
	for i := range inv {
		if inv[i].Name == "a torch" {
			torch = &inv[i]
		}
	}
	if torch == nil {
		t.Fatalf("no coalesced torch entry: %+v", inv)
	}
	if torch.Count != 2 {
		t.Fatalf("two identical torches should coalesce to count 2, got %d", torch.Count)
	}
	if !strings.HasPrefix(torch.ID, "g") {
		t.Fatalf("a coalesced group should carry a stable g<hash> id, got %q", torch.ID)
	}

	// Prime the diff, then add a third torch → a same-id Update to count 3 (id stable).
	z.sendPrompt(caster)
	drainGMCP(caster)
	mk()
	z.sendPrompt(caster)
	frames := drainGMCP(caster)
	upd, ok := frames["Char.Items.Update"]
	if !ok {
		t.Fatalf("a third identical item should Update the group count; frames = %v", frames)
	}
	if !strings.Contains(upd, torch.ID) || !strings.Contains(upd, `"count":3`) {
		t.Fatalf("Update should raise the same group id to count 3: %s", upd)
	}
	if _, churned := frames["Char.Items.Add"]; churned {
		t.Fatal("raising a coalesced count churned an Add instead of an Update")
	}
}

func TestCharStatsJSONOnlyFlaggedAttrs(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	src := &session{character: "Statty"}
	e := z.newPlayerEntity(src, "Statty")

	var m map[string]float64
	if err := json.Unmarshal(z.charStatsJSON(e), &m); err != nil {
		t.Fatalf("Char.Stats not valid JSON: %v", err)
	}
	// The demo flags strength/intellect/constitution/level as stats — they appear.
	for _, ref := range []string{"strength", "intellect", "constitution", "level"} {
		if _, ok := m[ref]; !ok {
			t.Errorf("Char.Stats missing flagged stat %q (payload %v)", ref, m)
		}
	}
	// Derived/internal attributes are NOT flagged — they must stay out of the stat panel.
	for _, ref := range []string{"max_hp", "accuracy", "soak_slash", "evasion"} {
		if _, ok := m[ref]; ok {
			t.Errorf("Char.Stats leaked the non-stat attribute %q", ref)
		}
	}
	if m["level"] != 1 {
		t.Errorf("level = %v, want 1", m["level"])
	}
}

func TestCharStatusJSONReflectsCombat(t *testing.T) {
	z, caster := abilityTestZone(t)

	// Standing by default.
	var st struct {
		State  string `json:"state"`
		Target string `json:"target"`
	}
	if err := json.Unmarshal(z.charStatusJSON(caster.entity), &st); err != nil {
		t.Fatalf("Char.Status (idle) not valid JSON: %v", err)
	}
	if st.State != "standing" || st.Target != "" {
		t.Fatalf("idle status = %+v, want standing + no target", st)
	}

	// Fighting a mob → state fighting + the target's name.
	mob := makeMobTarget(z, caster.entity, "goblin")
	z.startFight(caster.entity, mob)
	if err := json.Unmarshal(z.charStatusJSON(caster.entity), &st); err != nil {
		t.Fatalf("Char.Status (combat) not valid JSON: %v", err)
	}
	if st.State != "fighting" || st.Target != "goblin" {
		t.Fatalf("combat status = %+v, want fighting + goblin", st)
	}
}

// TestCharStatusTargetRoutesThroughVisibility pins #32: the Char.Status `target` name goes through the
// canSee chokepoint — an invisible opponent renders as "Someone", never its real name; holylight sees it.
func TestCharStatusTargetRoutesThroughVisibility(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	z.startFight(caster.entity, mob)

	target := func() string {
		var st struct {
			Target string `json:"target"`
		}
		if err := json.Unmarshal(z.charStatusJSON(caster.entity), &st); err != nil {
			t.Fatalf("Char.Status not valid JSON: %v", err)
		}
		return st.Target
	}

	if got := target(); got != "goblin" {
		t.Fatalf("baseline target = %q, want goblin", got)
	}
	setFlag(mob, flagInvisible, true)
	if got := target(); got != "Someone" {
		t.Fatalf("invisible opponent target = %q, want Someone (routed through nameFor)", got)
	}
	setFlag(caster.entity, flagHolylight, true)
	if got := target(); got != "goblin" {
		t.Fatalf("holylight target = %q, want the real name goblin", got)
	}
}

func TestSendPromptEmitsHUDOnChangeOnly(t *testing.T) {
	z, caster := abilityTestZone(t)
	setResourceCurrent(caster.entity, "hp", 100)

	// First prompt: the initial HUD is emitted (last-sent is empty).
	drainGMCP(caster) // clear
	z.sendPrompt(caster)
	first := drainGMCP(caster)
	if _, ok := first["Char.Vitals"]; !ok {
		t.Fatal("first sendPrompt did not emit Char.Vitals")
	}
	if _, ok := first["Char.Status"]; !ok {
		t.Fatal("first sendPrompt did not emit Char.Status")
	}

	// Second prompt, nothing changed: NO new HUD frame (only the prompt).
	z.sendPrompt(caster)
	second := drainGMCP(caster)
	if _, ok := second["Char.Vitals"]; ok {
		t.Fatal("unchanged Char.Vitals was re-emitted on the next prompt (change-detection failed)")
	}

	// HP changes → Char.Vitals re-emitted with the new value.
	setResourceCurrent(caster.entity, "hp", 55)
	z.sendPrompt(caster)
	third := drainGMCP(caster)
	v, ok := third["Char.Vitals"]
	if !ok {
		t.Fatal("a vitals change did not re-emit Char.Vitals")
	}
	var m map[string]int
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		t.Fatalf("re-emitted Char.Vitals not valid JSON: %v (%s)", err, v)
	}
	if m["hp"] != 55 {
		t.Fatalf("re-emitted Char.Vitals hp = %d, want 55", m["hp"])
	}
}

func TestSendPromptReEmitsStatusOnStateChange(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.sendPrompt(caster)
	drainGMCP(caster) // clear the initial HUD

	// Enter combat: vitals unchanged, but Char.Status changes (standing → fighting) and must re-emit.
	mob := makeMobTarget(z, caster.entity, "goblin")
	z.startFight(caster.entity, mob)
	z.sendPrompt(caster)
	got := drainGMCP(caster)
	st, ok := got["Char.Status"]
	if !ok {
		t.Fatal("entering combat did not re-emit Char.Status")
	}
	if !json.Valid([]byte(st)) || !contains([]string{st}, "fighting") {
		t.Fatalf("Char.Status after entering combat = %q, want fighting", st)
	}
}

func TestReconnectReprimesHUD(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.sendPrompt(caster)
	drainGMCP(caster) // initial HUD sent; lastVitals/lastStatus now populated

	// A reconnect reuses the same session but a NEW gate connection with no HUD state. The re-attach
	// handler clears the change-detection buffers (zone.go) so the next prompt re-primes the HUD even
	// though vitals are unchanged — this asserts that contract directly.
	caster.lastVitals, caster.lastStatus = nil, nil
	z.sendPrompt(caster)
	got := drainGMCP(caster)
	if _, ok := got["Char.Vitals"]; !ok {
		t.Fatal("after a HUD-buffer clear (reconnect), Char.Vitals was not re-primed")
	}
	if _, ok := got["Char.Status"]; !ok {
		t.Fatal("after a HUD-buffer clear (reconnect), Char.Status was not re-primed")
	}
}

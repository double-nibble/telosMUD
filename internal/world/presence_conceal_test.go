package world

import (
	"strings"
	"testing"
)

// presence_conceal_test.go — #100: the mundane-stealth concealment flag (flagHidden, pierced by
// flagSenseHidden) routed through visibleTo, and FULL presence-event concealment (actConceal): a presence
// line — arrival/departure — is SUPPRESSED ENTIRELY for a viewer who can't see the actor, not rendered as
// the leaky "Someone arrives."

// TestHiddenConcealment: a hidden target is concealed from an ordinary viewer; sense_hidden and holylight
// pierce it; detect_invis (the MAGICAL sense) deliberately does not.
func TestHiddenConcealment(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	thief := harmPlayer(z, room, "Thief")

	if !visibleTo(viewer, thief) {
		t.Fatal("baseline: an unconcealed target is visible")
	}
	setFlag(thief, flagHidden, true)
	if visibleTo(viewer, thief) {
		t.Fatal("a hidden target must be concealed from an ordinary viewer")
	}
	if !visibleTo(thief, thief) {
		t.Fatal("a hidden actor must always perceive itself")
	}

	setFlag(viewer, flagSenseHidden, true)
	if !visibleTo(viewer, thief) {
		t.Fatal("sense_hidden must pierce a hidden target")
	}
	setFlag(viewer, flagSenseHidden, false)

	setFlag(viewer, flagHolylight, true)
	if !visibleTo(viewer, thief) {
		t.Fatal("holylight must see a hidden target")
	}
	setFlag(viewer, flagHolylight, false)

	// detect_invis pierces MAGICAL invisibility, never mundane hiding — the two senses are distinct.
	setFlag(viewer, flagDetectInvis, true)
	if visibleTo(viewer, thief) {
		t.Fatal("detect_invis must NOT pierce mundane flagHidden (it is a distinct sense)")
	}
}

// TestPresenceConcealmentSuppressesLine is the headline #100 behavior: actConceal delivers a presence line to
// a viewer who CAN see the actor, and delivers NOTHING (not "Someone arrives.") to one who can't.
func TestPresenceConcealmentSuppressesLine(t *testing.T) {
	z, _, room := harmZone(t)
	actor := harmPlayer(z, room, "Actor")
	harmPlayer(z, room, "Observer")
	obs := z.players["Observer"]

	// Visible actor → the arrival is announced with the real name.
	drainOutputs(obs)
	z.actConceal("$n arrives.", actor, ToRoom)
	if got := strings.Join(drainOutputs(obs), ""); !strings.Contains(got, "Actor arrives") {
		t.Fatalf("a visible actor's arrival must be announced by name, got %q", got)
	}

	// Hidden actor → the whole line is suppressed for the ordinary observer (no leaky "Someone").
	setFlag(actor, flagHidden, true)
	drainOutputs(obs)
	z.actConceal("$n arrives.", actor, ToRoom)
	if got := drainOutputs(obs); len(got) != 0 {
		t.Fatalf("a hidden actor's arrival must be fully suppressed, got %v", got)
	}

	// A sense_hidden observer perceives the hidden actor, so the line is delivered again.
	setFlag(z.players["Observer"].entity, flagSenseHidden, true)
	drainOutputs(obs)
	z.actConceal("$n arrives.", actor, ToRoom)
	if got := strings.Join(drainOutputs(obs), ""); !strings.Contains(got, "Actor arrives") {
		t.Fatalf("a sense_hidden observer should see the hidden actor's arrival, got %q", got)
	}
}

// TestPresenceConcealmentInDarkRoom: darkness composes with presence concealment through the same canSee
// predicate — a dark-blind observer hears no arrival line, closing the "Someone arrives." dark-room leak the
// #99 review flagged as residual.
func TestPresenceConcealmentInDarkRoom(t *testing.T) {
	z, _, room := harmZone(t)
	actor := harmPlayer(z, room, "Actor")
	harmPlayer(z, room, "Observer")
	obs := z.players["Observer"]

	markRoomFlag(room, flagDark) // the observer is now dark-blind (no light, no infravision)
	drainOutputs(obs)
	z.actConceal("$n arrives.", actor, ToRoom)
	if got := drainOutputs(obs); len(got) != 0 {
		t.Fatalf("a dark-blind observer must not receive an arrival line, got %v", got)
	}
}

// TestPlainActStillRendersSomeone pins that the NON-presence path is unchanged: a plain act() (sound — a say)
// to a viewer who can't see the actor still delivers the line as "Someone", masking identity but not presence.
// Only presence lines (actConceal) suppress entirely; you still hear a disembodied voice in the dark.
func TestPlainActStillRendersSomeone(t *testing.T) {
	z, _, room := harmZone(t)
	actor := harmPlayer(z, room, "Actor")
	harmPlayer(z, room, "Observer")
	obs := z.players["Observer"]

	setFlag(actor, flagHidden, true)
	drainOutputs(obs)
	z.act("$n says, '$t'", actor, nil, nil, "hi", "", ToRoom)
	got := strings.Join(drainOutputs(obs), "")
	if !strings.Contains(got, "Someone") || strings.Contains(got, "Actor") {
		t.Fatalf("plain act() to an unseen actor should render 'Someone', not the name, got %q", got)
	}
}
